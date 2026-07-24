package cogneemock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewServer_URL(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	url := mock.URL()
	if url == "" {
		t.Fatal("URL() returned empty string")
	}
	if !strings.HasPrefix(url, "http://") {
		t.Fatalf("URL() should start with http://, got %s", url)
	}
}

func TestPort(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	port := mock.Port()
	if port <= 0 || port > 65535 {
		t.Fatalf("Port() returned invalid port: %d", port)
	}

	// Verify Port matches URL
	url := mock.URL()
	expected := fmt.Sprintf(":%d", port)
	if !strings.HasSuffix(url, expected) {
		t.Fatalf("URL %s does not end with port %d", url, port)
	}
}

func TestHealth_DefaultResponse(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	resp, err := http.Get(mock.URL() + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["status"] != "ready" {
		t.Fatalf("expected status=ready, got %v", body["status"])
	}
}

func TestRemember_MultipartForm(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	// Build multipart form
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("datasetName", "mybank")
	part, _ := writer.CreateFormFile("data", "data.txt")
	_, _ = io.WriteString(part, "hello world")
	writer.Close()

	req, _ := http.NewRequest("POST", mock.URL()+"/api/v1/remember", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/remember failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "completed" {
		t.Fatalf("expected status=completed, got %v", body["status"])
	}

	// Verify capture
	if len(mock.Requests()) != 1 {
		t.Fatalf("expected 1 captured request, got %d", len(mock.Requests()))
	}
	last := mock.LastRequest("/api/v1/remember")
	if last == nil {
		t.Fatal("LastRequest(/api/v1/remember) returned nil")
	}
	if !strings.Contains(last.Body, "mybank") {
		t.Fatalf("captured body missing datasetName: %s", last.Body)
	}
	if !strings.Contains(last.Body, "hello world") {
		t.Fatalf("captured body missing data content: %s", last.Body)
	}
}

func TestRecall_JSON(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	payload := `{"query":"test","datasets":["mybank"]}`
	req, _ := http.NewRequest("POST", mock.URL()+"/api/v1/recall", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/recall failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var body []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if len(body) == 0 {
		t.Fatal("expected non-empty array response")
	}
	if body[0]["_source"] != "mock" {
		t.Fatalf("expected _source=mock, got %v", body[0]["_source"])
	}

	// Verify capture
	last := mock.LastRequest("/api/v1/recall")
	if last == nil {
		t.Fatal("LastRequest(/api/v1/recall) returned nil")
	}
	if !strings.Contains(last.Body, "test") {
		t.Fatalf("captured body missing query: %s", last.Body)
	}
}

func TestImprove_JSON(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	payload := `{"dataset_name":"mybank","data":""}`
	req, _ := http.NewRequest("POST", mock.URL()+"/api/v1/improve", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/improve failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "PipelineRunCompleted" {
		t.Fatalf("expected status=PipelineRunCompleted, got %v", body["status"])
	}
}

func TestForget_JSON(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	payload := `{"dataset":"mybank","data_id":"id123","memory_only":true}`
	req, _ := http.NewRequest("POST", mock.URL()+"/api/v1/forget", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/forget failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "success" {
		t.Fatalf("expected status=success, got %v", body["status"])
	}
}

func TestRequestsEndpoint(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	// Make 3 requests
	http.Get(mock.URL() + "/health")

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.WriteField("datasetName", "bank")
	part, _ := writer.CreateFormFile("data", "data.txt")
	io.WriteString(part, "content")
	writer.Close()
	req, _ := http.NewRequest("POST", mock.URL()+"/api/v1/remember", &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	http.DefaultClient.Do(req)

	http.Get(mock.URL() + "/health")

	// Check via Requests()
	if len(mock.Requests()) != 3 {
		t.Fatalf("expected 3 captured requests, got %d", len(mock.Requests()))
	}

	// Check via /_requests endpoint
	resp, err := http.Get(mock.URL() + "/_requests")
	if err != nil {
		t.Fatalf("GET /_requests failed: %v", err)
	}
	defer resp.Body.Close()

	var logs []RequestLog
	json.NewDecoder(resp.Body).Decode(&logs)
	if len(logs) != 3 {
		t.Fatalf("expected 3 request logs, got %d", len(logs))
	}
}

func TestSetResponse_Override(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	mock.SetResponse("/health", ResponseConfig{
		StatusCode: 503,
		Body:       `{"status":"not ready"}`,
	})

	resp, err := http.Get(mock.URL() + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Fatalf("expected status 503, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"status":"not ready"}` {
		t.Fatalf("unexpected body: %s", string(body))
	}
}

func TestSetResponse_IsolatedEndpoints(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	// Override health to 503
	mock.SetResponse("/health", ResponseConfig{StatusCode: 503, Body: `{"status":"down"}`})

	// Recall should still return 200
	payload := `{"query":"test","datasets":["bank"]}`
	req, _ := http.NewRequest("POST", mock.URL()+"/api/v1/recall", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/recall failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("recall should still return 200, got %d", resp.StatusCode)
	}
}

func TestResetRequests(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	http.Get(mock.URL() + "/health")
	http.Get(mock.URL() + "/health")

	if len(mock.Requests()) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(mock.Requests()))
	}

	mock.ResetRequests()

	if len(mock.Requests()) != 0 {
		t.Fatalf("expected 0 requests after reset, got %d", len(mock.Requests()))
	}
}

func TestLastRequest(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	// No requests yet
	if mock.LastRequest("/health") != nil {
		t.Fatal("expected nil for no requests")
	}

	http.Get(mock.URL() + "/health")

	last := mock.LastRequest("/health")
	if last == nil {
		t.Fatal("expected non-nil LastRequest")
	}
	if last.Method != "GET" {
		t.Fatalf("expected GET, got %s", last.Method)
	}
	if last.Path != "/health" {
		t.Fatalf("expected /health, got %s", last.Path)
	}
}

func TestUnknownEndpoint_404(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	resp, err := http.Get(mock.URL() + "/api/v1/nonexistent")
	if err != nil {
		t.Fatalf("GET /api/v1/nonexistent failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	// Verify it was captured
	if len(mock.Requests()) != 1 {
		t.Fatalf("expected 1 captured request, got %d", len(mock.Requests()))
	}
	if mock.Requests()[0].Path != "/api/v1/nonexistent" {
		t.Fatalf("captured wrong path: %s", mock.Requests()[0].Path)
	}
}

func TestClose_Idempotent(t *testing.T) {
	mock := NewServer()
	mock.Close()
	mock.Close() // should not panic
}

func TestConcurrentAccess(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	var wg sync.WaitGroup

	// 10 goroutines doing SetResponse + Requests
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mock.SetResponse("/health", ResponseConfig{
					StatusCode: 200 + (i % 3),
					Body:       fmt.Sprintf(`{"goroutine":%d}`, i),
				})
				_ = mock.Requests()
			}
		}(i)
	}

	// 5 goroutines making HTTP requests
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				http.Get(mock.URL() + "/health")
			}
		}()
	}

	wg.Wait()

	// Verify no panic occurred
	reqs := mock.Requests()
	if len(reqs) != 100 {
		t.Fatalf("expected 100 HTTP requests captured, got %d", len(reqs))
	}
}

func TestTimestampsRecorded(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	before := time.Now().UTC()
	http.Get(mock.URL() + "/health")
	after := time.Now().UTC()

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}

	ts := reqs[0].Time
	if ts.Before(before) || ts.After(after) {
		t.Fatalf("timestamp %v not between %v and %v", ts, before, after)
	}
}

func TestHeadersRecorded(t *testing.T) {
	mock := NewServer()
	defer mock.Close()

	req, _ := http.NewRequest("GET", mock.URL()+"/health", nil)
	req.Header.Set("X-Test-Header", "test-value")
	http.DefaultClient.Do(req)

	reqs := mock.Requests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}

	val := reqs[0].Headers["X-Test-Header"]
	if len(val) == 0 || val[0] != "test-value" {
		t.Fatalf("expected X-Test-Header=test-value, got %v", val)
	}
}
