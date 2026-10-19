package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/codex-chatgpt-bridge/internal/domain"
)

func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJSONRPCParserRejectsMalformedFrames(t *testing.T) {
	for _, line := range []string{"{", `{"jsonrpc":"1.0","method":"initialize"}`, `{"jsonrpc":"2.0"}`} {
		if _, err := parseMessage([]byte(line)); err == nil {
			t.Errorf("accepted malformed frame %q", line)
		}
	}
	if _, err := parseMessage([]byte(`{"id":2,"result":null}`)); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
}

func TestPermissionProfileVersionGate(t *testing.T) {
	if got := appServerVersion("codex-chatgpt-bridge/0.163.0 (macOS)"); got != "0.163.0" {
		t.Fatalf("app-server version=%q", got)
	}
	for version, want := range map[string]bool{"0.137.9": false, "0.138.0": true, "0.163.0": true, "1.0.0": true, "unknown": false, "0.163.0-beta": false} {
		if got := supportsPermissionProfiles(version); got != want {
			t.Errorf("supportsPermissionProfiles(%q)=%t, want %t", version, got, want)
		}
	}
}

func TestConcurrentStartUsesSingleAppServerProcess(t *testing.T) {
	launches := filepath.Join(t.TempDir(), "launches")
	body := `printf '%s\n' started >> '` + launches + `'
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=$(printf '%s' "$line" | sed -E 's/.*"id":([0-9]+).*/\1/')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"codex-fixture/0.163.0"}}\n' "$id" ;;
  esac
done
`
	client := NewClient(fakeCodex(t, body), slog.New(slog.NewTextHandler(io.Discard, nil)))
	start := make(chan struct{})
	errs := make(chan error, 16)
	for range 16 {
		go func() {
			<-start
			errs <- client.Start(context.Background())
		}()
	}
	close(start)
	for range 16 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent App Server start timed out")
		}
	}
	contents, err := os.ReadFile(launches)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(contents), "started"); got != 1 {
		t.Fatalf("started %d App Server processes, want 1", got)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterUsesReadOnlyPolicyAndReceivesRealProtocolEvents(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "protocol.jsonl")
	body := `capture='` + capture + `'
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$capture"
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"codex-fixture/0.163.0 (macOS)"}}' ;;
    *'"method":"thread/start"'*) printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-fixture"}}}' ;;
    *'"method":"turn/start"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-fixture"}}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"thread-fixture","turnId":"turn-fixture","delta":"BRIDGE_MCP_CODEX_TURN_OK"}}'
      printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-fixture","turn":{"id":"turn-fixture","status":"completed","items":[]}}}' ;;
  esac
done
`
	client := NewClient(fakeCodex(t, body), slog.New(slog.NewTextHandler(io.Discard, nil)))
	adapter := NewAdapter(client, 64_000)
	defer adapter.Close()
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	var threads []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := adapter.Run(ctx, TaskRequest{TaskID: "task-fixture", Workspace: domain.Workspace{ID: "fixture", RootPath: root}, Prompt: "Read fixture", OnThread: func(thread, turn string) error { threads = append(threads, thread+"/"+turn); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.TaskSucceeded || !strings.Contains(result.FinalText, "BRIDGE_MCP_CODEX_TURN_OK") {
		t.Fatalf("bad task result: %+v", result)
	}
	if len(threads) != 2 || threads[0] != "thread-fixture/" || threads[1] != "thread-fixture/turn-fixture" {
		t.Fatalf("thread callbacks: %v", threads)
	}
	lines, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var start, turn map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(lines)), "\n") {
		var request map[string]any
		if json.Unmarshal([]byte(line), &request) != nil {
			continue
		}
		if request["method"] == "thread/start" {
			start = request
		}
		if request["method"] == "turn/start" {
			turn = request
		}
	}
	config, ok := start["params"].(map[string]any)["config"].(map[string]any)
	if !ok {
		t.Fatalf("missing permission profile: %s", lines)
	}
	if config["default_permissions"] != "bridge-readonly" || config["web_search"] != "disabled" {
		t.Fatalf("unsafe profile: %#v", config)
	}
	if _, ok := turn["params"].(map[string]any)["sandboxPolicy"]; ok {
		t.Fatalf("turn combined sandboxPolicy with the selected permission profile: %#v", turn)
	}
}

func TestCancellationDuringTurnStartStillInterruptsStartedTurn(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "protocol.jsonl")
	body := `capture='` + capture + `'
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$capture"
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"codex-fixture/0.163.0 (macOS)"}}' ;;
    *'"method":"thread/start"'*) printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thread-fixture"}}}' ;;
    *'"method":"turn/start"'*) sleep 0.2; printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"turn-fixture"}}}' ;;
    *'"method":"turn/interrupt"'*)
      id=$(printf '%s' "$line" | sed -E 's/.*"id":([0-9]+).*/\1/')
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      printf '%s\n' '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread-fixture","turn":{"id":"turn-fixture","status":"interrupted"}}}' ;;
  esac
done
`
	client := NewClient(fakeCodex(t, body), slog.New(slog.NewTextHandler(io.Discard, nil)))
	adapter := NewAdapter(client, 1024)
	defer adapter.Close()
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan TaskResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := adapter.Run(ctx, TaskRequest{Workspace: domain.Workspace{ID: "fixture", RootPath: root}, Prompt: "cancel me"})
		resultCh <- result
		errCh <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		contents, _ := os.ReadFile(capture)
		if strings.Contains(string(contents), `"method":"turn/start"`) {
			cancel()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case result := <-resultCh:
		if result.Status != domain.TaskInterrupted {
			t.Fatalf("cancelled task status=%s", result.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn cancellation timed out")
	}
	if err := <-errCh; err == nil {
		t.Fatal("cancelled turn returned no cancellation error")
	}
	contents, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `"method":"turn/interrupt"`) {
		t.Fatalf("turn/start cancellation left a live turn: %s", contents)
	}
}

func TestClientCorrelatesConcurrentRequestsAndDeniesRawMethods(t *testing.T) {
	body := `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}' ;;
    *'"method":"thread/start"'*)
      first="$line"
      IFS= read -r second
      id1=$(printf '%s' "$first" | sed -E 's/.*"id":([0-9]+).*/\1/')
      marker1=$(printf '%s' "$first" | sed -E 's/.*"marker":"([^"]+)".*/\1/')
      id2=$(printf '%s' "$second" | sed -E 's/.*"id":([0-9]+).*/\1/')
      marker2=$(printf '%s' "$second" | sed -E 's/.*"marker":"([^"]+)".*/\1/')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"%s"}}}\n' "$id2" "$marker2"
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"%s"}}}\n' "$id1" "$marker1" ;;
  esac
done
`
	client := NewClient(fakeCodex(t, body), slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "process/spawn", map[string]any{}, nil); err == nil {
		t.Fatal("raw App Server method was allowed")
	}
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for _, marker := range []string{"first", "second"} {
		go func(marker string) {
			var response map[string]any
			err := client.Call(ctx, "thread/start", map[string]string{"marker": marker}, &response)
			if err != nil {
				errs <- err
				return
			}
			thread := response["thread"].(map[string]any)["id"].(string)
			results <- thread
		}(marker)
	}
	got := map[string]bool{}
	for range 2 {
		select {
		case err := <-errs:
			t.Fatal(err)
		case value := <-results:
			got[value] = true
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if !got["first"] || !got["second"] {
		t.Fatalf("responses mismatched: %#v", got)
	}
}

func TestClientMalformedServerFrameFailsClosed(t *testing.T) {
	body := `printf '%s\n' '{not-json}'
sleep 1
`
	client := NewClient(fakeCodex(t, body), slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Start(ctx); err == nil {
		t.Fatal("malformed JSON-RPC did not fail initialization")
	}
}

func TestClientRequestTimeoutRemovesPendingRequest(t *testing.T) {
	body := `while IFS= read -r line; do case "$line" in *'"method":"initialize"'*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}' ;; esac; done
`
	client := NewClient(fakeCodex(t, body), slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer client.Close()
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := client.Call(ctx, "thread/start", map[string]string{"cwd": "fixture"}, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending requests leaked: %d", pending)
	}
}
