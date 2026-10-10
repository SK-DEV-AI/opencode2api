# Speed audit 2026-10-07 — free-model traffic over phone hotspot

Scope: single `direct` egress, carrier NAT, v4-only (v6 dial times out after 15s, measured today).
Live config: retry 6/300s, attempt_timeout 30s, connect 5s, idle 2048/256/120s, h2 forced,
6 free zen keys, anonymous-first, first_event_timeout 0 (disabled).
Baseline today (201 reqs): p50 14.4s, p95 186.7s. Legs: 194x200, 17 transport-error, 15x503.
12 transport legs 31-170s, all muse-spark-1.3 (provider stall, not proxy).

## WORTH DOING

### 1. Force IPv4 on the upstream dialer (saves up to 300ms per new connection)
- CLAIM: Every fresh TCP connection to upstream can pay a 300ms Happy Eyeballs penalty
  because the stock dialer races the dead v6 route before falling back to v4.
- EVIDENCE: `internal/gateway/pool.go:163-166` builds `net.Dialer{Timeout, KeepAlive}`
  with no `FallbackDelay` and `DualStack` default true, so Go's 300ms default applies
  (Go src `net/dial.go:162-173`: "If zero, a default delay of 300ms is used").
  Measured today on this hotspot: `-4` connects in 0.11s, `-6` hangs to the 15s timeout,
  AAAA answers in 51ms (fast enough to trigger the race, route is dead).
  Mitigating factor: h2 multiplexing + idle reuse means only cold/new connections pay it
  (live: 1 upstream conn ESTAB right now).
- VERDICT: worth doing. One-line, zero risk, helps every cold start, reconnect,
  and post-idle-timeout dial.
- CHANGE: in `newTransportPool`, pin the dialer to v4:
  `DialContext: (&net.Dialer{Timeout: ..., KeepAlive: 30*time.Second}).DialContext`
  becomes a dialer with a v4-only control, e.g. wrap `DialContext` to force network
  `"tcp4"` (or set `FallbackDelay: -1` plus prefer-v4; tcp4 is the honest fix on a
  v4-only link). Keep connect timeout 5s.

### 2. Enable TLS session resumption (saves ~1 RTT on reconnect handshakes)
- CLAIM: Every new TLS handshake to upstream does a full handshake because no
  `ClientSessionCache` is configured anywhere on the transports.
- EVIDENCE: `internal/gateway/pool.go:153-181` clones DefaultTransport and sets
  idle/timeout/h2/dial values but never `TLSClientConfig`. Go src
  `crypto/tls/handshake_client.go:359`: `if SessionTicketsDisabled ||
  ClientSessionCache == nil { return nil... }` (no resumption attempted).
  Measured full handshake today: tls 0.66s of the 1.08s cold start. Note: this stacks
  with finding 1 (both paid only on new connections, not on reused h2 conns).
- VERDICT: worth doing. Stdlib one-liner per transport:
  `transport.TLSClientConfig = &tls.Config{ClientSessionCache:
  tls.NewLRUClientSessionCache(128)}`. H2 carries its own session state over the
  same TLS conn, so this is safe with `ForceAttemptHTTP2`.
- CHANGE: add the above in `newTransportPool` next to the other transport knobs.

### 3. Set first_event_timeout to ~90s (converts 31-170s dead legs into fast failover)
- CLAIM: With first_event_timeout 0, a stalled provider holds an attempt until the
  30s header timeout or much longer: 12 transport legs today ran 31-170s, each burning
  request budget and delaying rotation to the next key.
- EVIDENCE: `internal/config/config.json` live has no `first_event_timeout_seconds`
  (defaults 0 = disabled per `internal/config/config.go:202-203`);
  `internal/gateway/first_event.go:32-33` bypasses the gate when `timeout <= 0`;
  all attempts call it (`internal/gateway/upstream.go:309,585,654`).
  Today's data: 12 `transport` legs 31-170s, all on muse-spark-1.3 — the exact shape
  (headers accepted, first SSE event never arrives) the gate exists for.
  Gate cost when healthy is ~zero: `first_event.go:45-61` buffers only the pre-first-event
  prelude (capped 1 MiB) and replays it via MultiReader; nothing downstream is delayed.
  False-positive cost is one extra attempt, which the retry budget absorbs.
  `pool.go:248` already exempts gate timeouts from proxy eviction, so a slow provider
  cannot mark `direct` unhealthy.
- VERDICT: worth doing. 90s is above the p50 (14s) with headroom for slow-but-alive
  providers, below the 170s worst observed. Revisit if legit first events exceed 60s.
- CHANGE: `"performance": { ..., "first_event_timeout_seconds": 90 }` in config.json.
  No code change (plumbing already wired).

### 4. Cut attempt_timeout 30s to 15s (fail over faster on header stalls)
- CLAIM: The 30s header wait is generous for a provider that normally starts the
  response in ~1s (measured starttransfer 1.08s cold, faster on reuse); a hung
  attempt holds a key slot for 30s before rotation.
- EVIDENCE: `internal/gateway/pool.go:161` installs `ResponseHeaderTimeout` from
  `AttemptTimeout()` (`internal/config/config.go:109-118`); live value 30s.
  Go semantics: the timer covers headers only, streams keep flowing after
  (`config.go:98-103`, verified against Go src transport.go:231).
  Today's legs show the gap: header-phase success arrives in ~1s, while the 12
  stalled legs each consumed 31-170s of budget (past the header phase, so finding 3
  is the primary fix; this tightens the earlier phase).
- VERDICT: worth doing, paired with finding 3. 15s still 10x the normal 1s header
  latency; false positives just rotate to the next of 6 keys + anon.
- CHANGE: `"attempt_timeout_seconds": 15` in config.json. No code change.

### 5. Stop double-encoding request bodies per request (2x marshal + 2x unmarshal on hot path)
- CLAIM: Every inference request pays a full JSON marshal/unmarshal cycle that adds
  nothing: `prepareRouteBodies` unmarshals via `PrepareRequest`, then `prepareAnonymousBody`
  unmarshals the result AGAIN and re-marshals it.
- EVIDENCE: `internal/gateway/gateway.go:495` (`PrepareRequest` -> `json.Marshal` at :510)
  then `internal/gateway/upstream.go:279` (`prepareAnonymousBody`: `json.Unmarshal`
  at :361 + `json.Marshal` at :378). On the anonymous lane (the engine for free models)
  the second pass ALWAYS rewrites (forces `stream:true`), so the first marshal is
  pure waste; then `shapeKeyBody` (`upstream.go:536-542`) does a third pass with a
  `bytes.Equal` comparison for key tiers. Bodies are MB-scale on long agentic turns;
  this is per-request, not per-attempt (good), but still 2-3 full serde passes.
- VERDICT: worth doing when touching that code. Pass the `map[string]any` upstream
  payload through and marshal once after anonymous shaping, instead of
  bytes -> map -> bytes -> map -> bytes.
- CHANGE (code): refactor `prepareRouteBodies` to return maps, apply
  `prepareAnonymousBody`-equivalent on the map, marshal once per tier. No config change.

### 6. Fix CloneMap Marshal/Unmarshal round-trip on same-protocol path (correctness + speed)
- CLAIM: Same-protocol requests (the common case: chat-in/chat-out) deep-copy via
  `json.Marshal` + `json.Unmarshal`, which is both slow and lossy (float64 for all
  numbers, map iteration order not preserved).
- EVIDENCE: `internal/protocol/bridge.go:110-113` calls `jsonutil.CloneMap`, which is
  marshal+unmarshal (`internal/jsonutil/value.go:44-49`).
- VERDICT: worth doing with finding 5 (same refactor area). A typed clone or
  copy-on-write is faster and preserves number types. Note `ConvertResponse`
  same-protocol already does the cheap thing (`response.go:14-15`: byte copy).
- CHANGE (code): replace `CloneMap` with a recursive typed clone (or reuse the
  request map read-only where the bridge guarantees no mutation).

## SKIP (measured or read, not worth it)

### 7. Keepalive / idle pool tuning — SKIP, already right-sized
- EVIDENCE: `pool.go:157-160`: MaxIdle 2048 / PerHost 256 / Timeout 120s, single
  upstream host, concurrency is single-digit. Live `ss` shows the h2 conn ESTAB and
  reused. `KeepAlive 30s` (`pool.go:165`) < carrier NAT timeout. Nothing to gain.

### 8. H2 / compression toggles — SKIP, already optimal
- EVIDENCE: `ForceAttemptHTTP2: true` (`pool.go:162`); upstream negotiates h2
  (measured `proto:2`). Go auto-sends `Accept-Encoding: gzip` and decompresses
  (Go src transport.go:3016); SSE is already framing-light. Disabling h2 or forcing
  gzip on uploads would add latency, not remove it.

### 9. Connection reuse across attempts — SKIP, already shared
- EVIDENCE: all attempts in all lanes share the per-proxy `http.Client`
  (`upstream.go:303,309` anon; `:642,654` keys; probes reuse `node.proxy.client`
  at `availability.go:117,123`). `DrainAndClose` before reuse (`upstream.go:236,300,639`,
  64KiB cap in `httpx/body.go:7-13`). Nothing per-attempt dials fresh.

### 10. Anonymous-first order — SKIP, optimal for free models
- EVIDENCE: `doUpstreamTiers` tries anon before KeyTiers (`upstream.go:206-216`).
  Per ZEN-FREE-TIER.md the IP bucket is the engine and the 6 keys are the seatbelt;
  anon-first also avoids spending key-minute budget. Reversing would burn the
  scarcer resource first. Session-affinity pinning (`CursorFor(ids.Session)`,
  `pool.go:439-446`) preserves upstream warm cache; cost is one FNV hash per attempt.

### 11. DNS caching / resolver tuning — SKIP, not on the hot path
- EVIDENCE: A/AAAA resolve in 51ms/533ms through systemd-resolved + Cloudflare
  (`1.1.1.1`, measured). But connections reuse the pooled h2 conn, and Go caches
  resolved IPs per connection; DNS is paid only on new dials (rare). A local
  override would add staleness risk for Cloudflare-fronted IPs for ~ms savings.

### 12. First_event gate ON vs OFF for our models — ON (see finding 3), with the tradeoff stated
- EVIDENCE: gate parses SSE prelude line-by-line (`first_event.go:65-94`), comments
  and incomplete frames do not count, 1 MiB cap. Cost on healthy streams: one
  bufio pass over pre-first-event bytes only; body replayed losslessly.
  Risk: a provider with legit >90s TTFT would false-trigger once per request and
  burn one extra attempt — acceptable with 6 keys + anon and 300s budget.
  spark's 31-170s stalls are the failure mode this gate was built for.

### 13. Telemetry / logging per-request overhead — SKIP (info level), one real note
- EVIDENCE: `telemetry/http.go:117-123` logs one `request_routed` line at info;
  Debug lines (`upstream.go:324,330,332`, etc.) are gated by level.
  `Monitor.Record` (`metrics.go:266-342`) takes one mutex for bucket + ring adds —
  sub-ms, off the network path. `hubHandler.Handle` (`logging.go:166-187`) formats
  twice (stdout + hub ring) but that is local string work, not latency.
  NOTE (not latency): `SecretRedactor.String` (`secrets.go:62-69`) runs
  `strings.ReplaceAll` per secret (8+ keys) per logged string — O(secrets x len) on
  every log line. Fine at this request rate; revisit only if request rate grows 100x.

### 14. Availability probes burning the IP bucket — SKIP, negligible as configured
- EVIDENCE: 10 tracked free models, 2 disabled (`config.json.609206f1a8.availability.json`);
  probes hourly per model, 24h on failure (`models/availability.go:11-12`), one
  max_tokens:64 turn each (`gateway/availability.go:99`). ~8 tiny turns/hour against
  daily IP budgets. `checkFreeModels` serializes on a 1-min ticker
  (`availability.go:44-59`). No action.

### 15. Per-request allocations (maps, cursors, DeriveRequestIDs) — SKIP
- EVIDENCE: `CursorFor`/`Cursor` allocate no memory (comment at `pool.go:427`,
  FNV hash only). `DeriveRequestIDs` (`identity/request.go:24-60`) does header reads
  + one sha256 + regex compile is package-level (`:90`). `conversationSeed` marshals
  only the first user turn (`:72`). All sub-ms vs 1s+ network times.

### 16. Streaming flush / buffer sizes — SKIP, already per-write flush
- EVIDENCE: passthrough flushes every `Write` (`stream.go:199-205`); transcode
  emitter flushes every SSE frame (`stream_emitter.go:648-659`). No batching delay
  exists to remove. `readSSE` scanner 64KiB-16MiB (`stream.go:396-397`); observer
  text bounded 4KiB (`:275-278`, `:362`). Observer parses each frame twice
  (TeeReader + downstream copy) but that is CPU-parallel with network, not added TTFT.

### 17. Expect: 100-continue — SKIP, not sent
- EVIDENCE: verified in Go src: only the client opt-in (`request.go:1537`
  `outgoingLength` checks the `Expect` header) causes the wait; proxy never sets
  `Expect` (`grep Expect internal` empty). No 1s `ExpectContinueTimeout` exposure.

### 18. Retry-After handling / cooldown growth — SKIP, already correct
- EVIDENCE: `parseRetryAfter` honors seconds + HTTP dates (`pool.go:493-501`);
  cooldowns back off 1x/2x/4x/8x capped (`pool.go:123,483-484`); Retry-After extends
  past backoff. End-of-UTC-day Retry-After on IP exhaustion would park anon until
  midnight — correct behavior (keys cover the gap), not a speed knob.

### 19. Rotation path (`handleRotation`) — out of scope, no free-model traffic
- EVIDENCE: `rotation.go:83` pins `RouteForTier(..., TierGo, ...)` and `goNodes`
  are empty (healthz: go 0). Also note `rotationAttempt` (`:165-211`) re-runs
  `prepareRouteBodies` + `shapeKeyBody` per model attempt — wasteful, but cold code
  for this workload. Flag only.

## Suggested apply order
1. Findings 1+2 (transport, one code change, restart) — permanent per-conn savings.
2. Findings 3+4 (config-only, `systemctl --user restart opencode2api`) — converts
   today's 31-170s dead legs into ~90s/15s failover.
3. Findings 5+6 (code refactor, same area) — per-request CPU, biggest on long turns.
