.PHONY: build test test-race vet run test-e2e

build:
	mkdir -p bin
	go build -o bin/bridge ./cmd/bridge
	go build -o bin/bridge-admin ./cmd/bridge-admin

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

run:
	go run ./cmd/bridge

test-e2e:
	BRIDGE_E2E=1 go test ./tests/e2e -run TestRealCodexThroughMCPAndSandbox -count=1 -v
