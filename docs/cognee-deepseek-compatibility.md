# Cognee + DeepSeek Compatibility

**Status: ✅ SOLVED — 2026-07-24**

## Problem

Cognee (both Python v1.4.0 and Rust v0.1.3) uses **tool calling** (`tool_choice="required"`) for entity extraction via the `instructor` library. DeepSeek models (`deepseek/deepseek-v4-flash`, `deepseek-v4-pro`) reject `tool_choice` when their built-in thinking mode is active:

```
"Thinking mode does not support this tool_choice"
```

This is a DeepSeek server-side restriction that cannot be bypassed via `extra_body={'thinking': {'type': 'disabled'}}` or any other API parameter. The thinking mode runs regardless of the parameter.

Other models tested and rejected:
- `xiaomi/mimo-v2.5` / `xiaomi/mimo-v2.5-pro` — silently ignore tool calls
- All fallback approaches (LiteLLM custom routes, `drop_params`, `allowed_openai_params`) — DeepSeek blocks `tool_choice` at the provider level through OpenRouter

## Solution

Set Cognee's `LLM_INSTRUCTOR_MODE` to `json_mode`. This makes the `instructor` library use `Mode.JSON` instead of the default `Mode.TOOLS`, which replaces `tool_choice="required"` + `tools=[...]` with `response_format={"type": "json_object"}`.

**`response_format={"type": "json_object"}` works perfectly with DeepSeek.**

## Required Configuration

Two env vars are needed for the Cognee Python uvicorn process:

```bash
LLM_INSTRUCTOR_MODE=json_mode          # ← THE FIX: use JSON mode instead of tool calling
EMBEDDING_DIMENSIONS=1024              # match our local qwen3-embedding-0.6b model (3072 is Cognee's default)
```

Also set `EMBEDDING_PROVIDER=llama_cpp` (not `openai`) to prevent litellm from prepending an `openai/` prefix to our local embedding model name.

### Full Working Cognee Config

```bash
LLM_INSTRUCTOR_MODE=json_mode
EMBEDDING_DIMENSIONS=1024
LLM_API_KEY=sk-or-v1-...
LLM_MODEL=deepseek/deepseek-v4-flash
LLM_ENDPOINT=https://openrouter.ai/api/v1
LLM_PROVIDER=openai
EMBEDDING_ENDPOINT=http://localhost:8080/v1
EMBEDDING_PROVIDER=llama_cpp
EMBEDDING_API_KEY=not-needed
VECTOR_DB_PROVIDER=lancedb
GRAPH_DB_PROVIDER=ladybug
ENABLE_BACKEND_ACCESS_CONTROL=false
COGNEE_SKIP_CONNECTION_TEST=true
```

## How It Works

### Default (broken) path:
```
Cognee → LLMGateway → get_llm_client → OpenAIAdapter →
  instructor.from_litellm(litellm.acompletion, mode=Mode.TOOLS) →
  tool_choice="required" + tools=[...] → DeepSeek ❌
```

### Fixed path with `LLM_INSTRUCTOR_MODE=json_mode`:
```
Cognee → LLMGateway → get_llm_client → OpenAIAdapter →
  instructor.from_litellm(litellm.acompletion, mode=Mode.JSON) →
  response_format={"type": "json_object"} → DeepSeek ✅
```

Cognee's `OpenAIAdapter` constructor checks `self.instructor_mode`. When non-empty, it creates the instructor client with an explicit mode. `"json_mode"` maps to `instructor.Mode.JSON` (value `"json_mode"`), which uses `response_format={"type": "json_object"}` instead of tool calling.

The `LLM_INSTRUCTOR_MODE` env var flows through:
1. Pydantic-settings → `LLMConfig.llm_instructor_mode`
2. `get_llm_client(llm_config.llm_instructor_mode.lower())` → `"json_mode"`
3. `OpenAIAdapter(instructor_mode="json_mode")` → `instructor.Mode("json_mode")`
4. `instructor.from_litellm(litellm.acompletion, mode=Mode.JSON)`

## Valid Instructor Modes

| Mode String | Instructor Mode | Mechanism |
|---|---|---|
| `json_mode` | `Mode.JSON` | `response_format={"type": "json_object"}` |
| `md_json_mode` | `Mode.MD_JSON` | Markdown-fenced JSON |
| `json_schema_mode` | `Mode.JSON_SCHEMA` | `response_format.json_schema` (may not work with OpenRouter) |
| `tool_call` | `Mode.TOOLS` | `tool_choice` (DEFAULT — broken with DeepSeek) |
| `openrouter_structured_outputs` | `Mode.OPENROUTER_STRUCTURED_OUTPUTS` | OpenRouter-native structured outputs |

## Verified

Tested end-to-end with Cognee Python v1.4.0 + DeepSeek v4 Flash via OpenRouter:

```
15 graph extractions across 7 data points
7 nodes, 7 edges in knowledge graph
recall: 1 results across sources=['graph']
Zero tool_choice errors
```

Test input: `"Alice works at Acme Corp as a senior engineer."`
Recall output: `"Acme Corp."` (knowledge graph node with full context stored in graph edges)

## What Needs Updating

In `services.go` → `cogneeEnv()`:
- Add `LLM_INSTRUCTOR_MODE=json_mode`
- Add `EMBEDDING_DIMENSIONS=1024`
- Change `EMBEDDING_PROVIDER` from `openai` to `llama_cpp`

## References

- Cognee `OpenAIAdapter`: `cognee/infrastructure/llm/structured_output_framework/litellm_instructor/llm/openai/adapter.py`
- Instructor mode resolution: `instructor.Mode` enum values
- Cognee `LLMConfig`: `cognee/infrastructure/llm/config.py` (field `llm_instructor_mode`)
- Cognee `get_llm_client`: `cognee/infrastructure/llm/structured_output_framework/litellm_instructor/llm/get_llm_client.py` (line 170)
