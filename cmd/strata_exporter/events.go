package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The event ring: one record per model call, pullable by a consumer that wants
// history rather than a rate.
//
// /metrics answers "what is this box doing right now". It cannot answer "what
// did the 400th call last night cost", because a Prometheus series is a
// derivative and the individual call is gone by the next scrape. Strata already
// keeps its own recent-request list, so this is that list turned into a ring
// with a cursor, which means a harvester can poll it and never lose or double
// count a call.
//
// Field names are deliberately the same ones rooter uses on /rooter/events
// wherever the two describe the same quantity, because the point of having both
// surfaces is to compare what the proxy saw with what the engine saw, and that
// comparison is impossible if the same measurement is spelled two ways. Where
// they measure different things the names stay different, and say so.
type engineEvent struct {
	// ID is Strata's own per-request id. It is the dedup key, so a consumer that
	// reads the same call twice (this ring, then a re-poll after a restart) folds
	// it into one row.
	//
	// It is NOT the id in the response body: Strata answers a chat completion with
	// "chatcmpl-23a156df..." while calling the same call "008f61fddf93" here. Rooter
	// publishes both sides of that, so the join is its problem, not this record's.
	ID string `json:"id"`

	// TS is when the engine clocked the call, in UTC RFC3339Nano.
	TS string `json:"ts"`
	// TSEpoch is the same instant as a float, because the engine's own clock is a
	// float and losing its sub-second part would break a join against a window.
	TSEpoch float64 `json:"ts_epoch"`

	// Model is the model the engine actually has loaded. Every call in a snapshot
	// comes from one engine, so this is stamped from the snapshot rather than
	// carried per request - and it is the reason a single-model box still yields
	// per-model rows.
	Model string `json:"model"`
	// Engine names the runtime, so a record is attributable without knowing which
	// deployment produced it.
	Engine string `json:"engine"`

	// PromptTokens is the full prompt length as the engine counted it, including
	// tokens it did not have to evaluate. Same meaning as rooter's field of the
	// same name: this is the denominator of a cache rate.
	PromptTokens *int `json:"prompt_tokens,omitempty"`
	// PromptTokensComputed is what the engine actually read and evaluated this
	// pass, i.e. the work. It is Strata's `prompt_read`, and it is the same
	// quantity as rooter's `prompt_tokens_computed` (which comes from
	// `timings.prompt_n`). It is never a prompt length.
	PromptTokensComputed *int `json:"prompt_tokens_computed,omitempty"`
	// PromptTokensTotal is the engine's own `prompt_total`, kept verbatim so the
	// arithmetic above can be audited rather than trusted.
	PromptTokensTotal *int `json:"prompt_tokens_total,omitempty"`
	// CachedTokens is Strata's `reused`: prompt tokens served out of the KV /
	// conversation cache.
	CachedTokens     *int `json:"cached_tokens,omitempty"`
	CompletionTokens *int `json:"completion_tokens,omitempty"`
	// EngineGenerated is the engine's own count of generated tokens before any
	// accounting it does afterwards; it differs from CompletionTokens when a run
	// was re-rolled.
	EngineGenerated *int `json:"engine_generated,omitempty"`

	// PrefillMs / DecodeMs / TotalMs are the engine's timings. Rooter measures the
	// same spans from outside and gets a larger number by however long the queue
	// and the wire took; both are kept by their own sides on purpose.
	PrefillMs *float64 `json:"prefill_ms,omitempty"`
	DecodeMs  *float64 `json:"decode_ms,omitempty"`
	TotalMs   *float64 `json:"total_ms,omitempty"`

	PrefillTokS *float64 `json:"prefill_tok_s,omitempty"`
	DecodeTokS  *float64 `json:"decode_tok_s,omitempty"`

	// EngineFirstTokenMs is Strata's `first_engine_token_s`. It is neither
	// rooter's `ttft_ms` (first byte on the wire, which includes queueing) nor a
	// delivery measurement at all: it is internal to the engine, from the moment
	// it began the call to the moment it produced a token. It is published under
	// its own name so it cannot be mistaken for either of the other two.
	EngineFirstTokenMs *float64 `json:"engine_first_token_ms,omitempty"`

	// CacheHitRate is cached/prompt, refused (nil) unless 0 <= cached <= prompt.
	// The same veto rooter applies: a runner reporting 90000 cached against 45000
	// prompt has measured something else, and inventing a rate out of that is how
	// a dashboard ends up lying.
	CacheHitRate *float64 `json:"cache_hit_rate,omitempty"`

	// FinishReason is how the engine ended the call.
	FinishReason string `json:"finish_reason,omitempty"`
	// Error is set when the engine reported a failed call; empty otherwise.
	Error string `json:"error,omitempty"`

	// Extra is the engine's remaining per-request detail, verbatim. The rule for
	// what goes here rather than becoming a named column: it is something the
	// engine invented, so naming it here would be minting a vocabulary that no
	// other surface shares. Draft acceptance, PCIe spill and reasoning recovery
	// are all engine-specific measurements.
	Extra map[string]any `json:"extra,omitempty"`
}

// ---------------------------------------------------------------------------
// ring
// ---------------------------------------------------------------------------

// eventRing holds finished calls, oldest first, with a monotonic sequence so a
// consumer can ask for "everything after N" without trusting anybody's clock.
//
// Deduplication is by the engine's request id, not by the sequence: Strata's
// recent list is re-read on every poll, so the same twelve calls come back over
// and over until they age out. A poll that changed nothing must not move the
// cursor, and a call whose record the engine revises is updated in place rather
// than inserted again.
type eventRing struct {
	mu      sync.Mutex
	events  []ringEvent
	index   map[string]int // engine id -> position in events
	seq     int64
	dropped int64
	max     int
}

type ringEvent struct {
	Seq int64        `json:"seq"`
	Rec *engineEvent `json:"rec"`
}

func newEventRing(max int) *eventRing {
	if max <= 0 {
		max = 2048
	}
	if max < 16 {
		max = 16
	}
	return &eventRing{max: max, index: map[string]int{}}
}

// push adds or updates one call, returning the sequence it now sits at.
//
// Re-inserting a known id updates the stored record without allocating a new
// sequence number, so a consumer holding a cursor sees no movement from a poll
// that changed nothing, and a record the engine corrects is not counted twice.
func (e *eventRing) push(rec *engineEvent) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	if pos, ok := e.index[rec.ID]; ok {
		e.events[pos].Rec = rec
		return e.events[pos].Seq
	}

	e.seq++
	e.events = append(e.events, ringEvent{Seq: e.seq, Rec: rec})
	e.index[rec.ID] = len(e.events) - 1

	if len(e.events) > e.max {
		// Drop the oldest half in one go rather than one record at a time:
		// rebuilding the index on every push is real work on a box whose whole
		// job is not getting in the way of a GPU.
		keep := e.max / 2
		drop := len(e.events) - keep
		for i := 0; i < drop; i++ {
			delete(e.index, e.events[i].Rec.ID)
		}
		copy(e.events, e.events[drop:])
		for i := range e.events[:keep] {
			e.index[e.events[i].Rec.ID] = i
		}
		e.events = e.events[:keep]
		e.dropped += int64(drop)
	}
	return e.seq
}

// since returns up to limit records with seq > after, oldest first, along with
// the cursor they end at and the total ever dropped.
func (e *eventRing) since(after int64, limit int) ([]*engineEvent, int64, int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if limit <= 0 || limit > e.max {
		limit = e.max
	}
	out := make([]*engineEvent, 0, limit)
	cur := after
	for _, ev := range e.events {
		if ev.Seq <= after {
			continue
		}
		out = append(out, ev.Rec)
		cur = ev.Seq
		if len(out) >= limit {
			break
		}
	}
	return out, cur, e.dropped
}

func (e *eventRing) len() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.events)
}

// stale says whether a consumer's cursor points behind something the ring has
// already evicted.
//
// It is judged against the oldest surviving record rather than against "the page
// came back empty", which is how it was first written and which is wrong in both
// directions: a consumer that is perfectly caught up gets an empty page and would
// be told it had missed data, while a consumer whose cursor was behind the
// eviction boundary but happened to have new records after it would be told
// nothing. A gap is a property of the ring and the cursor, not of the page.
func (e *eventRing) stale(after int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if after <= 0 {
		// A consumer that never had a cursor is starting fresh, not losing its place.
		return false
	}
	if len(e.events) == 0 {
		return e.dropped > 0
	}
	return e.events[0].Seq > after+1
}

// eventsPage is the pull response. It is the same envelope rooter serves, field
// for field, so one harvester can read both rings by speaking one protocol;
// only `events[]` differs, because the two surfaces genuinely measure different
// things.
//
// `stale` says the cursor asked for something the ring has already evicted, so
// "nothing new" is distinguishable from "you missed it" without guessing from an
// empty list.
type eventsPage struct {
	Cursor    int64          `json:"cursor"`
	Stale     bool           `json:"stale"`
	Dropped   int64          `json:"dropped_total"`
	Available int            `json:"available"`
	BootID    string         `json:"boot_id"`
	Events    []*engineEvent `json:"events"`
}

// ---------------------------------------------------------------------------
// turning a snapshot into records
// ---------------------------------------------------------------------------

// eventsFromSnapshot converts the recent-request list of one snapshot into
// records. Everything a call's identity needs comes from the snapshot itself,
// so this is a pure function and the tests do not need an HTTP server.
func eventsFromSnapshot(s *snapshot) []*engineEvent {
	out := make([]*engineEvent, 0, len(s.Requests))
	for i := range s.Requests {
		r := s.Requests[i]
		// An entry without an id cannot be deduplicated, and re-inserting the
		// same call on every poll would look like traffic. Drop it instead: a
		// ring that double counts is worse than one that under counts, because
		// the under-count is visible in `dropped_total` and the double count is
		// invisible in every query.
		if r.ID == "" {
			continue
		}
		out = append(out, eventFromRequest(s, &r))
	}
	return out
}

func eventFromRequest(s *snapshot, r *request) *engineEvent {
	ev := &engineEvent{
		ID:                   r.ID,
		Model:                s.Engine.Model,
		Engine:               s.Engine.Engine,
		PromptTokens:         r.PromptTokens,
		PromptTokensComputed: r.PromptRead,
		PromptTokensTotal:    r.PromptTotal,
		CachedTokens:         r.Reused,
		CompletionTokens:     r.OutputTokens,
		EngineGenerated:      r.EngineGenerated,
		PrefillMs:            round(r.PromptMs),
		DecodeMs:             round(r.DecodeMs),
		DecodeTokS:           round(r.DecodeTokS),
		FinishReason:         r.Finish,
	}
	if r.Time > 0 {
		ev.TS = time.Unix(int64(r.Time), int64((r.Time-float64(int64(r.Time)))*1e9)).UTC().Format(time.RFC3339Nano)
		ev.TSEpoch = r.Time
	}
	if r.DurationS != nil {
		v := round3(*r.DurationS * 1000)
		ev.TotalMs = &v
	}
	if r.FirstEngineTokenS != nil {
		v := round3(*r.FirstEngineTokenS * 1000)
		ev.EngineFirstTokenMs = &v
	}
	// Prefill rate is not something Strata reports per request, only as a mean
	// across recent ones, so it is computed here from the two numbers that are
	// per-request. It is labelled as such rather than passed off as measured.
	if r.PromptRead != nil && r.PromptMs != nil && *r.PromptMs > 0 && *r.PromptRead > 0 {
		v := round3(float64(*r.PromptRead) / (*r.PromptMs / 1000))
		ev.PrefillTokS = &v
	}
	if r.PromptTokens != nil && r.Reused != nil && *r.PromptTokens > 0 && *r.Reused >= 0 && *r.Reused <= *r.PromptTokens {
		v := round3(float64(*r.Reused) / float64(*r.PromptTokens))
		ev.CacheHitRate = &v
	}
	if r.Finish != "" && r.Finish != "stop" && r.Finish != "length" {
		// A call that ended in anything else did fail in some way the engine
		// named, and a consumer counting failures should not have to keep a list
		// of engine-specific finish reasons to recognise that.
		ev.Error = "finish=" + r.Finish
	}

	// The engine's own detail, kept as-is. See the note on Extra.
	x := map[string]any{}
	if r.HitRate != nil {
		x["hit_rate"] = *r.HitRate
	}
	if r.DraftsOffered != nil {
		x["drafts_offered"] = *r.DraftsOffered
	}
	if r.DraftsAccepted != nil {
		x["drafts_accepted"] = *r.DraftsAccepted
	}
	if r.ReasoningRecoveries != nil {
		x["reasoning_recoveries"] = *r.ReasoningRecoveries
	}
	if r.ReasoningRepeatCoverage != nil {
		x["reasoning_repeat_coverage"] = *r.ReasoningRepeatCoverage
	}
	if r.PCIEShare != nil {
		x["pcie_share"] = *r.PCIEShare
	}
	if r.RAMBlobs != nil {
		x["ram_blobs"] = *r.RAMBlobs
	}
	if r.FileBlobs != nil {
		x["file_blobs"] = *r.FileBlobs
	}
	if r.FileMB != nil {
		x["file_mb"] = *r.FileMB
	}
	if r.Projection != nil {
		x["projection"] = r.Projection
	}
	if len(x) > 0 {
		ev.Extra = x
	}
	return ev
}

// ---------------------------------------------------------------------------
// serving
// ---------------------------------------------------------------------------

// eventsJSON serves the ring. A consumer stores the cursor it is handed back and
// asks for everything after it; the record id is the dedup key, the cursor is
// only a position.
func (e *eventRing) handler(bootID string, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if token != "" {
			got := bearerToken(req)
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="strata-events"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		after := int64(0)
		if c := req.URL.Query().Get("cursor"); c != "" {
			v, err := strconv.ParseInt(c, 10, 64)
			if err != nil {
				http.Error(w, "cursor must be a number", http.StatusBadRequest)
				return
			}
			after = v
		}
		limit := 0
		if l := req.URL.Query().Get("limit"); l != "" {
			v, err := strconv.Atoi(l)
			if err != nil || v < 0 {
				http.Error(w, "limit must be a number", http.StatusBadRequest)
				return
			}
			limit = v
		}
		model := req.URL.Query().Get("model")
		failedOnly := req.URL.Query().Get("failed") == "1"

		recs, cursor, dropped := e.since(after, limit)
		out := make([]*engineEvent, 0, len(recs))
		for _, ev := range recs {
			if model != "" && ev.Model != model {
				continue
			}
			if failedOnly && ev.Error == "" {
				continue
			}
			out = append(out, ev)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(eventsPage{
			Cursor:    cursor,
			Dropped:   dropped,
			Available: e.len(),
			BootID:    bootID,
			Stale:     e.stale(after),
			Events:    out,
		})
	}
}

func bearerToken(req *http.Request) string {
	h := req.Header.Get("Authorization")
	if len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	return ""
}

// newBootID identifies this process, so a consumer that sees a fresh boot_id
// knows the ring restarted and its cursor is from a life that no longer exists.
func newBootID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 16)
}

// ---------------------------------------------------------------------------
// the poller
// ---------------------------------------------------------------------------

// poller feeds the ring out of band from /metrics.
//
// It exists because the ring's consumer is not Prometheus: a harvester polling
// every few seconds should not have its view of history depend on how often a
// scrape happened to run, and /metrics must keep scraping live so a failed
// scrape still answers 503 rather than serving something plausible-looking.
// That means the snapshot is fetched twice at a low rate; Strata answers it from
// memory, which is a cheaper mistake than either coupling.
type poller struct {
	sc       *scraper
	ring     *eventRing
	interval time.Duration
	logf     func(string, ...any)
}

func (p *poller) run(stop <-chan struct{}) {
	tick := time.NewTicker(p.interval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			p.once()
		}
	}
}

func (p *poller) once() int {
	snap, err := p.sc.snapshot()
	if err != nil {
		p.logf("events poll: %v", err)
		return 0
	}
	evs := eventsFromSnapshot(snap)
	for _, ev := range evs {
		p.ring.push(ev)
	}
	return len(evs)
}

// round3 keeps a derived float from trailing binary noise, which is what makes a
// record a consumer can compare against itself. The proxy surface rounds the same
// way; two surfaces printing 0.9995054901828563 and 0.9995 for the same quantity
// is how a join starts to look like a mismatch.
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// round applies round3 to a pointer, leaving an unmeasured value unmeasured
// rather than publishing it as zero.
func round(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := round3(*f)
	return &v
}
