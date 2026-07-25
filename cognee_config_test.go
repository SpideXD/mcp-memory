package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"mcp-memory/backend"
	"mcp-memory/internal/testutil/cogneemock"
)

func testBackendConfig(port int) backend.BackendConfig {
	return backend.BackendConfig{
		Backend:                 "cognee-rust",
		CogneePort:              fmt.Sprintf("%d", port),
		TemporalCognify:         true,
		MemoryOnly:              true,
		BackendRetainTimeout:    30 * time.Second,
		BackendRecallTimeout:    30 * time.Second,
		BackendReflectTimeout:   30 * time.Second,
		CogneeRetainTimeout:     30 * time.Second,
		RetryAttempts:           1,
		RetryDelay:              100 * time.Millisecond,
		RetryMaxDelay:           1 * time.Second,
		CircuitBreakerThreshold: 5,
		CircuitBreakerCooldown:  10 * time.Second,
	}
}

func TestCogneeBackend_SendsTemporalCognify(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	be := backend.New(testBackendConfig(mock.Port()))

	// Content WITHOUT a year → should get auto-stamped
	_, err := be.Retain(t.Context(), "testbank", "Alice loves coffee")
	if err != nil {
		t.Fatalf("retain failed: %v", err)
	}

	req := mock.LastRequest("/api/v1/remember")
	if req == nil {
		t.Fatal("no request captured")
	}

	if !strings.Contains(req.Body, "temporalCognify") {
		t.Errorf("temporalCognify not found: %s", req.Body)
	}

	today := time.Now().Format("2006-01-02")
	if !strings.Contains(req.Body, today) {
		t.Errorf("date %q not auto-stamped: %s", today, req.Body)
	}

	// Content WITH a year → should NOT get auto-stamped
	mock.ResetRequests()
	_, err = be.Retain(t.Context(), "testbank", "Alice graduated from MIT in 2018")
	if err != nil {
		t.Fatalf("retain failed: %v", err)
	}

	req2 := mock.LastRequest("/api/v1/remember")
	if req2 == nil {
		t.Fatal("no request captured")
	}
	if strings.Contains(req2.Body, today) {
		t.Errorf("date stamped on content with year: %s", req2.Body)
	}
}

func TestCogneeBackend_ForgetSendsMemoryOnly(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	be := backend.New(testBackendConfig(mock.Port()))

	_, err := be.Forget(t.Context(), "testbank", "some-id")
	if err != nil {
		t.Fatalf("forget failed: %v", err)
	}

	req := mock.LastRequest("/api/v1/forget")
	if req == nil {
		t.Fatal("no request captured")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(req.Body), &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if v, ok := payload["memory_only"]; !ok || v != true {
		t.Errorf("expected memory_only=true, got %v", v)
	}
}

func TestCogneeBackend_DisabledFlags(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	cfg := testBackendConfig(mock.Port())
	cfg.TemporalCognify = false
	cfg.MemoryOnly = false
	be := backend.New(cfg)

	_, err := be.Retain(t.Context(), "testbank", "test content")
	if err != nil {
		t.Fatalf("retain failed: %v", err)
	}

	req := mock.LastRequest("/api/v1/remember")
	if !strings.Contains(req.Body, "temporalCognify") {
		t.Error("temporalCognify field missing")
	}

	_, err = be.Forget(t.Context(), "testbank", "id")
	if err != nil {
		t.Fatalf("forget failed: %v", err)
	}

	req2 := mock.LastRequest("/api/v1/forget")
	var payload map[string]interface{}
	json.Unmarshal([]byte(req2.Body), &payload)
	if v, ok := payload["memory_only"]; !ok || v != false {
		t.Errorf("expected memory_only=false, got %v", v)
	}
}
