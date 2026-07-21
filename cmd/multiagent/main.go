//go:build ignore

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type mcpClient struct {
	baseURL   string
	sessionID string
	bank      string
}

func (c *mcpClient) connect() error {
	resp, err := http.Get(c.baseURL + "/mcp/sse?bank=" + c.bank)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("SSE connect: %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			c.sessionID = strings.TrimPrefix(line, "data: ")
			return nil
		}
	}
	return fmt.Errorf("no session ID in SSE response")
}

func (c *mcpClient) call(method string, params map[string]interface{}) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      time.Now().UnixNano(),
		"method":  method,
		"params":  params,
	}
	b, _ := json.Marshal(body)

	url := fmt.Sprintf("%s/mcp/message?session_id=%s", c.baseURL, c.sessionID)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(raw, &result)

	time.Sleep(500 * time.Millisecond)
	return result, nil
}

func (c *mcpClient) retain(content string) error {
	_, err := c.call("memory_retain", map[string]interface{}{"content": content})
	return err
}

func (c *mcpClient) recall(query string) (string, error) {
	resp, err := c.call("memory_recall", map[string]interface{}{"query": query})
	if err != nil {
		return "", err
	}
	r, _ := resp["result"].(map[string]interface{})
	content, _ := r["content"].([]interface{})
	if len(content) > 0 {
		return content[0].(map[string]interface{})["text"].(string), nil
	}
	return "", nil
}

func (c *mcpClient) reflect() (string, error) {
	resp, err := c.call("memory_reflect", map[string]interface{}{})
	if err != nil {
		return "", err
	}
	r, _ := resp["result"].(map[string]interface{})
	content, _ := r["content"].([]interface{})
	if len(content) > 0 {
		return content[0].(map[string]interface{})["text"].(string), nil
	}
	return "", nil
}

type Agent struct {
	Name   string
	Bank   string
	client *mcpClient
}

func main() {
	baseURL := os.Getenv("MEMORY_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8899"
	}

	if _, err := http.Get(baseURL + "/health"); err != nil {
		fmt.Println("Memory server not running. Start it first:")
		fmt.Println("  cd mcp/memory && go run .")
		os.Exit(0)
	}

	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║   Multi-Agent Memory Simulation             ║")
	fmt.Println("╚══════════════════════════════════════════════╝\n")

	agents := []*Agent{
		{Name: "scanner", Bank: "multi-test:scanner"},
		{Name: "writer", Bank: "multi-test:writer"},
		{Name: "debugger", Bank: "multi-test:debugger"},
		{Name: "explorer", Bank: "multi-test:explorer"},
		{Name: "reviewer", Bank: "multi-test:reviewer"},
	}

	// Phase 1: Connect
	fmt.Println("─── Phase 1: Connect 5 Agents ───")
	for _, a := range agents {
		a.client = &mcpClient{baseURL: baseURL, bank: a.Bank}
		if err := a.client.connect(); err != nil {
			fmt.Printf("[%s] connect failed: %v\n", a.Name, err)
			return
		}
		fmt.Printf("[%s] connected (bank: %s)\n", a.Name, a.Bank)
	}

	// Phase 2: Retain unique data
	fmt.Println("\n─── Phase 2: Retain Unique Data ───")
	secrets := map[string]string{
		"scanner":  "Scanner found 3 vulnerabilities in auth module",
		"writer":   "Writer drafted quarterly report with 12 pages",
		"debugger": "Debugger traced memory leak to cache invalidation",
		"explorer": "Explorer mapped 42 new API endpoints",
		"reviewer": "Reviewer approved PR #847 with 3 suggestions",
	}

	var wg sync.WaitGroup
	results := make(map[string]time.Duration)
	var mu sync.Mutex

	for _, a := range agents {
		wg.Add(1)
		go func(agent *Agent) {
			defer wg.Done()
			start := time.Now()
			if err := agent.client.retain(secrets[agent.Name]); err != nil {
				fmt.Printf("[%s] retain failed: %v\n", agent.Name, err)
				return
			}
			elapsed := time.Since(start)
			mu.Lock()
			results[agent.Name] = elapsed
			mu.Unlock()
			fmt.Printf("[%s] retained (%.2fs)\n", agent.Name, elapsed.Seconds())
		}(a)
	}
	wg.Wait()

	// Phase 3: Bank isolation
	fmt.Println("\n─── Phase 3: Bank Isolation ───")
	for i, a := range agents {
		result, err := a.client.recall(secrets[a.Name][:10])
		if err != nil {
			fmt.Printf("[%s] recall failed: %v\n", a.Name, err)
			continue
		}
		if strings.Contains(strings.ToLower(result), strings.ToLower(a.Name)) {
			fmt.Printf("[%s] ✅ found own data\n", a.Name)
		} else {
			fmt.Printf("[%s] ⚠ got: %s...\n", a.Name, truncate(result, 40))
		}

		other := agents[(i+1)%len(agents)]
		result, err = a.client.recall(other.Name)
		if err == nil && strings.Contains(strings.ToLower(result), strings.ToLower(other.Name)) {
			fmt.Printf("[%s] ❌ BANK ISOLATION BROKEN: sees %s!\n", a.Name, other.Name)
		} else {
			fmt.Printf("[%s] ✅ isolated from %s\n", a.Name, other.Name)
		}
	}

	// Phase 4: Concurrent blast
	fmt.Println("\n─── Phase 4: Concurrent Recall Blast (50) ───")
	blastStart := time.Now()
	for i := 0; i < 10; i++ {
		for _, a := range agents {
			wg.Add(1)
			go func(agent *Agent, n int) {
				defer wg.Done()
				agent.client.recall(fmt.Sprintf("q-%d", n))
			}(a, i)
		}
	}
	wg.Wait()
	blastTime := time.Since(blastStart)
	fmt.Printf("50 recalls in %v (%.0fms avg)\n", blastTime, float64(blastTime.Milliseconds())/50.0)

	// Phase 5: Reflect
	fmt.Println("\n─── Phase 5: Reflect ───")
	for _, a := range agents {
		start := time.Now()
		result, err := a.client.reflect()
		elapsed := time.Since(start)
		if err != nil {
			fmt.Printf("[%s] reflect failed: %v\n", a.Name, err)
		} else {
			fmt.Printf("[%s] reflect: %s (%.2fs)\n", a.Name, truncate(result, 50), elapsed.Seconds())
		}
	}

	// Summary
	fmt.Println("\n╔══════════════════════════════════════════════╗")
	fmt.Println("║              PERFORMANCE REPORT              ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Printf("Agents: %d, Banks: %d\n", len(agents), len(agents))
	fmt.Printf("Retain times:\n")
	for _, a := range agents {
		fmt.Printf("  %-10s: %v\n", a.Name, results[a.Name])
	}
	fmt.Printf("Concurrent: %v (50 recalls)\n", blastTime)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n]) + "..."
	}
	return s
}
