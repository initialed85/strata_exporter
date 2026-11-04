package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ptrF(v float64) *float64 { return &v }
func ptrI(v int) *int         { return &v }

// decodePage reads an /events answer back into the envelope struct, so the tests
// assert against the same shape a consumer parses rather than against a string
// search that passes when the field is spelled right but nested wrong.
func decodePage(t *testing.T, body string) eventsPage {
	t.Helper()
	var p eventsPage
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("envelope is not the JSON shape the consumer expects: %v", err)
	}
	return p
}

func get(t *testing.T, h http.HandlerFunc, target string, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// TestRingDedupsByEngineID is the whole reason the ring is safe to poll on a
// timer: Strata re-reports the same recent calls on every snapshot, and a
// consumer that counted them again would invent traffic.
func TestRingDedupsByEngineID(t *testing.T) {
	ring := newEventRing(64)
	snap := &snapshot{
		Requests: []request{
			{ID: "aaa", Time: 1793752131.355, PromptTokens: ptrI(1000), Reused: ptrI(900), OutputTokens: ptrI(10)},
		},
	}
	first := ring.push(eventsFromSnapshot(snap)[0])
	after := ring.len()

	// Twenty more polls of the identical call, then a poll that differs only in
	// the engine's own timestamp bookkeeping.
	for i := 0; i < 20; i++ {
		ring.push(eventsFromSnapshot(snap)[0])
	}
	snap.Requests[0].PromptTokens = ptrI(1000)
	ring.push(eventsFromSnapshot(snap)[0])

	if ring.len() != after {
		t.Fatalf("re-polling the same call grew the ring: %d -> %d", after, ring.len())
	}
	cur, _, _ := ring.since(0, 0)
	if len(cur) != 1 {
		t.Fatalf("expected one record, got %d", len(cur))
	}
	if _, cursor, _ := ring.since(first, 0); cursor != first {
		t.Fatalf("a poll that changed nothing moved the cursor: %d -> %d", first, cursor)
	}
}

// TestRingCursorOnlyAdvancesOnNewWork is the contract the harvester builds on:
// a poll with nothing new must return an empty page and the same cursor, so it
// can insert nothing without having to compare records itself.
func TestRingCursorOnlyAdvancesOnNewWork(t *testing.T) {
	ring := newEventRing(64)
	boot := "deadbeef01"
	h := ring.handler(boot, "")

	ring.push(&engineEvent{ID: "one", Model: "m"})
	p1 := decodePage(t, get(t, h, "/events", "").Body.String())
	if p1.Cursor != 1 || len(p1.Events) != 1 || p1.Events[0].ID != "one" {
		t.Fatalf("first pull wrong: %+v", p1)
	}
	if p1.BootID != boot {
		t.Fatalf("boot_id missing: %+v", p1)
	}

	// Same cursor, nothing new.
	p2 := decodePage(t, get(t, h, "/events?cursor="+strconv.FormatInt(p1.Cursor, 10), "").Body.String())
	if len(p2.Events) != 0 || p2.Cursor != p1.Cursor || p2.Stale {
		t.Fatalf("a no-change poll should be empty and not stale: %+v", p2)
	}

	ring.push(&engineEvent{ID: "two", Model: "m"})
	p3 := decodePage(t, get(t, h, "/events?cursor="+strconv.FormatInt(p2.Cursor, 10), "").Body.String())
	if len(p3.Events) != 1 || p3.Events[0].ID != "two" || p3.Cursor != 2 {
		t.Fatalf("expected only the new call: %+v", p3)
	}
}

// TestRingDropsAndReportsStale covers the difference between "you are caught up"
// and "you missed it", which an empty list alone cannot express.
func TestRingDropsAndReportsStale(t *testing.T) {
	ring := newEventRing(16) // evicts the oldest half at 16
	boot := "abcd"
	h := ring.handler(boot, "")

	for i := 0; i < 40; i++ {
		ring.push(&engineEvent{ID: "id" + strconv.Itoa(i), Model: "m"})
	}
	page := decodePage(t, get(t, h, "/events?cursor=1", "").Body.String())
	if page.Dropped == 0 {
		t.Fatal("a ring that evicted records must report dropped_total")
	}
	if page.Available != ring.len() {
		t.Fatalf("available %d does not match the ring %d", page.Available, ring.len())
	}
	// The oldest surviving record is what tells "you missed it" from "you are
	// caught up". A cursor that sits behind it is a gap; a cursor at the end is
	// not, and must not be reported as one.
	first := ring.events[0].Seq
	missed := decodePage(t, get(t, h, "/events?cursor="+strconv.FormatInt(first-2, 10), "").Body.String())
	if !missed.Stale {
		t.Fatalf("a cursor behind the oldest retained record (seq %d) must be stale: %+v", first, missed)
	}
	caught := decodePage(t, get(t, h, "/events?cursor="+strconv.FormatInt(ring.seq, 10), "").Body.String())
	if caught.Stale {
		t.Fatalf("a cursor at the end of the ring is 'nothing new', not 'stale': %+v", caught)
	}
	if len(caught.Events) != 0 {
		t.Fatalf("a caught-up cursor returned events: %+v", caught.Events)
	}
	_ = page
}

// TestRingGating: 401 when a token is configured, 200 when it is not, and 401
// rather than 403 for a wrong one so a probe cannot tell the two apart.
func TestRingGating(t *testing.T) {
	ring := newEventRing(16)
	ring.push(&engineEvent{ID: "x", Model: "m"})

	gated := ring.handler("boot", "sekrit")
	if rec := get(t, gated, "/events", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("gated ring with no token: %d, want 401", rec.Code)
	}
	if rec := get(t, gated, "/events", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("gated ring with a wrong token: %d, want 401", rec.Code)
	}
	rec := get(t, gated, "/events", "sekrit")
	if rec.Code != http.StatusOK {
		t.Fatalf("gated ring with the right token: %d, want 200", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("an event page must not be cacheable")
	}

	open := ring.handler("boot", "")
	if rec := get(t, open, "/events", ""); rec.Code != http.StatusOK {
		t.Fatalf("open ring: %d, want 200", rec.Code)
	}
}

func TestRingRejectsGarbageCursors(t *testing.T) {
	ring := newEventRing(16)
	h := ring.handler("boot", "")
	for _, target := range []string{"/events?cursor=abc", "/events?limit=-5", "/events?limit=x"} {
		if rec := get(t, h, target, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", target, rec.Code)
		}
	}
}

// TestEventFieldMapping is the contract with the telemetry store: the engine's
// vocabulary has to land in the names the proxy surface already uses, and the
// three prompt numbers have to keep their distinct meanings.
func TestEventFieldMapping(t *testing.T) {
	snap := &snapshot{
		Engine: struct {
			Model                  string `json:"model"`
			Engine                 string `json:"engine"`
			Images                 bool   `json:"images"`
			Context                int    `json:"context"`
			MaxContext             int    `json:"max_context"`
			VramFreeMiB            int    `json:"vram_free_mib"`
			ArenaMiB               int    `json:"arena_mib"`
			ExpertSlots            int    `json:"expert_slots"`
			ExpertCacheMiB         int    `json:"expert_cache_mib"`
			ConversationCacheMiB   int    `json:"conversation_cache_mib"`
			ConversationCacheSlots int    `json:"conversation_cache_slots"`
			PoolWorkers            int    `json:"pool_workers"`
		}{Model: "qwen3.8-flash-next", Engine: "0.1.51"},
		Requests: []request{
			{
				ID:                "008f61fddf93",
				Time:              1793771120.7364929,
				DurationS:         ptrF(6.8),
				Finish:            "stop",
				PromptTokens:      ptrI(88977),
				Reused:            ptrI(88933),
				PromptRead:        ptrI(44),
				PromptTotal:       ptrI(88977),
				OutputTokens:      ptrI(390),
				FirstEngineTokenS: ptrF(0.558),
				PromptMs:          ptrF(517.1),
				DecodeMs:          ptrF(6271.7),
				DecodeTokS:        ptrF(62.2),
				HitRate:           ptrF(0.872),
				DraftsOffered:     ptrI(366),
				DraftsAccepted:    ptrI(222),
			},
		},
	}
	ev := eventsFromSnapshot(snap)[0]

	if ev.ID != "008f61fddf93" || ev.Model != "qwen3.8-flash-next" || ev.Engine != "0.1.51" {
		t.Fatalf("identity fields wrong: %+v", ev)
	}
	if ev.PromptTokens == nil || *ev.PromptTokens != 88977 {
		t.Fatalf("prompt_tokens should be the full length: %v", ev.PromptTokens)
	}
	if ev.PromptTokensComputed == nil || *ev.PromptTokensComputed != 44 {
		t.Fatalf("prompt_tokens_computed should be prompt_read, the work done: %v", ev.PromptTokensComputed)
	}
	if ev.CachedTokens == nil || *ev.CachedTokens != 88933 {
		t.Fatalf("cached_tokens should be reused: %v", ev.CachedTokens)
	}
	// The engine's own arithmetic has to be reproducible from the published
	// numbers, or the join against the proxy surface is unverifiable.
	if *ev.CachedTokens+*ev.PromptTokensComputed != *ev.PromptTokens {
		t.Fatalf("cached+computed != prompt: %d+%d != %d", *ev.CachedTokens, *ev.PromptTokensComputed, *ev.PromptTokens)
	}
	if ev.CompletionTokens == nil || *ev.CompletionTokens != 390 {
		t.Fatalf("completion_tokens should be output_tokens: %v", ev.CompletionTokens)
	}
	if ev.TotalMs == nil || *ev.TotalMs != 6800 {
		t.Fatalf("total_ms should be duration_s*1000: %v", ev.TotalMs)
	}
	if ev.EngineFirstTokenMs == nil || *ev.EngineFirstTokenMs != 558 {
		t.Fatalf("engine_first_token_ms should be first_engine_token_s*1000: %v", ev.EngineFirstTokenMs)
	}
	if ev.PrefillMs == nil || *ev.PrefillMs != 517.1 {
		t.Fatalf("prefill_ms should be prompt_ms: %v", ev.PrefillMs)
	}
	if ev.PrefillTokS == nil || *ev.PrefillTokS < 84 || *ev.PrefillTokS > 86 {
		t.Fatalf("prefill_tok_s should be prompt_read over prompt_ms: %v", ev.PrefillTokS)
	}
	if ev.CacheHitRate == nil || *ev.CacheHitRate < 0.999 || *ev.CacheHitRate > 1 {
		t.Fatalf("cache_hit_rate should be reused/prompt: %v", ev.CacheHitRate)
	}
	if !strings.HasPrefix(ev.TS, "2026-") || !strings.HasSuffix(ev.TS, "Z") {
		t.Fatalf("ts should be UTC RFC3339 of the engine's epoch: %q", ev.TS)
	}
	if ev.TSEpoch != 1793771120.7364929 {
		t.Fatalf("ts_epoch must keep the engine's sub-second value for a window join: %v", ev.TSEpoch)
	}
	if ev.Extra == nil {
		t.Fatal("engine-specific detail should survive verbatim in extra")
	}
	raw, _ := json.Marshal(ev.Extra)
	for _, want := range []string{`"drafts_accepted":222`, `"drafts_offered":366`, `"hit_rate":0.872`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("extra should carry %s verbatim, got %s", want, raw)
		}
	}
	// A normal finish is not an error.
	if ev.Error != "" {
		t.Fatalf("finish=stop must not be reported as an error: %q", ev.Error)
	}
}

// TestCacheRateVetoedWhenImpossible: a runner that reports more cached than
// prompt measured something else. Publishing the ratio would be publishing a
// number over 100%, and the honest answer is "no rate".
func TestCacheRateVetoedWhenImpossible(t *testing.T) {
	snap := &snapshot{Requests: []request{{ID: "x", PromptTokens: ptrI(45000), Reused: ptrI(90000)}}}
	ev := eventsFromSnapshot(snap)[0]
	if ev.CacheHitRate != nil {
		t.Fatalf("expected no rate, got %v", *ev.CacheHitRate)
	}
	if ev.PromptTokens == nil || *ev.PromptTokens != 45000 {
		t.Fatal("the raw counts must still be published; only the ratio is refused")
	}
}

// TestEntriesWithoutIDAreDropped: without an id there is nothing to dedup on, and
// a ring that double counts is invisible in every query.
func TestEntriesWithoutIDAreDropped(t *testing.T) {
	snap := &snapshot{Requests: []request{{ID: "", PromptTokens: ptrI(5)}, {ID: "ok", PromptTokens: ptrI(5)}}}
	evs := eventsFromSnapshot(snap)
	if len(evs) != 1 || evs[0].ID != "ok" {
		t.Fatalf("expected only the identified call, got %+v", evs)
	}
}

// TestFailedFilter: a consumer that only wants the bad calls should not have to
// keep a list of engine-specific finish reasons to recognise them.
func TestFailedFilter(t *testing.T) {
	ring := newEventRing(16)
	h := ring.handler("boot", "")
	ring.push(&engineEvent{ID: "good", Model: "m", FinishReason: "stop"})
	ring.push(&engineEvent{ID: "bad", Model: "m", FinishReason: "abort", Error: "finish=abort"})

	page := decodePage(t, get(t, h, "/events?failed=1", "").Body.String())
	if len(page.Events) != 1 || page.Events[0].ID != "bad" {
		t.Fatalf("failed=1 returned %+v", page.Events)
	}
	byModel := decodePage(t, get(t, h, "/events?model=other", "").Body.String())
	if len(byModel.Events) != 0 {
		t.Fatalf("model filter leaked: %+v", byModel.Events)
	}
}

// TestBootIDDistinguishesLives is what stops a harvester from resuming a cursor
// into a ring that has forgotten everything it knew.
func TestBootIDDistinguishesLives(t *testing.T) {
	a, b := newBootID(), newBootID()
	if a == b {
		t.Fatal("two processes must not share a boot id")
	}
	if len(a) != 12 {
		t.Fatalf("boot id should be 12 hex characters like the proxy surface's, got %q", a)
	}
}

// TestPollerFeedsRing exercises the out-of-band path end to end against a fake
// engine, so a green test suite means the harvester's source is actually being
// filled rather than merely servable.
func TestPollerFeedsRing(t *testing.T) {
	var hits int
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer engine-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		if hits == 1 {
			w.Write([]byte(`{"engine":{"model":"m"},"requests":[{"id":"one","time":1793752131.35,"prompt_tokens":10,"reused":5,"output_tokens":2}]}`))
			return
		}
		w.Write([]byte(`{"engine":{"model":"m"},"requests":[{"id":"one","time":1793752131.35,"prompt_tokens":10,"reused":5,"output_tokens":2},{"id":"two","time":1793752132.35,"prompt_tokens":20,"reused":0,"output_tokens":4}]}`))
	}))
	defer fake.Close()

	sc := &scraper{client: fake.Client(), strataURL: fake.URL, token: "engine-token"}
	ring := newEventRing(64)
	p := &poller{sc: sc, ring: ring, interval: 20 * time.Millisecond, logf: func(string, ...any) {}}
	stop := make(chan struct{})
	go p.run(stop)
	defer close(stop)

	// Wait for two polls, so the dedup case is actually exercised.
	for ring.len() < 2 {
		select {
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("ring never filled past %d after %d polls", ring.len(), hits)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if ring.len() != 2 {
		t.Fatalf("expected two distinct calls, got %d", ring.len())
	}
}

// TestPollerSurvivesAnEngineThatIsDown: the ring must not be a new way for the
// exporter to break, and a scrape failure should say so without taking the
// process with it.
func TestPollerSurvivesAnEngineThatIsDown(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer fake.Close()

	var logged int
	sc := &scraper{client: fake.Client(), strataURL: fake.URL, token: "nope"}
	ring := newEventRing(64)
	p := &poller{sc: sc, ring: ring, interval: time.Second, logf: func(string, ...any) { logged++ }}
	p.once()
	if logged != 1 {
		t.Fatal("a failed poll should be logged, not silently swallowed")
	}
	if ring.len() != 0 {
		t.Fatal("a failed poll must not invent records")
	}
}

// TestRoutingTable exercises the real mux, not the handler in isolation, so
// "404 on a non-endpoint path" is a property of what production serves.
func TestRoutingTable(t *testing.T) {
	sc := &scraper{client: &http.Client{}, strataURL: "http://127.0.0.1:1", token: ""}
	ring := newEventRing(64)
	ring.push(&engineEvent{ID: "x", Model: "m"})
	mux := newMux(sc, ring, "boot", "")

	serve := func(target string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// The endpoints that exist.
	if code, body := serve("/events"); code != 200 || !strings.Contains(body, `"boot_id"`) {
		t.Fatalf("/events: %d %s", code, body)
	}
	if code, _ := serve("/healthz"); code != 200 {
		t.Fatalf("/healthz: %d", code)
	}
	// A guessed subpath of a real endpoint must be a 404, not the index page and
	// not the ring answering a path it never registered.
	for _, target := range []string{"/events/stream", "/events/next", "/rooter/events", "/nope"} {
		if code, _ := serve(target); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, code)
		}
	}
}
