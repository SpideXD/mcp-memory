// Package queue provides a SQLite-backed job queue with worker pool.
//
// This package compiles without CGO. Verify with: CGO_ENABLED=0 go build ./queue/...
package queue

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status represents the state of a job in the queue.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusDead      Status = "dead"
)

// Default constants for the queue package.
const (
	DefaultMaxRetries  = 3
	DefaultMaxPending  = 1000
	DefaultJobTTL      = 24 * time.Hour
	DefaultWorkerCount = 4
	DefaultSemSize     = 3
	DefaultTTLInterval = 5 * time.Minute
)

// Sentinel errors for the queue package.
var (
	ErrQueueFull   = errors.New("queue is full: too many pending jobs")
	ErrJobNotFound = errors.New("job not found")
)

// Job represents a unit of work in the queue.
// Job is a plain data struct — callers provide synchronization.
type Job struct {
	ID         string `json:"id"`
	Bank       string `json:"bank"`
	Type       string `json:"type"`
	Payload    string `json:"payload"`
	Status     Status `json:"status"`
	RetryCount int    `json:"retry_count"`
	MaxRetries int    `json:"max_retries"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// Validate checks that all required fields are present and valid.
// Validate focuses on required-field presence; the Store handles status defaults.
func (j *Job) Validate() error {
	if strings.TrimSpace(j.ID) == "" {
		return fmt.Errorf("job ID must not be empty")
	}
	if strings.TrimSpace(j.Bank) == "" {
		return fmt.Errorf("bank must not be empty")
	}
	if j.Type != "retain" && j.Type != "reflect" {
		return fmt.Errorf("job type must be 'retain' or 'reflect'")
	}
	if j.Type == "retain" && strings.TrimSpace(j.Payload) == "" {
		return fmt.Errorf("payload must not be empty")
	}
	if j.MaxRetries < 0 || j.MaxRetries > 10 {
		return fmt.Errorf("max_retries must be between 0 and 10")
	}
	return nil
}

// CanRetry returns true if the job is in failed state and has retries remaining.
func (j *Job) CanRetry() bool {
	return j.Status == StatusFailed && j.RetryCount < j.MaxRetries
}

// Clone returns a deep copy of the job.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	clone := *j
	return &clone
}
