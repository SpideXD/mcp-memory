package cogneemock

import (
	"context"
	"net/http"
	"time"
)

// Behavior describes a transport-level fault the mock injects instead of
// returning a normal response. Status-code and body overrides are handled by
// SetResponse; these are the failures a plain httptest handler cannot express.
type Behavior int

const (
	// BehaviorNormal returns the configured response (default).
	BehaviorNormal Behavior = iota
	// BehaviorHang writes nothing and blocks until the client disconnects or
	// the mock is closed. Exercises client-side timeouts and ctx cancellation.
	BehaviorHang
	// BehaviorCloseMidBody writes a partial body then aborts the connection
	// without completing it, producing an unexpected-EOF on the client.
	BehaviorCloseMidBody
	// BehaviorTruncated declares a Content-Length larger than the bytes
	// actually written, so the client's read ends short.
	BehaviorTruncated
	// BehaviorMalformedJSON returns HTTP 200 with a body that is not valid JSON.
	BehaviorMalformedJSON
	// BehaviorHTMLError returns an HTML error page with a 200 status, the way a
	// misconfigured reverse proxy does.
	BehaviorHTMLError
	// BehaviorHugeBody returns a body larger than the client's 10 MB read limit.
	BehaviorHugeBody
	// BehaviorEmptyBody returns HTTP 200 with a zero-length body.
	BehaviorEmptyBody
)

// faultState holds per-endpoint fault configuration.
type faultState struct {
	behavior Behavior
	latency  time.Duration
	sequence []ResponseConfig // consumed per call; last entry repeats
	seqPos   int
	calls    int
}

// SetBehavior installs a transport-level fault for an endpoint.
// BehaviorNormal clears it. Thread-safe.
func (s *Server) SetBehavior(endpoint string, b Behavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureFaults()
	f := s.faults[endpoint]
	f.behavior = b
	s.faults[endpoint] = f
}

// SetLatency makes an endpoint sleep for d before responding. The sleep is
// abandoned early if the client disconnects. Thread-safe.
func (s *Server) SetLatency(endpoint string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureFaults()
	f := s.faults[endpoint]
	f.latency = d
	s.faults[endpoint] = f
}

// SetSequence programs successive responses for an endpoint: call N returns
// entry N, and the final entry repeats once the sequence is exhausted. This is
// what makes retry and circuit-breaker state machines testable — "fail five
// times, then succeed" cannot be expressed with a single static response.
// Passing an empty slice clears the sequence. Thread-safe.
func (s *Server) SetSequence(endpoint string, seq []ResponseConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureFaults()
	f := s.faults[endpoint]
	f.sequence = append([]ResponseConfig(nil), seq...)
	f.seqPos = 0
	s.faults[endpoint] = f
}

// CallCount returns how many requests an endpoint has received since the mock
// started or ResetCounts was called. Thread-safe.
func (s *Server) CallCount(endpoint string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.faults == nil {
		return 0
	}
	return s.faults[endpoint].calls
}

// ResetCounts zeroes all per-endpoint call counters and rewinds sequences,
// leaving behaviors and latencies in place. Thread-safe.
func (s *Server) ResetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, f := range s.faults {
		f.calls = 0
		f.seqPos = 0
		s.faults[k] = f
	}
}

// ResetFaults clears all fault configuration and counters. Thread-safe.
func (s *Server) ResetFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = make(map[string]faultState)
}

// ensureFaults initialises the fault map. Caller must hold s.mu.
func (s *Server) ensureFaults() {
	if s.faults == nil {
		s.faults = make(map[string]faultState)
	}
}

// nextFault records a call against endpoint and returns the fault to apply plus
// the sequenced response override, if any.
func (s *Server) nextFault(endpoint string) (Behavior, time.Duration, *ResponseConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureFaults()
	f := s.faults[endpoint]
	f.calls++

	var override *ResponseConfig
	if len(f.sequence) > 0 {
		idx := f.seqPos
		if idx >= len(f.sequence) {
			idx = len(f.sequence) - 1 // last entry repeats
		}
		cfg := f.sequence[idx]
		override = &cfg
		f.seqPos++
	}

	s.faults[endpoint] = f
	return f.behavior, f.latency, override
}

// applyFault runs the configured fault for endpoint. It returns handled=true
// when the fault produced the entire response and the normal handler must be
// skipped, along with any sequenced response override.
func (s *Server) applyFault(w http.ResponseWriter, r *http.Request, endpoint string) (handled bool, override *ResponseConfig) {
	behavior, latency, override := s.nextFault(endpoint)

	if latency > 0 {
		select {
		case <-time.After(latency):
		case <-r.Context().Done():
			return true, nil // client gave up; nothing more to write
		}
	}

	switch behavior {
	case BehaviorNormal:
		return false, override

	case BehaviorHang:
		<-r.Context().Done()
		return true, nil

	case BehaviorCloseMidBody:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "512")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"partia`))
		flush(w)
		hijackClose(w)
		return true, nil

	case BehaviorTruncated:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"short"}`))
		flush(w)
		hijackClose(w)
		return true, nil

	case BehaviorMalformedJSON:
		writeResponse(w, http.StatusOK, `{"status": "completed", "dangling"`)
		return true, nil

	case BehaviorHTMLError:
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body><h1>502 Bad Gateway</h1></body></html>"))
		return true, nil

	case BehaviorHugeBody:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 1<<20) // 1 MB
		for i := range chunk {
			chunk[i] = 'x'
		}
		_, _ = w.Write([]byte(`{"pad":"`))
		for i := 0; i < 11; i++ { // 11 MB — past the client's 10 MB cap
			if _, err := w.Write(chunk); err != nil {
				return true, nil
			}
			flush(w)
		}
		_, _ = w.Write([]byte(`"}`))
		return true, nil

	case BehaviorEmptyBody:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return true, nil
	}

	return false, override
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// hijackClose rips the connection down without a clean close, so the client
// sees an unexpected EOF rather than a well-formed short response.
func hijackClose(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// overrideKey carries a sequenced ResponseConfig from the fault layer to the
// endpoint handler via request context.
type overrideKeyType struct{}

var overrideKey overrideKeyType

func withOverride(ctx context.Context, cfg ResponseConfig) context.Context {
	return context.WithValue(ctx, overrideKey, cfg)
}

// overrideFrom returns the sequenced response for this request, if any.
func overrideFrom(ctx context.Context) (ResponseConfig, bool) {
	cfg, ok := ctx.Value(overrideKey).(ResponseConfig)
	return cfg, ok
}
