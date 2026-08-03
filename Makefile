LLAMA_VERSION ?= b10085
LLAMA_DIR := bin/llama
COGNEE_BINARY := bin/cognee-http-server

.PHONY: setup run build clean test stop vet download-llama download-models build-cognee

MODEL_DIR := model
EMBED_MODEL := $(MODEL_DIR)/qwen3-embedding-0.6b-Q8_0.gguf

# Hugging Face download URLs
EMBED_URL := https://huggingface.co/Qwen/Qwen3-Embedding-0.6B-GGUF/resolve/main/Qwen3-Embedding-0.6B-Q8_0.gguf

setup:
	@command -v go >/dev/null 2>&1 || { echo "Error: go is required but not installed."; exit 1; }
	@command -v cargo >/dev/null 2>&1 || { echo "Error: cargo is required but not installed. Install from https://rustup.rs"; exit 1; }
	git submodule update --init --recursive
	$(MAKE) download-llama
	$(MAKE) download-models
	$(MAKE) build-cognee

run:
	@if [ ! -x $(LLAMA_DIR)/llama-server ]; then \
		echo "Hint: run 'make setup' to download llama-server, or install it system-wide and ensure it's on PATH."; \
	fi
	@if [ ! -x $(COGNEE_BINARY) ]; then \
		echo "Hint: run 'make build-cognee' to build the Cognee Rust binary."; \
	fi
	go run .

build:
	go build -o bin/mcp-memory .

build-cognee:
	@if [ ! -d cognee-rs ]; then \
		echo "cognee-rs submodule not found. Run: git submodule update --init --recursive"; \
		exit 1; \
	fi
	cd cognee-rs && cargo build --release -p cognee-http-server --features bin
	cp cognee-rs/target/release/cognee-http-server $(COGNEE_BINARY)
	@echo "Cognee Rust binary built: $(COGNEE_BINARY)"

clean:
	rm -rf bin/mcp-memory $(COGNEE_BINARY) $(LLAMA_DIR)
	cd cognee-rs && cargo clean 2>/dev/null || true

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

stop:
	./scripts/stop.sh
