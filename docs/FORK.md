# opencode2api — patched fork

Local proxy that bridges AI harnesses (opencode, Pi, jcode) to OpenCode's Zen gateway. Runs on `127.0.0.1:8787` as a systemd user service.

## Fork

- **Upstream:** [jasonxu114514/opencode2api](https://github.com/jasonxu114514/opencode2api) v1.3.5 (`79a208a`, released 2026-09-23)
- **Fork:** [SK-DEV-AI/opencode2api](https://github.com/SK-DEV-AI/opencode2api) branch `feat/observe-v130`
- **Local merge:** `cee34a4` — v1.3.5 merged into `feat/observe-v130` 2026-09-25, all unique local patches preserved
- **Backup:** tag `backup-pre-v135-20260925` + branch `backup-feat-observe-v130-20260925`
- **Base:** v1.3.0 (`46def7f`); local branch was at `4437b64` before the v1.3.5 merge
- **Upstream state 2026-10-04:** `origin/main` still v1.3.5 (`79a208a`), zero new commits — fully current. 4 PRs open (#46 docs, #48 error logs, #49 dead code, #50 ledger). #47 (example default) withdrawn — argued against an explicit maintainer default without measured evidence. Old #32 closed as superseded with review responses. Rescue/replay (patches 3, 5, 6 + Delivered fix + double-reset emit) stay local pending live evidence.
- **Zen research:** `ZEN-FREE-TIER.md` (write-once, from `sst/opencode@907b3bc` source). All keys are free; anonymous IP buckets are the engine, free keys the seatbelt.

### Already upstreamed (safe to drop, do not re-apply)

- PR #17 `/v1/models` metadata (context window, reasoning, modalities) — merged 2026-09-05
- Canonical OpenCode session IDs (`CanonicalSessionID`, UA 1.18.31) — upstream v1.3.1
- Anonymous agent-shaped streams (`prepareAnonymousBody`, `CollapseStream`) — upstream v1.3.2
- Cache-affinity knobs (`prompt_cache_key`, `safety_identifier`, `service_tier`, `store`) — upstream v1.3.3 via PR #34
- Key-tier free-model shaping (`shapeKeyBody`, `IsFreeModel`) — upstream v1.3.3 via PR #33 (superset: adds `ensureAnonymousChatUsage` for Chat usage)
- Cached token logging — absorbed into upstream telemetry
- Retry-loop serving fixes (attempt guards, session preserve, tool_calls finish, param passthrough) — superseded by upstream #26
- Phantom tool-block filtering (`usableToolBlocks`, demote phantom `tool_use`) — upstream via #27

### New in upstream v1.3.3–v1.3.5 (gained by this update)

- Configurable reasoning effort (`reasoning.effort`, `reasoning.effort_by_model`) with budget-rung mapping (`minimal`/`low`/`medium`/`high`/`xhigh`/`max`/`none`)
- System One decision models on `POST /v1/systemone` plus verbatim decision-payload relay on message endpoints
- `ensureAnonymousChatUsage` (`stream_options.include_usage`) so shaped Chat streams keep token usage
- Phantom non-stream tool-block filtering and Chat usage preservation for shaped free requests

## What we patched (still custom on top of v1.3.5)

12 patches across 9 files. All are observability + stream resilience; no routing or protocol behavior changes beyond the rescue paths. Plus the Oct-07/08 incident fixes at the end.

### 1. Per-request attempt ledger (`internal/gateway/upstream.go`)

`recordUpstreamAttempt` appends `key:status:ms` per leg to `meta.Legs` (`anon` label for anonymous, `timeout`/`transport` for errors). Surfaced on the `request_routed` log line as `legs` via `internal/telemetry/http.go`. A 5-attempt turn reads `anon:429/2511ms nM3OH:200/4615ms` in one glance instead of an hour of code reading. Observability only.

### 2. Stop/tail census (`internal/protocol/stream.go`, `internal/protocol/stream_emitter.go`)

`TranscodeStream` now returns `StreamOutcome{Stop, Tail, Delivered}`; emitter gains `StopReason`, `Delivered`, `TextLen`, `ToolCount`, `TextTail`. The `request_routed` line (in `internal/telemetry/http.go`) logs `stop`, `in`/`out`/`reasoning_tokens`/`cached`, and a 200-char `tail` on truncated turns. This is how the cached-token meter and early-yield diagnosis work.

### 3. Mid-stream rescue: partial reset finishes as `length` (`internal/protocol/stream.go`)

A Cloudflare TCP reset after partial content already streamed used to kill the turn with an error event (stranded partial text, forced manual continue). Header-phase failures never reach this path (attempt loop retries them), so a reset here is a truncated generation, not a dead request: `SetStop("length")` + `Finish()` preserves the partial reply and lets the client continue from it.

### 4. Capped upstream-error snippet (`internal/gateway/upstream.go`)

Non-2xx upstream bodies log a 240-char snippet (`upstream_error_body`, hub redactor scrubs keys) naming the 400 class instantly. Observability only.

### 5. Zero-delivery replay (`internal/gateway/gateway.go`, `internal/protocol/stream.go`)

A Cloudflare reset before any bytes reach downstream is indistinguishable from a request never served. The transcoder returns `ErrZeroDeliveryReset` with nothing emitted, the gateway replays once on a fresh attempt inside the same downstream SSE connection, and `EmitStreamError` fires only if the replay also fails. Exactly once, never on client-cancel, never after partial delivery (that path keeps the `length` finish). Converts error-then-manual-continue into slow success. Proven live in the pre-v1.3.5 journal; needs re-proving on the new binary.

### 6. Passthrough-lane zero-delivery replay (2026-10-04, commits `0728780` + `49be257`)

The 14:38 `big-pickle` miss: same-protocol requests take `ForwardStream` (passthrough), which had no zero-delivery concept. A pre-delivery reset surfaced as raw `stream_error` with `in:0 out:0` and never replayed. `ForwardStream` now returns `ErrZeroDeliveryReset` when a read failure happens before any SSE **data frame** arrives, and the gateway streams via a shared `serveOne` helper so both lanes replay. Delivery is counted in data frames, not bytes: an edge probe showed keepalive-only resets (`: ping` comments forwarded downstream) would otherwise bypass the replay. Clean empty closes keep the old error-frame path. Covered by `internal/protocol/stream_forward_test.go` (11 tests: 7 passthrough + 4 transcode parity) and `internal/gateway/replay_test.go` (2 end-to-end: reset-then-success delivers cleanly, reset-then-503 emits the error frame). The end-to-end success test fails on pre-fix v1.3.5 (1 upstream hit + error frame) and passes with the replay. The transcode side documents two lane asymmetries: partial resets rescue as `length` (vs error frame on passthrough), and usage-only frames still replay (transcode counts content frames, passthrough counts any data frame). Live smoke test: `mimo-v2.5-free` streams 200 on the new binary.

Known remaining gap: `forwardSystemOne` relays streaming replies with a raw `io.Copy` (no reset recovery). Streaming System One replies are rare today; left as-is deliberately.

### 7. Zero-delivery replay attempt accounting (2026-10-04)

The replay fired a fresh `doUpstream` with an independent attempt counter, so the replay's legs restarted at attempt 1 while the first round's legs stayed in the log — monitoring showed duplicate attempt numbers and the request line undercounted total upstream spend. `doUpstream` now returns its attempt count and the replay continues numbering via `doUpstreamWithOffset`, so a reset-then-success turn reads as one continuous attempt sequence. Also fixed the failure path: when the replay itself fails or returns non-2xx, the emitted error frame now carries the replay's outcome (transport error or `upstream returned HTTP N on zero-delivery replay`) instead of the suppressed first-attempt reset cause, which misattributed the failure in logs and downstream.

### 8. Dead-code removal + fallback-tier skip log (2026-10-04)

Removed three helpers orphaned since the package refactor: `forceStreamBody` (superseded by `prepareAnonymousBody`), `transcodeStream`/`transcodeStreamWithUsage`, `forwardSSEWithUsage`. Fixed the stale `gateway.go` comment pointing at `forceStreamBody`. Added a debug-level `fallback_tier_skipped` log in `prepareRouteBodies` naming the tier, protocol, and error when a fallback tier's request shape fails to prepare — previously silent, only surfacing later as a missing route at retry time.

### 9. Passthrough stop/tail census (2026-10-04, commit `15d690f`)

The live smoke test caught this one: `ForwardStream` never reported an outcome, so every same-protocol turn (the common case) logged an empty `stop`. `ForwardStream` now returns `StreamOutcome` like the transcode lane, and the gateway wires it into `meta.Stop`/`meta.Tail` for both lanes. The observer tracks the raw upstream finish reason plus a 4 KiB-bounded text accumulator (only the last 200 chars ever surface as `tail`). Live proof on the new binary: `mimo-v2.5-free` passthrough turn logs `stop: tool_calls` where it logged `""` before. Two existing tests strengthened to assert the outcome (`HealthyPassthrough` checks `stop` + delivery, `PartialReset` checks delivery).

### 10. Upstream review holes closed (2026-10-04, commit `280ab1c`)

The maintainer's review on (now-closed) #32 flagged two genuine bugs, both fixed:
- `Delivered()` ignored the emitted opening frame: a reset after `message_start`/`response.created`/first-chunk replayed and double-emitted downstream. Now `started` counts as delivered; those resets take the error-frame path (`TestTranscodeStreamStartOnlyResetDoesNotReplay`, plus the `StartEmitting` usage variant).
- Double reset hung the client: a replay that itself reset before delivery returned `ErrZeroDeliveryReset` with headers already sent and no frame. Now the gateway emits a terminal error frame on that path (`stream_zero_replay_double_reset` log, `TestZeroDeliveryDoubleResetEmitsErrorFrame`). Third objection (passthrough lane) was already covered by patch 6.

### 11. Per-session burn visibility (2026-10-04, commit `6f09c36`)

Motivated by the Zen research: every session shares one upstream IP bucket on the anonymous lane, but nothing showed which conversation was draining it. The canonical `ses_` ID now flows into `RequestMeta.Session`, the `request_routed` log line, the monitor usage aggregates (`sessions` map in lifetime + hourly snapshots), and the recent-request ring. Research filed write-once as `ZEN-FREE-TIER.md`.

### 12. Slow-upload watchdog + honest cancels (2026-10-05, commit `83b4f30`)

Live session `ses_004a7f1b91170Wxx8E0vnkxL5D` (300K-token Spark history over phone hotspot) proved the hole: three turns stalled 53-72s per attempt inside `client.Do` before headers, burning the 300s budget in single attempts. Root cause verified in Go 1.27 transport source: `ResponseHeaderTimeout` starts only after the FULL body is written, so a stalled multi-MB upload has no timer running. Fix: `httpx.WatchdogReader` bounds each body Read by the per-attempt timeout (stored per-proxy, all three Do sites; key loop reordered so proxy resolves first). Same commit: header-phase failures from the client's own disconnect report 499 `client_canceled` instead of 502 `server_error` (message endpoints + System One), and `AttemptTimeout` docs corrected to upload+header scope in the code comment and both READMEs.
### 13. Availability probe canonical session (2026-10-07, commit `ad2d5e7`, KEEP)

The free-model availability prober built its upstream session as the raw string
`"availability:"+model`. Zen free tier 403s any non-canonical session shape
(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`, enforced since 2026-09-16), so 9 healthy
models got `disabled:true reason=http_403` persisted to
`config.json.<hash>.availability.json` — which survives restarts by design.
One-line fix: `identity.CanonicalSessionID("availability:"+model)`. Log proof:
same minute `request_routed ... anon:200 outcome=success` vs
`availability_checked ... available=false reason=http_403 channel=zen`.
Runbook: `rm config.json.*.availability.json && systemctl --user restart
opencode2api` (restart alone never clears it; WebUI Restore needs
`webui.enabled:true`).

### 14. Hedged parallel race — REVERTED, do not revive (2026-10-08, commits `701f900` → `e9eac0d`)

`doUpstreamRace` fired anon + all 6 key lanes concurrently per request, first
2xx won, losers drained; plus tightened timeouts (20s attempt default, 3s
connect, 5s cooldown, doubled pools). Sounded fast, was wrong: the bottleneck
is time-to-first-token on one thinking turn, not lane selection. Every 200k
spark turn became ~7 identical upstream uploads × jcode's up-to-8 retries,
triggering the 429/503 legs in canceled rows. Fully reverted; serial
anon→key failover is correct for this workload. Net diff of the revert commit
vs pre-race = only §15.

### 15. SSE heartbeat for silent thinking (2026-10-08, commit `e9eac0d`, KEEP)

`streamHeartbeat` (`internal/protocol/stream.go`) emits `: keepalive` every
15s on both stream lanes while upstream is silent, stopping permanently at the
first real frame. Reason: spark at `xhigh` over 150-200k context emits zero
SSE data frames for minutes; jcode's 180s idle timer killed those turns (`no
data for 180s`). SSE comments are ignored as data but count as activity, so
the timer resets during legitimate thinking. Evidence: 2026-10-07, 253
requests — pickle 22/24 success vs spark 156/229 + 63 canceled with
`anon:200:3-9s` headers then silence. If kills persist past the heartbeat, the
turn is truly stuck upstream → drop spark effort to `high`, or fix jcode's
openai-compatible chat path to use `stream_idle_timeout_for_effort` (today
only the Responses path scales the budget; our Chat `reasoning_effort` is
never read there).


## Deployment

**Service:** `~/.config/systemd/user/opencode2api.service` (runs as user service)

```
ExecStart=/home/sk/session-root/repos/opencode2api/opencode2api -config config.json
Restart=on-failure
```

**Config:** `~/session-root/repos/opencode2api/config.json` (gitignored, not in backup tag)
- `anonymous: true` — free models route through Zen's anonymous channel (no upstream key needed)
- `listen: "127.0.0.1:8787"` — localhost only
- `prefer: "zen"`, 6 zen keys, 0 go keys
- `retry.max_attempts: 6`, `attempt_timeout_seconds: 30`
- `models.refresh_seconds: 300` — catalog refresh every 5 min
- `webui.enabled: false`

**Build:** `cd ~/session-root/repos/opencode2api && go vet ./... && gofmt -l cmd internal webui && go build -o opencode2api ./cmd/opencode2api` (binary is gitignored; stop the service before `cp` or you get `Text file busy`)

**Health:** `curl -s http://127.0.0.1:8787/healthz` — 2026-10-10: `ok`, ready, 83 total / 73 exposed, 6 zen keys, anonymous on

**Logs:** `journalctl --user -u opencode2api -f` (look for `legs`, `stop`, `cached`, `stream_zero_replay`)

**Note:** the v1.3.5 merge (`cee34a4`) was local-only at first; the fork remote has since been pushed (branch `feat/observe-v130` is fully on `fork`).

## Consumers

### opencode (primary)

Uses the proxy via `opencode.jsonc` model config. No special setup — opencode sends `Authorization: Bearer local-dev-key` and the proxy routes to Zen.

### jcode

Auto-discovers models via `/v1/models` (named profile `opencode-proxy` in `~/.jcode/config.toml`). The upstreamed metadata patch gives jcode correct context windows without any per-model overrides. Config: `model_catalog = true`, `requires_api_key = true`.

### Pi

Pi 0.85.0 bakes free models into the bundle at Zen-direct (`https://opencode.ai/zen/v1`), overriding any store-level `baseUrl` patch. The only documented override mechanism is `~/.pi/agent/models.json`, which composes above registered native providers.

A thin wrapper at `~/.local/bin/pi` registers the proxy's free models under a **fresh provider id** (`opencode-proxy`) instead of patching the built-in `opencode` provider. Pi has no baked-in reference for that id, so the provider's own `baseUrl` (the proxy) is honored — no Zen-direct fallback, no 401. On every launch the wrapper:

1. Fetches the live free set from the proxy's `/v1/models` (new/removed models auto-track, nothing hardcoded).
2. Enriches each entry with metadata from opencode's own catalog (`https://models.opencode.ai/api.json`, live primary, `~/.cache/opencode/models.json` offline fallback): name, reasoning, thinkingLevelMap, input modalities (text/image only — pi's schema rejects the rest), contextWindow, maxTokens, cost. Total metadata failure degrades to bare `{id, baseUrl}` (correct routing, default metadata).
3. Writes `~/.pi/agent/models.json` as `{providers: {"opencode-proxy": {baseUrl, api: "openai-completions", apiKey, models}}}` and points `settings.json` defaultProvider/defaultModel at it.

Entries must NOT include `compat` — the proxy is fully OpenAI-compliant and `compat` is a server-behavior knob for partial implementations only.

The wrapper never touches `models-store.json` and never fails fatally: any error leaves existing files untouched and just execs real pi (`~/.bun/bin/pi`).

## Pi 0.85.0 routing regression (what broke, 2026-09-04)

Pi 0.85.0 update baked the 5 then-current free opencode models directly into the JS bundle with hardcoded `baseUrl: "https://opencode.ai/zen/v1"`. Previously, free models came only from the remote catalog/store where the wrapper's per-model `baseUrl` patch worked. After 0.85.0, the bundled entries override the store during model resolution, sending `local-dev-key` to Zen which returns `{"type":"AuthError","message":"Invalid API key."}`.

**Diagnostic chain:** strace showed Pi connecting to `127.0.0.1:8787` for the wrapper's `/v1/models` probe but `104.20.32.17:443` / `172.66.173.149:443` (Cloudflare = `api.opencode.ai`) for the actual chat. The 401 error format (`AuthError`) is Zen gateway, not the proxy's format. Empty proxy log during the chat confirmed zero proxy traffic.

**Root cause:** Pi's `getApiKeyAndHeaders` uses `resolution.auth.baseUrl` from provider auth resolution, which overrides per-model `baseUrl` from the store. The 0.85.0 bundle hardcodes Zen-direct for the baked-in models.

**Fix:** `models.json` override (documented at `docs/models.md:325-337`) — custom model id matching built-in id replaces the built-in entirely. The wrapper dynamically generates this file from the proxy's live free set.

## PR assessment (what's worth upstreaming)

| Patch | PR-able? | Notes |
|-------|----------|-------|
| 4. Upstream-error snippet | Yes, strongest | Tiny, additive, no signature changes; redact keys (already done via hub redactor), keep 240-char cap. Upstream already logs attempts — this just names the 400 class. |
| 1. Attempt ledger | Yes, with rework | Upstream telemetry already carries `Legs`/`Tail`/`Stop` struct fields (merged from our PRs) but dropped the recording logic. Re-propose as opt-in or sampled field to avoid log-volume objections; the struct half is already theirs. |
| 2. Stop/tail census | Maybe, split it | `StreamOutcome` changes the `TranscodeStream` signature (breaking for downstream forks). Split: propose `Stop`/`Tail` logging without the signature change, or gate behind the existing meta struct. Expect pushback on the 200-char tail in default logs. |
| 3. Length rescue | Risky | Changes failure semantics: a truncated stream reports success-ish (`length`) instead of error. Correct for your harness (continue from partial) but upstream may consider it masking real failures. Needs live evidence + a config flag to have a chance. |
| 5. Zero-delivery replay | Riskiest | Extra upstream call per reset, doubles spend on flaky networks; replay-inside-SSE-connection is subtle. Upstream would want idempotency reasoning, metrics, and probably a knob. Keep local unless resets keep hurting. |

Recommended order if you PR: snippet first (easy win), ledger second (struct already upstream), census third (split), rescue + replay stay local.
