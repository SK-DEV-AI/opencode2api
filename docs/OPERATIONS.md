# Fork operations handbook (SK-DEV-AI `opencode2api`)

Branch: `feat/observe-v130`. Upstream: `jasonxu114514/opencode2api`.
Audience: future maintainers (human or LLM). Goal: full context in one read.

## 1. Deployment reality (this host)

- Service: `systemctl --user` unit `opencode2api` (user `sk`, host `cachyos-sk`).
- Binary: repo root `./opencode2api` (built from `./cmd/opencode2api`, `go build -o opencode2api ./cmd/opencode2api`).
- Config: repo root `config.json` (listen `127.0.0.1:8787`, WebUI `127.0.0.1:8081`).
- Deploy ritual: `systemctl --user stop opencode2api`, `cp <new-binary> ./opencode2api`, `systemctl --user start opencode2api`, then `curl -sf http://127.0.0.1:8787/healthz`.
- `healthz` `ready:true` with `models.total` ~84 / `exposed` ~75 is healthy. A `400` from the API with `model ... is disabled after a failed availability check` is the catalog guard (`internal/models/catalog.go routeLocked`), not a crash.
- Logs: `journalctl --user -u opencode2api`. Key events: `request_routed`, `request_failed`, `availability_checked`, `upstream_error_body`, `catalog_refreshed`.

## 2. Log reading guide (`request_routed` fields)

- `duration_ms`: total downstream request time (accept → response end/cancel).
- `legs`: per-attempt `key:status:ms` (`anon` = anonymous lane, `transport` = network error, `429/503` = upstream status). Attempt count = number of legs.
- `outcome`: `success` | `client_canceled` (client went away first — usually jcode's idle timeout, NOT a proxy failure) | `stream_error` | `client_error`/`server_error`.
- `in`/`out`: token usage, present only when upstream reported usage (terminal SSE frame). `0/0` on canceled streams is expected — no usage frame ever arrived.
- `stop`: upstream finish reason. Empty + `client_canceled` = killed while thinking.
- Critical distinction: legs show **headers** timing (`anon:200:5s` = upstream accepted in 5s). Silence *after* headers is model thinking, invisible in legs.
- Query pattern: `journalctl --user -u opencode2api --no-pager --since 'YYYY-MM-DD 00:00' --until '...' | grep request_routed`.

## 3. Finding A — availability guard false-disables (commit `ad2d5e7`, KEEP)

- Symptom: `muse-spark-1.3-contributor-free is disabled after a failed availability check`, survives restarts.
- Persistence: `config.json.<hash>.availability.json` (`disabled:true reason=http_403 channel=zen next_check=+24h`). Restart alone never clears it.
- Root cause: `internal/gateway/availability.go` probe used `Session: "availability:"+model`, violating `identity/request.go` canonical pattern `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$` (Zen free tier 403s non-canonical sessions since 2026-09-16). Real traffic canonicalizes; the probe didn't.
- Fix: `identity.CanonicalSessionID("availability:"+model)`. Log proof: same minute `request_routed ... anon:200 outcome=success` vs `availability_checked ... available=false reason=http_403 channel=zen`.
- Runbook: `rm config.json.*.availability.json && systemctl --user restart opencode2api`, or WebUI Restore (needs `webui.enabled:true`), or wait `next_check`. Probe cadence: 1h ok / 24h fail (`internal/models/availability.go`).

## 4. Finding B — hedged race reverted, heartbeat added (commit `e9eac0d`, KEEP)

- `701f900` fanned every request to ALL lanes in parallel (anon + every key = ~7x upstream load per turn on one shared IP bucket). Reverted whole: zero latency win available there (legs already 3-9s; the stall was downstream-side silence, not header latency), and first-wins breaks session affinity (winner is whichever lane answers first, so warm-cache stickiness is lost and burn multiplies).
- Perf defaults restored (`2048/256/5s/15s`). `AttemptTimeout` doc restored (no 20s fail-fast default).
- `e9eac0d` added the SSE heartbeat instead (`internal/protocol/stream.go`, `: keepalive` comment every 15s, both lanes, stops permanently at first upstream byte). This fixes the real stall: jcode's idle timer (180s base, longer on xhigh) firing while the model thinks silently. `go vet` + `go test ./...` clean, live smoke passes.
- Known negligible race (documented, not fixed on purpose): the timer write and the emitter write share `w` with no mutex. Worst case is one garbled SSE frame in a multi-minute stream, self-healing. Threading a lock through every emitter path costs more than it buys.
- `first_event_timeout_seconds` stays `0` (disabled): bounding time-to-first-token would retry thinking models (muse xhigh is silent for minutes) instead of waiting. The heartbeat covers the downstream side.

## 5. Muse-spark verdict (2026-10-07/08 logs — model-side, NOT proxy)

- "Loads forever" = thinking + 160-200K context. Legs usually `anon:200:3-9s` (headers fast); `duration_ms ~184s outcome=client_canceled` (jcode gave up first, proxy was still streaming); one turn `in=206673 out=11993 reasoning=11835`. Headers are not ALWAYS fast: 2026-10-08 log has one `anon:200:61413ms` cancel (upstream took 61s for headers, still thinking-side slowness, not proxy queueing). Small-context muse = fast (user-confirmed). xhigh multiplies thinking time; high is the sane default.
- `opencode2api.service: Failed with result 'timeout'` + SIGKILL on restart = expected, not a bug: streams live minutes, `TimeoutStopUSec=10s` < drain time, so every restart SIGKILLs in-flight turns. Gateway is stateless, clients retry. No unit change.

## 6. Free-suffix naming (NOT a proxy bug)

- `-free` is upstream's model-ID naming, not our label. Free-ness = `models.dev` costs `0/0` (`metadata_free`, e.g. `big-pickle`) OR name fallback (`free` in ID). Verified against live `config.json.models.dev.json` (132 entries): `jev-1.13`, `sota`, `sweet` are absent from models.dev AND lack the substring, so the anonymous lane denies them — but they route via the 6 free keys' authenticated lanes and still appear in `/v1/models`. Paid IDs (gpt-5, claude, kimi...) take the same key-lane path on workspace allowance. Nothing to change.

## 7. Finding C — offline boot false-disables free models (commit `2f65507`, KEEP)

- Symptom after laptop boot without network: `model "muse-spark-1.3-contributor-free" is disabled after a failed availability check` on every request; fixed only by `rm config.json.*.availability.json && restart`.
- Root cause: `startAvailabilityChecks` probes immediately at startup. Offline, every lane fails with `transport_error`, `probeFreeModel` returned `attempted=true success=false`, and `Record` persisted `disabled:true` with a 24h `next_check`. The probe could not distinguish "model dead" from "nobody home".
- Fix: `probeFreeModel` tracks `gotResponse`. If NO lane produced any HTTP response, it returns `attempted=false` (`no_upstream_contact`), and `checkFreeModels` skips the `Record` entirely — persisted state and `next_check` untouched. A reachable-but-rejecting upstream (503 etc) still records, so genuinely dead models keep being disabled.
- Tests: `TestProbeOfflineIsUnattempted` (refused connection → unattempted) and `TestProbeHTTPErrorStillRecords` (503 → attempted) in `internal/gateway/availability_test.go`.
- jcode selector corollary: `handleModels` serves `availableModels()`, which skips disabled models — so an offline boot also shrinks `/v1/models` and jcode's selector loses the `-free` entries until re-discovery. After this fix, boot offline leaves the list intact; if the selector still looks thin, force jcode re-discovery (restart jcode) after confirming `curl /v1/models` lists the model.
