package queue

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Store provides SQLite-backed CRUD operations for the job queue.
// Safe for concurrent use — mu serializes write operations.
type Store struct {
	db         *sql.DB
	mu         sync.Mutex
	maxPending int
	jobTTL     time.Duration
	closed     atomic.Bool
}

// StoreConfig configures the Store.
type StoreConfig struct {
	DBPath     string        // path to SQLite file (e.g., "./data/queue.db"). Use ":memory:" for tests.
	MaxPending int           // max pending jobs before Insert rejects (0 = use DefaultMaxPending)
	JobTTL     time.Duration // completed/failed/dead job retention (0 = use DefaultJobTTL, negative = forever)
}

// StoreStats holds queue statistics.
type StoreStats struct {
	Pending       int
	Running       int
	Completed     int
	Failed        int
	Dead          int
	OldestPending int64 // unix timestamp of oldest pending job, 0 if none
}

const createSchemaSQL = `
CREATE TABLE IF NOT EXISTS jobs (
    id          TEXT PRIMARY KEY,
    bank        TEXT NOT NULL,
    type        TEXT NOT NULL,
    payload     TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    retry_count INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER NOT NULL DEFAULT 3,
    result      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_status_created ON jobs(status, created_at);
`

// NewStore opens a SQLite database, applies pragmas, creates the schema,
// and runs startup recovery. Returns an error if any step fails.
func NewStore(cfg StoreConfig) (*Store, error) {
	// Apply defaults
	maxPending := cfg.MaxPending
	if maxPending == 0 {
		maxPending = DefaultMaxPending
	}
	jobTTL := cfg.JobTTL
	if jobTTL == 0 {
		jobTTL = DefaultJobTTL
	}

	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = ":memory:"
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Limit to 1 connection. Critical for :memory: databases where each
	// connection gets its own isolated in-memory DB. Also prevents SQLITE_BUSY
	// contention on file-based databases since Store.mu already serializes writes.
	db.SetMaxOpenConns(1)

	// Apply pragmas — these are safe to execute even on :memory:
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA cache_size=-8000",
		"PRAGMA mmap_size=67108864",
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}

	// Create schema
	if _, err := db.Exec(createSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	s := &Store{
		db:         db,
		maxPending: maxPending,
		jobTTL:     jobTTL,
	}

	// Run startup recovery (unlocked — store not yet shared)
	if _, err := s.recoverLocked(); err != nil {
		db.Close()
		return nil, fmt.Errorf("startup recovery: %w", err)
	}

	return s, nil
}

// Insert adds a new job to the queue.
// Returns ErrQueueFull if pending count >= maxPending.
// Validates the job before insertion.
func (s *Store) Insert(job *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return fmt.Errorf("store is closed")
	}

	// Apply defaults
	if job.Status == "" {
		job.Status = StatusPending
	}
	if job.MaxRetries == 0 {
		job.MaxRetries = DefaultMaxRetries
	}

	if err := job.Validate(); err != nil {
		return fmt.Errorf("validation: %w", err)
	}

	// Check pending count
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status = 'pending'").Scan(&count)
	if err != nil {
		return fmt.Errorf("count pending: %w", err)
	}
	if count >= s.maxPending {
		return ErrQueueFull
	}

	now := time.Now().Unix()
	job.CreatedAt = now
	job.UpdatedAt = now

	_, err = s.db.Exec(
		`INSERT INTO jobs (id, bank, type, payload, status, retry_count, max_retries, result, error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Bank, job.Type, job.Payload, string(job.Status),
		job.RetryCount, job.MaxRetries, job.Result, job.Error,
		job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}

	return nil
}

// NextPending atomically claims the oldest pending job and sets it to running.
// Returns nil, nil when the queue is empty or another worker claimed the job.
func (s *Store) NextPending() (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return nil, fmt.Errorf("store is closed")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() // no-op if committed

	// Use BEGIN IMMEDIATE semantics — Go's database/sql doesn't directly support it,
	// but WAL mode + mutex serialization handles concurrent writes safely.
	// We execute the SELECT+UPDATE as a unit.

	row := tx.QueryRow(
		`SELECT id, bank, type, payload, retry_count, max_retries
		 FROM jobs WHERE status = 'pending' ORDER BY created_at ASC LIMIT 1`,
	)

	var job Job
	err = row.Scan(&job.ID, &job.Bank, &job.Type, &job.Payload, &job.RetryCount, &job.MaxRetries)
	if err == sql.ErrNoRows {
		tx.Rollback()
		return nil, nil // empty queue
	}
	if err != nil {
		return nil, fmt.Errorf("select pending: %w", err)
	}

	now := time.Now().Unix()
	result, err := tx.Exec(
		`UPDATE jobs SET status = 'running', updated_at = ? WHERE id = ? AND status = 'pending'`,
		now, job.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("update to running: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("rows affected: %w", err)
	}
	if rowsAffected == 0 {
		// Another worker claimed it — optimistic locking guard
		tx.Rollback()
		return nil, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	job.Status = StatusRunning
	job.UpdatedAt = now
	return &job, nil
}

// UpdateStatus transitions a job to the given status.
// When status is StatusFailed, retry_count is incremented by 1.
// Returns ErrJobNotFound if no job matches the ID.
func (s *Store) UpdateStatus(id string, status Status, result string, errStr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return fmt.Errorf("store is closed")
	}

	now := time.Now().Unix()

	if status == StatusFailed {
		// Increment retry_count only when transitioning from running to failed.
		// This prevents retry_count inflation on illegal transitions
		// (e.g., completed→failed or pending→failed).
		res, err := s.db.Exec(
			`UPDATE jobs SET status = ?, result = ?, error = ?, retry_count = retry_count + 1, updated_at = ?
			 WHERE id = ? AND status = 'running'`,
			string(status), result, errStr, now, id,
		)
		if err != nil {
			return fmt.Errorf("update status: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrJobNotFound
		}
		return nil
	}

	res, err := s.db.Exec(
		`UPDATE jobs SET status = ?, result = ?, error = ?, updated_at = ?
		 WHERE id = ?`,
		string(status), result, errStr, now, id,
	)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrJobNotFound
	}
	return nil
}

// Get retrieves a job by ID. Returns nil, nil if not found.
func (s *Store) Get(id string) (*Job, error) {
	if s.closed.Load() {
		return nil, fmt.Errorf("store is closed")
	}

	var j Job
	err := s.db.QueryRow(
		`SELECT id, bank, type, payload, status, retry_count, max_retries, result, error, created_at, updated_at
		 FROM jobs WHERE id = ?`, id,
	).Scan(&j.ID, &j.Bank, &j.Type, &j.Payload, &j.Status, &j.RetryCount,
		&j.MaxRetries, &j.Result, &j.Error, &j.CreatedAt, &j.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return &j, nil
}

// CountByStatus returns the number of jobs with the given status.
func (s *Store) CountByStatus(status Status) (int, error) {
	if s.closed.Load() {
		return 0, fmt.Errorf("store is closed")
	}

	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status = ?", string(status)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count by status: %w", err)
	}
	return count, nil
}

// Stats returns counts by status and the age of the oldest pending job.
func (s *Store) Stats() (StoreStats, error) {
	if s.closed.Load() {
		return StoreStats{}, fmt.Errorf("store is closed")
	}

	var stats StoreStats

	rows, err := s.db.Query("SELECT status, COUNT(*), MIN(CASE WHEN status = 'pending' THEN created_at ELSE NULL END) FROM jobs GROUP BY status")
	if err != nil {
		return stats, fmt.Errorf("stats query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var st string
		var count int
		var oldest sql.NullInt64
		if err := rows.Scan(&st, &count, &oldest); err != nil {
			return stats, fmt.Errorf("stats scan: %w", err)
		}
		switch Status(st) {
		case StatusPending:
			stats.Pending = count
			if oldest.Valid {
				stats.OldestPending = oldest.Int64
			}
		case StatusRunning:
			stats.Running = count
		case StatusCompleted:
			stats.Completed = count
		case StatusFailed:
			stats.Failed = count
		case StatusDead:
			stats.Dead = count
		}
	}

	return stats, rows.Err()
}

// Recover resets orphaned jobs on startup:
//   - running → pending (server crashed while processing)
//   - failed with retries left → pending (retry)
//   - failed with exhausted retries → dead
//
// Safe for external use — acquires the mutex and checks closed state.
func (s *Store) Recover() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return 0, fmt.Errorf("store is closed")
	}

	return s.recoverLocked()
}

// recoverLocked performs startup recovery without acquiring the mutex.
// Called internally by NewStore before the store is shared.
func (s *Store) recoverLocked() (int, error) {

	now := time.Now().Unix()
	total := 0

	// running → pending
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'pending', updated_at = ? WHERE status = 'running'`,
		now,
	)
	if err != nil {
		return total, fmt.Errorf("recover running: %w", err)
	}
	n, _ := res.RowsAffected()
	total += int(n)

	// failed with retries left → pending
	res, err = s.db.Exec(
		`UPDATE jobs SET status = 'pending', updated_at = ? WHERE status = 'failed' AND retry_count < max_retries`,
		now,
	)
	if err != nil {
		return total, fmt.Errorf("recover failed retryable: %w", err)
	}
	n, _ = res.RowsAffected()
	total += int(n)

	// failed with exhausted retries → dead
	res, err = s.db.Exec(
		`UPDATE jobs SET status = 'dead', updated_at = ? WHERE status = 'failed' AND retry_count >= max_retries`,
		now,
	)
	if err != nil {
		return total, fmt.Errorf("recover failed exhausted: %w", err)
	}
	n, _ = res.RowsAffected()
	total += int(n)

	return total, nil
}

// StartTTLCleanup spawns a background goroutine that periodically deletes expired jobs.
// The goroutine exits when ctx is cancelled.
// If jobTTL <= 0, TTL cleanup is disabled and no goroutine is spawned.
func (s *Store) StartTTLCleanup(ctx context.Context, interval time.Duration) {
	if s.jobTTL <= 0 {
		return // TTL disabled
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("queue: TTL cleanup goroutine panicked: %v", r)
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.cleanupOnce()
			}
		}
	}()
}

// cleanupOnce runs a single TTL cleanup pass.
func (s *Store) cleanupOnce() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return
	}

	cutoff := time.Now().Add(-s.jobTTL).Unix()
	result, err := s.db.Exec(
		`DELETE FROM jobs WHERE status IN ('completed', 'failed', 'dead') AND updated_at < ?`,
		cutoff,
	)
	if err != nil {
		log.Printf("queue: TTL cleanup error: %v", err)
		return
	}
	n, _ := result.RowsAffected()
	if n > 0 {
		log.Printf("queue: TTL cleanup removed %d expired jobs", n)
	}
}

// Close closes the database. Safe to call multiple times.
// After Close, all Store methods return errors.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed.Load() {
		return nil // already closed
	}
	s.closed.Store(true)

	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
