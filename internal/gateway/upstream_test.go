package gateway

import (
	"context"
	"testing"

	"opencode2api/internal/identity"
	wire "opencode2api/internal/protocol"
)

// TestNewUpstreamRequestSetsContentLength guards the 2026-10-05 chunked-upload
// regression: wrapping the body hid *bytes.Reader from NewRequest, leaving
// ContentLength 0 and forcing chunked uploads on every attempt. The request
// must carry a known length.
func TestNewUpstreamRequestSetsContentLength(t *testing.T) {
	body := []byte(`{"model":"mimo-v2.5-free","stream":true}`)
	ids := identity.RequestIDs{Session: "ses_test", Request: "req_test", Project: "proj_test"}
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		req, err := newUpstreamRequest(context.Background(), "https://opencode.ai/zen", protocol, body, ids, "test-key")
		if err != nil {
			t.Fatalf("protocol %v: %v", protocol, err)
		}
		if req.ContentLength != int64(len(body)) {
			t.Errorf("protocol %v: ContentLength = %d, want %d (chunked upload)", protocol, req.ContentLength, len(body))
		}
		if req.Body == nil {
			t.Errorf("protocol %v: nil body", protocol)
		}
	}
}
