LLAMA_VERSION ?= b10085

.PHONY: setup run build clean test stop vet download-llama

setup:
	@command -v python3 >/dev/null 2>&1 || { echo "Error: python3 is required but not installed."; exit 1; }
	@test -d .venv || python3 -m venv .venv
	.venv/bin/pip install hindsight-api-slim==0.8.2 && \
	.venv/bin/pip install hindsight-client==0.8.2
	@$(MAKE) download-llama

run:
	@if [ ! -x vendor/bin/llama-server ]; then \
		echo "Hint: run 'make setup' to download llama-server, or install it system-wide and ensure it's on PATH."; \
	fi
	go run .

build:
	go build -o bin/mcp-memory .

clean:
	rm -rf .venv bin/mcp-memory mcp-memory vendor/bin/llama-server

test:
	go test -race -count=1 -timeout 240s ./...

vet:
	go vet ./...

download-llama:
	@if [ -x vendor/bin/llama-server ]; then \
		echo "llama-server already downloaded."; \
		exit 0; \
	fi
	@echo "Downloading llama-server $(LLAMA_VERSION)..."
	@case $$(uname -s) in \
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
	curl -fSL --connect-timeout 30 --max-time 300 "$${URL}" | tar xz -C "$${TMPDIR}"; \
	mkdir -p vendor/bin; \
	mv "$${TMPDIR}/build/bin/llama-server" vendor/bin/llama-server; \
	chmod +x vendor/bin/llama-server; \
	rm -rf "$${TMPDIR}"; \
	echo "llama-server $(LLAMA_VERSION) downloaded to vendor/bin/llama-server."

stop:
	./scripts/stop.sh
