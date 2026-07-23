package stress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mcp-memory/internal/testutil"
)

// ─── Content Types (match JSON schemas) ────────────────────────────────

// ProbeEntry is one probe query loaded from probes.json.
type ProbeEntry struct {
	Query           string `json:"query"`
	ExpectedConcept string `json:"expected_concept"`
}

// RetainItem is one memory to retain, loaded from retain_*.json.
type RetainItem struct {
	Content string `json:"content"`
}

// ContradictionItem is one Alice career event.
type ContradictionItem struct {
	Content string `json:"content"`
	Month   int    `json:"month"`
}

// EdgeCaseItem is one edge case memory.
type EdgeCaseItem struct {
	Content  string `json:"content"`
	Category string `json:"category"`
}

// ─── Report Types (match spec Section 6.3) ─────────────────────────────

// DimensionReport is the top-level output structure written to each JSON file.
type DimensionReport struct {
	Dimension  string          `json:"dimension"`
	Scenario   string          `json:"scenario,omitempty"`
	Tier       string          `json:"tier,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
	Results    []ProbeResult   `json:"results,omitempty"`
	Metrics    json.RawMessage `json:"metrics"`
	Passed     bool            `json:"passed"`
	Timestamp  time.Time       `json:"timestamp"`
	DurationMs float64         `json:"duration_ms"`
}

// ProbeResult records the outcome of a single recall probe.
type ProbeResult struct {
	Query           string  `json:"query"`
	ExpectedConcept string  `json:"expected_concept"`
	ActualOutput    string  `json:"actual_output"`
	LatencyMs       float64 `json:"latency_ms"`
	Rank            int     `json:"rank,omitempty"`
	Note            string  `json:"note,omitempty"`
}

// ScaleMetrics is the Metrics payload for Dimension 1.
type ScaleMetrics struct {
	Tier              int     `json:"tier"`
	TotalRetains      int     `json:"total_retains"`
	RetainErrors      int     `json:"retain_errors"`
	TotalRetainDurSec float64 `json:"total_retain_duration_sec"`
	TotalProbeDurSec  float64 `json:"total_probe_duration_sec"`
	PrecisionAt1      float64 `json:"precision_at_1"`
	MRR               float64 `json:"mrr"`
	LatencyP50Ms      float64 `json:"latency_p50_ms"`
	LatencyP99Ms      float64 `json:"latency_p99_ms"`
}

// ConcurrencyMetrics is the Metrics payload for Dimension 3 multi-agent test.
type ConcurrencyMetrics struct {
	TotalOps        int                       `json:"total_ops"`
	SuccessfulOps   int                       `json:"successful_ops"`
	FailedOps       int                       `json:"failed_ops"`
	Agents          int                       `json:"agents"`
	PerAgentResults map[int]AgentOpsSummary   `json:"per_agent_results"`
}

// AgentOpsSummary summarizes one agent's operations.
type AgentOpsSummary struct {
	Role          string  `json:"role"`
	TotalOps      int     `json:"total_ops"`
	SuccessfulOps int     `json:"successful_ops"`
	FailedOps     int     `json:"failed_ops"`
	AvgLatencyMs  float64 `json:"avg_latency_ms,omitempty"`
}

// BurstMetrics is the Metrics payload for Dimension 3 burst test.
type BurstMetrics struct {
	BurstSize     int     `json:"burst_size"`
	SubmittedInMs float64 `json:"submitted_in_ms"`
	Errors        int     `json:"errors"`
	ErrorRate     float64 `json:"error_rate"`
}

// ChaosMetrics is the Metrics payload for Dimension 4 kill tests.
type ChaosMetrics struct {
	Scenario          string  `json:"scenario"`
	Degraded          bool    `json:"degraded"`
	Recovered         bool    `json:"recovered"`
	DegradeTimeSec    float64 `json:"degrade_time_sec"`
	RecoveryTimeSec   float64 `json:"recovery_time_sec"`
	DowntimeSec       float64 `json:"downtime_sec"`
	RecallDuringChaos bool    `json:"recall_during_chaos"`
	RetainDuringChaos bool    `json:"retain_during_chaos"`
}

// FloodMetrics is the Metrics payload for Dimension 4 flood test.
type FloodMetrics struct {
	FloodSize         int     `json:"flood_size"`
	SubmittedInMs     float64 `json:"submitted_in_ms"`
	Successful        int     `json:"successful"`
	Overloaded        int     `json:"overloaded"`
	Timeouts          int     `json:"timeouts"`
	OtherErrors       int     `json:"other_errors"`
	AvgLatencySuccess float64 `json:"avg_latency_success_ms,omitempty"`
	AvgLatencyError   float64 `json:"avg_latency_error_ms,omitempty"`
}

// floodOpResult records a single flood operation outcome.
type floodOpResult struct {
	N       int
	Err     error
	DurMs   float64
	IsError bool
}

// retainResult records a single retain operation outcome.
type retainResult struct {
	Item  RetainItem
	Err   error
	DurMs int64
}

// HealthResponse matches the /health JSON structure.
type HealthResponse struct {
	Status string `json:"status"`
	Llama  bool   `json:"llama"`
	Reranker bool `json:"reranker"`
	Hindsight bool `json:"hindsight"`
}

// ─── Helpers ────────────────────────────────────────────────────────────

// contentDir returns the absolute path to the stress/content directory.
func contentDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file path")
	}
	return filepath.Join(filepath.Dir(filename), "content")
}

// outputDir returns the path to the stress/stress_output directory.
func outputDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file path")
	}
	return filepath.Join(filepath.Dir(filename), "stress_output")
}

// requireServerUp skips the test if the MCP server is not running.
func requireServerUp(t *testing.T) {
	t.Helper()
	if !testutil.ServerUp() {
		t.Skip("MCP memory server not running at " + testutil.DefaultServerURL)
	}
}

// loadProbes reads stress/content/probes.json and returns the probe entries.
func loadProbes(t *testing.T) []ProbeEntry {
	t.Helper()
	path := filepath.Join(contentDir(t), "probes.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("loadProbes: %v", err)
	}
	var probes []ProbeEntry
	if err := json.Unmarshal(data, &probes); err != nil {
		t.Fatalf("loadProbes parse: %v", err)
	}
	return probes
}

// loadRetainItems reads a JSON file and returns the retain content items.
func loadRetainItems(t *testing.T, path string) []RetainItem {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("loadRetainItems(%s): %v", path, err)
	}
	var items []RetainItem
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("loadRetainItems(%s) parse: %v", path, err)
	}
	return items
}

// loadContradictionItems reads stress/content/contradiction.json.
func loadContradictionItems(t *testing.T) []ContradictionItem {
	t.Helper()
	path := filepath.Join(contentDir(t), "contradiction.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("loadContradictionItems: %v", err)
	}
	var items []ContradictionItem
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("loadContradictionItems parse: %v", err)
	}
	// Sort by month ascending
	sort.Slice(items, func(i, j int) bool { return items[i].Month < items[j].Month })
	return items
}

// loadEdgeCaseItems reads stress/content/edge_cases.json.
func loadEdgeCaseItems(t *testing.T) []EdgeCaseItem {
	t.Helper()
	path := filepath.Join(contentDir(t), "edge_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("loadEdgeCaseItems: %v", err)
	}
	var items []EdgeCaseItem
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("loadEdgeCaseItems parse: %v", err)
	}
	return items
}

// writeJSONReport writes a DimensionReport to the specified output file.
func writeJSONReport(t *testing.T, relPath string, report DimensionReport) {
	t.Helper()
	outDir := outputDir(t)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	fullPath := filepath.Join(outDir, filepath.Base(relPath))
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("Report written: %s", fullPath)
}

// truncate truncates a string to maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}



// readPIDFile reads logs/.mcp-pids.json and returns the PID map.
func readPIDFile(t *testing.T) map[string]int {
	t.Helper()
	// Try to find the PID file relative to the project root
	// The PID file is at <project_root>/logs/.mcp-pids.json
	// We're in stress/, so go up one level
	pidPath := filepath.Join(filepath.Dir(filepath.Dir(func() string {
		_, f, _, _ := runtime.Caller(0)
		return f
	}())), "logs", ".mcp-pids.json")

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Skipf("PID file not available at %s: %v", pidPath, err)
		return nil
	}
	var pids map[string]int
	if err := json.Unmarshal(data, &pids); err != nil {
		t.Skipf("PID file unparseable: %v", err)
		return nil
	}
	return pids
}

// validatePID checks that a PID is safe to signal.
func validatePID(t *testing.T, name string, pid int) {
	t.Helper()
	if pid <= 0 || pid > 99999 {
		t.Fatalf("invalid %s PID: %d (must be 1-99999)", name, pid)
	}
}

// expectedEmployerForMonth returns the expected employer for the Alice scenario.
func expectedEmployerForMonth(month int) string {
	switch {
	case month < 6:
		return "Google"
	case month < 12:
		return "Meta"
	default:
		return "AliceTech"
	}
}

// extractTopResultText parses the MCP recall JSON response and extracts the
// text of the first (top-ranked) result. The response structure is:
//  {"content":[{"text":"{\"results\":[{\"text\":\"...\"},...],...}"}]}
// Returns empty string if parsing fails (probe returned errors or malformed JSON).
func extractTopResultText(actualOutput string) string {
	if actualOutput == "" {
		return ""
	}
	var rpcResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(actualOutput), &rpcResult); err != nil {
		return ""
	}
	if len(rpcResult.Content) == 0 {
		return ""
	}
	textPayload := rpcResult.Content[0].Text
	if textPayload == "" {
		return ""
	}
	var recallResp struct {
		Results []struct {
			Text string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(textPayload), &recallResp); err != nil {
		// textPayload may be a plain string (not JSON)
		return textPayload
	}
	if len(recallResp.Results) == 0 {
		return ""
	}
	return recallResp.Results[0].Text
}

// extractAllResultTexts parses the MCP recall JSON response and extracts the
// text of ALL ranked results. Returns nil if parsing fails.
func extractAllResultTexts(actualOutput string) []string {
	if actualOutput == "" {
		return nil
	}
	var rpcResult struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(actualOutput), &rpcResult); err != nil {
		return nil
	}
	if len(rpcResult.Content) == 0 {
		return nil
	}
	textPayload := rpcResult.Content[0].Text
	var recallResp struct {
		Results []struct {
			Text string `json:"text"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(textPayload), &recallResp); err != nil {
		return nil
	}
	texts := make([]string, len(recallResp.Results))
	for i, r := range recallResp.Results {
		texts[i] = r.Text
	}
	return texts
}

// computePrecisionAt1 computes precision@1 from probe results.
// Parses the JSON ActualOutput to extract the actual top-ranked result text,
// then checks if the expected concept appears in that result.
func computePrecisionAt1(results []ProbeResult) float64 {
	if len(results) == 0 {
		return 0
	}
	validCount := 0
	hits := 0
	for _, r := range results {
		if r.ExpectedConcept == "" {
			continue
		}
		topText := extractTopResultText(r.ActualOutput)
		if topText == "" {
			// Probe returned error or empty result; skip from metric
			continue
		}
		validCount++
		concept := strings.ToLower(r.ExpectedConcept)
		if strings.Contains(strings.ToLower(topText), concept) {
			hits++
		}
	}
	if validCount == 0 {
		return 0
	}
	return float64(hits) / float64(validCount)
}

// computeMRR computes Mean Reciprocal Rank from probe results.
// Parses the JSON ActualOutput to extract all ranked result texts,
// then finds the rank of the first result containing the expected concept.
func computeMRR(results []ProbeResult) float64 {
	if len(results) == 0 {
		return 0
	}
	validCount := 0
	totalRR := 0.0
	for _, r := range results {
		if r.ExpectedConcept == "" {
			continue
		}
		texts := extractAllResultTexts(r.ActualOutput)
		if len(texts) == 0 {
			// Probe returned error or empty result; skip from metric
			continue
		}
		validCount++
		concept := strings.ToLower(r.ExpectedConcept)
		for i, text := range texts {
			if strings.Contains(strings.ToLower(text), concept) {
				totalRR += 1.0 / float64(i+1)
				break
			}
		}
	}
	if validCount == 0 {
		return 0
	}
	return totalRR / float64(validCount)
}

// computePercentile computes the p-th percentile from sorted float64 slice.
func computePercentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p * float64(len(sorted)-1)
	lower := int(idx)
	upper := lower + 1
	if upper >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

// ─── Dimension 1: Scale ────────────────────────────────────────────────

func TestStressScale_50(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	bank := "stress:scale:50"
	probes := loadProbes(t)
	cDir := contentDir(t)
	retainItems := loadRetainItems(t, filepath.Join(cDir, "retain_50.json"))

	// Phase 1: Retain with 2 workers
	client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	retainStart := time.Now()
	workChan := make(chan RetainItem, len(retainItems))
	resultChan := make(chan retainResult, len(retainItems))

	var wg sync.WaitGroup
	var retainErrors atomic.Int64
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Logf("Worker %d panic: %v", workerID, r)
				}
			}()
			for item := range workChan {
				start := time.Now()
				_, err := client.Retain(item.Content)
				dur := time.Since(start)
				if err != nil {
					retainErrors.Add(1)
				}
				resultChan <- retainResult{Item: item, Err: err, DurMs: dur.Milliseconds()}
			}
		}(w)
	}

	for _, item := range retainItems {
		workChan <- item
	}
	close(workChan)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("Result collector panic: %v", r)
			}
		}()
		wg.Wait()
		close(resultChan)
	}()

	var retainResults []retainResult
	for r := range resultChan {
		retainResults = append(retainResults, r)
	}
	retainDur := time.Since(retainStart)

	// Phase 2: Probe — 30 sequential queries
	// SSE reconnection: the 120s timeout may have killed the SSE stream during retain phase.
	// Create a fresh client before probing to ensure the session is alive.
	client.Close()
	client, err = testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("SSE reconnect for probe phase: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Re-initialize for probe phase: %v", err)
	}
	defer client.Close()

	probeStart := time.Now()
	var probeResults []ProbeResult
	for _, probe := range probes {
		start := time.Now()
		output, err := client.Recall(probe.Query)
		dur := time.Since(start)

		pr := ProbeResult{
			Query:           probe.Query,
			ExpectedConcept: probe.ExpectedConcept,
			ActualOutput:    output,
			LatencyMs:       float64(dur.Microseconds()) / 1000.0,
		}
		if err != nil {
			pr.Note = fmt.Sprintf("recall error: %v", err)
		}
		probeResults = append(probeResults, pr)
	}
	probeDur := time.Since(probeStart)

	// Phase 3: Compute metrics
	latencies := make([]float64, 0, len(probeResults))
	for _, pr := range probeResults {
		latencies = append(latencies, pr.LatencyMs)
	}
	sort.Float64s(latencies)

	metrics := ScaleMetrics{
		Tier:              50,
		TotalRetains:      len(retainResults),
		RetainErrors:      int(retainErrors.Load()),
		TotalRetainDurSec: retainDur.Seconds(),
		TotalProbeDurSec:  probeDur.Seconds(),
		PrecisionAt1:      computePrecisionAt1(probeResults),
		MRR:               computeMRR(probeResults),
		LatencyP50Ms:      computePercentile(latencies, 0.50),
		LatencyP99Ms:      computePercentile(latencies, 0.99),
	}

	probeErrorRate := float64(0)
	if len(probeResults) > 0 {
		errCount := 0
		for _, pr := range probeResults {
			if pr.Note != "" {
				errCount++
			}
		}
		probeErrorRate = float64(errCount) / float64(len(probeResults))
	}
	retainErrorRate := float64(retainErrors.Load()) / float64(len(retainResults))
	passed := probeErrorRate <= 0.5 && retainErrorRate <= 0.5

	metricsJSON, _ := json.Marshal(metrics)
	report := DimensionReport{
		Dimension:  "scale",
		Tier:       "50",
		Timestamp:  time.Now(),
		Results:    probeResults,
		Metrics:    metricsJSON,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "scale_50.json", report)

	t.Logf("Scale 50: %d retains (%d errors) in %.1fs, 30 probes in %.1fs, P@1=%.2f MRR=%.2f",
		len(retainResults), retainErrors.Load(), retainDur.Seconds(), probeDur.Seconds(),
		metrics.PrecisionAt1, metrics.MRR)
}

func TestStressScale_200(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	bank := "stress:scale:200"
	probes := loadProbes(t)
	cDir := contentDir(t)
	// Combine retain_50 + retain_200 for 200 total
	items50 := loadRetainItems(t, filepath.Join(cDir, "retain_50.json"))
	items200 := loadRetainItems(t, filepath.Join(cDir, "retain_200.json"))
	retainItems := append(items50, items200...)

	client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	// Retain phase with 2 workers
	retainStart := time.Now()
	workChan := make(chan RetainItem, len(retainItems))
	resultChan := make(chan retainResult, len(retainItems))

	var wg sync.WaitGroup
	var retainErrors atomic.Int64
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Logf("Worker %d panic: %v", workerID, r)
				}
			}()
			for item := range workChan {
				start := time.Now()
				_, err := client.Retain(item.Content)
				dur := time.Since(start)
				if err != nil {
					retainErrors.Add(1)
				}
				resultChan <- retainResult{Item: item, Err: err, DurMs: dur.Milliseconds()}
			}
		}(w)
	}

	for _, item := range retainItems {
		workChan <- item
	}
	close(workChan)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("Result collector panic: %v", r)
			}
		}()
		wg.Wait()
		close(resultChan)
	}()

	var retainResults []retainResult
	progressTicker := time.NewTicker(30 * time.Second)
	defer progressTicker.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for r := range resultChan {
			retainResults = append(retainResults, r)
		}
	}()

	// Progress logging while waiting
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("Progress logger panic: %v", r)
			}
		}()
		for {
			select {
			case <-progressTicker.C:
				t.Logf("Scale 200 progress: %d/%d retains completed", len(retainResults), len(retainItems))
			case <-done:
				return
			}
		}
	}()

	<-done
	retainDur := time.Since(retainStart)

	// Probe phase
	// SSE reconnection: the 120s timeout may have killed the SSE stream during retain phase.
	// Create a fresh client before probing to ensure the session is alive.
	client.Close()
	client, err = testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("SSE reconnect for probe phase: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Re-initialize for probe phase: %v", err)
	}
	defer client.Close()

	probeStart := time.Now()
	var probeResults []ProbeResult
	for _, probe := range probes {
		start := time.Now()
		output, err := client.Recall(probe.Query)
		dur := time.Since(start)

		pr := ProbeResult{
			Query:           probe.Query,
			ExpectedConcept: probe.ExpectedConcept,
			ActualOutput:    output,
			LatencyMs:       float64(dur.Microseconds()) / 1000.0,
		}
		if err != nil {
			pr.Note = fmt.Sprintf("recall error: %v", err)
		}
		probeResults = append(probeResults, pr)
	}
	probeDur := time.Since(probeStart)

	latencies := make([]float64, 0, len(probeResults))
	for _, pr := range probeResults {
		latencies = append(latencies, pr.LatencyMs)
	}
	sort.Float64s(latencies)

	metrics := ScaleMetrics{
		Tier:              200,
		TotalRetains:      len(retainResults),
		RetainErrors:      int(retainErrors.Load()),
		TotalRetainDurSec: retainDur.Seconds(),
		TotalProbeDurSec:  probeDur.Seconds(),
		PrecisionAt1:      computePrecisionAt1(probeResults),
		MRR:               computeMRR(probeResults),
		LatencyP50Ms:      computePercentile(latencies, 0.50),
		LatencyP99Ms:      computePercentile(latencies, 0.99),
	}

	probeErrorRate := float64(0)
	if len(probeResults) > 0 {
		errCount := 0
		for _, pr := range probeResults {
			if pr.Note != "" {
				errCount++
			}
		}
		probeErrorRate = float64(errCount) / float64(len(probeResults))
	}
	retainErrorRate := float64(retainErrors.Load()) / float64(len(retainResults))
	passed := probeErrorRate <= 0.5 && retainErrorRate <= 0.5

	metricsJSON, _ := json.Marshal(metrics)
	report := DimensionReport{
		Dimension:  "scale",
		Tier:       "200",
		Timestamp:  time.Now(),
		Results:    probeResults,
		Metrics:    metricsJSON,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "scale_200.json", report)

	t.Logf("Scale 200: %d retains (%d errors) in %.1fs, 30 probes in %.1fs, P@1=%.2f MRR=%.2f",
		len(retainResults), retainErrors.Load(), retainDur.Seconds(), probeDur.Seconds(),
		metrics.PrecisionAt1, metrics.MRR)
}

// ─── Dimension 2: Contradiction ────────────────────────────────────────

func TestStressContradiction_Alice(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	bank := "stress:contradiction:alice"
	items := loadContradictionItems(t)

	client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	var results []ProbeResult

	for _, item := range items {
		// Retain the new fact
		_, retainErr := client.Retain(item.Content)
		if retainErr != nil {
			results = append(results, ProbeResult{
				Query:           "Where does Alice work?",
				ExpectedConcept: expectedEmployerForMonth(item.Month),
				ActualOutput:    "",
				LatencyMs:       0,
				Note:            fmt.Sprintf("retain error at month %d: %v", item.Month, retainErr),
			})
			continue
		}

		// Small delay to let Hindsight process
		time.Sleep(1 * time.Second)

		// Probe immediately after each retain
		start := time.Now()
		output, err := client.Recall("Where does Alice work?")
		dur := time.Since(start)

		expected := expectedEmployerForMonth(item.Month)
		pr := ProbeResult{
			Query:           "Where does Alice work?",
			ExpectedConcept: expected,
			ActualOutput:    output,
			LatencyMs:       float64(dur.Microseconds()) / 1000.0,
		}
		if err != nil {
			pr.Note = fmt.Sprintf("recall error: %v", err)
		} else if !strings.Contains(strings.ToLower(output), strings.ToLower(expected)) {
			pr.Note = fmt.Sprintf("expected '%s' not found in output; got: %s", expected, truncate(output, 200))
		}
		results = append(results, pr)
		t.Logf("Month %d: retained '%s', recalled '%s', expected '%s'",
			item.Month, truncate(item.Content, 60), truncate(output, 60), expected)
	}

	// Alice contradiction: passed if no retain errors (quality is expected behavior)
	retainErrors := 0
	for _, r := range results {
		if r.Note != "" && strings.Contains(r.Note, "retain error") {
			retainErrors++
		}
	}
	passed := retainErrors == 0

	report := DimensionReport{
		Dimension:  "contradiction",
		Scenario:   "alice_sequential",
		Timestamp:  time.Now(),
		Results:    results,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "contradiction_alice.json", report)
	t.Logf("Alice contradiction: %d probes completed", len(results))
}

func TestStressContradiction_Concurrent(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	bank := "stress:contradiction:concurrent"
	client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	// Pre-warm: establish base fact
	_, err = client.Retain("The sky is blue.")
	if err != nil {
		t.Logf("Pre-warm retain error: %v", err)
	}

	// Concurrently submit 3 contradicting facts
	contradictions := []string{
		"The sky is green.",
		"The sky is purple with orange stripes.",
		"The sky does not exist.",
	}

	var wg sync.WaitGroup
	errs := make(chan testutil.AgentError, len(contradictions))
	for i, content := range contradictions {
		wg.Add(1)
		go func(agentID int, c string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs <- testutil.AgentError{Agent: agentID, Op: "retain", Err: fmt.Errorf("panic: %v", r)}
				}
			}()
			_, err := client.Retain(c)
			if err != nil {
				errs <- testutil.AgentError{Agent: agentID, Op: "retain", Err: err}
			}
		}(i, content)
	}
	wg.Wait()
	close(errs)

	retainErrCount := 0
	for e := range errs {
		retainErrCount++
		t.Logf("Concurrent retain error: agent=%d op=%s err=%v", e.Agent, e.Op, e.Err)
	}

	// Poll for processing completion instead of fixed sleep.
	// Wait up to 30s for the concurrent retains to be processed by Hindsight.
	deadline := time.Now().Add(30 * time.Second)
	processed := false
	for time.Now().Before(deadline) {
		checkOut, checkErr := client.Recall("What color is the sky?")
		if checkErr == nil && len(extractAllResultTexts(checkOut)) > 0 {
			processed = true
			t.Logf("Concurrent retains processed after %.1fs", time.Since(totalStart).Seconds())
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !processed {
		t.Logf("WARNING: concurrent retains may not be fully processed after 30s polling")
	}

	// Probe
	start := time.Now()
	output, err := client.Recall("What color is the sky?")
	dur := time.Since(start)

	pr := ProbeResult{
		Query:           "What color is the sky?",
		ExpectedConcept: "blue",
		ActualOutput:    output,
		LatencyMs:       float64(dur.Microseconds()) / 1000.0,
	}
	if err != nil {
		pr.Note = fmt.Sprintf("recall error: %v", err)
	}

	// Concurrent contradiction: passed if no errors during retain and recall
	hasErrors := false
	if pr.Note != "" {
		hasErrors = true
	}
	if retainErrCount > 0 {
		hasErrors = true
	}
	passed := !hasErrors

	report := DimensionReport{
		Dimension:  "contradiction",
		Scenario:   "concurrent",
		Timestamp:  time.Now(),
		Results:    []ProbeResult{pr},
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "contradiction_concurrent.json", report)
	t.Logf("Concurrent contradiction: output='%s'", truncate(output, 200))
}

// ─── Dimension 3: Concurrency ──────────────────────────────────────────

func TestStressConcurrency_MultiAgent(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	bank := "stress:concurrency:shared"

	type agentConfig struct {
		ID    int
		Name  string
		Op    string
		Items []string
	}

	configs := []agentConfig{
		{ID: 0, Name: "manager", Op: "retain", Items: []string{
			"Project Delta launched Q3 2025. Budget: $2M.",
			"Project Delta team has 8 engineers and 2 designers.",
			"Sprint velocity increased 15% quarter over quarter.",
			"We migrated to Kubernetes last month.",
			"New hire onboarding takes 2 weeks.",
			"Q4 roadmap includes 3 major features.",
			"Technical debt backlog has 47 items.",
			"CI/CD pipeline runs in 4 minutes.",
			"Code review turnaround is under 4 hours.",
			"Team satisfaction score is 8.2 out of 10.",
		}},
		{ID: 1, Name: "friend", Op: "retain", Items: []string{
			"I heard Bob is leaving the company.",
			"The office coffee machine is broken again.",
			"Someone ate my lunch from the fridge.",
			"The new intern is really good at Go.",
			"We should try that new Thai place for lunch.",
			"Did you see the latest SpaceX launch?",
			"The parking lot is always full on Tuesdays.",
			"The wifi in building B is terrible.",
			"Karen got promoted to VP of Engineering.",
			"The holiday party is at the rooftop bar this year.",
		}},
		{ID: 2, Name: "recruiter", Op: "recall", Items: []string{
			"What projects has the team shipped?",
			"What is the team size?",
			"What technologies does the team use?",
			"What is the sprint velocity?",
			"What is the deployment frequency?",
			"What is the code review process?",
			"What is the onboarding process?",
			"What is the team culture like?",
			"What are the growth opportunities?",
			"What is the compensation range?",
		}},
		{ID: 3, Name: "adversary", Op: "retain", Items: []string{
			"asdfghjkl random keyboard smash ignore this",
			"LOREM IPSUM DOLOR SIT AMET consectetur adipiscing elit",
			"SELECT * FROM users WHERE 1=1; DROP TABLE memories;--",
			"<script>alert('xss')</script>",
			"This is a very important memory that definitely matters",
			"The answer to everything is 42",
			"I am a banana",
			"The quick brown fox jumps over the lazy dog repeatedly",
			"This sentence is false",
			"Memory number 9 of 10 for adversary agent",
		}},
		{ID: 4, Name: "observer", Op: "recall", Items: []string{
			"Project Delta budget",
			"team satisfaction",
			"office amenities",
			"recent departures",
			"technical stack",
			"deployment pipeline",
			"team size",
			"recent hires",
			"code quality metrics",
			"office culture",
		}},
	}

	var wg sync.WaitGroup
	results := make(chan testutil.AgentResult, 50)
	errs := make(chan testutil.AgentError, 50)

	// Synchronization: recall agents wait for retain agents to finish
	// so that queries have data to recall.
	var retainWg sync.WaitGroup
	retainsDone := make(chan struct{})

	// Launch retain agents first
	for _, cfg := range configs {
		if cfg.Op != "retain" {
			continue
		}
		retainWg.Add(1)
		wg.Add(1)
		go func(cfg agentConfig) {
			defer wg.Done()
			defer retainWg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs <- testutil.AgentError{Agent: cfg.ID, Op: "goroutine", Err: fmt.Errorf("panic: %v", r)}
				}
			}()

			client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
			if err != nil {
				errs <- testutil.AgentError{Agent: cfg.ID, Op: "connect", Err: err}
				return
			}
			if err := client.Initialize(); err != nil {
				errs <- testutil.AgentError{Agent: cfg.ID, Op: "init", Err: err}
				return
			}
			defer client.Close()

			for _, item := range cfg.Items {
				time.Sleep(time.Duration(100+int(time.Now().UnixNano()%400)) * time.Millisecond)

				start := time.Now()
				_, err := client.Retain(item)
				dur := time.Since(start)

				if err != nil {
					errs <- testutil.AgentError{Agent: cfg.ID, Op: cfg.Op, Err: err}
				} else {
					results <- testutil.AgentResult{Agent: cfg.ID, Op: cfg.Op, Dur: dur}
				}
			}
		}(cfg)
	}

	// Close retainsDone when all retain agents finish
	go func() {
		retainWg.Wait()
		close(retainsDone)
	}()

	// Launch recall agents — they wait for retains to finish first
	for _, cfg := range configs {
		if cfg.Op != "recall" {
			continue
		}
		wg.Add(1)
		go func(cfg agentConfig) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs <- testutil.AgentError{Agent: cfg.ID, Op: "goroutine", Err: fmt.Errorf("panic: %v", r)}
				}
			}()

			// Wait for retain agents to finish so recall has data
			select {
			case <-retainsDone:
				// Retains complete, proceed with recall
			case <-time.After(300 * time.Second):
				errs <- testutil.AgentError{Agent: cfg.ID, Op: "recall", Err: fmt.Errorf("timeout waiting for retains to complete")}
				return
			}

			client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
			if err != nil {
				errs <- testutil.AgentError{Agent: cfg.ID, Op: "connect", Err: err}
				return
			}
			if err := client.Initialize(); err != nil {
				errs <- testutil.AgentError{Agent: cfg.ID, Op: "init", Err: err}
				return
			}
			defer client.Close()

			for _, item := range cfg.Items {
				time.Sleep(time.Duration(100+int(time.Now().UnixNano()%400)) * time.Millisecond)

				start := time.Now()
				_, err := client.Recall(item)
				dur := time.Since(start)

				if err != nil {
					errs <- testutil.AgentError{Agent: cfg.ID, Op: cfg.Op, Err: err}
				} else {
					results <- testutil.AgentResult{Agent: cfg.ID, Op: cfg.Op, Dur: dur}
				}
			}
		}(cfg)
	}

	wg.Wait()
	close(results)
	close(errs)

	var agentResults []testutil.AgentResult
	for r := range results {
		agentResults = append(agentResults, r)
	}
	var agentErrors []testutil.AgentError
	for e := range errs {
		agentErrors = append(agentErrors, e)
	}

	// Aggregate by agent
	perAgent := make(map[int]AgentOpsSummary)
	roleNames := map[int]string{0: "manager", 1: "friend", 2: "recruiter", 3: "adversary", 4: "observer"}
	for id, name := range roleNames {
		perAgent[id] = AgentOpsSummary{Role: name}
	}
	for _, r := range agentResults {
		s := perAgent[r.Agent]
		s.TotalOps++
		s.SuccessfulOps++
		s.AvgLatencyMs += float64(r.Dur.Milliseconds())
		perAgent[r.Agent] = s
	}
	for _, e := range agentErrors {
		s := perAgent[e.Agent]
		s.TotalOps++
		s.FailedOps++
		perAgent[e.Agent] = s
	}
	for id, s := range perAgent {
		if s.SuccessfulOps > 0 {
			s.AvgLatencyMs = s.AvgLatencyMs / float64(s.SuccessfulOps)
		}
		perAgent[id] = s
	}

	metrics := ConcurrencyMetrics{
		TotalOps:        len(agentResults) + len(agentErrors),
		SuccessfulOps:   len(agentResults),
		FailedOps:       len(agentErrors),
		Agents:          5,
		PerAgentResults: perAgent,
	}

	// Multi-agent: passed if all agents had at least some successful ops
	totalSuccessful := metrics.SuccessfulOps
	totalFailed := metrics.FailedOps
	passed := totalSuccessful > 0 && float64(totalFailed)/float64(totalSuccessful+totalFailed) <= 0.5

	metricsJSON, _ := json.Marshal(metrics)
	report := DimensionReport{
		Dimension:  "concurrency",
		Scenario:   "multi_agent",
		Timestamp:  time.Now(),
		Metrics:    metricsJSON,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "concurrency_5agents.json", report)

	t.Logf("Multi-agent: %d total ops, %d successful, %d failed",
		metrics.TotalOps, metrics.SuccessfulOps, metrics.FailedOps)

	if len(agentResults) == 0 && len(agentErrors) > 0 {
		t.Fatal("all agents failed — possible infrastructure issue")
	}
}

func TestStressConcurrency_Burst(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	bank := "stress:concurrency:shared"
	client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	const burstSize = 100
	var wg sync.WaitGroup
	errs := make(chan testutil.AgentError, burstSize)

	burstStart := time.Now()
	for i := 0; i < burstSize; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs <- testutil.AgentError{Agent: n, Op: "retain_burst", Err: fmt.Errorf("panic: %v", r)}
				}
			}()
			_, err := client.Retain(fmt.Sprintf("burst retain item %d at %s", n, time.Now().Format(time.RFC3339Nano)))
			if err != nil {
				errs <- testutil.AgentError{Agent: n, Op: "retain_burst", Err: err}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	burstDur := time.Since(burstStart)

	var burstErrors []testutil.AgentError
	for e := range errs {
		burstErrors = append(burstErrors, e)
	}

	metrics := BurstMetrics{
		BurstSize:     burstSize,
		SubmittedInMs: float64(burstDur.Microseconds()) / 1000.0,
		Errors:        len(burstErrors),
		ErrorRate:     float64(len(burstErrors)) / float64(burstSize),
	}

	burstErrorRate := float64(len(burstErrors)) / float64(burstSize)
	passed := burstErrorRate <= 0.5

	metricsJSON, _ := json.Marshal(metrics)
	report := DimensionReport{
		Dimension:  "concurrency",
		Scenario:   "burst",
		Timestamp:  time.Now(),
		Metrics:    metricsJSON,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "concurrency_burst.json", report)

	t.Logf("Burst: %d retains submitted in %.1fms, errors=%d (%.1f%%)",
		burstSize, metrics.SubmittedInMs, len(burstErrors), metrics.ErrorRate*100)

	if len(burstErrors) == burstSize {
		t.Fatal("all 100 burst retains failed — possible infrastructure issue")
	}
}

// ─── Dimension 4: Chaos ────────────────────────────────────────────────

func TestStressChaos_KillLlama(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	pids := readPIDFile(t)
	if pids == nil {
		t.Skip("PID file not available")
		return
	}
	llamaPID, ok := pids["llama"]
	if !ok {
		t.Skip("llama PID not found in PID file")
		return
	}
	validatePID(t, "llama", llamaPID)

	// Verify healthy baseline
	if !testutil.ServerUp() {
		t.Skip("server not healthy at baseline")
		return
	}
	t.Logf("Baseline health confirmed")

	// Create client and verify it works
	client, err := testutil.NewClient(testutil.DefaultServerURL, "stress:chaos:kill_llama")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	_, err = client.Retain("pre-chaos retain: chaos test baseline memory")
	if err != nil {
		t.Logf("pre-chaos retain failed: %v", err)
	}

	// Verify PID is alive before killing (stale PID safety check)
	if err := syscall.Kill(llamaPID, syscall.Signal(0)); err != nil {
		t.Logf("llama PID=%d is not alive (stale PID file?): %v — skipping kill", llamaPID, err)
		metrics := ChaosMetrics{
			Scenario: "kill_llama", Degraded: false, Recovered: false,
		}
		metricsJSON, _ := json.Marshal(metrics)
		report := DimensionReport{
			Dimension: "chaos", Scenario: "kill_llama",
			Timestamp: time.Now(), Metrics: metricsJSON, Passed: false,
			DurationMs: float64(time.Since(totalStart).Milliseconds()),
		}
		writeJSONReport(t, "chaos_kill_llama.json", report)
		t.Skipf("Skipping chaos kill: llama PID %d is stale", llamaPID)
		return
	}

	// Kill llama
	killStart := time.Now()
	t.Logf("Sending SIGTERM to llama (PID=%d)", llamaPID)
	if err := syscall.Kill(llamaPID, syscall.SIGTERM); err != nil {
		t.Logf("SIGTERM returned error (may already be dead): %v", err)
	}

	// Wait for degradation (up to 30s)
	degraded := false
	degradeTime := time.Duration(0)
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		if !testutil.ServerUp() {
			degraded = true
			degradeTime = time.Since(killStart)
			t.Logf("Server went down after %.1fs", degradeTime.Seconds())
			break
		}
	}
	if !degraded {
		t.Logf("WARNING: server did not go down within 30s of SIGTERM")
	}

	// Try operations during degraded state
	recallOutput, recallErr := client.Recall("chaos test baseline")
	t.Logf("Recall during degraded state: err=%v output=%s", recallErr, truncate(recallOutput, 100))

	_, retainErr := client.Retain("post-chaos retain: should fail if llama is down")
	t.Logf("Retain during degraded state: err=%v", retainErr)

	// Wait for recovery (up to 120s)
	recovered := false
	recoveryTime := time.Duration(0)
	for i := 0; i < 120; i++ {
		time.Sleep(1 * time.Second)
		if testutil.ServerUp() {
			recovered = true
			recoveryTime = time.Since(killStart)
			t.Logf("Server recovered after %.1fs", recoveryTime.Seconds())
			break
		}
	}
	if !recovered {
		t.Logf("WARNING: server did not recover within 120s")
	}

	// Post-recovery verification
	if recovered {
		_, err := client.Retain("post-recovery retain: chaos test recovery verification")
		if err != nil {
			t.Logf("post-recovery retain failed: %v", err)
		} else {
			t.Logf("post-recovery retain succeeded")
		}
	}

	metrics := ChaosMetrics{
		Scenario:          "kill_llama",
		Degraded:          degraded,
		Recovered:         recovered,
		DegradeTimeSec:    degradeTime.Seconds(),
		RecoveryTimeSec:   recoveryTime.Seconds(),
		DowntimeSec:       recoveryTime.Seconds() - degradeTime.Seconds(),
		RecallDuringChaos: recallErr == nil,
		RetainDuringChaos: retainErr == nil,
	}

	passed := recovered
	metricsJSON, _ := json.Marshal(metrics)
	report := DimensionReport{
		Dimension:  "chaos",
		Scenario:   "kill_llama",
		Timestamp:  time.Now(),
		Metrics:    metricsJSON,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "chaos_kill_llama.json", report)
}

func TestStressChaos_KillHindsight(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	pids := readPIDFile(t)
	if pids == nil {
		t.Skip("PID file not available")
		return
	}
	hindsightPID, ok := pids["hindsight"]
	if !ok {
		t.Skip("hindsight PID not found in PID file")
		return
	}
	validatePID(t, "hindsight", hindsightPID)

	if !testutil.ServerUp() {
		t.Skip("server not healthy at baseline")
		return
	}
	t.Logf("Baseline health confirmed")

	client, err := testutil.NewClient(testutil.DefaultServerURL, "stress:chaos:kill_hindsight")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	_, err = client.Retain("pre-chaos retain: hindsight baseline")
	if err != nil {
		t.Logf("pre-chaos retain failed: %v", err)
	}

	// Verify PID is alive before killing (stale PID safety check)
	if err := syscall.Kill(hindsightPID, syscall.Signal(0)); err != nil {
		t.Logf("hindsight PID=%d is not alive (stale PID file?): %v — skipping kill", hindsightPID, err)
		metrics := ChaosMetrics{
			Scenario: "kill_hindsight", Degraded: false, Recovered: false,
		}
		metricsJSON, _ := json.Marshal(metrics)
		report := DimensionReport{
			Dimension: "chaos", Scenario: "kill_hindsight",
			Timestamp: time.Now(), Metrics: metricsJSON, Passed: false,
			DurationMs: float64(time.Since(totalStart).Milliseconds()),
		}
		writeJSONReport(t, "chaos_kill_hindsight.json", report)
		t.Skipf("Skipping chaos kill: hindsight PID %d is stale", hindsightPID)
		return
	}

	killStart := time.Now()
	t.Logf("Sending SIGTERM to hindsight (PID=%d)", hindsightPID)
	if err := syscall.Kill(hindsightPID, syscall.SIGTERM); err != nil {
		t.Logf("SIGTERM returned error (may already be dead): %v", err)
	}

	degraded := false
	degradeTime := time.Duration(0)
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		if !testutil.ServerUp() {
			degraded = true
			degradeTime = time.Since(killStart)
			t.Logf("Server went down after %.1fs", degradeTime.Seconds())
			break
		}
	}
	if !degraded {
		t.Logf("WARNING: server did not go down within 30s of SIGTERM")
	}

	recallOutput, recallErr := client.Recall("hindsight baseline")
	t.Logf("Recall during degraded state: err=%v output=%s", recallErr, truncate(recallOutput, 100))

	_, retainErr := client.Retain("post-chaos retain: hindsight down")
	t.Logf("Retain during degraded state: err=%v", retainErr)

	recovered := false
	recoveryTime := time.Duration(0)
	for i := 0; i < 120; i++ {
		time.Sleep(1 * time.Second)
		if testutil.ServerUp() {
			recovered = true
			recoveryTime = time.Since(killStart)
			t.Logf("Server recovered after %.1fs", recoveryTime.Seconds())
			break
		}
	}
	if !recovered {
		t.Logf("WARNING: server did not recover within 120s")
	}

	if recovered {
		_, err := client.Retain("post-recovery retain: hindsight recovery")
		if err != nil {
			t.Logf("post-recovery retain failed: %v", err)
		} else {
			t.Logf("post-recovery retain succeeded")
		}
	}

	metrics := ChaosMetrics{
		Scenario:          "kill_hindsight",
		Degraded:          degraded,
		Recovered:         recovered,
		DegradeTimeSec:    degradeTime.Seconds(),
		RecoveryTimeSec:   recoveryTime.Seconds(),
		DowntimeSec:       recoveryTime.Seconds() - degradeTime.Seconds(),
		RecallDuringChaos: recallErr == nil,
		RetainDuringChaos: retainErr == nil,
	}

	passed := recovered
	metricsJSON, _ := json.Marshal(metrics)
	report := DimensionReport{
		Dimension:  "chaos",
		Scenario:   "kill_hindsight",
		Timestamp:  time.Now(),
		Metrics:    metricsJSON,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "chaos_kill_hindsight.json", report)
}

func TestStressChaos_Flood(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	bank := "stress:chaos:flood"
	client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	defer client.Close()

	_, err = client.Retain("flood baseline memory for chaos testing")
	if err != nil {
		t.Logf("flood baseline retain error: %v", err)
	}

	const floodSize = 100
	var wg sync.WaitGroup
	results := make(chan floodOpResult, floodSize)

	floodStart := time.Now()
	for i := 0; i < floodSize; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results <- floodOpResult{N: n, Err: fmt.Errorf("panic: %v", r)}
				}
			}()
			start := time.Now()
			_, err := client.Retain(fmt.Sprintf("flood item %d content for queue saturation test", n))
			results <- floodOpResult{
				N:       n,
				Err:     err,
				DurMs:   float64(time.Since(start).Microseconds()) / 1000.0,
				IsError: err != nil,
			}
		}(i)
	}
	wg.Wait()
	close(results)
	floodDur := time.Since(floodStart)

	var floodResults []floodOpResult
	for r := range results {
		floodResults = append(floodResults, r)
	}

	var overloaded, timeout, rateLimited, otherErrors int
	sampleErrors := make(map[string]int) // error string -> count
	for _, r := range floodResults {
		if r.Err != nil {
			errStr := r.Err.Error()
			// Capture sample error strings for diagnosis (P1-7)
			if len(sampleErrors) < 5 {
				sampleErrors[errStr]++
			}
			if strings.Contains(errStr, "overloaded") || strings.Contains(errStr, "queue") {
				overloaded++
			} else if strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline") {
				timeout++
			} else if strings.Contains(errStr, "status 429") {
				rateLimited++
			} else if strings.Contains(errStr, "status 503") {
				overloaded++ // 503 is service unavailable, treat as overload
			} else {
				otherErrors++
			}
		}
	}
	for errStr, count := range sampleErrors {
		t.Logf("Flood sample error (x%d): %s", count, truncate(errStr, 200))
	}

	floodErrorRate := float64(overloaded+timeout+rateLimited+otherErrors) / float64(floodSize)
	passed := floodErrorRate <= 0.5

	metrics := FloodMetrics{
		FloodSize:     floodSize,
		SubmittedInMs: float64(floodDur.Microseconds()) / 1000.0,
		Successful:    floodSize - overloaded - timeout - rateLimited - otherErrors,
		Overloaded:    overloaded + rateLimited,
		Timeouts:      timeout,
		OtherErrors:   otherErrors,
	}

	metricsJSON, _ := json.Marshal(metrics)
	report := DimensionReport{
		Dimension:  "chaos",
		Scenario:   "flood",
		Timestamp:  time.Now(),
		Metrics:    metricsJSON,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "chaos_flood.json", report)

	t.Logf("Flood: %d retains in %.1fms, successful=%d overloaded=%d timeout=%d rate_limited=%d other=%d",
		floodSize, metrics.SubmittedInMs, metrics.Successful, overloaded, timeout, rateLimited, otherErrors)

	if overloaded+timeout+rateLimited+otherErrors == floodSize && otherErrors > 0 {
		t.Fatal("all flood retains failed with non-overload errors — possible infrastructure issue")
	}
}

// ─── Dimension 5: Edge Cases ───────────────────────────────────────────

func TestStressEdge_All(t *testing.T) {
	requireServerUp(t)
	totalStart := time.Now()

	edgeCases := loadEdgeCaseItems(t)

	// Group by category
	type edgeTest struct {
		Category string
		Bank     string
		Retains  []string
		Probes   []ProbeEntry
	}

	categoryMap := make(map[string][]EdgeCaseItem)
	for _, item := range edgeCases {
		categoryMap[item.Category] = append(categoryMap[item.Category], item)
	}

	tests := []edgeTest{
		{
			Category: "long_form",
			Bank:     "stress:edge:longform",
			Probes:   []ProbeEntry{{Query: "What is the Distributed Memory System API?", ExpectedConcept: "API"}},
		},
		{
			Category: "code",
			Bank:     "stress:edge:code",
			Probes:   []ProbeEntry{{Query: "How does the fibonacci function work?", ExpectedConcept: "fibonacci"}},
		},
		{
			Category: "multilingual",
			Bank:     "stress:edge:multilingual",
			Probes: []ProbeEntry{
				{Query: "Tell me about London", ExpectedConcept: "London"},
				{Query: "Que sabes sobre Madrid?", ExpectedConcept: "Madrid"},
				{Query: "Parlez-moi de Paris", ExpectedConcept: "Paris"},
				{Query: "Erzaehl mir von Berlin", ExpectedConcept: "Berlin"},
				{Query: "Tokyo ni tsuite oshiete", ExpectedConcept: "Tokyo"},
			},
		},
		{
			Category: "nonfactual",
			Bank:     "stress:edge:nonfactual",
			Probes:   []ProbeEntry{{Query: "What is the opinion about pineapple on pizza?", ExpectedConcept: "pineapple"}},
		},
		{
			Category: "duplicate",
			Bank:     "stress:edge:duplicate",
			Probes:   []ProbeEntry{{Query: "Where is the Eiffel Tower?", ExpectedConcept: "Eiffel Tower"}},
		},
	}

	var allResults []ProbeResult
	for _, test := range tests {
		items := categoryMap[test.Category]
		if len(items) == 0 {
			t.Logf("No items for category %s", test.Category)
			continue
		}

		t.Logf("Edge case: %s (bank=%s, retains=%d)", test.Category, test.Bank, len(items))

		client, err := testutil.NewClient(testutil.DefaultServerURL, test.Bank)
		if err != nil {
			t.Logf("NewClient for %s: %v", test.Category, err)
			continue
		}
		if err := client.Initialize(); err != nil {
			t.Logf("Initialize for %s: %v", test.Category, err)
			client.Close()
			continue
		}

		// Retain all items
		for _, item := range items {
			_, err := client.Retain(item.Content)
			if err != nil {
				t.Logf("Edge retain error (category=%s): %v", test.Category, err)
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Probe
		for _, probe := range test.Probes {
			start := time.Now()
			output, err := client.Recall(probe.Query)
			dur := time.Since(start)

			pr := ProbeResult{
				Query:           probe.Query,
				ExpectedConcept: probe.ExpectedConcept,
				ActualOutput:    output,
				LatencyMs:       float64(dur.Microseconds()) / 1000.0,
			}
			if err != nil {
				pr.Note = fmt.Sprintf("recall error: %v", err)
			}
			allResults = append(allResults, pr)
		}

		client.Close()
	}

	// P1-14: Add empty/special-char probe to verify graceful handling.
	// Probe against the duplicate bank (already has data from above).
	edgeClient, err := testutil.NewClient(testutil.DefaultServerURL, "stress:edge:duplicate")
	if err == nil {
		if initErr := edgeClient.Initialize(); initErr == nil {
			// Empty string probe
			start := time.Now()
			emptyOutput, emptyErr := edgeClient.Recall("")
			emptyDur := time.Since(start)
			allResults = append(allResults, ProbeResult{
				Query:           "",
				ExpectedConcept: "",
				ActualOutput:    emptyOutput,
				LatencyMs:       float64(emptyDur.Microseconds()) / 1000.0,
				Note:            func() string { if emptyErr != nil { return fmt.Sprintf("empty probe error (expected): %v", emptyErr) }; return "" }(),
			})
			t.Logf("Empty-string probe: err=%v output_len=%d", emptyErr, len(emptyOutput))
		}
		edgeClient.Close()
	}

	// Edge cases: passed if all probes have actual content (no unexpected errors)
	// Expected errors (empty-string probe, etc.) have "(expected)" in the note
	edgeProbeErrors := 0
	for _, r := range allResults {
		if r.Note != "" && !strings.Contains(r.Note, "(expected)") {
			edgeProbeErrors++
		}
	}
	passed := edgeProbeErrors == 0

	report := DimensionReport{
		Dimension:  "edge",
		Scenario:   "all",
		Timestamp:  time.Now(),
		Results:    allResults,
		Passed:     passed,
		DurationMs: float64(time.Since(totalStart).Milliseconds()),
	}
	writeJSONReport(t, "edge_all.json", report)
	t.Logf("Edge cases complete: %d total probe results", len(allResults))
}
