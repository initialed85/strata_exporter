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

type request struct {
	DurationS           float64 `json:"duration_s"`
	Finish              string  `json:"finish"`
	PromptTokens        int     `json:"prompt_tokens"`
	Reused              int     `json:"reused"`
	OutputTokens        int     `json:"output_tokens"`
	PromptMs            float64 `json:"prompt_ms"`
	DecodeMs            float64 `json:"decode_ms"`
	DecodeTokS          float64 `json:"decode_tok_s"`
	HitRate             float64 `json:"hit_rate"`
	DraftsOffered       int     `json:"drafts_offered"`
	DraftsAccepted      int     `json:"drafts_accepted"`
	ReasoningRecoveries int     `json:"reasoning_recoveries"`
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
		o.gauge("strata_last_request_duration_seconds", "Duration of the most recent request.", r.DurationS)
		o.gauge("strata_last_request_prompt_tokens", "Prompt tokens of the most recent request.", float64(r.PromptTokens))
		o.gauge("strata_last_request_reused_tokens", "Tokens reused by the most recent request.", float64(r.Reused))
		o.gauge("strata_last_request_output_tokens", "Tokens generated by the most recent request.", float64(r.OutputTokens))
		o.gauge("strata_last_request_prompt_seconds", "Prefill time of the most recent request.", r.PromptMs/1000)
		o.gauge("strata_last_request_decode_seconds", "Decode time of the most recent request.", r.DecodeMs/1000)
		o.gauge("strata_last_request_decode_tokens_per_second", "Decode rate of the most recent request.", r.DecodeTokS)
		o.gauge("strata_last_request_cache_hit_ratio", "Cache hit ratio of the most recent request.", r.HitRate)
		o.gauge("strata_last_request_reasoning_recoveries", "Reasoning recoveries in the most recent request.", float64(r.ReasoningRecoveries))
		o.info("strata_last_request_info", "How the most recent request finished.", fmt.Sprintf("{finish=%q}", r.Finish))
	}
	o.gauge("strata_requests_recent", "Requests in the recent list Strata returns.", float64(len(s.Requests)))

	return o.String()
}

func main() {
	strataURL := flag.String("strata-url", envOr("STRATA_URL", "http://127.0.0.1:8088"), "base URL of the Strata runner")
	listen := flag.String("listen", envOr("LISTEN_ADDR", ":9137"), "address to serve /metrics on")
	timeout := flag.Duration("timeout", 10*time.Second, "timeout for fetching Strata's metrics")
	flag.Parse()

	client := &http.Client{Timeout: *timeout}

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequest(http.MethodGet, *strataURL+"/metrics", nil)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "# HELP strata_up 1 when Strata's /metrics answered.\n# TYPE strata_up gauge\nstrata_up 0\n# fetch failed: %s\n", err)
			return
		}

		// TODO(initialed85): parametize this
		req.Header.Set("Authorization", "Bearer Password1")

		start := time.Now()
		res, err := client.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "# HELP strata_up 1 when Strata's /metrics answered.\n# TYPE strata_up gauge\nstrata_up 0\n# fetch failed: %s\n", err)
			return
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "# HELP strata_up 1 when Strata's /metrics answered.\n# TYPE strata_up gauge\nstrata_up 0\n# read failed: %s\n", err)
			return
		}
		var s snapshot
		if err := json.Unmarshal(raw, &s); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "# HELP strata_up 1 when Strata's /metrics answered.\n# TYPE strata_up gauge\nstrata_up 0\n# decode failed: %s\n", err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write([]byte(render(&s, time.Since(start))))
	})
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`<html><head><title>strata exporter</title></head><body><a href="/metrics">metrics</a></body></html>`))
	})

	log.Printf("strata_exporter listening on %s, scraping %s", *listen, *strataURL)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
