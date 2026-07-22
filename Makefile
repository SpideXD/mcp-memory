LLAMA_VERSION ?= b10085
LLAMA_DIR := bin/llama

.PHONY: setup run build clean test stop vet download-llama download-models

MODEL_DIR := model
EMBED_MODEL := $(MODEL_DIR)/qwen3-embedding-0.6b-Q8_0.gguf
RERANK_MODEL := $(MODEL_DIR)/bge-reranker-base-Q4_k_m.gguf

# Hugging Face download URLs
EMBED_URL := https://huggingface.co/Qwen/Qwen3-Embedding-0.6B-GGUF/resolve/main/Qwen3-Embedding-0.6B-Q8_0.gguf
RERANK_URL := https://huggingface.co/sinjab/bge-reranker-base-Q4_K_M-GGUF/resolve/main/bge-reranker-base-Q4_K_M.gguf

setup:
	@command -v python3 >/dev/null 2>&1 || { echo "Error: python3 is required but not installed."; exit 1; }
	@test -d .venv || python3 -m venv .venv
	.venv/bin/pip install hindsight-api-slim==0.8.2 && \
	.venv/bin/pip install hindsight-client==0.8.2
	@$(MAKE) download-llama
	@$(MAKE) download-models

run:
	@if [ ! -x $(LLAMA_DIR)/llama-server ]; then \
		echo "Hint: run 'make setup' to download llama-server, or install it system-wide and ensure it's on PATH."; \
	fi
	go run .

build:
	go build -o bin/mcp-memory .

clean:
	rm -rf .venv bin/mcp-memory mcp-memory $(LLAMA_DIR)

test:
	go test -race -count=1 -timeout 240s ./...

vet:
	go vet ./...

download-llama:
	@set -eo pipefail; \
	if [ -x $(LLAMA_DIR)/llama-server ]; then \
		echo "llama-server already downloaded."; \
		exit 0; \
	fi; \
	echo "Downloading llama-server $(LLAMA_VERSION)..."; \
	case $$(uname -s) in \
		Darwin) OSNAME=macos ;; \
		Linux)  OSNAME=ubuntu ;; \
		*)      echo "Unsupported platform: $$(uname -s). Install llama-server manually or set LLAMA_PATH."; exit 1 ;; \
	esac; \
	case $$(uname -m) in \
		arm64|aarch64) ARCH=arm64 ;; \
		x86_64)         ARCH=x64 ;; \
		*)              echo "Unsupported architecture: $$(uname -m). Install llama-server manually or set LLAMA_PATH."; exit 1 ;; \
	esac; \
	PLATFORM="$${OSNAME}-$${ARCH}"; \
	URL="https://github.com/ggml-org/llama.cpp/releases/download/$(LLAMA_VERSION)/llama-$(LLAMA_VERSION)-bin-$${PLATFORM}.tar.gz"; \
	TMPDIR=$$(mktemp -d /tmp/llama-download-XXXXXX); \
	curl -fSL --connect-timeout 30 --max-time 300 "$${URL}" | tar xz --strip-components=1 -C "$${TMPDIR}"; \
	mkdir -p $(LLAMA_DIR); \
	mv "$${TMPDIR}"/* $(LLAMA_DIR)/; \
	chmod +x $(LLAMA_DIR)/llama-server; \
	rm -rf "$${TMPDIR}"; \
	echo "llama-server $(LLAMA_VERSION) downloaded to $(LLAMA_DIR)/llama-server."

download-models:
	@mkdir -p $(MODEL_DIR)
	@if [ ! -f $(EMBED_MODEL) ]; then \
		echo "Downloading embedding model (610MB)..."; \
		curl -fSL --connect-timeout 30 --max-time 900 -o $(EMBED_MODEL) "$(EMBED_URL)"; \
		echo "Embedding model downloaded."; \
	else \
		echo "Embedding model already present: $(EMBED_MODEL)"; \
	fi
	@if [ ! -f $(RERANK_MODEL) ]; then \
		echo "Downloading reranker model (209MB)..."; \
		curl -fSL --connect-timeout 30 --max-time 600 -o $(RERANK_MODEL) "$(RERANK_URL)"; \
		echo "Reranker model downloaded."; \
	else \
		echo "Reranker model already present: $(RERANK_MODEL)"; \
	fi

stop:
	./scripts/stop.sh
