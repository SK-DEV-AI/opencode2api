// Package gateway routes inference requests through managed upstream pools.
package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/rotation"
	"opencode2api/internal/telemetry"
)

const maxRequestBody = 32 << 20

const anonymousZenKey = "public"

// attemptDeadline is the per-attempt context deadline. It is armed only when
// attempt_timeout_seconds is set: otherwise the request-level budget already
// bounds the single attempt it would cover.
func attemptDeadline(cfg config.Config) time.Duration {
	if cfg.Performance.AttemptTimeoutSeconds <= 0 {
		return 0
	}
	return cfg.Performance.AttemptTimeout(time.Duration(cfg.Retry.TimeoutSeconds) * time.Second)
}

// streamContextKey carries the context that bounds an established upstream
// stream, as opposed to the request-level context that bounds the attempt
// phase. It travels in the request context so the upstream HTTP request (and
// therefore the connection that carries the stream) outlives the request
// budget when the operator configured a separate stream budget.
type streamContextKey struct{}

// withStreamContext attaches the stream-phase context to ctx.
func withStreamContext(ctx, streamCtx context.Context) context.Context {
	return context.WithValue(ctx, streamContextKey{}, streamCtx)
}

// streamContextFor returns the stream-phase context carried by ctx, or ctx
// itself when no separate stream budget is configured.
func streamContextFor(ctx context.Context) context.Context {
	if streamCtx, ok := ctx.Value(streamContextKey{}).(context.Context); ok && streamCtx != nil {
		return streamCtx
	}
	return ctx
}

// statusClientClosedRequest is nginx's 499: the client went away before the
// gateway could answer, so the failure is neither upstream nor ours.
const statusClientClosedRequest = 499

// proxyUnavailableMessage is returned when every attempt died on the proxy
// route itself, so operators look at their proxies instead of the upstream.
const proxyUnavailableMessage = "all proxies are unavailable; the upstream could not be reached"

type Gateway struct {
	rotation     *rotation.Manager
	cfg          config.Config
	logger       *slog.Logger
	transports   *transportPool
	zenNodes     *nodePool
	goNodes      *nodePool
	anonymous    *anonymousPool
	catalog      *models.Catalog
	monitor      *telemetry.Monitor
	availability *models.AvailabilityStore
	// attemptTimeout bounds a whole upstream attempt (dial + write + headers +
	// first event), not just the header wait. Zero keeps the legacy behavior.
	attemptTimeout time.Duration
}

func New(cfg config.Config, logger *slog.Logger, monitor *telemetry.Monitor) (*Gateway, error) {
	cfg.Rotation.Defaults()
	rotationManager, err := rotation.New("", cfg.Rotation)
	if err != nil {
		return nil, err
	}
	transports, err := newTransportPool(cfg.RuntimeProxies(), cfg.Performance, cfg.Performance.AttemptTimeout(time.Duration(cfg.Retry.TimeoutSeconds)*time.Second))
	if err != nil {
		return nil, err
	}
	cooldown := time.Duration(cfg.Performance.FailureCooldownSeconds) * time.Second
	zenNodes, err := newNodePool(cfg.ZenKeys, transports, cooldown)
	if err != nil {
		return nil, fmt.Errorf("zen node pool: %w", err)
	}
	goNodes, err := newNodePool(cfg.GoKeys, transports, cooldown)
	if err != nil {
		return nil, fmt.Errorf("go node pool: %w", err)
	}
	catalog := models.NewCatalog(cfg.Prefer, cfg.Models.Protocols)
	catalog.SetRefreshInterval(time.Duration(cfg.Models.RefreshSeconds) * time.Second)
	catalog.SetFreeOnly(cfg.Models.FreeOnly)
	return &Gateway{
		cfg:            cfg,
		rotation:       rotationManager,
		logger:         logger,
		transports:     transports,
		zenNodes:       zenNodes,
		goNodes:        goNodes,
		anonymous:      newAnonymousPool(cfg.Anonymous, transports, cooldown),
		catalog:        catalog,
		monitor:        monitor,
		attemptTimeout: attemptDeadline(cfg),
	}, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.authenticate(g.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", g.authenticate(g.handleInference(wire.Chat)))
	mux.HandleFunc("POST /v1/responses", g.authenticate(g.handleInference(wire.Responses)))
	mux.HandleFunc("POST /v1/messages", g.authenticate(g.handleInference(wire.Anthropic)))
	mux.HandleFunc("POST /v1/systemone", g.authenticate(g.handleSystemOne))
	mux.HandleFunc("GET /healthz", g.handleHealth)
	return telemetry.Recover(g.logger, mux)
}

func (g *Gateway) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		candidates := []string{strings.TrimSpace(r.Header.Get("x-api-key"))}
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			candidates = append(candidates, strings.TrimSpace(auth[7:]))
		}
		matched := ""
		for _, key := range g.cfg.ServerKeys {
			if !g.cfg.ServerKeyEnabled(key) {
				continue
			}
			for _, candidate := range candidates {
				if len(candidate) == len(key) && subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
					matched = key
				}
			}
		}
		if matched == "" {
			protocol := wire.Chat
			if r.URL.Path == "/v1/messages" {
				protocol = wire.Anthropic
			}
			wire.WriteError(w, protocol, http.StatusUnauthorized, "invalid local API key", "authentication_error", "")
			return
		}
		if meta := telemetry.MetaFromRequest(r); meta != nil {
			// The Playground borrows the first enabled key to pass this check, so
			// its traffic gets its own bucket instead of being billed to that key.
			meta.ServerKeyID = config.Fingerprint(matched)
			if isDiagnosticRequest(r.Context()) {
				meta.ServerKeyID = telemetry.PlaygroundServerKey
			}
		}
		next(w, r)
	}
}

func (g *Gateway) handleInference(external wire.Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
			return
		}
		model := jsonutil.StringAt(payload, "model")
		meta := telemetry.MetaFromRequest(r)
		if meta != nil {
			meta.Model = model
		}
		if model == "" {
			wire.WriteError(w, external, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
			return
		}
		if id := g.rotation.GroupID(model); id != "" {
			g.handleRotation(w, r, external, payload, id, model)
			return
		}
		if !g.catalog.Supported(model) {
			// A model the catalog has never heard of is a bad model NAME: 404
			// tells a client to pick a different model. A model the catalog
			// knows but cannot serve is a capability or routing problem, and
			// the old 400 phrasing ("the model uses an upstream protocol that
			// opencode2api does not expose") is accurate for it. The old code
			// returned that 400 for BOTH, so a request for a model the
			// upstream retired (space-bunny-free, 410 Gone upstream) looked
			// like a gateway capability bug and clients retried forever.
			if g.catalog.Known(model) {
				wire.WriteError(w, external, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
			} else {
				wire.WriteError(w, external, http.StatusNotFound, fmt.Sprintf("model %q was not found in the upstream model catalog", model), "model_not_found", "model")
			}
			return
		}
		route, err := g.catalog.Route(model, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous)
		if override, selected := debugKeyOverrideFrom(r.Context()); selected {
			// A per-key diagnostic must not silently be served by another key,
			// another tier or the anonymous lane.
			route, err = g.catalog.RouteForTier(model, override.Tier, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0)
		}
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
			return
		}
		if meta != nil {
			meta.Tier = string(route.Tier)
			meta.Protocol = route.Protocol
		}
		// A System One model has no message-shaped equivalent, so a decision
		// payload submitted on a message endpoint is relayed verbatim instead of
		// being converted. This keeps the model reachable for clients that can
		// only address /v1/chat/completions or /v1/responses — for example a
		// gateway whose OpenAI platform pins every request to Responses.
		if route.Protocol == wire.SystemOne {
			if len(jsonutil.MapAt(payload, "questions")) == 0 || payload["state"] == nil {
				wire.WriteError(w, external, http.StatusBadRequest, "this model uses System One decisions, not chat; send state and typed questions to /v1/systemone", "invalid_request_error", "model")
				return
			}
			g.forwardSystemOne(w, r, body, payload, model, route)
			return
		}
		bodies, err := g.prepareRouteBodies(external, route, payload)
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}
		ids := identity.DeriveRequestIDs(r, payload)
		if meta != nil {
			meta.Request = ids.Request
			meta.Session = ids.Session
		}
		stream := jsonutil.BoolAt(payload, "stream")
		requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
		defer cancel()
		// The attempt phase is bounded by requestCtx (dial, write, headers,
		// first event). Once an upstream stream is established it needs its own
		// budget: a long thinking turn legitimately runs for many minutes, and
		// sharing the request budget with the stream killed those turns
		// mid-generation. The client saw a truncated stream and read the Go
		// error text "context deadline exceeded" as a context-limit failure.
		//
		// The stream context is derived from the CLIENT context, not from
		// requestCtx: a client disconnect must still tear the stream down
		// immediately, and only the deadline changes. The upstream HTTP
		// request is built with it, so the connection survives past the
		// request budget; the attempt phase keeps its own bounds
		// (attempt_timeout_seconds, ResponseHeaderTimeout, connect timeout).
		streamCtx := requestCtx
		streamCancel := func() {}
		if budget := time.Duration(g.cfg.Retry.StreamTimeoutSeconds) * time.Second; budget > 0 {
			streamCtx, streamCancel = context.WithTimeout(r.Context(), budget)
		}
		defer streamCancel()
		requestCtx = withStreamContext(requestCtx, streamCtx)
		resp, upstreamRoute, spent, err := g.doUpstreamCounted(requestCtx, route, bodies, ids, 0)
		if err != nil {
			finalTier := route.Tier
			if meta != nil && meta.Tier != "" {
				finalTier = config.Tier(meta.Tier)
			}
			keyID, channel, anonymous := requestCredential(requestCtx)
			// A downstream disconnect is not an upstream failure: report the
			// real cause so telemetry and clients see a cancel, not a 502.
			if !errors.Is(err, context.DeadlineExceeded) && wire.ClientCanceled(r.Context(), err) {
				g.logger.Debug("client canceled before upstream response", "component", "upstream", "event", "request_canceled", "request_id", ids.Request, "tier", finalTier, "key_id", keyID, "channel", channel, "anonymous", anonymous)
				if meta != nil {
					meta.Outcome = "client_canceled"
				}
				wire.WriteError(w, external, statusClientClosedRequest, "client closed request", "client_canceled", ids.Request)
				return
			}
			g.logger.Warn("all upstream attempts failed", "component", "upstream", "event", "request_failed", "request_id", ids.Request, "tier", finalTier, "key_id", keyID, "channel", channel, "anonymous", anonymous, "error", err)
			// The request never reached the upstream because the proxy route
			// failed; report that instead of blaming the upstream with a 502.
			if errors.Is(err, errProxyUnavailable) {
				wire.WriteError(w, external, http.StatusServiceUnavailable, proxyUnavailableMessage, "proxy_error", ids.Request)
				return
			}
			// An exhausted request budget is a timeout, not a bad gateway: the
			// distinction matters to clients that retry on 502.
			if errors.Is(err, context.DeadlineExceeded) {
				// A write-phase failure means the upstream never finished
				// reading the request body: the payload is too large. Report
				// it as 413 so a client that can shrink the request (a harness
				// that strips oversized inline images) knows to shrink it
				// instead of retrying the same oversized payload forever. A
				// header- or first-event-phase failure is the upstream being
				// slow, which is not the client's fault, and stays a 504.
				status, message, code := timeoutStatus(err, requestCtx.Err() == nil)
				wire.WriteError(w, external, status, message, code, ids.Request)
				return
			}
			wire.WriteError(w, external, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
			return
		}
		defer resp.Body.Close()
		if meta != nil {
			meta.Tier = string(upstreamRoute.Tier)
			meta.Protocol = upstreamRoute.Protocol
		}
		w.Header().Set("x-request-id", ids.Request)
		if resp.StatusCode/100 != 2 {
			copyErrorResponse(w, external, resp, ids.Request)
			return
		}
		var toolAlias string
		if meta != nil {
			toolAlias = meta.ToolAlias
		}
		if stream {
			if meta != nil {
				meta.Stream = true
			}
			if g.monitor != nil {
				g.monitor.BeginStream()
				defer g.monitor.EndStream()
			}
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(resp.StatusCode)
			streamBody := wire.RenameStreamReader(resp.Body, upstreamRoute.Protocol, legacyAnonymousReadTool, toolAlias)
			var usage wire.Usage
			var usageReported bool
			// serveOne streams a single upstream attempt downstream on the
			// lane this turn needs: passthrough when client and upstream
			// speak the same protocol, transcode otherwise.
			serveOne := func(attempt models.Route, body io.Reader) (wire.Usage, bool, error) {
				if external == attempt.Protocol {
					served, reported, outcome, streamErr := wire.ForwardStream(r.Context(), w, body, attempt.Protocol, model)
					if meta != nil {
						meta.Stop, meta.Tail = outcome.Stop, outcome.Tail
					}
					return served, reported, streamErr
				}
				served, reported, outcome, streamErr := wire.TranscodeStream(r.Context(), w, body, attempt.Protocol, external, model)
				if meta != nil {
					meta.Stop, meta.Tail = outcome.Stop, outcome.Tail
				}
				return served, reported, streamErr
			}
			usage, usageReported, err = serveOne(upstreamRoute, streamBody)
			// ponytail: zero-delivery reset replay. Upstream cut the
			// stream before anything reached downstream (Cloudflare
			// shed during the thinking phase). The client saw
			// nothing, so one fresh attempt is indistinguishable
			// from a slow first attempt. Exactly once, never on
			// client-cancel, never after partial delivery.
			if errors.Is(err, wire.ErrZeroDeliveryReset) && !wire.ClientCanceled(r.Context(), err) {
				resp.Body.Close()
				g.logger.Info("replaying zero-delivery stream", "component", "stream", "event", "stream_zero_replay", "request_id", ids.Request, "model", model, "error", err)
				retryResp, retryRoute, total, retryErr := g.doUpstreamCounted(requestCtx, route, bodies, ids, spent)
				if retryErr == nil && retryResp != nil && retryResp.StatusCode/100 == 2 {
					defer retryResp.Body.Close()
					upstreamRoute = retryRoute
					spent = total
					usage, usageReported, err = serveOne(upstreamRoute, wire.RenameStreamReader(retryResp.Body, upstreamRoute.Protocol, legacyAnonymousReadTool, toolAlias))
					if meta != nil {
						meta.Tier = string(upstreamRoute.Tier)
						meta.Protocol = upstreamRoute.Protocol
					}
					// ponytail: the replay itself reset before delivery.
					// No second replay: emit the error frame now so the
					// client gets a terminal event instead of an idle
					// hang on headers-sent-but-silent. Any other replay
					// error already carries its own terminal frame (or a
					// length rescue), so only this path needs the emit.
					if errors.Is(err, wire.ErrZeroDeliveryReset) && !wire.ClientCanceled(r.Context(), err) {
						g.logger.Warn("zero-delivery replay reset again; emitting error frame", "component", "stream", "event", "stream_zero_replay_double_reset", "request_id", ids.Request, "model", model, "error", err)
						err = fmt.Errorf("upstream reset the replayed stream before delivery: %w", err)
						if emitErr := wire.EmitStreamError(w, external, model, err); emitErr != nil {
							err = emitErr
						}
					}
				} else {
					// ponytail: replay failed or non-2xx. The first
					// attempt's error frame was suppressed, so emit
					// now: a clean error beats a client hang. Report the
					// replay's outcome, not the suppressed first error:
					// emitting the stale cause misattributes the failure.
					if retryErr != nil {
						err = retryErr
						g.logger.Warn("zero-delivery replay failed", "component", "stream", "event", "stream_zero_replay_failed", "request_id", ids.Request, "model", model, "error", retryErr)
					} else if retryResp != nil {
						err = fmt.Errorf("upstream returned HTTP %d on zero-delivery replay", retryResp.StatusCode)
					}
					if retryResp != nil {
						retryResp.Body.Close()
					}
					if emitErr := wire.EmitStreamError(w, external, model, err); emitErr != nil {
						err = emitErr
					}
				}
			}
			if meta != nil {
				meta.Usage, meta.UsageReported = usage, usageReported
				if err != nil {
					meta.Outcome = "stream_error"
					if wire.ClientCanceled(r.Context(), err) {
						meta.Outcome = "client_canceled"
					}
				}
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Warn("downstream stream ended with an error", "component", "stream", "event", "stream_failed", "request_id", ids.Request, "model", model, "tier", upstreamRoute.Tier, "error", err)
			}
			return
		}
		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			wire.WriteError(w, external, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
			return
		}
		if upstreamRoute.Anonymous || (meta != nil && meta.Shaped) {
			// Key-tier shaped requests were force-streamed like the
			// anonymous lane; collapse the same way.
			// The anonymous lane is served streaming (see prepareAnonymousBody);
			// collapse the events back into the single document this
			// non-streaming client asked for.
			collapsed, err := wire.CollapseStream(bytes.NewReader(responseBody), upstreamRoute.Protocol, model)
			if err != nil {
				g.logger.Warn("anonymous stream collapse failed", "component", "conversion", "event", "anonymous_collapse_failed", "request_id", ids.Request, "model", model, "source_protocol", upstreamRoute.Protocol, "error", err)
				wire.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
				return
			}
			responseBody = collapsed
		}
		responseBody = wire.RenameResponseTool(responseBody, upstreamRoute.Protocol, legacyAnonymousReadTool, toolAlias)
		if usage, reported := wire.ResponseUsage(upstreamRoute.Protocol, responseBody); meta != nil {
			meta.Usage, meta.UsageReported = usage, reported
		}
		if external != upstreamRoute.Protocol {
			responseBody, err = wire.ConvertResponse(upstreamRoute.Protocol, external, responseBody)
			if err != nil {
				g.logger.Warn("response protocol conversion failed", "component", "conversion", "event", "response_conversion_failed", "request_id", ids.Request, "model", model, "source_protocol", upstreamRoute.Protocol, "target_protocol", external, "error", err)
				wire.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
	}
}

// handleSystemOne serves System One decision requests on their own endpoint.
// Only models whose upstream protocol is System One can be served here; a chat
// or responses model belongs on its own endpoint, where the bridge can convert
// it.
func (g *Gateway) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
		return
	}
	model := jsonutil.StringAt(payload, "model")
	if meta := telemetry.MetaFromRequest(r); meta != nil {
		meta.Model = model
	}
	if model == "" {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
		return
	}
	if !g.catalog.Supported(model) {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
		return
	}
	route, err := g.catalog.Route(model, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous)
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
		return
	}
	if route.Protocol != wire.SystemOne {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, fmt.Sprintf("the model does not use the %s protocol", wire.SystemOne), "invalid_request_error", "model")
		return
	}
	g.forwardSystemOne(w, r, body, payload, model, route)
}

// forwardSystemOne relays a System One decision payload verbatim to the
// upstream systemone endpoint and returns the typed answer document unchanged.
// The payload pairs a free-form state with typed questions, which no
// message-shaped upstream protocol accepts, so it is never translated. Answers
// are non-streaming today; a streaming upstream reply is still relayed.
func (g *Gateway) forwardSystemOne(w http.ResponseWriter, r *http.Request, body []byte, payload map[string]any, model string, route models.Route) {
	meta := telemetry.MetaFromRequest(r)
	if meta != nil {
		meta.Model = model
		meta.Tier = string(route.Tier)
		meta.Protocol = route.Protocol
	}
	// One verbatim body serves every tier: a decision payload has no per-tier
	// encoding, so no protocol conversion is attempted.
	bodies := make(map[config.Tier][]byte, len(route.KeyTiers)+1)
	bodies[route.Tier] = body
	for _, tier := range route.KeyTiers {
		bodies[tier] = body
	}
	ids := identity.DeriveRequestIDs(r, payload)
	if meta != nil {
		meta.Request = ids.Request
		meta.Session = ids.Session
	}
	requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, upstreamRoute, err := g.doUpstream(requestCtx, route, bodies, ids)
	if err != nil {
		finalTier := route.Tier
		if meta != nil && meta.Tier != "" {
			finalTier = config.Tier(meta.Tier)
		}
		keyID, channel, anonymous := requestCredential(requestCtx)
		if !errors.Is(err, context.DeadlineExceeded) && wire.ClientCanceled(r.Context(), err) {
			g.logger.Debug("client canceled before upstream response", "component", "upstream", "event", "request_canceled", "request_id", ids.Request, "tier", finalTier, "key_id", keyID, "channel", channel, "anonymous", anonymous)
			if meta != nil {
				meta.Outcome = "client_canceled"
			}
			wire.WriteError(w, wire.SystemOne, statusClientClosedRequest, "client closed request", "client_canceled", ids.Request)
			return
		}
		g.logger.Warn("all upstream attempts failed", "component", "upstream", "event", "request_failed", "request_id", ids.Request, "tier", finalTier, "key_id", keyID, "channel", channel, "anonymous", anonymous, "error", err)
		if errors.Is(err, errProxyUnavailable) {
			wire.WriteError(w, wire.SystemOne, http.StatusServiceUnavailable, proxyUnavailableMessage, "proxy_error", ids.Request)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			wire.WriteError(w, wire.SystemOne, http.StatusGatewayTimeout, "upstream request timed out", "upstream_timeout", ids.Request)
			return
		}
		wire.WriteError(w, wire.SystemOne, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
		return
	}
	defer resp.Body.Close()
	if meta != nil {
		meta.Tier = string(upstreamRoute.Tier)
		meta.Protocol = upstreamRoute.Protocol
	}
	w.Header().Set("x-request-id", ids.Request)
	if resp.StatusCode/100 != 2 {
		copyErrorResponse(w, wire.SystemOne, resp, ids.Request)
		return
	}
	if contentType := resp.Header.Get("Content-Type"); strings.HasPrefix(contentType, "text/event-stream") {
		if meta != nil {
			meta.Stream = true
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
		return
	}
	if usage, reported := wire.ResponseUsage(upstreamRoute.Protocol, responseBody); meta != nil {
		meta.Usage, meta.UsageReported = usage, reported
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

func (g *Gateway) prepareRouteBodies(from wire.Protocol, route models.Route, input map[string]any) (map[config.Tier][]byte, error) {
	tiers := make([]config.Tier, 0, len(route.KeyTiers)+1)
	seen := make(map[config.Tier]bool, len(route.KeyTiers)+1)
	addTier := func(tier config.Tier) {
		if tier != config.TierZen && tier != config.TierGo || seen[tier] {
			return
		}
		seen[tier] = true
		tiers = append(tiers, tier)
	}
	addTier(route.Tier)
	for _, tier := range route.KeyTiers {
		addTier(tier)
	}
	if len(tiers) == 0 {
		return nil, errors.New("no usable upstream tier")
	}
	bodies := make(map[config.Tier][]byte, len(tiers))
	for _, tier := range tiers {
		protocol := route.ProtocolFor(tier)
		baseURL := g.cfg.Upstream.Zen
		if tier == config.TierGo {
			baseURL = g.cfg.Upstream.Go
		}
		upstreamPayload, err := wire.PrepareRequest(from, protocol, input, baseURL)
		if err != nil {
			if tier != route.Tier {
				// A fallback tier may use a stricter wire format than the
				// preferred tier. Do not reject a request before the preferred
				// upstream has even been tried; that tier is attempted only if
				// the request actually falls back.
				g.logger.Debug("skipping fallback tier with incompatible request shape", "component", "conversion", "event", "fallback_tier_skipped", "model", jsonutil.StringAt(input, "model"), "tier", tier, "protocol", protocol, "error", err)
				continue
			}
			return nil, fmt.Errorf("prepare %s upstream request: %w", tier, err)
		}
		if effort := g.resolvedEffort(jsonutil.StringAt(upstreamPayload, "model"), tier, protocol); effort != "" {
			wire.ForcedEffort(protocol, upstreamPayload, effort)
		}
		// Anonymous-lane shaping now lives upstream-side in
		// prepareAnonymousBody: v1.4.1 reshapes per upstream lane for the
		// legacy /zen/v1 vs /inference split, so a map-level pre-shape
		// would be overwritten for the lane that needs it most.
		encoded, err := json.Marshal(upstreamPayload)
		if err != nil {
			return nil, errors.New("request contains unsupported JSON values")
		}
		bodies[tier] = encoded
	}
	return bodies, nil
}

func copyErrorResponse(w http.ResponseWriter, protocol wire.Protocol, resp *http.Response, requestID string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	message := http.StatusText(resp.StatusCode)
	var value map[string]any
	if json.Unmarshal(body, &value) == nil {
		message = jsonutil.FirstString(jsonutil.StringAt(value, "error", "message"), jsonutil.StringAt(value, "message"), message)
	}
	wire.WriteError(w, protocol, resp.StatusCode, message, "upstream_error", requestID)
}
