package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Strata's /metrics is a JSON snapshot, not Prometheus text, so this exporter
// maps the fields we care about onto Prometheus series.
type snapshot struct {
	Engine struct {
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
	} `json:"engine"`

	Live struct {
		State           *string  `json:"state"`
		Queued          int      `json:"queued"`
		PromptTokens    *int     `json:"prompt_tokens"`
		PromptRead      *int     `json:"prompt_read"`
		PromptTotal     *int     `json:"prompt_total"`
		Generated       *int     `json:"generated"`
		MaxTokens       *int     `json:"max_tokens"`
		ElapsedS        *float64 `json:"elapsed_s"`
		TokS            *float64 `json:"tok_s"`
		TokSMean        *float64 `json:"tok_s_mean"`
		PrefillTokSMean *float64 `json:"prefill_tok_s_mean"`
	} `json:"live"`

	Requests     []request `json:"requests"`
	RequestsKept int       `json:"requests_kept"`

	ConversationCache struct {
		Enabled        bool `json:"enabled"`
		BudgetMiB      int  `json:"budget_mib"`
		Slots          int  `json:"slots"`
		Parked         int  `json:"parked"`
		Bytes          int  `json:"bytes"`
		Evictions      int  `json:"evictions"`
		Parks          int  `json:"parks"`
		Restores       int  `json:"restores"`
		Requests       int  `json:"requests"`
		RequestsReused int  `json:"requests_reused"`
		ReusedTokens   int  `json:"reused_tokens"`
		PromptTokens   int  `json:"prompt_tokens"`
		LastReused     int  `json:"last_reused"`
		LastPrompt     int  `json:"last_prompt"`
	} `json:"conversation_cache"`

	Totals struct {
		Requests       int     `json:"requests"`
		PromptTokens   int     `json:"prompt_tokens"`
		Reused         int     `json:"reused"`
		OutputTokens   int     `json:"output_tokens"`
		PromptMs       float64 `json:"prompt_ms"`
		DecodeMs       float64 `json:"decode_ms"`
		DraftsOffered  int     `json:"drafts_offered"`
		DraftsAccepted int     `json:"drafts_accepted"`
	} `json:"totals"`

	Hardware struct {
		GPUUtil         float64 `json:"gpu_util"`
		GPUMemUsed      int64   `json:"gpu_mem_used"`
		GPUMemTotal     int64   `json:"gpu_mem_total"`
		GPUTemp         float64 `json:"gpu_temp"`
		GPUPower        float64 `json:"gpu_power"`
		GPUPowerLimit   float64 `json:"gpu_power_limit"`
		GPUPCIeGen      int     `json:"gpu_pcie_gen"`
		GPUPCIeWidth    int     `json:"gpu_pcie_width"`
		CPU             float64 `json:"cpu"`
		RAMUsed         int64   `json:"ram_used"`
		RAMTotal        int64   `json:"ram_total"`
		DiskReadMB      float64 `json:"disk_read_mb"`
		DiskWriteMB     float64 `json:"disk_write_mb"`
		TokS            float64 `json:"tok_s"`
		TokSMean        float64 `json:"tok_s_mean"`
		PrefillTokSMean float64 `json:"prefill_tok_s_mean"`
	} `json:"hardware"`

	HardwareStatic struct {
		GPUName string `json:"gpu_name"`
		CPUName string `json:"cpu_name"`
		Cores   int    `json:"cores"`
		Threads int    `json:"threads"`
	} `json:"hardware_static"`
}

// request is one entry of Strata's recent-call list.
//
// Everything numeric is a pointer, because "the engine did not report this" and
// "the engine reported zero" are different facts and the last of them used to be
// published as a gauge of 0. The id, the timestamp and the split between what the
// engine read and what it reused are here for the event ring; see events.go.
type request struct {
	ID                  string   `json:"id"`
	Time                float64  `json:"time"`
	Projection          *string  `json:"projection,omitempty"`
	DurationS           *float64 `json:"duration_s"`
	Finish              string   `json:"finish"`
	PromptTokens        *int     `json:"prompt_tokens"`
	Reused              *int     `json:"reused"`
	OutputTokens        *int     `json:"output_tokens"`
	PromptTotal         *int     `json:"prompt_total"`
	PromptRead          *int     `json:"prompt_read"`
	EngineGenerated     *int     `json:"engine_generated"`
	FirstEngineTokenS   *float64 `json:"first_engine_token_s"`
	PromptMs            *float64 `json:"prompt_ms"`
	DecodeMs            *float64 `json:"decode_ms"`
	DecodeTokS          *float64 `json:"decode_tok_s"`
	HitRate             *float64 `json:"hit_rate"`
	DraftsOffered       *int     `json:"drafts_offered"`
	DraftsAccepted      *int     `json:"drafts_accepted"`
	ReasoningRecoveries *int     `json:"reasoning_recoveries"`

	// Engine-side spill detail: how much of the KV had to be moved off the card
	// and how much of that went to RAM rather than to disk.
	PCIEShare               *float64 `json:"pcie_share"`
	RAMBlobs                *int     `json:"ram_blobs"`
	FileBlobs               *int     `json:"file_blobs"`
	FileMB                  *float64 `json:"file_mb"`
	ReasoningRepeatCoverage *float64 `json:"reasoning_repeat_coverage"`
}

type body struct{ strings.Builder }

func (o *body) metric(name, help, kind string, value float64) {
	fmt.Fprintf(o, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", name, help, name, kind, name, strconv.FormatFloat(value, 'f', -1, 64))
}

func (o *body) counter(name, help string, value float64) { o.metric(name, help, "counter", value) }
func (o *body) gauge(name, help string, value float64)   { o.metric(name, help, "gauge", value) }

func (o *body) info(name, help, labels string) {
	fmt.Fprintf(o, "# HELP %s %s\n# TYPE %s gauge\n%s%s 1\n", name, help, name, name, labels)
}

func (o *body) boolGauge(name, help string, value bool) {
	var v float64
	if value {
		v = 1
	}
	o.gauge(name, help, v)
}

func render(s *snapshot, dur time.Duration) string {
	o := &body{}
	o.gauge("strata_up", "1 when Strata's /metrics answered.", 1)
	o.gauge("strata_scrape_duration_seconds", "Time spent fetching Strata's metrics.", dur.Seconds())

	o.info("strata_model_info", "Build and host details of the Strata runner.", fmt.Sprintf("{model=%q,engine=%q,gpu=%q,cpu=%q}", s.Engine.Model, s.Engine.Engine, s.HardwareStatic.GPUName, s.HardwareStatic.CPUName))

	// engine
	o.gauge("strata_engine_context_tokens", "Context window in use by the engine.", float64(s.Engine.Context))
	o.gauge("strata_engine_max_context_tokens", "Largest context the engine can address.", float64(s.Engine.MaxContext))
	o.gauge("strata_engine_vram_free_bytes", "Free VRAM.", float64(s.Engine.VramFreeMiB)*1024*1024)
	o.gauge("strata_engine_arena_bytes", "Size of the engine's arena.", float64(s.Engine.ArenaMiB)*1024*1024)
	o.gauge("strata_engine_expert_slots", "Expert slots loaded for the MoE cache.", float64(s.Engine.ExpertSlots))
	o.gauge("strata_engine_expert_cache_bytes", "MoE expert cache footprint.", float64(s.Engine.ExpertCacheMiB)*1024*1024)
	o.gauge("strata_engine_conversation_cache_slots", "Conversation cache slots configured.", float64(s.Engine.ConversationCacheSlots))
	o.gauge("strata_engine_conversation_cache_bytes", "Conversation cache budget.", float64(s.Engine.ConversationCacheMiB)*1024*1024)
	o.gauge("strata_engine_pool_workers", "Engine worker threads.", float64(s.Engine.PoolWorkers))
	o.boolGauge("strata_engine_images_enabled", "1 when the engine accepts images.", s.Engine.Images)

	// live
	live := 0.0
	if s.Live.State != nil && *s.Live.State != "idle" {
		live = 1
	}
	o.gauge("strata_live_active", "1 while a request is being served.", live)
	o.gauge("strata_live_queued_requests", "Requests waiting in the queue.", float64(s.Live.Queued))
	for _, p := range []struct {
		name, help string
		v          *int
	}{
		{"strata_live_prompt_tokens", "Prompt tokens of the request in flight.", s.Live.PromptTokens},
		{"strata_live_prompt_read_tokens", "Prompt tokens read so far for the request in flight.", s.Live.PromptRead},
		{"strata_live_prompt_total_tokens", "Prompt tokens of the request in flight (total).", s.Live.PromptTotal},
		{"strata_live_generated_tokens", "Tokens generated for the request in flight.", s.Live.Generated},
		{"strata_live_max_tokens", "Token budget of the request in flight.", s.Live.MaxTokens},
	} {
		if p.v != nil {
			o.gauge(p.name, p.help, float64(*p.v))
		}
	}
	for _, p := range []struct {
		name, help string
		v          *float64
	}{
		{"strata_live_elapsed_seconds", "Seconds elapsed for the request in flight.", s.Live.ElapsedS},
		{"strata_live_tokens_per_second", "Decode rate of the request in flight.", s.Live.TokS},
		{"strata_live_tokens_per_second_mean", "Mean decode rate over recent requests.", s.Live.TokSMean},
		{"strata_live_prefill_tokens_per_second_mean", "Mean prefill rate over recent requests.", s.Live.PrefillTokSMean},
	} {
		if p.v != nil {
			o.gauge(p.name, p.help, *p.v)
		}
	}

	// totals
	o.counter("strata_requests_total", "Requests served since Strata started.", float64(s.Totals.Requests))
	o.counter("strata_prompt_tokens_total", "Prompt tokens consumed.", float64(s.Totals.PromptTokens))
	o.counter("strata_prompt_reused_tokens_total", "Prompt tokens served from cache.", float64(s.Totals.Reused))
	o.counter("strata_output_tokens_total", "Tokens generated.", float64(s.Totals.OutputTokens))
	o.counter("strata_prompt_seconds_total", "Time spent prefilling.", s.Totals.PromptMs/1000)
	o.counter("strata_decode_seconds_total", "Time spent decoding.", s.Totals.DecodeMs/1000)
	o.counter("strata_spec_drafts_offered_total", "Speculative drafts offered.", float64(s.Totals.DraftsOffered))
	o.counter("strata_spec_drafts_accepted_total", "Speculative drafts accepted.", float64(s.Totals.DraftsAccepted))
	o.gauge("strata_requests_kept", "Recent requests Strata still reports.", float64(s.RequestsKept))

	// conversation cache
	o.boolGauge("strata_conversation_cache_enabled", "1 when the conversation cache is on.", s.ConversationCache.Enabled)
	o.gauge("strata_conversation_cache_parked", "Parked conversations.", float64(s.ConversationCache.Parked))
	o.gauge("strata_conversation_cache_bytes", "Conversation cache bytes in use.", float64(s.ConversationCache.Bytes))
	o.gauge("strata_conversation_cache_slots", "Conversation cache slots in use.", float64(s.ConversationCache.Slots))
	o.gauge("strata_conversation_cache_budget_bytes", "Conversation cache budget.", float64(s.ConversationCache.BudgetMiB)*1024*1024)
	o.counter("strata_conversation_cache_evictions_total", "Conversations evicted.", float64(s.ConversationCache.Evictions))
	o.counter("strata_conversation_cache_parks_total", "Conversations parked.", float64(s.ConversationCache.Parks))
	o.counter("strata_conversation_cache_restores_total", "Conversations restored.", float64(s.ConversationCache.Restores))
	o.counter("strata_conversation_cache_requests_total", "Requests the cache saw.", float64(s.ConversationCache.Requests))
	o.counter("strata_conversation_cache_requests_reused_total", "Requests that reused a cached conversation.", float64(s.ConversationCache.RequestsReused))
	o.counter("strata_conversation_cache_reused_tokens_total", "Tokens reused from cached conversations.", float64(s.ConversationCache.ReusedTokens))
	o.gauge("strata_conversation_cache_last_prompt_tokens", "Prompt tokens of the last cache-visible request.", float64(s.ConversationCache.LastPrompt))
	o.gauge("strata_conversation_cache_last_reused_tokens", "Tokens reused by the last cache-visible request.", float64(s.ConversationCache.LastReused))

	// hardware
	o.gauge("strata_gpu_utilization_percent", "GPU utilisation.", s.Hardware.GPUUtil)
	o.gauge("strata_gpu_memory_used_bytes", "GPU memory in use.", float64(s.Hardware.GPUMemUsed))
	o.gauge("strata_gpu_memory_total_bytes", "GPU memory installed.", float64(s.Hardware.GPUMemTotal))
	o.gauge("strata_gpu_temperature_celsius", "GPU temperature.", s.Hardware.GPUTemp)
	o.gauge("strata_gpu_power_watts", "GPU power draw.", s.Hardware.GPUPower)
	o.gauge("strata_gpu_power_limit_watts", "GPU power limit.", s.Hardware.GPUPowerLimit)
	o.gauge("strata_gpu_pcie_generation", "PCIe generation the GPU link trained at.", float64(s.Hardware.GPUPCIeGen))
	o.gauge("strata_gpu_pcie_width", "PCIe lane width the GPU link trained at.", float64(s.Hardware.GPUPCIeWidth))
	o.gauge("strata_cpu_utilization_percent", "Host CPU utilisation.", s.Hardware.CPU)
	o.gauge("strata_memory_used_bytes", "Host memory in use.", float64(s.Hardware.RAMUsed))
	o.gauge("strata_memory_total_bytes", "Host memory installed.", float64(s.Hardware.RAMTotal))
	o.gauge("strata_disk_read_bytes_per_second", "Host disk read rate.", s.Hardware.DiskReadMB*1024*1024)
	o.gauge("strata_disk_write_bytes_per_second", "Host disk write rate.", s.Hardware.DiskWriteMB*1024*1024)
	o.gauge("strata_tokens_per_second", "Decode rate across recent requests.", s.Hardware.TokS)
	o.gauge("strata_tokens_per_second_mean", "Mean decode rate.", s.Hardware.TokSMean)
	o.gauge("strata_prefill_tokens_per_second_mean", "Mean prefill rate.", s.Hardware.PrefillTokSMean)
	o.gauge("strata_host_cores", "Host cores.", float64(s.HardwareStatic.Cores))
	o.gauge("strata_host_threads", "Host threads.", float64(s.HardwareStatic.Threads))

	// last request
	if n := len(s.Requests); n > 0 {
		r := s.Requests[n-1]
		// Each of these is skipped rather than emitted as 0 when the engine did not
		// report it, so a missing measurement stays a gap in a graph instead of
		// becoming a datapoint that looks like a very fast model.
		for _, p := range []struct {
			name, help string
			v          *float64
			scale      float64
		}{
			{"strata_last_request_duration_seconds", "Duration of the most recent request.", r.DurationS, 1},
			{"strata_last_request_prompt_seconds", "Prefill time of the most recent request.", r.PromptMs, 1e-3},
			{"strata_last_request_decode_seconds", "Decode time of the most recent request.", r.DecodeMs, 1e-3},
			{"strata_last_request_decode_tokens_per_second", "Decode rate of the most recent request.", r.DecodeTokS, 1},
			{"strata_last_request_cache_hit_ratio", "Cache hit ratio of the most recent request.", r.HitRate, 1},
			{"strata_last_request_first_token_seconds", "Time the engine took to its own first token, for the most recent request.", r.FirstEngineTokenS, 1},
			{"strata_last_request_file_mb", "KV spilled to disk for the most recent request.", r.FileMB, 1},
			{"strata_last_request_pcie_share", "Share of the most recent request's KV traffic that crossed PCIe.", r.PCIEShare, 1},
		} {
			if p.v != nil {
				o.gauge(p.name, p.help, *p.v*p.scale)
			}
		}
		for _, p := range []struct {
			name, help string
			v          *int
		}{
			{"strata_last_request_prompt_tokens", "Prompt tokens of the most recent request.", r.PromptTokens},
			{"strata_last_request_reused_tokens", "Tokens reused by the most recent request.", r.Reused},
			{"strata_last_request_read_tokens", "Prompt tokens the most recent request actually had to evaluate.", r.PromptRead},
			{"strata_last_request_output_tokens", "Tokens generated by the most recent request.", r.OutputTokens},
			{"strata_last_request_reasoning_recoveries", "Reasoning recoveries in the most recent request.", r.ReasoningRecoveries},
			{"strata_last_request_drafts_offered", "Speculative drafts offered by the most recent request.", r.DraftsOffered},
			{"strata_last_request_drafts_accepted", "Speculative drafts accepted by the most recent request.", r.DraftsAccepted},
			{"strata_last_request_ram_blobs", "KV blobs spilled to RAM for the most recent request.", r.RAMBlobs},
			{"strata_last_request_file_blobs", "KV blobs spilled to file for the most recent request.", r.FileBlobs},
		} {
			if p.v != nil {
				o.gauge(p.name, p.help, float64(*p.v))
			}
		}
		o.info("strata_last_request_info", "How the most recent request finished.", fmt.Sprintf("{finish=%q}", r.Finish))
	}
	o.gauge("strata_requests_recent", "Requests in the recent list Strata returns.", float64(len(s.Requests)))

	return o.String()
}

// scraper turns one Strata JSON snapshot into Prometheus text.
//
// Strata guards /metrics with a bearer token and answers 401 to everything else, but which
// token is correct is a deployment fact rather than something this program should know, so it
// arrives at runtime through -strata-token / STRATA_TOKEN. An empty token sends no Authorization
// header at all rather than a guessed one, which keeps the credential out of the source, out of
// the published image and out of this public repository.
type scraper struct {
	client    *http.Client
	strataURL string
	token     string
}

// scrape returns the body to serve and the HTTP status to serve it with.
//
// A failed scrape answers 503 so Prometheus records a failed scrape instead of ingesting a
// plausible-looking series set.
func (s *scraper) scrape() (string, int) {
	req, err := http.NewRequest(http.MethodGet, s.strataURL+"/metrics", nil)
	if err != nil {
		return downBody(0, "request build failed: "+err.Error()), http.StatusServiceUnavailable
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	start := time.Now()
	res, err := s.client.Do(req)
	if err != nil {
		return downBody(0, "fetch failed: "+err.Error()), http.StatusServiceUnavailable
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return downBody(res.StatusCode, "read failed: "+err.Error()), http.StatusServiceUnavailable
	}
	// Checked before decoding, so an unauthenticated or misrouted scrape says which one it is
	// instead of surfacing as a JSON syntax error.
	if res.StatusCode != http.StatusOK {
		reason := fmt.Sprintf("HTTP %d from %s: %s (check -strata-token / STRATA_TOKEN)",
			res.StatusCode, s.strataURL, clip(raw, 200))
		return downBody(res.StatusCode, reason), http.StatusServiceUnavailable
	}
	var snap snapshot
	if err := decodeSnapshot(raw, &snap); err != nil {
		return downBody(res.StatusCode, "decode failed: "+err.Error()), http.StatusServiceUnavailable
	}
	return render(&snap, time.Since(start)), http.StatusOK
}

// snapshot fetches and decodes one Strata snapshot for a caller that wants the
// structured answer rather than the Prometheus text. The event poller uses this;
// /metrics keeps its own live path so a failed scrape still answers 503.
func (s *scraper) snapshot() (*snapshot, error) {
	req, err := http.NewRequest(http.MethodGet, s.strataURL+"/metrics", nil)
	if err != nil {
		return nil, fmt.Errorf("request build failed: %w", err)
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s (check -strata-token / STRATA_TOKEN)", res.StatusCode, s.strataURL)
	}
	var snap snapshot
	if err := decodeSnapshot(raw, &snap); err != nil {
		return nil, fmt.Errorf("decode failed: %w", err)
	}
	return &snap, nil
}

// decodeSnapshot parses Strata's JSON answer into the shape declared above. It exists as a named
// function so the tests exercise the same decode path production uses rather than a copy of it.
func decodeSnapshot(raw []byte, dst *snapshot) error {
	return json.Unmarshal(raw, dst)
}

// downBody is what /metrics serves when the runner did not answer.
//
// strata_scrape_http_status is the difference between "down, wrong path or wrong token" (a real
// status code) and "never got a response" (0) that strata_up 0 on its own cannot express.
func downBody(status int, reason string) string {
	o := &body{}
	o.gauge("strata_up", "1 when Strata's /metrics answered.", 0)
	o.gauge("strata_scrape_http_status", "HTTP status Strata answered with; 0 when no response was received.", float64(status))
	fmt.Fprintf(o, "# scrape failed: %s\n", reason)
	return o.String()
}

// clip keeps a snippet of somebody else's response body in a diagnostic line without letting it
// grow unbounded.
func clip(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func main() {
	strataURL := flag.String("strata-url", envOr("STRATA_URL", "http://127.0.0.1:8088"), "base URL of the Strata runner")
	strataToken := flag.String("strata-token", envOr("STRATA_TOKEN", ""), "bearer token for Strata; empty sends no Authorization header")
	listen := flag.String("listen", envOr("LISTEN_ADDR", ":9137"), "address to serve /metrics and /events on")
	timeout := flag.Duration("timeout", 10*time.Second, "timeout for fetching Strata's metrics")
	eventsToken := flag.String("events-token", envOr("EVENTS_TOKEN", ""), "bearer token required on /events; empty leaves it open, which is what /metrics already is")
	eventsInterval := flag.Duration("events-interval", envDur("EVENTS_INTERVAL", 5*time.Second), "how often to poll Strata for the event ring")
	eventsMax := flag.Int("events-max", envInt("EVENTS_MAX", 2048), "how many calls the event ring keeps")
	flag.Parse()

	sc := &scraper{
		client:    &http.Client{Timeout: *timeout},
		strataURL: *strataURL,
		token:     *strataToken,
	}
	ring := newEventRing(*eventsMax)
	bootID := newBootID()

	// Only whether a token is configured gets logged, never the token itself.
	authMode := "none"
	if *strataToken != "" {
		authMode = "bearer"
	}
	eventsAuth := "open"
	if *eventsToken != "" {
		eventsAuth = "bearer"
	}

	stop := make(chan struct{})
	p := &poller{sc: sc, ring: ring, interval: *eventsInterval, logf: log.Printf}
	if p.interval <= 0 {
		p.interval = time.Second
	}
	go p.run(stop)

	log.Printf("strata_exporter listening on %s, scraping %s (auth: %s), /events %s keeping %d calls every %s",
		*listen, *strataURL, authMode, eventsAuth, ring.max, p.interval)
	log.Fatal(http.ListenAndServe(*listen, newMux(sc, ring, bootID, *eventsToken)))
}

// newMux is the routing table, built rather than registered on the default mux so
// the tests exercise the same table production serves.
//
// Anything that is not one of the four known paths is a 404, including subpaths
// under /events. That matters more than it sounds: a consumer that guesses
// /events/stream or /events/next gets "no such endpoint" rather than a page of
// HTML it has to notice is HTML.
func newMux(sc *scraper, ring *eventRing, bootID, eventsToken string) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		out, code := sc.scrape()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(code)
		w.Write([]byte(out))
	})
	m.HandleFunc("/events", ring.handler(bootID, eventsToken))
	m.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`<html><head><title>strata exporter</title></head><body><a href="/metrics">metrics</a> <a href="/events">events</a></body></html>`))
	})
	return m
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDur and envInt exist so the three knobs that a deployment is most likely to
// want to change (how often the ring is polled, how big it is, and whether it is
// gated) can be set by the manifest without also having to set a command-line
// arg, which the container's args list is already short of room for.
func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
