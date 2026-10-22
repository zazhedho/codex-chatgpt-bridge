package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// deterministicResponsesProvider drives a real local App Server without a model account.
type deterministicResponsesProvider struct {
	server *httptest.Server

	mu               sync.Mutex
	contextMarkers   map[string]bool
	responseContexts map[string]string
	probeID          string
	probeCommand     string
	probeSent        bool
	toolNames        map[string]bool
	lastError        string
	sequence         atomic.Uint64
}

func setupDeterministicCodexCLI(t *testing.T, base string) (string, []string, *deterministicResponsesProvider) {
	t.Helper()
	codexPath := os.Getenv("CODEX_EXECUTABLE")
	if codexPath == "" {
		codexPath = "codex"
	}
	var err error
	codexPath, err = exec.LookPath(codexPath)
	if err != nil {
		t.Fatal("Codex CLI is unavailable")
	}
	if resolved, resolveErr := filepath.EvalSymlinks(codexPath); resolveErr == nil {
		codexPath = resolved
	}
	provider := newDeterministicResponsesProvider()
	t.Cleanup(provider.server.Close)

	codexHome := filepath.Join(base, "codex-home")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatal("create isolated Codex home")
	}
	config := fmt.Sprintf(`model = "mock-model"
approval_policy = "never"
sandbox_mode = "read-only"
model_provider = "bridge_test"

[model_providers.bridge_test]
name = "Local deterministic Bridge test provider"
base_url = "%s/v1"
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
`, provider.server.URL)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal("write isolated Codex test configuration")
	}
	t.Setenv("CODEX_HOME", codexHome)
	return codexPath, codexTestEnvironment(codexHome), provider
}

func codexTestEnvironment(codexHome string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			env = append(env, entry)
		}
	}
	return append(env, "CODEX_HOME="+codexHome)
}

func newDeterministicResponsesProvider() *deterministicResponsesProvider {
	provider := &deterministicResponsesProvider{
		contextMarkers:   make(map[string]bool),
		responseContexts: make(map[string]string),
		toolNames:        make(map[string]bool),
	}
	provider.server = httptest.NewServer(http.HandlerFunc(provider.serveHTTP))
	return provider
}

func (p *deterministicResponsesProvider) queueProbe(id, command string) {
	p.mu.Lock()
	p.probeID, p.probeCommand, p.probeSent = id, command, false
	p.mu.Unlock()
}

func (p *deterministicResponsesProvider) sawExecCommand() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.toolNames["exec_command"]
}

func (p *deterministicResponsesProvider) error() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastError
}

func (p *deterministicResponsesProvider) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "mock-model", "object": "model", "created": 0, "owned_by": "local-test"}}})
		return
	case r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses"):
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		http.Error(w, "invalid test request", http.StatusBadRequest)
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "invalid test request", http.StatusBadRequest)
		return
	}
	inputText := responseInputText(request["input"])
	responseID := fmt.Sprintf("bridge-test-response-%d", p.sequence.Add(1))

	p.mu.Lock()
	contextMarker := p.findContext(inputText)
	if contextMarker == "" {
		if previousID, _ := request["previous_response_id"].(string); previousID != "" {
			contextMarker = p.responseContexts[previousID]
		}
	}
	for name := range responseToolNames(request["tools"]) {
		p.toolNames[name] = true
	}
	isProbe := p.probeID != "" && strings.Contains(inputText, "ROOT_SCOPE_PROBE:"+p.probeID)
	probeCommand := p.probeCommand
	sendProbe := isProbe && !p.probeSent
	if sendProbe {
		if !p.toolNames["exec_command"] {
			p.lastError = "Codex request did not expose exec_command"
			p.mu.Unlock()
			http.Error(w, "required test tool unavailable", http.StatusBadGateway)
			return
		}
		p.probeSent = true
	}
	if contextMarker != "" {
		p.responseContexts[responseID] = contextMarker
	}
	p.mu.Unlock()

	events := []map[string]any{{"type": "response.created", "response": map[string]any{"id": responseID}}}
	if sendProbe {
		arguments, _ := json.Marshal(map[string]any{"cmd": probeCommand, "yield_time_ms": 1000})
		events = append(events, map[string]any{"type": "response.output_item.done", "item": map[string]any{
			"type": "function_call", "call_id": "bridge-root-scope-probe", "name": "exec_command", "arguments": string(arguments),
		}})
	} else {
		text := "deterministic test response"
		if contextMarker != "" {
			text = "SESSION_CONTEXT " + contextMarker
		} else if isProbe {
			text = "ROOT_SCOPE_PROBE_COMPLETED"
		}
		events = append(events, map[string]any{"type": "response.output_item.done", "item": map[string]any{
			"type": "message", "role": "assistant", "id": responseID + "-message", "content": []any{map[string]any{"type": "output_text", "text": text}},
		}})
	}
	events = append(events, map[string]any{"type": "response.completed", "response": map[string]any{
		"id": responseID, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2, "input_tokens_details": nil, "output_tokens_details": nil},
	}})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for _, event := range events {
		encoded, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
	}
}

func (p *deterministicResponsesProvider) findContext(input string) string {
	for marker := range p.contextMarkers {
		if strings.Contains(input, marker) {
			return marker
		}
	}
	if marker := markerWithPrefix(input, "ROOT_SCOPE_CONTEXT_"); marker != "" {
		p.contextMarkers[marker] = true
		return marker
	}
	return ""
}

func responseInputText(value any) string {
	var parts []string
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if value["type"] == "input_text" || value["type"] == "output_text" {
				if text, ok := value["text"].(string); ok {
					parts = append(parts, text)
				}
			}
			for _, child := range value {
				visit(child)
			}
		case []any:
			for _, child := range value {
				visit(child)
			}
		}
	}
	visit(value)
	return strings.Join(parts, "\n")
}

func markerWithPrefix(text, prefix string) string {
	start := strings.Index(text, prefix)
	if start < 0 {
		return ""
	}
	marker := strings.Fields(text[start:])[0]
	return strings.TrimRight(marker, ".,;:!?)\"'")
}

func responseToolNames(value any) map[string]bool {
	names := make(map[string]bool)
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if name, ok := value["name"].(string); ok {
				names[name] = true
			}
			for _, child := range value {
				visit(child)
			}
		case []any:
			for _, child := range value {
				visit(child)
			}
		}
	}
	visit(value)
	return names
}
