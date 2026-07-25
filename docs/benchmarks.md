# Cognee Backend Benchmarks

## DeepSeek V4 Flash — best overall

| Metric | Value |
|--------|-------|
| Model | `deepseek/deepseek-v4-flash` |
| Price | $0.14/$0.28 per M input/output |
| Contradiction (22 facts, 15 probes) | **14/15 (93%)** without temporal_cognify; **14/14** with temporal_cognify |
| Scale 50 | **11/11 (100%)** |
| Concurrent (5x) | 5/5 facts preserved |
| Avg retain | 22.7s |

Failures without temporal_cognify: "Who started their own company?" (cross-person aggregation miss). Fixed by temporal_cognify.

## gpt-oss-120b (Groq)

| Metric | Value |
|--------|-------|
| Model | `openai/gpt-oss-120b` |
| Price | $0.15/$0.60 per M input/output |
| Contradiction (22 facts) | **12/15** without temporal_cognify; **14/14** with temporal_cognify |
| Avg retain | 20.9s (8% faster than DeepSeek) |
| TPS | 300 (5x LLM speed) |

LLM 5x faster but pipeline overhead (embedding, graph writes) limits end-to-end gain to 8%.

## Model Comparison (same graph, recall only)

| Model | Score | Notes |
|-------|-------|-------|
| **DeepSeek V4 Flash** | 14/15 | Terse but accurate |
| **gpt-oss-120b** | 14/15 | Detailed, only model to find Carol→Anthropic without temporal |
| DeepSeek V4 Pro | 11/15 | Worst — vague on temporal questions |
| Mimo 2.5 Pro | 12/15 | Verbose, gets Stripe/Square order wrong |

## Temporal Cognify Impact

| Probe | Without | With temporal_cognify |
|-------|---------|----------------------|
| Alice work **now**? | Google ❌ | **Sentinela** ✅ |
| Who started company? | "no information" ❌ | **Alice and Bob** ✅ |
| Carol at OpenAI? | "research lead" ⚠️ | **"research lead → director → GPT-5 safety"** ✅ |

Temporal cognify structures the graph as a timeline. Old facts preserved for historical queries, "now" resolves to latest.

## Rust vs Python Cognee

| Factor | Rust | Python |
|--------|------|--------|
| Retain speed | 20-30s | **275s** (10x slower) |
| Temporal cognify | ✅ Patched + verified | ❌ Not in HTTP layer |
| DeepSeek compat | Native | json_mode hack breaks recall |
| Memory idle | 64MB | ~90MB |
| Startup | 2s | 30s |

Python's LanceDB/Ladybug Python bindings use PyArrow FFI — every write crosses Python/C++ boundary. Rust native crates operate directly on Arrow memory.

## Final Configuration

- **Backend**: Rust Cognee (+ mcp-memory patches submodule)
- **LLM**: `deepseek/deepseek-v4-flash` (best quality-to-cost)
- **Embedding**: local `qwen3-embedding-0.6b-Q8_0` via llama-server (Metal GPU, 30ms)
- **Store**: `temporal_cognify=true`, auto-stamp date if content lacks year
- **Recall**: default `GRAPH_COMPLETION` (TEMPORAL search broken)
- **Forget**: `memory_only=true`
