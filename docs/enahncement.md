# Resilience Enhancements: Road to 9.5/10

This roadmap assumes Cognee remains an LLM-heavy external dependency. The target
is not to make Cognee artificially fast. The target is to make the surrounding
memory system resilient when Cognee is slow, temporarily unavailable, or under
load.

## What a 9.5/10 system means here

A 9.5 system should provide:

- fast and durable request admission;
- predictable backpressure instead of overload collapse;
- strict authenticated user/profile isolation;
- fair access across tenants;
- at-least-once processing with idempotent effects;
- crash recovery without manual cleanup;
- graceful degradation when Cognee or the LLM provider is slow;
- clear operational evidence of capacity and failure state.

Cognee retain latency can remain tens of seconds or longer. That is acceptable if
the gateway accepts work safely, reports its state, and eventually completes or
dead-letters it honestly.

## 1. Measure admission separately from completion

Do not use one latency number for the entire system.

Track two separate paths:

### Admission path

The MCP request should validate identity, validate content, persist a job, and
return a job ID quickly. Suggested initial targets:

- admission p95 below 250 ms when the queue is healthy;
- no accepted job disappears from SQLite;
- queue-full responses are explicit and actionable.

### Processing path

Retain and reflect completion time should be measured against the actual LLM and
Cognee provider. Track:

- time pending;
- time running;
- total completion time;
- retry count;
- dead-letter rate;
- oldest pending job age;
- completion rate per bank and globally.

Capacity is approximately:

```text
throughput ≈ backend concurrency / average Cognee processing time
```

For example, three concurrent operations at 23 seconds average is roughly eight
retains per minute before retries, reflection, provider throttling, or failures.
That is a capacity fact to expose to operators, not necessarily a latency bug.

## 2. Add bulkheads around expensive work

Use separate capacity pools instead of allowing every operation to compete
indiscriminately:

- recall pool;
- user-triggered retain pool;
- user-triggered reflect pool;
- low-priority auto-reflect pool;
- low-priority auto-improve pool.

All pools must still respect a global Cognee/LLM budget. Auto-improve should never
silently bypass the capacity controller.

When Cognee is saturated, recalls should remain available if the provider allows
it, while new writes receive controlled backpressure.

## 3. Make fairness a first-class feature

The system serves many logical tenants, even if they share one Cognee process.

Recommended controls:

- per-bank pending-job quota;
- per-bank retain rate limit;
- global queue limit;
- fair scheduler or weighted round-robin;
- optional tenant priority classes;
- maximum concurrent operations per bank;
- maximum total memory payload accepted per time window.

This prevents one user/profile from creating a global queue outage.

## 4. Use durable jobs with leases, not permanent running states

Improve the queue from a simple status flag into a recoverable job protocol:

```text
pending → leased/running → completed
                    └──→ retryable failure → pending
                    └──→ terminal failure → dead
```

Each running job should have:

- lease owner;
- lease expiry;
- attempt number;
- idempotency key;
- last heartbeat or update time;
- final result/error.

If a worker or process dies, another worker can reclaim expired jobs without
requiring a full server restart.

## 5. Make retries safe and calm

- Use idempotency keys for all side-effecting operations.
- Retry only transient failures.
- Do not retry known validation or authorization errors.
- Add exponential backoff with random jitter.
- Respect provider `Retry-After` responses when available.
- Apply a per-bank retry budget so one failing tenant cannot consume all workers.
- Keep circuit breakers separate for recall, retain, and improve when their
  failure behavior differs.

The objective is not maximum retry volume. It is eventual correctness without a
retry storm.

## 6. Guarantee user/profile isolation in depth

Use multiple layers:

1. Authenticated identity produces the canonical bank.
2. Session bank is immutable.
3. Every job stores the bank explicitly.
4. Every backend request includes the bank.
5. Status reads verify bank ownership.
6. Cognee and llama ports are unreachable from untrusted networks.
7. Logs and alerts never expose memory content across tenants.

The unique `user + profile` key should be treated as a tenant identity, not as a
user-provided string.

## 7. Add graceful degradation modes

Define behavior for each dependency state:

### Cognee healthy

Accept and process normal traffic.

### Cognee slow

Continue accepting only while queue age and capacity remain within limits. Return
explicit queue status; do not pretend the memory was already stored.

### Cognee unavailable

Open the circuit, stop wasting retries, preserve durable jobs, and expose a
degraded health state.

### LLM provider throttled

Reduce backend concurrency, honor retry hints, and preserve pending jobs.

### Gateway shutting down

Stop admission, finish or safely requeue bounded work, close sessions, stop child
processes, and report what was drained versus requeued.

## 8. Improve observability without high-cardinality damage

Every operation should be traceable by:

- request ID;
- job ID;
- bank hash or redacted tenant ID;
- operation type;
- attempt number;
- backend/provider;
- queue state transitions.

Useful aggregate metrics include:

- accepted, completed, failed, and dead jobs;
- queue depth and oldest age;
- running jobs by operation type;
- backend latency distributions;
- circuit state;
- retries and retry causes;
- SSE disconnects and response drops;
- per-tenant quota rejections.

Avoid putting raw user IDs or profile names into metric labels if the number of
tenants can grow substantially. Use bounded labels or hashed/redacted IDs.

## 9. Test resilience, not Cognee speed

The most valuable evidence is failure and load behavior:

- 10, 25, and 50 concurrent authenticated agents;
- burst retain load with a slow mocked Cognee;
- Cognee process kill during retain;
- LLM timeout and rate-limit responses;
- connection loss after a successful backend side effect;
- gateway restart with running and pending jobs;
- repeated start/stop cycles;
- queue-full and per-bank quota behavior;
- simultaneous session cleanup and SSE writes;
- cross-bank authorization attempts;
- long soak with many distinct banks;
- auto-reflect and auto-improve enabled under load.

The success criteria should focus on no lost jobs, no cross-bank access, bounded
resource usage, correct recovery, and fair progress—not on making Cognee's LLM
pipeline fast.

## 10. Know when to split services

For 10–50 agents, one gateway process with a durable local queue can be a sensible
starting point.

If the workload grows beyond one host, local SQLite and child-process ownership
become limitations. At that point consider:

- a shared durable queue;
- a shared database or queue coordinator;
- separate Cognee workers;
- distributed concurrency limits;
- centralized tenant quotas;
- external health and alerting.

Do not horizontally scale the gateway blindly while every replica starts its own
Cognee and llama processes against the same ports or data directory.

## Suggested implementation order

1. Complete [`need_fix.md`](need_fix.md), especially identity, lifecycle, SSE,
   idempotency, and queue correctness.
2. Add global and per-bank backpressure.
3. Add leases and safe job recovery.
4. Add bulkheads for retain, recall, reflect, and background work.
5. Add structured capacity metrics and alerts.
6. Run 10/25/50-agent resilience and chaos scenarios.
7. Document the measured operating envelope and recommended deployment limits.

The path to 9.5 is resilience around Cognee, not pretending Cognee is a low-
latency database.

