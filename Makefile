.PHONY: setup run build clean test stop

setup:
	python3 -m venv .venv
	.venv/bin/pip install hindsight-api-slim==0.8.2 hindsight-client==0.8.2

run:
	go run .

build:
	go build -o bin/mcp-memory .

clean:
	rm -rf .venv bin/mcp-memory mcp-memory

test:
	go test -race -count=1 -timeout 240s ./...

stop:
	./scripts/stop.sh
