# SQLite Job Queue + Hindsight Removal

## Motivation

50 agents simultaneously calling `memory_retain`. Current semaphore-reject pattern drops 48/50 requests. Need proper queuing with crash-safe state management.

## Architecture

```
Agent: memory_retain("Alice works at Sentinela")
       │
       ▼
handler: INSERT INTO jobs (id, bank, type, payload, status='pending')
handler: return {"status":"queued","job_id":"abc123"}
       │
       ▼  (async)
worker(4): SELECT ... WHERE status='pending' ORDER BY created_at LIMIT 1
worker: UPDATE status='running'
worker: semaphore(3) ← acquire
worker: backend.Retain(ctx, bank, payload)
worker: on success → UPDATE status='completed', result=...
worker: on failure → retry_count < max → UPDATE status='pending', retry_count++
worker: on failure → retry_count >= max → UPDATE status='dead'
worker: semaphore → release
       │
       ▼
Agent: memory_retain_status(job_id) → "completed" / "failed" / "dead"
```

## State Machine

```
  ┌─────────┐
  │ pending  │──→ worker picks up
  └─────────┘
       │
       ▼
  ┌─────────┐
  │ running  │──→ server crash → restart → pending (startup recovery)
  └─────────┘
       │
  ┌────┴────┐
  ▼         ▼
┌──────────┐ ┌────────┐
│completed │ │ failed │──→ retry_count < 3 → pending
└──────────┘ └────────┘
                  │
             retry_count >= 3
                  │
                  ▼
             ┌────────┐
             │  dead  │
             └────────┘
```

## Startup Recovery

```go
db.Exec(`UPDATE jobs SET status='pending' WHERE status='running'`)
db.Exec(`UPDATE jobs SET status='pending' WHERE status='failed' AND retry_count < max_retries`)
db.Exec(`UPDATE jobs SET status='dead' WHERE status='failed' AND retry_count >= max_retries`)
```

Server just started → nothing is running → all `running` jobs were orphaned → reset to pending.

## Schema

```sql
CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    bank TEXT NOT NULL,
    type TEXT NOT NULL,              -- 'retain' | 'reflect'
    payload TEXT NOT NULL,
    status TEXT DEFAULT 'pending',   -- pending|running|completed|failed|dead
    retry_count INTEGER DEFAULT 0,
    max_retries INTEGER DEFAULT 3,
    result TEXT,
    error TEXT,
    created_at INTEGER,
    updated_at INTEGER
);

CREATE INDEX idx_jobs_status_created ON jobs(status, created_at);
```

## Per-Tool Strategy

| Tool | Queue | Workers | Semaphore |
|------|-------|---------|-----------|
| memory_retain | SQLite, async | 4 shared | heavy(3) |
| memory_recall | No queue, sync | — | light(50) |
| memory_reflect | SQLite, async | 4 shared | heavy(3) |
| memory_forget | No queue, sync | — | — |

## Auto-Reflect Scheduling

Two triggers, OR logic:

```go
after each successful retain:
  bank.reflect_counter++

  if bank.reflect_counter >= AUTO_REFLECT_AFTER_N (default 10):
    schedule_reflect(bank)
    bank.reflect_counter = 0

  if time.Since(bank.last_reflect) > AUTO_REFLECT_TIMEOUT (default 6h):
    if bank.reflect_counter > 0:
      schedule_reflect(bank)
      bank.reflect_counter = 0
```

`schedule_reflect` inserts a `type='reflect'` job into the same SQLite queue.

## Hindsight Removal

### Deleted
- `backend/hindsight.go` — entire file
- `services.go`: startHindsight(), Hindsight health checks, Hindsight config branching
- `config.go`: HindsightPort, all HINDSIGHT_* env vars
- `handlers.go`: IsSync() branching (always Cognee path now)
- `workers.go`: Hindsight worker pools (retainJobs, reflectJobs, retainPool, reflectPool)
- `model/bge-reranker-base-Q4_k_m.gguf` — reranker model (209MB)
- `.env.example` — remove Hindsight config

### Simplified (always Cognee)
- `handlers.go`: no more IsSync() checks — single code path
- `services.go`: only llama-server + cognee-http-server subprocesses
- `config.go`: only Cognee-related config

## Preserved (untouched)
- SSE transport, bank URL pattern (`?bank=user:profile`)
- All MCP tools (minus memory_improve already removed)
- Auto-improve (per-bank counter, persistence, cooldown)
- Cognee mock (`internal/testutil/cogneemock/`)
- Date auto-stamp on retain
- temporal_cognify, memory_only config

## File Manifest

| File | Action | Lines |
|------|--------|-------|
| `backend/hindsight.go` | DELETE | ~140 |
| `queue/job.go` | NEW | ~80 (types, state machine) |
| `queue/store.go` | NEW | ~120 (SQLite CRUD, recovery) |
| `queue/worker.go` | NEW | ~60 (worker loop) |
| `handlers.go` | MODIFY | -50/+30 |
| `services.go` | MODIFY | -200/+10 |
| `workers.go` | MODIFY | -120/+10 |
| `config.go` | MODIFY | -30/+20 |
| `server.go` | MODIFY | -20/+30 |
| `model/bge-reranker-base-Q4_k_m.gguf` | DELETE | — |
| `.env.example` | MODIFY | -20 |
