package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// The fork pins every upstream dial to tcp4: this box is v4-only over carrier
// NAT, and a v6 address that still answers AAAA fast enough triggers Happy
// Eyeballs, so cold dials stall on a v6 connection that the carrier resets
// mid-read. That cost a measurable 504 spike during the v1.4.1 merge, when
// pool.go sat at upstream's --theirs state and interim binaries silently lost
// the pin. These tests fail if a future upstream rewrite, merge, or
// `git checkout --theirs` drops it again.
func TestTransportPinsTCP4(t *testing.T) {
	pool, err := newTransportPool([]string{"direct"}, testPerformanceConfig(), time.Second)
	if err != nil {
		t.Fatalf("transport pool: %v", err)
	}
	if len(pool.items) != 1 {
		t.Fatalf("expected 1 transport, got %d", len(pool.items))
	}
	transport, ok := pool.items[0].client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want *http.Transport", pool.items[0].client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("transport has no DialContext override; the tcp4 pin is gone")
	}
	// Dialing an address that only exists on the loopback must still be
	// attempted as tcp4. The pin must not be a no-op wrapper that forwards the
	// requested network verbatim.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := transport.DialContext(ctx, "tcp", "127.0.0.1:1")
	if err == nil {
		conn.Close()
		t.Fatal("expected the probe dial to fail, got a live connection")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("unexpected dial error type %T: %v", err, err)
	}
	if opErr.Net != "tcp4" {
		t.Fatalf("dial used network %q, want tcp4 (the pin forwards the requested network verbatim)", opErr.Net)
	}
}

// The same pin has to survive on pool entries, which upstream builds with
// their own TLS/ALPN overrides in the same constructor.
func TestTransportPinsTCP4OnPoolEntries(t *testing.T) {
	pool, err := newTransportPool([]string{"pool+socks5://127.0.0.1:1080"}, testPerformanceConfig(), time.Second)
	if err != nil {
		t.Fatalf("transport pool: %v", err)
	}
	if len(pool.items) != 1 {
		t.Fatalf("expected 1 pool entry transport, got %d", len(pool.items))
	}
	if !pool.items[0].pool {
		t.Fatal("entry is not marked as a pool transport")
	}
	transport, ok := pool.items[0].client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want *http.Transport", pool.items[0].client.Transport)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := transport.DialContext(ctx, "tcp", "127.0.0.1:1")
	if err == nil {
		conn.Close()
		t.Fatal("expected the probe dial to fail, got a live connection")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("unexpected dial error type %T: %v", err, err)
	}
	if opErr.Net != "tcp4" {
		t.Fatalf("pool entry dial used network %q, want tcp4", opErr.Net)
	}
}

// TLS session resumption rides alongside the tcp4 pin; dropping it costs an
// extra RTT on every reconnect handshake.
func TestTransportKeepsTLSSessionCache(t *testing.T) {
	pool, err := newTransportPool([]string{"direct"}, testPerformanceConfig(), time.Second)
	if err != nil {
		t.Fatalf("transport pool: %v", err)
	}
	transport, ok := pool.items[0].client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want *http.Transport", pool.items[0].client.Transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.ClientSessionCache == nil {
		t.Fatal("TLS session cache is nil; reconnects pay a full handshake")
	}
}
