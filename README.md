# strata_exporter

Scrapes a Strata LLM inference runner's JSON status endpoint and re-publishes it in Prometheus text
format. Strata does not expose `/metrics`, so this exporter polls its status endpoint and maps the
fields onto gauges and counters.

## Usage

```
strata_exporter [-strata-url http://127.0.0.1:8088] [-listen :9137] [-timeout 10s]
```

| Flag | Environment | Default | Purpose |
| --- | --- | --- | --- |
| `-strata-url` | `STRATA_URL` | `http://127.0.0.1:8088` | base URL of the Strata runner |
| `-listen` | `LISTEN_ADDR` | `:9137` | address serving `/metrics` |
| `-timeout` | – | `10s` | timeout for fetching Strata's status |

`strata_up 1` means the status endpoint answered; a failed poll emits `strata_up 0` plus the scrape
duration, so a dashboard can tell "runner down" from "runner idle".

## Metrics

Requests and tokens: `strata_requests_total`, `strata_requests_recent`, `strata_requests_kept`,
`strata_prompt_tokens_total`, `strata_prompt_reused_tokens_total`, `strata_output_tokens_total`,
`strata_prompt_seconds_total`, `strata_decode_seconds_total`, `strata_tokens_per_second`,
`strata_tokens_per_second_mean`, `strata_prefill_tokens_per_second_mean`,
`strata_spec_drafts_offered_total`, `strata_spec_drafts_accepted_total`.

Request in flight: `strata_live_active`, `strata_live_elapsed_seconds`, `strata_live_prompt_tokens`,
`strata_live_prompt_read_tokens`, `strata_live_prompt_total_tokens`, `strata_live_generated_tokens`,
`strata_live_max_tokens`, `strata_live_queued_requests`, `strata_live_tokens_per_second`,
`strata_live_tokens_per_second_mean`, `strata_live_prefill_tokens_per_second_mean`.

Last request: `strata_last_request_info` (labels carry the request's identity),
`strata_last_request_duration_seconds`, `strata_last_request_prompt_seconds`,
`strata_last_request_decode_seconds`, `strata_last_request_prompt_tokens`,
`strata_last_request_reused_tokens`, `strata_last_request_output_tokens`,
`strata_last_request_cache_hit_ratio`, `strata_last_request_reasoning_recoveries`.

Conversation cache: `strata_conversation_cache_enabled`, `_bytes`, `_budget_bytes`, `_slots`,
`_parked`, `_requests_total`, `_requests_reused_total`, `_reused_tokens_total`, `_last_prompt_tokens`,
`_last_reused_tokens`, `_evictions_total`, `_parks_total`, `_restores_total`.

Engines, GPUs and hosts: `strata_engine_context_tokens`, `_max_context_tokens`, `_arena_bytes`,
`_pool_workers`, `_vram_free_bytes`, `_conversation_cache_bytes`, `_conversation_cache_slots`,
`_expert_cache_bytes`, `_expert_slots`, `_images_enabled`, `strata_gpu_utilization_percent`,
`_temperature_celsius`, `_power_watts`, `_power_limit_watts`, `_memory_used_bytes`,
`_memory_total_bytes`, `_pcie_generation`, `_pcie_width`, `strata_cpu_utilization_percent`,
`strata_memory_used_bytes`, `_memory_total_bytes`, `strata_disk_read_bytes_per_second`,
`strata_disk_write_bytes_per_second`, `strata_host_cores`, `strata_host_threads`,
`strata_model_info`, `strata_scrape_duration_seconds`, `strata_up`.

## Build

```
./build-tag-and-push.sh          # sets DOCKER_HOST if you build against a remote daemon
```

Produces `initialed85/strata-exporter:latest` (`linux/amd64`, built by Docker Buildx).

## Deploy

```yaml
spec:
  template:
    spec:
      containers:
        - name: strata-exporter
          image: initialed85/strata-exporter:latest
          command: ["/srv/entrypoint"]           # the image has no ENTRYPOINT
          args: ["-strata-url", "http://strata.example:8088"]
          ports:
            - name: metrics
              containerPort: 9137
```

Scrape the port named `metrics`, or point a job at it directly:

```yaml
- job_name: strata
  static_configs:
    - targets: ["strata-exporter:9137"]
```

No license.
