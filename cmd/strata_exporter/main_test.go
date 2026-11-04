package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// snapshotFixture is a trimmed capture of a real Strata /metrics answer. It exists so that a
// rename or a moved field on the Strata side shows up as a failing test rather than as a dashboard
// quietly full of zeros, which is the failure mode a JSON-to-gauge adapter is most likely to have.
const snapshotFixture = `{
  "engine": {
    "model": "qwen3.8-flash-next-iq3_xxs",
    "engine": "0.1.51",
    "images": true,
    "context": 131072,
    "max_context": 131072,
    "vram_free_mib": 646,
    "arena_mib": 40925,
    "expert_slots": 6202,
    "expert_cache_mib": 10278,
    "conversation_cache_mib": 8192,
    "conversation_cache_slots": 4,
    "pool_workers": 5
  },
  "live": {
    "state": "idle",
    "queued": 0,
    "prompt_tokens": 114218,
    "prompt_read": 47,
    "prompt_total": 114218,
    "generated": 595,
    "max_tokens": 8192,
    "elapsed_s": 9.5,
    "tok_s": 66.9,
    "tok_s_mean": 65.1,
    "prefill_tok_s_mean": 2094.5
  },
  "requests": [
    {
      "id": "a2791399bf1c",
      "time": 1793752131.3558712,
      "duration_s": 9.5,
      "finish": "stop",
      "prompt_tokens": 114218,
      "reused": 114171,
      "output_tokens": 595,
      "prompt_ms": 543.0,
      "decode_ms": 8899.4,
      "decode_tok_s": 66.9,
      "hit_rate": 0.878,
      "drafts_offered": 567,
      "drafts_accepted": 361,
      "reasoning_recoveries": 0
    }
  ],
  "requests_kept": 142,
  "conversation_cache": {
    "enabled": true,
    "budget_mib": 8192,
    "slots": 2,
    "parked": 1,
    "bytes": 402653184,
    "evictions": 3,
    "parks": 9,
    "restores": 9,
    "requests": 140,
    "requests_reused": 118,
    "reused_tokens": 11000000,
    "prompt_tokens": 114218,
    "last_reused": 114171,
    "last_prompt": 114218
  },
  "totals": {
    "requests": 142,
    "prompt_tokens": 15000000,
    "reused": 14000000,
    "output_tokens": 200000,
    "prompt_ms": 543.0,
    "decode_ms": 8899.4,
    "drafts_offered": 567,
    "drafts_accepted": 361
  },
  "hardware": {
    "gpu_util": 91.0,
    "gpu_mem_used": 31000000000,
    "gpu_mem_total": 34000000000,
    "gpu_temp": 63.0,
    "gpu_power": 250.0,
    "gpu_power_limit": 300.0,
    "gpu_pcie_gen": 4,
    "gpu_pcie_width": 16,
    "cpu": 42.0,
    "ram_used": 40000000000,
    "ram_total": 68000000000,
    "disk_read_mb": 0.0,
    "disk_write_mb": 1.2,
    "tok_s": 66.9,
    "tok_s_mean": 65.1,
    "prefill_tok_s_mean": 2094.5
  },
  "hardware_static": {
    "gpu_name": "AMD Radeon",
    "cpu_name": "AMD Ryzen 5 5600X 6-Core Processor",
    "cores": 6,
    "threads": 12
  }
}`

// newRunner starts a stand-in for Strata's JSON endpoint and records what it was asked for.
func newRunner(t *testing.T, status int, body string) (*httptest.Server, *string, *string) {
	t.Helper()
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &auth, &path
}

func TestScrapeSendsNoAuthorizationHeaderWhenNoTokenIsConfigured(t *testing.T) {
	srv, auth, path := newRunner(t, http.StatusOK, snapshotFixture)
	sc := &scraper{client: srv.Client(), strataURL: srv.URL}

	out, code := sc.scrape()

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, out)
	}
	if *auth != "" {
		t.Errorf("Authorization = %q, want no header at all when no token is configured", *auth)
	}
	if *path != "/metrics" {
		t.Errorf("path = %q, want /metrics", *path)
	}
	if !strings.Contains(out, "strata_up 1") {
		t.Errorf("want strata_up 1, got:\n%s", out)
	}
}

func TestScrapeSendsTheConfiguredBearerToken(t *testing.T) {
	srv, auth, _ := newRunner(t, http.StatusOK, snapshotFixture)
	sc := &scraper{client: srv.Client(), strataURL: srv.URL, token: "test-token"}

	out, code := sc.scrape()

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", code, out)
	}
	if want := "Bearer test-token"; *auth != want {
		t.Errorf("Authorization = %q, want %q", *auth, want)
	}
	// The credential must not leak back out into the exposition either.
	if strings.Contains(out, "test-token") || strings.Contains(out, "Bearer") {
		t.Errorf("scrape output leaked the configured token:\n%s", out)
	}
}

// Strata answers 401 to an unauthenticated or wrongly-authenticated scrape. Reporting that as a
// decode error would be the lie the old code told: it read the 401 body and handed it to the JSON
// decoder, so "wrong token" looked identical to "runner is broken".
func TestScrapeNamesUnauthorizedScrapesInsteadOfMisreportingThem(t *testing.T) {
	srv, _, _ := newRunner(t, http.StatusUnauthorized, `{"error": {"message": "unauthorized"}}`)
	sc := &scraper{client: srv.Client(), strataURL: srv.URL, token: "wrong-token"}

	out, code := sc.scrape()

	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so the scrape is recorded as failed", code)
	}
	for _, want := range []string{
		"strata_up 0",
		"strata_scrape_http_status 401",
		"HTTP 401",
		"STRATA_TOKEN",
		"unauthorized",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in the failed-scrape body, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "wrong-token") {
		t.Errorf("failed-scrape body echoed the configured token:\n%s", out)
	}
}

// No response at all is a different fault from a rejected one, and the exposition has to keep them
// apart: strata_scrape_http_status 0 means nothing ever answered.
func TestScrapeDistinguishesNoResponseFromRejectedResponse(t *testing.T) {
	srv, _, _ := newRunner(t, http.StatusOK, snapshotFixture)
	url := srv.URL
	srv.Close()

	sc := &scraper{client: &http.Client{Timeout: time.Second}, strataURL: url}
	out, code := sc.scrape()

	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if !strings.Contains(out, "strata_up 0") || !strings.Contains(out, "strata_scrape_http_status 0") {
		t.Errorf("want strata_up 0 with strata_scrape_http_status 0, got:\n%s", out)
	}
}

// 200 with a body that is not the snapshot is its own failure mode: a proxy, a login page, or a
// renamed endpoint all arrive as 200.
func TestScrapeReportsDecodeFailureWhenStatusIsFine(t *testing.T) {
	srv, _, _ := newRunner(t, http.StatusOK, `<html><body>login</body></html>`)
	sc := &scraper{client: srv.Client(), strataURL: srv.URL, token: "test-token"}

	out, code := sc.scrape()

	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
	if !strings.Contains(out, "decode failed") {
		t.Errorf("want decode failed, got:\n%s", out)
	}
	if !strings.Contains(out, "strata_scrape_http_status 200") {
		t.Errorf("want strata_scrape_http_status 200 so a 200-shaped wrong answer is visible, got:\n%s", out)
	}
}

func TestRenderMapsTheRealSnapshotOntoSeries(t *testing.T) {
	var snap snapshot
	if err := jsonUnmarshalFixture(t, snapshotFixture, &snap); err != nil {
		t.Fatal(err)
	}
	out := render(&snap, 12*time.Millisecond)

	// Model and build identity, which every dashboard groups on.
	for _, want := range []string{
		`strata_model_info{model="qwen3.8-flash-next-iq3_xxs",engine="0.1.51",gpu="AMD Radeon",cpu="AMD Ryzen 5 5600X 6-Core Processor"} 1`,
		"strata_up 1",
		"strata_scrape_duration_seconds 0.012",
		"strata_requests_total 142",
		"strata_requests_kept 142",
		"strata_requests_recent 1",
		"strata_engine_images_enabled 1",
		"strata_engine_arena_bytes 42912972800",
		"strata_gpu_pcie_width 16",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in rendered output, got:\n%s", want, out)
		}
	}

	// Per-request numbers, seconds not milliseconds, because the exporter converts and a silent
	// unit change is exactly the sort of thing nobody notices until a graph is wrong.
	for _, want := range []string{
		"strata_last_request_duration_seconds 9.5",
		"strata_last_request_prompt_seconds 0.543",
		"strata_last_request_decode_seconds 8.8994",
		"strata_last_request_output_tokens 595",
		"strata_last_request_reused_tokens 114171",
		"strata_last_request_cache_hit_ratio 0.878",
		`strata_last_request_info{finish="stop"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in rendered output, got:\n%s", want, out)
		}
	}
}

// jsonUnmarshalFixture keeps json.Unmarshal out of the test bodies so the tests read as assertions.
func jsonUnmarshalFixture(t *testing.T, s string, dst *snapshot) error {
	t.Helper()
	return decodeSnapshot([]byte(s), dst)
}

func TestClipLimitsForeignBodies(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := clip([]byte(long), 200); len(got) != 203 {
		t.Errorf("clip len = %d, want 203 (200 chars plus the ellipsis)", len(got))
	}
	if got := clip([]byte("  short  "), 200); got != "short" {
		t.Errorf("clip = %q, want the value trimmed", got)
	}
}
