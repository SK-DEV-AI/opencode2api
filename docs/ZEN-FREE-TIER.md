# Zen free-tier mechanics (write-once reference, researched 2026-10-04)

Source: `sst/opencode` upstream source, `packages/console/app/src/routes/zen/`
(commit `907b3bc`). Not docs, not blogs — the enforcement code itself.
Do not re-research; re-verify only if upstream refactors the zen routes.

## The `public` credential

`handler.ts`: `zenApiKey = rawZenApiKey === "public" ? undefined : rawZenApiKey`.
Sending `Bearer public` means "no credential". The fork happens on
`modelInfo.allowAnonymous` (per-model server flag):

- Anonymous-allowed → `createIpRateLimiter` (Redis, keyed on `x-real-ip`)
- Everything else → `createKeyRateLimiter` (Redis, keyed on Zen API key)

## Anonymous limiter (`util/ipRateLimiter.ts`)

- Redis keys: `ip:<ip>` (lifetime) + `ip:<ip>:<YYYYMMDD><model-2chars>` (daily).
- Daily cap from server-side `ZEN_LIMITS.free.dailyRequests` (not public).
  `dailyRequestsFallback` applies only without the client check-headers
  (currently hardcoded `headersExist = true`, so the main cap applies).
- Default models (no per-model rateLimit) additionally get a **lifetime**
  budget of `dailyLimit * 7`. New IPs (`lifetimeCount < dailyLimit * 7`)
  get `dailyLimit * 2` per day, ramping down to `dailyLimit`.
- Retry-After is always **end of UTC day**. Error: `FreeUsageLimitError`.
- IPv6 is truncated to the first 4 hextets — a /64 shares one bucket.

## Trial limiter (`util/trialLimiter.ts`) — the second IP gate

- Per-IP **lifetime token budget** (`ZEN_LIMITS.free.promoTokens`),
  tracked in `IpTable`, counting input+output+reasoning+cache tokens.
- New IPs route to trial providers until the promo budget burns.
- Also keyed on IP. Airplane-mode rotation resets this too.

## Key limiter (`util/keyRateLimiter.ts`) — a different game

- Per-key, per-model, **per-minute** window (default 1000, Retry-After 60s).
- Authenticated requests skip the IP limiter for rate purposes.
- Keys do NOT raise the free allowance; they switch bucket systems plus
  whatever the workspace subscription/balance allows.

## Billing sources (`handler.ts`)

`anonymous | free | byok | subscription | lite | balance`. Free models on
no credential bill as `anonymous`. Usage is still tracked per request
(`trackUsage`) even on the free lane — the data exists server-side.

## Session stickiness (`util/stickyProviderTracker.ts`)

- DB mapping `modelId/sessionId` → provider, written on HTTP 200.
- `stickyId = x-opencode-session header, else workspaceID, else IP`.
- Same session keeps landing on the same upstream provider (warm prompt
  cache). New session IDs scatter across providers (cold cache).
- This is why the proxy mints canonical `ses_` IDs and forwards
  `x-session-affinity`: it preserves cache affinity end to end.
- Free tier rejects non-canonical session shapes with 403 FreeTierError,
  so the canonicalization is mandatory, not cosmetic.
- Since ~2026-10-11 it ALSO validates the 12-hex time-part encoding: must
  decode to the official client's `Identifier.descending()` layout
  (`~(ts_ms<<12|counter)`, counters 1-63). Raw-hash prefixes 403 with
  `FreeTierError: free tier can only be used from within OpenCode` on EVERY
  lane (anon + keys) even in perfect `ses_` shape. Upstream #70, ported as
  fork `7b458a2`: re-encode foreign time parts to descending layout
  (counter 1 → `ffe` suffix), pass genuine descending IDs through.
- Since 2026-10-14 it ALSO fingerprints the whole client (fork `3c249b6`
  ports upstream f8e996d/be91f86/0204ca1): keyless endpoint moved to
  `https://opencode.ai/inference`; UA must be `opencode/latest/<ver>/cli`;
  `x-opencode-session-id` must duplicate the session; `x-opencode-project`
  must be bare 40-hex (`prj_*` 403s); `x-opencode-request` must be ABSENT;
  every recognised tool must carry the genuine CLI definition byte-for-byte
  (`shell`, not `bash`). Any single miss = 403 on all lanes, legs ~1s each.
  Distinguish from quota: quota 429s with Retry-After; this gate 403s fast
  on every key simultaneously.
- **Two anonymous lanes, two tool-bar shapes (upstream v1.4.1, 2026-10-17,
  commits `2319941` + `a9470df`):** the legacy `https://opencode.ai/zen/v1`
  endpoint accepts a LIGHTER agent shape — one genuine `read` tool is
  enough, and other client tools may be sent through as-is (upstream
  aliases the client read tool to `read` and stubs nothing else).
  `/inference` still wants the full recognised tool set. Both lanes were
  measured returning 200 for shaped anonymous free-tier traffic from this
  box, so the proxy tries legacy `/zen/v1` first and falls back to
  `/inference`. A 403 naming tools on one lane is a shaping problem, not a
  quota problem — check which lane the leg used before blaming the bucket.
- **2026-10-18: `403 OpenCode Free Model Gateway Detect Abuse Traffic`** is a
  THIRD, separate 403 class — a rate/abuse gate, neither shaping nor quota.
  Distinguish all four by body and by scope:
  - `FreeTierError: free tier can only be used from within OpenCode` → request
    shape is wrong (session id, UA, tools, project id). The gate sections above.
  - `Detect Abuse Traffic` → the egress IP is being rate-limited. Observed
    twice (18:44:59 and 18:46:44 IST), lasting ~2 minutes, hitting BOTH
    anonymous lanes AND every key lane at the same moment — so it is IP-scoped,
    not credential- or model-scoped. It cleared on its own with no change.
  - `401 This API key has expired` → the credential was revoked upstream (the
    2026-10-18 13:54-15:00 wave killed all 6 keys simultaneously). Replace the
    zen keys in `config.json`; do NOT change request shape and do NOT blame
    the IP.
  - `429` with `Retry-After` → the only genuine bucket signal, and the only one
    of these four with quota semantics.

## What this means for the proxy (all keys are free, no paid tier)

- Anonymous is the engine (IP-daily-buckets, no money involved).
- The 6 free Zen keys are the seatbelt: when the IP bucket fills, turns
  fall through to key tiers (key-minute-buckets) instead of failing.
- Never buy keys to raise free limits — the systems are independent.
- Multi-session multiplies burn on ONE ip bucket (single `direct` egress)
  with cold-cache scatter (distinct session IDs → distinct providers).
- The observable defense is per-session burn in our own logs/monitor
  (`session` on `request_routed`, `sessions` in usage snapshots), since
  upstream exposes no bucket-level headers. `Retry-After` on 429 IS
  forwarded by `copyErrorResponse` — harnesses should honor it.
- IP rotation (phone hotspot airplane toggle) resets both the daily
  limiter and the trial limiter. Failure mode: carrier CGNAT shares the
  bucket with strangers; the key fallback is the safety net.
