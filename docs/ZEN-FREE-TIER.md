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
