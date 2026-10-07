package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func (m *RuntimeManager) AvailabilitySnapshot() []models.Availability {
	items := []models.Availability{}
	r := m.current.Load()
	if r == nil {
		return items
	}
	for _, model := range r.gateway.catalog.List() {
		if r.gateway.catalog.IsFreeModel(model) {
			items = append(items, r.availability.Get(model))
		}
	}
	return items
}

func (m *RuntimeManager) RestoreModel(model string) error {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	for _, item := range m.AvailabilitySnapshot() {
		if item.Model == model {
			return m.current.Load().availability.Restore(model, time.Now().UTC())
		}
	}
	return errors.New("unknown free model")
}

func (m *RuntimeManager) startAvailabilityChecks() {
	ctx, cancel := context.WithCancel(m.root)
	m.availabilityCancel = cancel
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			m.checkFreeModels(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *RuntimeManager) checkFreeModels(ctx context.Context) {
	for _, item := range m.AvailabilitySnapshot() {
		if ctx.Err() != nil {
			return
		}
		r := m.current.Load()
		if r == nil {
			return
		}
		item = r.availability.Get(item.Model)
		if time.Now().Before(item.NextCheck) {
			continue
		}
		ok, reason, channel, attempted := r.gateway.probeFreeModel(ctx, item.Model)
		if !attempted || ctx.Err() != nil {
			continue
		}
		// Config changes may replace credentials or endpoints during a probe.
		m.updateMu.Lock()
		if m.current.Load() == r {
			if err := r.availability.Record(item.Model, item.Generation, ok, reason, channel, time.Now().UTC()); err != nil {
				m.logger.Warn("model availability state save failed", "error", err)
			}
			m.logger.Info("free model availability checked", "component", "models", "event", "availability_checked", "model", item.Model, "available", ok, "reason", reason, "channel", channel)
		}
		m.updateMu.Unlock()
	}
}

func (g *Gateway) probeFreeModel(ctx context.Context, model string) (success bool, reason, channel string, attempted bool) {
	if !g.catalog.IsFreeModel(model) {
		return false, "not_free", "", false
	}
	d := g.catalog.Diagnostic(model, wire.Chat, g.zenNodes.Len() > 0, false, g.cfg.Anonymous)
	protocol := d.NativeProtocols[config.TierZen]
	if !d.AvailableZen || !g.catalog.Supported(model) || protocol == wire.SystemOne {
		return false, "unsupported_probe", "", false
	}
	payload := map[string]any{"model": model, "stream": true, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Reply with OK."}}}
	converted, err := wire.ConvertRequest(wire.Chat, protocol, payload)
	if err != nil {
		return false, "request_conversion_error", "", false
	}
	b, err := json.Marshal(converted)
	if err != nil {
		return false, "request_conversion_error", "", false
	}
	b = prepareAnonymousBody(b, protocol)
	type lane struct {
		key, name string
		client    *http.Client
	}
	var lanes []lane
	if g.cfg.Anonymous {
		cursor := g.anonymous.CursorFor("availability:" + model)
		if node := cursor.Next(); node != nil {
			lanes = append(lanes, lane{anonymousZenKey, "anonymous", node.proxy.client})
		}
	}
	cursor := g.zenNodes.CursorFor("availability:" + model)
	if node := cursor.Next(); node != nil {
		if proxy := g.zenNodes.Proxy(node); proxy != nil {
			lanes = append(lanes, lane{node.key, "zen", proxy.client})
		}
	}
	for _, lane := range lanes {
		if ctx.Err() != nil {
			return false, "canceled", "", false
		}
		attempted, channel = true, lane.name
		probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		req, err := newUpstreamRequest(probeCtx, g.cfg.Upstream.Zen, protocol, b, identity.RequestIDs{Session: "availability:" + model}, lane.key)
		if err == nil {
			// Probe traffic never updates production key/proxy cooldowns or usage.
			var resp *http.Response
			resp, err = lane.client.Do(req)
			if err == nil {
				reason = fmt.Sprintf("http_%d", resp.StatusCode)
				if resp.StatusCode/100 == 2 {
					if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
						_, err = wire.CollapseStream(io.LimitReader(resp.Body, 1<<20), protocol, model)
					} else {
						var document map[string]any
						err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&document)
						if err == nil && (document == nil || document["error"] != nil) {
							err = errors.New("invalid response")
						}
						if err == nil {
							field := "choices"
							if protocol == wire.Anthropic {
								field = "content"
							} else if protocol == wire.Responses {
								field = "output"
							}
							if _, ok := document[field].([]any); !ok {
								err = errors.New("invalid response schema")
							}
						}
					}
					success = err == nil
					if !success {
						reason = "invalid_or_incomplete_response"
					}
				}
				resp.Body.Close()
			} else {
				reason = "transport_error"
			}
		}
		if probeCtx.Err() != nil {
			success = false
			reason = "probe_timeout"
		}
		cancel()
		if success {
			return true, "probe_succeeded", channel, true
		}
	}
	return success, reason, channel, attempted
}
