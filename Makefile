.PHONY: setup run build clean test stop vet

setup:
	@command -v python3 >/dev/null 2>&1 || { echo "Error: python3 is required but not installed."; exit 1; }
	@test -d .venv || python3 -m venv .venv
	.venv/bin/pip install hindsight-api-slim==0.8.2 && \
	.venv/bin/pip install hindsight-client==0.8.2

run:
	@echo "Hint: run 'make setup' first if you haven't installed hindsight yet."
	go run .

build:
	go build -o bin/mcp-memory .

clean:
	rm -rf .venv bin/mcp-memory mcp-memory

test:
	go test -race -count=1 -timeout 240s ./...

vet:
	go vet ./...

stop:
	./scripts/stop.sh
