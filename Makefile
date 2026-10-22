.PHONY: build test test-race vet run test-e2e test-write-e2e test-session-catalog-e2e

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

test-write-e2e:
	BRIDGE_WRITE_E2E=1 go test ./tests/e2e -run '^TestRealCodexWorkspaceWriteApprovalThroughMCP$$' -count=1 -v

test-session-catalog-e2e:
	BRIDGE_SESSION_CATALOG_E2E=1 go test ./tests/e2e -run '^TestGlobalCatalogSwitchesDisposableCLISessionsAcrossWorkspaces$$' -count=1 -v
