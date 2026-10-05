package httpx

import (
	"fmt"
	"io"
	"time"
)

func DrainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

// watchdogReader bounds how long a single Read may take while sending a
// request body upstream. Go's http.Transport only starts the
// ResponseHeaderTimeout timer after the full body is written, so a stalled
// upload (slow uplink, huge history payload, congested exit) can otherwise
// burn the entire request budget inside one client.Do with nothing to show
// for it. Each slow Read fails the attempt fast so the retry loop can move
// to the next key instead of waiting out the whole budget on one upload.
//
// The wrapped bodies here are always in-memory (bytes.Reader), whose Reads
// never block, so the timed-out helper goroutine always completes and its
// buffered send never leaks.
type watchdogReader struct {
	reader  io.Reader
	timeout time.Duration
}

func WatchdogReader(reader io.Reader, timeout time.Duration) io.Reader {
	if timeout <= 0 {
		return reader
	}
	return &watchdogReader{reader: reader, timeout: timeout}
}

func (r *watchdogReader) Read(data []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := r.reader.Read(data)
		done <- result{n, err}
	}()
	timer := time.NewTimer(r.timeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.n, res.err
	case <-timer.C:
		return 0, fmt.Errorf("upstream upload stalled: no body bytes for %s", r.timeout)
	}
}
