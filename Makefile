VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run test fmt clean

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o mctl-agent ./cmd/agent

run:
	DRY_RUN=true DB_PATH=./mctl-agent.db PORT=8081 go run ./cmd/agent

test:
	go test ./...

fmt:
	goimports -w .

clean:
	rm -f mctl-agent mctl-agent.db
