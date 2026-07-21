package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// AlertLevel represents severity.
type AlertLevel string

const (
	AlertInfo     AlertLevel = "info"
	AlertWarn     AlertLevel = "warn"
	AlertError    AlertLevel = "error"
	AlertCritical AlertLevel = "critical"
)

// Alert is a structured alert sent to the main layer's HTTP endpoint.
type Alert struct {
	Service string                 `json:"service"`
	Level   AlertLevel             `json:"level"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
}

// AlertClient sends alerts to the main Go layer's HTTP endpoint.
// Supports optional mode (skip if down) and required mode (fail startup).
type AlertClient struct {
	url    string
	mode   string // "optional" or "required"
	client *http.Client
}

// NewAlertClient creates an alert client. Returns nil if ALERT_URL is empty.
func NewAlertClient(url, mode string) *AlertClient {
	if url == "" {
		return nil
	}
	return &AlertClient{
		url:    url + "/alert",
		mode:   mode,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// CheckHealth verifies the alert endpoint is reachable.
// Called during startup. If mode is "required" and the endpoint is down,
// the server should refuse to start.
func (a *AlertClient) CheckHealth() error {
	resp, err := a.client.Get(a.url + "/health")
	if err != nil {
		return fmt.Errorf("alert endpoint not reachable: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("alert endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// Send fires an alert. Retries once. Silently drops if optional and unreachable.
func (a *AlertClient) Send(level AlertLevel, message string, details map[string]interface{}) {
	if a == nil {
		return
	}
	alert := Alert{
		Service: "memory",
		Level:   level,
		Message: message,
		Details: details,
	}
	body, err := json.Marshal(alert)
	if err != nil {
		return
	}

	for attempt := 0; attempt < 2; attempt++ {
		resp, err := a.client.Post(a.url, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Optional mode — silently skip if down
}

// IsRequired returns true if alerting is mandatory.
func (a *AlertClient) IsRequired() bool {
	return a != nil && a.mode == "required"
}
