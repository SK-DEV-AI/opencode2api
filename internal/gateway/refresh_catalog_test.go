package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// TestApplyModelRefreshKeepsProtocolsWhenCapabilityFetchFails pins the
// 2026-10-18 incident: the OpenCode capability endpoint failed for ~2 hours
// while the Zen model list kept succeeding, and every failed capability fetch
// shrank the catalog (85 -> 44 total, 72 -> 35 exposed). The 41 dropped models
// then failed with a misleading 400 "the model uses an upstream protocol that
// opencode2api does not expose", and /v1/models lost paid-tier entries.
//
// Cause: StartModelRefresh passed the zero-value Capabilities (all maps nil)
// into ReplaceWithCapabilities on the error path. Nil maps there mean "this
// release supports nothing", which wipes nativeProtocols/unsupported/metadata.
// ReplaceWithCapabilities already skips nil arguments, so the fix is to pass
// nil and keep the last good snapshot.
func TestApplyModelRefreshKeepsProtocolsWhenCapabilityFetchFails(t *testing.T) {
	catalog := modelcatalog.Catalog(config.TierZen, nil)
	gw := &Gateway{
		catalog: catalog,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	modelNames := []string{"paid-model-a", "paid-model-b", "free-model"}

	good := modelcatalog.Capabilities{
		Protocols: map[config.Tier]map[string]wire.Protocol{
			config.TierZen: {"paid-model-a": wire.Chat, "paid-model-b": wire.Responses},
			config.TierGo:  {},
		},
		Unsupported: map[config.Tier]map[string]bool{config.TierZen: {}, config.TierGo: {}},
		Metadata:    map[config.Tier]map[string]modelcatalog.Metadata{config.TierZen: {}, config.TierGo: {}},
	}
	// First refresh succeeds end to end.
	gw.applyModelRefresh(context.Background(), modelNames, nil, good, nil)
	if !catalog.Supported("paid-model-b") {
		t.Fatalf("precondition failed: paid-model-b unsupported after a good refresh")
	}
	before := len(catalog.List())

	// The 2026-10-18 failure: the model list still resolves, the capability
	// fetch returns an error, so capabilities is the zero value.
	var zero modelcatalog.Capabilities
	gw.applyModelRefresh(context.Background(), modelNames, nil, zero, errors.New("no healthy proxy available for OpenCode capability catalog"))

	if !catalog.Supported("paid-model-b") {
		t.Fatalf("paid-model-b lost its protocol after a failed capability refresh; the catalog shrank (2026-10-18 incident)")
	}
	if !catalog.Supported("paid-model-a") {
		t.Fatalf("paid-model-a lost its protocol after a failed capability refresh")
	}
	if after := len(catalog.List()); after != before {
		t.Fatalf("exposed models went %d -> %d after a failed capability refresh; a transient outage must not shrink the catalog", before, after)
	}
}

// A model list that genuinely drops a model must still shrink the catalog: the
// fix must not freeze the catalog against real removals.
func TestApplyModelRefreshStillAppliesModelRemovals(t *testing.T) {
	catalog := modelcatalog.Catalog(config.TierZen, nil)
	gw := &Gateway{
		catalog: catalog,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	caps := modelcatalog.Capabilities{
		Protocols: map[config.Tier]map[string]wire.Protocol{
			config.TierZen: {"gone": wire.Chat, "stays": wire.Responses},
			config.TierGo:  {},
		},
		Unsupported: map[config.Tier]map[string]bool{config.TierZen: {}, config.TierGo: {}},
		Metadata:    map[config.Tier]map[string]modelcatalog.Metadata{config.TierZen: {}, config.TierGo: {}},
	}
	gw.applyModelRefresh(context.Background(), []string{"gone", "stays"}, nil, caps, nil)

	// Capability fetch fails, and the model list no longer advertises "gone".
	var zero modelcatalog.Capabilities
	gw.applyModelRefresh(context.Background(), []string{"stays"}, nil, zero, errors.New("capability fetch failed"))

	if catalog.Supported("gone") {
		t.Fatalf("a model dropped from the upstream list is still served")
	}
	if !catalog.Supported("stays") {
		t.Fatalf("a surviving model lost its protocol")
	}
}

// TestRefreshProtocolCapabilitiesReturnsErrorWhenNoProxy pins the error the
// 2026-10-18 journal showed ("no healthy proxy available for OpenCode
// capability catalog"): an all-unhealthy pool must not silently report success,
// because a silent success is what let the zero-value snapshot through.
func TestRefreshProtocolCapabilitiesReturnsErrorWhenNoProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	pool, err := transportPool([]string{server.URL}, testPerformanceConfig(), time.Second)
	if err != nil {
		t.Fatalf("transport pool: %v", err)
	}
	// Simulate the incident: every proxy marked unhealthy.
	for _, item := range pool.items {
		item.healthy.Store(false)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := &Gateway{transports: pool, logger: logger, monitor: telemetry.Monitor()}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	caps, err := gw.refreshProtocolCapabilities(ctx)
	if err == nil {
		t.Fatalf("refreshProtocolCapabilities returned success with no healthy proxy; err=%v caps=%+v", err, caps)
	}
	if len(caps.Protocols) != 0 || len(caps.Unsupported) != 0 || len(caps.Metadata) != 0 {
		t.Fatalf("failed refresh returned non-empty capabilities: %+v", caps)
	}
}
