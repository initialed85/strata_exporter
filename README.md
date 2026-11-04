# strata_exporter

Scrapes a Strata LLM inference runner's JSON status endpoint and re-publishes it in Prometheus text
format. Strata's `/metrics` is a JSON snapshot rather than the exposition format, so this exporter
maps the fields onto gauges and counters - and, separately, keeps the runner's own recent-call list
as a pullable [event ring](#events).

## Usage

```
strata_exporter [-strata-url http://127.0.0.1:8088] [-strata-token TOKEN] [-listen :9137] [-timeout 10s]
                [-events-token TOKEN] [-events-interval 5s] [-events-max 2048]
```

| Flag | Environment | Default | Purpose |
| --- | --- | --- | --- |
| `-strata-url` | `STRATA_URL` | `http://127.0.0.1:8088` | base URL of the Strata runner |
| `-strata-token` | `STRATA_TOKEN` | *(empty)* | bearer token for Strata; empty sends no `Authorization` header |
| `-listen` | `LISTEN_ADDR` | `:9137` | address serving `/metrics` and `/events` |
| `-timeout` | – | `10s` | timeout for fetching Strata's status |
| `-events-token` | `EVENTS_TOKEN` | *(empty)* | bearer token required on `/events`; empty leaves it open |
| `-events-interval` | `EVENTS_INTERVAL` | `5s` | how often the ring is refreshed from the runner |
| `-events-max` | `EVENTS_MAX` | `2048` | how many calls the ring keeps |

The two tokens point in different directions and are not interchangeable: `-strata-token` is what
this process presents to the runner, `-events-token` is what a consumer presents to this process.
Nothing prints either one, and the startup line reports only whether each is configured.

Strata serves `/metrics` behind a bearer token and answers **401** to anything else, so the token
has to be supplied. It is deliberately not baked into the binary: this repository is public and the
image is published to Docker Hub, so a compiled-in credential would be a published one. Pass it from
a Secret (see [Deploy](#deploy)); nothing prints it, and it never appears in the exposition.

`strata_up 1` means the status endpoint answered with a decodable snapshot. A failed poll answers
HTTP 503 with `strata_up 0` and `strata_scrape_http_status`, which separates "nothing answered" (`0`)
from "answered, but rejected, redirected or not the snapshot" (`401`, `404`, a `200` that fails to
decode) - `strata_up 0` on its own cannot tell those apart, and treating them as one condition is how
a wrong token ends up looking like a broken runner.

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

Last request: `strata_last_request_info` (labels carry how it finished),
`strata_last_request_duration_seconds`, `strata_last_request_prompt_seconds`,
`strata_last_request_decode_seconds`, `strata_last_request_prompt_tokens`,
`strata_last_request_reused_tokens`, `strata_last_request_read_tokens`,
`strata_last_request_output_tokens`, `strata_last_request_cache_hit_ratio`,
`strata_last_request_first_token_seconds`, `strata_last_request_reasoning_recoveries`,
`strata_last_request_drafts_offered`, `strata_last_request_drafts_accepted`,
`strata_last_request_ram_blobs`, `strata_last_request_file_blobs`, `strata_last_request_file_mb`,
`strata_last_request_pcie_share`.

Each of these is omitted rather than published as `0` when the runner did not report it. A series
that is missing is a measurement nobody took; a series sitting at zero is a claim that the model did
nothing, and the two look identical on a graph until you compare one to a bill.

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

## Events

`GET /events` answers one JSON object per call, newest last, for a consumer that wants history
rather than a rate:

```json
{
  "cursor": 142,
  "stale": false,
  "dropped_total": 0,
  "available": 12,
  "boot_id": "0a1b2c3d4e5f",
  "events": [{
    "id": "008f61fddf93",
    "ts": "2026-11-04T03:05:20.736492864Z",
    "ts_epoch": 1793771120.7364929,
    "model": "qwen3.8-flash-next-iq3_xxs",
    "engine": "0.1.51",
    "prompt_tokens": 88977,
    "prompt_tokens_computed": 44,
    "prompt_tokens_total": 88977,
    "cached_tokens": 88933,
    "completion_tokens": 390,
    "prefill_ms": 517.1,
    "decode_ms": 6271.7,
    "total_ms": 6800,
    "prefill_tok_s": 85.09,
    "decode_tok_s": 62.2,
    "engine_first_token_ms": 558,
    "cache_hit_rate": 0.9995,
    "finish_reason": "stop",
    "extra": {"hit_rate": 0.872, "drafts_offered": 366, "drafts_accepted": 222, "pcie_share": 0.052}
  }]
}
```

**`prompt_tokens` is the length of the prompt. `prompt_tokens_computed` is what the engine actually
had to evaluate**, which is the prompt minus whatever it served from KV - Strata's `prompt_read`.
They are not two spellings of one number: on the example above the engine read 44 tokens and reused
88,933 of the same 88,977-token prompt, and `cached + computed == prompt` is the identity both
surfaces publish against. Reading `prompt_tokens_computed` as a prompt size turns a 99.9% cache hit
into a graph that says a 44-token prompt.

**`engine_first_token_ms` is neither of the two time-to-first-token measurements anybody else
makes.** It is internal to the engine: the moment it began the call to the moment it produced a
token. Rooter publishes `ttft_ms` (first byte on the wire, which includes queueing) and
`first_content_ms` (first word the client could read) for the same call from outside. All three are
true and none is a refinement of another, which is why this record does not borrow either name.

`cache_hit_rate` is omitted rather than published when the counts contradict each other - a runner
reporting more cached than prompt measured something else, and the honest answer is "no rate", not
a number over 100%.

Query parameters, all optional: `cursor` (return only calls after this sequence), `limit`, `model`,
`failed=1`.

### Reading it as a consumer

Store the `cursor` you were handed back and ask for everything after it. `events[].id` is the dedup
key; the cursor is only a position.

- The ring is filled by a background poll of the runner, independently of how often Prometheus
  scrapes, so a harvester's view of history does not depend on scrape cadence. That does mean the
  snapshot is fetched twice at a low rate; the runner answers it from memory, which is a cheaper
  mistake than coupling the two consumers.
- **A poll that changed nothing returns an empty page and the same cursor.** The runner re-reports
  its recent calls on every snapshot and each one is re-published; records are deduplicated by id,
  so re-reading the same call does not move the cursor and does not count twice.
- **`stale` means the ring no longer holds what your cursor pointed at**, judged against the oldest
  surviving record rather than against an empty page - a consumer that is perfectly caught up also
  gets an empty page, and calling that data loss would be a flag that trains you to ignore it.
- **`boot_id` changes when this process restarts.** The sequence resets, so a cursor carried into a
  new life is meaningless; compare it before trusting a resume.
- The ring is in-memory and bounded. Anything past `-events-max` is gone, counted in
  `dropped_total`. This is not an archive; it is the handoff to whatever archives it.
- An entry the runner reports without an id is dropped rather than inserted, because with nothing to
  dedup on it would re-appear on every poll. Under-counting shows up in `dropped_total`;
  double-counting is invisible in every query.
- Whatever the runner does not report per call is not invented here. `prefill_tok_s` is the one
  derived number, and it is derived from two per-call measurements rather than from a mean.

The envelope is deliberately the same one rooter serves on `/rooter/events`, so one harvester can
read both surfaces by speaking one protocol. Only `events[]` differs, because the two surfaces
genuinely measure different things: rooter describes what a *client* experienced through the proxy,
this describes what the *engine* did. Their shared key is weak by nature - Strata calls a request
`008f61fddf93` while answering the client with `chatcmpl-23a156df...` - so join on `upstream_created`
plus token counts from rooter's record rather than on ids.

When `-events-token` is set, `/events` answers 401 without the matching bearer. When it is unset it
is open, which is what `/metrics` already is: both are inside the cluster, and a gate nobody asked
for is a gate somebody will forget to wire up.

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
          env:
            - name: STRATA_TOKEN
              valueFrom:
                secretKeyRef:
                  name: strata-exporter
                  key: strata-token
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
