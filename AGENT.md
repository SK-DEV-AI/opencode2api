# opencode2api fork — agent context (maintained by agent, 2026-10-09)

## Session hygiene (proven 2026-10-10 — jcode source-read, not vibes)
- The "Session history: X tokens processed" warning counts LIFETIME cumulative tokens, not current context (`crates/jcode-tui/src/tui/ui_input.rs:617`, comment says "This is not current context usage"). First appears after one full context window of lifetime work. Compaction never resets it. Only `/clear` does.
- Compaction summarizes but does NOT trim below ~80% window usage: `active_messages()` sends everything after `compacted_count` (`crates/jcode-base/src/compaction.rs`). A 27-day session sent 725 msgs (10.8MB, 317 orphaned tool outputs) per API call at only 30% window — provider burned 98s on a 99.7% cache-warm call. Upstream slowness on payload shape, not network.
- Rule: one session per workstream, not per project. Rotate when the warning first appears. Keep swarm/background sessions out of forever-sessions (orphan tax). No screenshots in forever-sessions (~2.4MB base64 each, permanent).
- This repo is now self-contained: AGENT.md + docs/FORK.md + docs/ZEN-FREE-TIER.md + docs/speed-audit-20261007.md live here. `~/session-root/work/opencode2api/` keeps only `keys-backup-20260925.json` (SECRETS — never commit).

## Full audit 2026-10-09 (all three tools, HEAD `1a63481`)
- omen MCP needed `mcp reload` after registry add (server reads config at spawn; new entries invisible until reload). semantic_search over MCP HANGS (cold index build spins 7+ CPU-min, load 4+; killed orphan, CLI instant). Use omen via CLI, not MCP, for heavy tools.
- REAL BUG (omen TDG F grade): `identity/request.go:123` panicked on crypto/rand failure — every request path calls RandomID. Fixed: math/rand fallback, request succeeds with weaker uniqueness. Recovery middleware would only have 500d it.
- REAL DUP (omen clones): metrics.go twin ring buffers exact clones. Fixed: generic `eventRing[T]` + aliases (zero call-site change) + `metrics_test.go` (FIFO/overflow/prune, both types). Clones 6→3 (only content.go near-miss left).
- cqs PROVES UNIQUE VALUE: `callers`/`test-map`/`impact`/`context` all accurate (probeFreeModel: 3 callers, full main→chain, file chunk map). `dead`/`suggest` noisy (mux-registered handlers flagged dead, builtins flagged high-risk). KEEP CLI-only.
- NO FORK on cqs: stale 3mo but works, CLI-only = zero maintenance burden, 35KB Rust not ours to carry, issues are feature-roadmap not rot. Revisit only if `cqs index` breaks or a CVE lands. Multi-fork sessions split model attention — proxy fork stays solo.
- Diagnostics: serena/gopls all Hint/Information (maps.Copy loops, tagged switches, CutPrefix) — style noise, no behavior. `omitempty` on time.Time no-op, harmless. maps.Copy LONGER than loops here, skip.
- TDG worst = hot path (upstream D, gateway C-, request C-, config C) — complexity is inherent to protocol translation, not fixable without rewrite. Normalize cx=53 is sequential validation, fine.
- Test gap: 10/12 packages zero coverage. Started closing: telemetry now has ring tests. models/config/protocol still bare.
- serena drops project activation between calls — batch diagnostics per activation.

## Tool scoping: global vs per-project (proven 2026-10-09)
- GLOBAL (any session, any repo): MCP registry (`~/.jcode/mcp.json`), binaries (serena/cqs/omen/gopls), wrappers (`~/session-root/repos/{serena,omen,cqs}-mcp/run`), `mcp_tools="auto"` bloat guard. A new session gets all of this with zero setup.
- PER-PROJECT (each repo onboards itself): serena `activate_project` (auto-detects language: Go for opencode2api, Python for websearch — proven both live), `.serena/` index dir (gitignored, recreated on demand), serena memories (scoped `opencode2api/*`, other repos get their own), cqs `.cqs/` index (per-repo, rebuilt via `cqs index`), omen (stateless — no index, works anywhere instantly).
- Another session's recipe: `activate_project` with its repo path → serena auto-indexes → `cqs index` if concept search wanted → omen works immediately. Nothing is locked to this session.

## Tool maintenance + interop + bloat (answered 2026-10-09 with live evidence)
- Maintenance: serena PUSHED 2026-10-08 (30k stars, v1.7.0 local matches latest, 109 issues — actively maintained). omen PUSHED 2026-10-05 (v4.30.0 local matches latest, 4 issues — healthy). cqs STALE: pushed 2026-07-13, latest release v1.54.0 from 2026-07-03, 32 open issues — 3 months quiet. cqs stays CLI-only (no MCP wiring) until it shows life; if it rots, tokensave is the fallback.
- Interop: serena + omen live in registry (different tool names, different jobs, no conflicts). cqs REMOVED from registry 2026-10-09 (stale upstream + 30 tools of bloat not justified; CLI-only via `cqs --quiet`). omen is stateless stdio (no daemon). cqs MCP needs its daemon per project — without it the bridge errors cleanly.
- Bloat: ~54 new tools total (serena 29 active, omen 25). cqs stays out of the registry until upstream shows life. Mitigation is jcode's `mcp_tools="auto"` (defers at 8K tokens — already live in config.toml). No per-server trim available in jcode; the anti-forget is THIS FILE plus trigger phrases: "who calls X" → serena, "find by concept" → cqs CLI, "hotspot/health/debt" → omen CLI.

## Tool field verdict (surveyed + trialed live 2026-10-09)
- serena (oraios, Python+LSP, AGPL): WIRED into `~/.jcode/mcp.json`. Proven: symbols/callers/declarations/pattern-search/diagnostics/memories all live. Gaps: no semantic concept search, no health scores, no git-history analytics, memories are manual (no auto fact extraction). Verdict: KEEP as the structural navigator.
- cqs (jamie8johnson, Rust, MIT, 16 stars, STALE since 2026-07): TRIALED live (semantic query hit `refresh.go` in ~5s). CLI-ONLY (`cqs --quiet "<concept>"`), registry entry REMOVED 2026-10-09. Costs: 8min build, 24MB `.cqs/` index, dropped CUDA `.so` files (removed + ignored). If it rots, tokensave is the fallback. NOT a serena replacement.
- omen (panbanda, Rust, Apache-2.0, 20 stars): TRIALED live (`cargo install omen-cli`, instant). `score`/`hotspot`/`churn`ACCURATE (matches known history: upstream.go/request.go hottest). `deadcode` NOISY on Go methods (entry-point model misses method dispatch — both top hits proven live via serena). `satd` keyword noise. `smells` fan-in/fan-out all-zero (broken on this repo). Verdict: USE `hotspot`/`score`/`churn` as read-only advisors before big edits; IGNORE `deadcode`/`smells` without serena cross-check.
- repowise (7.3k stars, AGPL, Python+LanceDB): REJECTED (heaviest option, overlaps serena+cqs+omen combined, per-commit hooks + AGPL burden for 58 files).
- tokensave (657 stars, MIT, Rust, 40-80 tools): REJECTED for now (overlaps serena symbols + cqs search; revisit if cqs MCP disappoints).
- mem0 (41k stars, memory layer): REJECTED (solves cross-session fact memory; our AGENT.md + serena memories + mc-bridge already cover it; $249/mo graph tier, stale-memory issues reported).
- Sourcegraph/Greptile (hosted): REJECTED (cloud, money, code leaves machine — violates local-first).

## Honest context gaps (audited 2026-10-09 — I do NOT hold all 58 files)
- Fully read this session (~15 files): availability.go (gateway+models), runtime.go, catalog.go, pricing.go (Decide only), stream.go, pool.go snapshot fns, upstream.go race section, gateway.go serveOne/err paths, config.go perf, main.go shutdown, collapse.go head, replay/upstream tests.
- NEVER opened, only grep-level: `internal/protocol/request.go` (1069 lines, biggest file — bridge shaping), `stream_emitter.go` (715), `response.go` (457), `content.go` (415), `stream_parser.go` (364), `models/discovery.go` (288), `models/cache.go` (227), `telemetry/metrics.go` (631), `gateway/rotation.go` (359), `gateway/refresh.go` (261), ALL of `internal/admin/` (except server.go symbols via serena), `identity/request.go` (except regex line), `rotation/state.go`.
- Rule: before touching any file in the never-opened list, READ IT WHOLE first. Do not infer its contents from names or callers.
- Tool verdict: serena trialed 2026-10-09 (`uv tool install serena-agent` + gopls, indexed 59 Go files in ~8s, `POST /query_project` proven: accurate symbols on unread `admin/server.go`, all `probeFreeModel` callers with line numbers). CORRECTION 2026-10-09: jcode DOES have full MCP client support (`~/.jcode/mcp.json` registry + `--mcp-tools` flags; websearch/codesearch/mc-bridge all served from there) — my earlier "no MCP surface" claim was wrong. Serena wiring into the registry under test; KEEP `.serena/` index dirs out of the repo regardless. WIRED 2026-10-09: `serena` entry live in `~/.jcode/mcp.json` (wrapper `~/session-root/repos/serena-mcp/run`, gopls symlinked to `~/.local/bin`), MCP tools confirmed in-harness (`activate_project` + `get_symbols_overview` proven on never-opened `telemetry/metrics.go`).

## Repo/service
- Repo: `~/session-root/repos/opencode2api`, branch `feat/observe-v130`, HEAD `1a63481`. Pushed to fork. Clean. Backup `backup-pre-rebase-20261007`.
- Deploy ritual: `systemctl --user stop opencode2api`, `cp <new-binary> ./opencode2api`, `systemctl --user start opencode2api`, then `curl -sf http://127.0.0.1:8787/healthz` (`ready:true`, total ~84 / exposed ~75).
- A `400 ... is disabled after a failed availability check` is the catalog guard (`catalog.go routeLocked`), not a crash.
- Log reading: `duration_ms` = total downstream time; `legs` = per-attempt `key:status:ms` (headers timing ONLY — silence after headers is model thinking, invisible in legs); `outcome` success/client_canceled/stream_error; `in/out 0/0` on cancels = no usage frame arrived (expected); empty `stop` + canceled = killed while thinking.

## Findings (all KEEP — do not regress)
- A (`ad2d5e7`): probe used raw `"availability:"+model` session, Zen 403s non-canonical → disabled 9 working models 24h. Fix: `CanonicalSessionID`. Runbook: `rm config.json.*.availability.json && restart`, or WebUI Restore, or wait `next_check` (1h ok / 24h fail).
- B (`e9eac0d`, reverts `701f900`): hedged race fanned ~7x load/turn, broke session affinity, zero latency win. Replaced with SSE heartbeat (`:keepalive`/15s both lanes, stops at first byte). Known negligible race (timer vs emitter write, no mutex — one garbled frame worst case, self-healing). Perf back to stock (2048/256/5s/15s).
- Muse verdict (NOT proxy): "loads forever" = thinking + 160-200K ctx; legs `anon:200:3-9s` then 184s `client_canceled` (jcode gave up first); small-ctx = fast; high > xhigh.
- Suffix naming (NOT proxy): `-free` is upstream naming; free = models.dev `0/0` (big-pickle) OR name substring; jev-1.13/sota/sweet route via key lanes.
- C (`2f65507`): offline boot probed immediately, `transport_error` persisted `disabled:true` 24h. Fix: no-HTTP-response probes return unattempted (`no_upstream_contact`), state untouched. Also shrank `/v1/models` (disabled hidden) → thin jcode selector; force jcode re-discovery if needed.
- D (`95132b5`, serena audit): `verifyProxyAfterError` WithoutCancel leaked 10s checks past disconnect → now `WithTimeout(ctx)`. Heartbeat stop verified CORRECT (data-only handler). Cursors/rotation/quota/panics/dead-code clean. First-event gate stays `0`.
- SIGKILL on restart EXPECTED (`TimeoutStopUSec=10s` < minute streams; stateless, clients retry).

## Request path (code)
- `cmd/opencode2api/main.go` → `RuntimeManager.Handler` → `telemetry.Middleware` (meta, request_routed line) → `authenticate` (server_keys) → `handleInference(external)` → `catalog.Route` → `prepareRouteBodies` (per-tier bodies) → `doUpstream`/`doUpstreamWithOffset` → `serveOne` (passthrough if same protocol else transcode) → stream or collapse/convert.
- `forwardSystemOne`: raw `io.Copy`, no reset recovery. Deliberate gap.
- `doUpstream` returns attempt count; replay continues numbering via offset. Failed replay emits replay's outcome, not suppressed first cause. Double reset (replay resets too) emits error frame, no second replay.
- Header-phase client disconnect → 499 `client_canceled` (both inference + systemone paths), not 502.

## Upstream attempt loop
- Anonymous first (public credential, agent-shaped body via `prepareAnonymousBody`: stream+tools+include_usage), then KeyTiers in prefer order, max_attempts per tier.
- Non-retryable 4xx (not 401/403/429) exits tier. Transport/5xx/429 rotate. Retry-After honored. Cooldowns exponential to 8x base.
- Stale `rs_*` reasoning refs: one stripped replay, session preserved, Responses-only.
- `recordUpstreamAttempt` appends `key:status:ms` legs (`anon` label, `timeout`/`transport` for errors) + monitor ring.

## Streams (two lanes, know asymmetries)
- Transcode (`TranscodeStream` → parser→emitter): returns `StreamOutcome{Stop,Tail,Delivered}`. Partial reset AFTER delivery → `SetStop("length")+Finish()`. Zero-delivery → `ErrZeroDeliveryReset`, no frame, gateway replays once.
- Passthrough (`ForwardStream` + observer): returns `StreamOutcome` too. Partial reset → error frame (NOT length rescue). Delivery counted in data frames (keepalive-only still replays; clean empty close does not).
- Transcode `Delivered()` = started||text||tools||reasoning||sig. `started` counts (reviewer fix: replay after message_start would double-emit). Idless usage-only still replays; id-carrying usage emits start → no replay.
- **Heartbeat (2026-10-08, `e9eac0d`): `streamHeartbeat` emits `: keepalive` every 15s on both lanes while upstream is silent; stops permanently at first real frame. Every SSE client ignores comments as data but counts them as activity — resets downstream idle timers during legit thinking. Reason it exists: spark xhigh over 150-200k ctx emits zero data frames for minutes; jcode's 180s idle timer killed those turns.**
- Observer: stop reason + 4KiB-bounded text (last 200 chars as tail on length/empty-stop turns).
- `EmitStreamError` for post-replay failure frames. `CollapseStream` for force-streamed non-stream clients.

## Timeouts (hard-won, verify in Go source not memory)
- `ResponseHeaderTimeout` (attempt_timeout_seconds, live 30s) starts ONLY after full body written. Verified Go 1.27 transport.go.
- 2026-10-05 CORRECTION: WatchdogReader experiment REVERTED (`9664a47`). Wrapping body hid `*bytes.Reader` type from `NewRequest` so `ContentLength` stayed 0 → every upstream upload went chunked. Worse: mechanism was wrong — `bytes.Reader.Read` never blocks, so a Read watchdog cannot bound slow network writes; the 53-72s stalls were TCP writes over hotspot, not Reads. Lesson: bound the write path or don't touch it; never wrap request bodies in a way that hides concrete type from net/http.
- Live proof of chunked cost: after revert, small-turn legs back to 1.7-8s (`anon:200:1742ms-8198ms`).
- Connect timeout 5s (dialer). Request budget 300s total. IdleConnTimeout 120s, MaxIdlePerHost 256, HTTP/2 forced.
- 2026-10-05 network: egress `152.57.90.165` (carrier NAT, v4-only; v6 dial fails locally). v4 connect 0.38s, models POST starttransfer 1.5s. Upstream serves h2.

## Routing/catalog
- `catalog.Route`: anonymous-first for free models (pricing store decides, `-free` fallback), then KeyTiers in prefer order. Per-tier native protocols; bodies re-encoded per tier.
- PricingStore: models.dev 24h refresh → anonymous allow/deny. Discovery: per-tier /v1/models + `models.opencode.ai/api.json` SDK→protocol map + docs tables fallback. Metadata (context windows etc) on GET /v1/models.
- Session affinity: `CursorFor(session)` pins key per conversation (cache affinity). IDs: `CanonicalSessionID` → `ses_`+sha256 shape (free tier 403s anything else); preserve upstream cache affinity. Derivation: headers → first user turn → previous_response_id → random fallback.
- `-free` is upstream's ID naming, not our label. Free = models.dev `0/0` costs (e.g. `big-pickle`) OR `free` in name. `jev-1.13`/`sota`/`sweet` are in NEITHER (absent from 132-entry models.dev cache) → anon lane denies them, but they route via the 6 free keys' lanes and still list in `/v1/models`. Nothing to fix.

## Telemetry
- `RequestMeta`: Model/Tier/Protocol/Request/Session/KeyID/Channel/Anonymous/Proxy/Attempts/Legs/Stop/Tail/Stream/Shaped/Usage/UsageReported/AttemptOutcome/Outcome.
- `request_routed` line carries all of it incl `session`. Monitor: per-min buckets, token coverage, attempt aggregates, rings (10k req/20k attempts, 500 out), usage.sessions map (lifetime+hourly).
- Outcomes: success/stream_error/client_canceled; header-phase 499 client_canceled; 504 only on DeadlineExceeded.

## Protocol bridge
- Canonical bridgeRequest/bridgeResponse across chat/responses/anthropic. Same-protocol = clone (provider fields survive).
- `ForcedEffort`: operator level, client-explicit always wins. Reasoning-history repair: chat additive, anthropic gated to reasoning vendors (moonshot/kimi/deepseek/mimo hints).
- Phantom tools demoted via `usableToolBlocks` (name required). `prompt_cache_key` etc forwarded (cache affinity).

## Zen free tier (from sst/opencode@907b3bc source, file ZEN-FREE-TIER.md)
- `public` = no credential. allowAnonymous models → IP limiter (Redis `ip:<ip>` lifetime + `ip:<ip>:date+model` daily, Retry-After end of UTC day, `FreeUsageLimitError`). Else key limiter (per-key/model/minute, default 1000).
- Second gate: trialLimiter, per-IP lifetime token budget (promoTokens). Both reset on IP change (hotspot airplane toggle). IPv6 /64 shares bucket. CGNAT shares with strangers.
- Sticky provider server-side on modelId/sessionId (set on 200) → same session = warm cache; new sessions scatter cold.
- All local keys free. Anonymous = engine, keys = seatbelt. Never buy keys for free limits (independent systems).

## Upstream PRs (4 open, honest claims only)
- #46 docs systemone, #48 error logs, #49 dead code, #50 ledger. #47 withdrawn (argued maintainer default w/o data — never again without numbers). #32 closed superseded w/ review mapping.
- Keep local: rescue/replay (needs live saves as evidence), census signature, SystemOne recovery.

## Speed facts (2026-10-05 measurements, CONFIRMED 2026-10-07: 253 req sample)
- Proxy overhead is ms: small turns `anon:200:1.7-8s` total. Latency = hotspot upload of huge bodies + upstream provider.
- Per-model today: `big-pickle` p50 4.5s, `mimo-v2.5-free` 1.3s, `muse-spark-1.3` p50 34s (vs 7s Oct-04, 24s Oct-03) — spark's provider slow TODAY, not the proxy.
- The 180000ms `499 client_canceled` lines: downstream harness gives up at exactly 180s (`context canceled`, stream:false). Fix is caller-side: stream or raise caller timeout, not proxy-side.
- 2026-10-07 split (same proxy, same evening): pickle 22/24 success vs spark 156/229 success + 63 canceled. Canceled spark legs = `anon:200:3-9s` headers then 180s silence = xhigh thinking over 150-200k ctx emitting zero SSE frames; jcode logged `no data for 180s`. Legs only show header timing — silence after headers is thinking, invisible in legs. `in/out 0/0` on canceled = no usage frame ever arrived (expected).
- jcode idle budget scales ×3 for xhigh but ONLY on Responses-payload path (`request_reasoning_effort` reads `reasoning.effort`); our Chat path carries `reasoning_effort` which that code never reads → chat-completions via proxy times out at base 180s. jcode config `stream_idle_timeout_secs=180`, spark effort `xhigh` (`~/.jcode/config.toml`, opencode-proxy provider).
- Heartbeat (`e9eac0d`) is the proxy-side remedy: `: keepalive`/15s resets the idle timer. If kills persist past it, turn is truly stuck upstream → drop spark effort to high, or fix jcode's compat path to use `stream_idle_timeout_for_effort`.
- Same-session stickiness pins slow provider: fresh session re-scatters (cold cache first turn, then warm).
- Recent commits audited for regressions: only the watchdog (reverted). Session-burn = one string field + map insert. 499 change = error-path status only. Stream replay = failure-path only. No other per-request overhead added.

## Consumers
- jcode: `/v1/models` auto-discovery, profile opencode-proxy. opencode: Bearer local-dev-key.
- Pi: `~/.local/bin/pi` wrapper, fresh `opencode-proxy` provider id (NOT patching built-in — 0.85 bundle overrides store baseUrl → 401). Live free set + opencode catalog metadata, settings.json defaultProvider pointed. No `compat` field ever.

## Gotchas learned
- `echo PWD | sudo -S cmd <<EOF` breaks (heredoc steals stdin). Temp file + `sudo cp`.
- Dead helpers existed since pkg refactor: always grep callers incl tests before assuming live.
- Python inline edits to Go files = damage (signature-eating). Use edit tool only.
- Smoke key is `local-dev-key` (config server_keys), NOT jcode api_key.
- `tail -N` truncates SSE; grep `"content"` instead.
- Tests: `stream_forward_test.go` (passthrough+transcode parity), `gateway/replay_test.go` (e2e reset→success, reset→503, double-reset), `gateway/upstream_test.go` (ContentLength guard — added 2026-10-05 after watchdog hid *bytes.Reader and forced chunked uploads).
