package codex

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"

	"example.com/codex-chatgpt-bridge/internal/domain"
	"example.com/codex-chatgpt-bridge/internal/security"
)

type TaskRequest struct {
	TaskID     string
	Workspace  domain.Workspace
	Prompt     string
	ThreadID   string
	OnProgress func(string)
	OnThread   func(string, string) error
}

type TaskResult struct {
	ThreadID  string
	TurnID    string
	Status    domain.TaskStatus
	FinalText string
	Diff      string
	Error     string
}

type Gateway interface {
	Run(context.Context, TaskRequest) (TaskResult, error)
	Connected() bool
	Close() error
}

type Adapter struct {
	client    *Client
	mu        sync.Mutex
	runs      map[string]*activeRun
	maxOutput int
}

type activeRun struct {
	mu        sync.Mutex
	threadID  string
	turnID    string
	request   TaskRequest
	terminal  chan TaskResult
	text      string
	diff      string
	maxOutput int
	once      sync.Once
}

func NewAdapter(client *Client, maxOutput int) *Adapter {
	if maxOutput < 1 {
		maxOutput = 64_000
	}
	a := &Adapter{client: client, runs: make(map[string]*activeRun), maxOutput: maxOutput}
	client.SetNotificationHandler(a.onNotification)
	return a
}

func (a *Adapter) Connected() bool { return a.client.Connected() }

func (a *Adapter) AuthorizeExecution(ctx context.Context, _ domain.Workspace) error {
	if err := a.client.Start(ctx); err != nil || !a.client.SupportsPermissionProfiles() {
		return domain.ErrServiceUnavailable
	}
	return nil
}

func (a *Adapter) Run(ctx context.Context, request TaskRequest) (TaskResult, error) {
	if request.Workspace.RootPath == "" {
		return TaskResult{}, domain.ErrInvalidInput
	}
	if err := a.client.Start(ctx); err != nil {
		return TaskResult{}, err
	}
	if !a.client.SupportsPermissionProfiles() {
		return TaskResult{}, domain.ErrServiceUnavailable
	}
	if err := ctx.Err(); err != nil {
		return TaskResult{Status: domain.TaskInterrupted}, err
	}
	profile := security.CodexReadOnlyProfile(request.Workspace.RootPath)
	threadID := request.ThreadID
	if threadID == "" {
		var response map[string]any
		params := map[string]any{"cwd": request.Workspace.RootPath, "approvalPolicy": "never", "config": profile.AppServerConfig(), "serviceName": "codex-chatgpt-bridge"}
		if err := a.callStart("thread/start", params, &response); err != nil {
			return TaskResult{}, err
		}
		if err := validateEffectiveSandbox(response); err != nil {
			return TaskResult{}, err
		}
		threadID = nestedID(response, "thread")
		if threadID == "" {
			return TaskResult{}, errors.New("Codex App Server returned no thread ID")
		}
	} else {
		var response map[string]any
		params := map[string]any{"threadId": threadID, "cwd": request.Workspace.RootPath, "approvalPolicy": "never", "config": profile.AppServerConfig(), "excludeTurns": true}
		if err := a.callStart("thread/resume", params, &response); err != nil {
			return TaskResult{}, err
		}
		if err := validateEffectiveSandbox(response); err != nil {
			return TaskResult{}, err
		}
		if id := nestedID(response, "thread"); id != "" && id != threadID {
			return TaskResult{}, errors.New("Codex App Server resumed an unexpected thread")
		}
	}
	run := &activeRun{threadID: threadID, request: request, terminal: make(chan TaskResult, 1), maxOutput: a.maxOutput}
	a.mu.Lock()
	if _, exists := a.runs[threadID]; exists {
		a.mu.Unlock()
		return TaskResult{}, errors.New("Codex thread is already active")
	}
	a.runs[threadID] = run
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.runs, threadID); a.mu.Unlock() }()
	if request.OnThread != nil {
		if err := request.OnThread(threadID, ""); err != nil {
			return TaskResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return TaskResult{ThreadID: threadID, Status: domain.TaskInterrupted}, err
	}
	var turnResponse map[string]any
	params := map[string]any{"threadId": threadID, "cwd": request.Workspace.RootPath, "approvalPolicy": "never", "input": []map[string]string{{"type": "text", "text": request.Prompt}}}
	if err := a.callStart("turn/start", params, &turnResponse); err != nil {
		return TaskResult{}, err
	}
	run.setTurnID(nestedID(turnResponse, "turn"))
	turnID := run.getTurnID()
	if turnID == "" {
		return TaskResult{}, errors.New("Codex App Server returned no turn ID")
	}
	if request.OnThread != nil {
		if err := request.OnThread(threadID, turnID); err != nil {
			return TaskResult{}, err
		}
	}
	select {
	case result := <-run.terminal:
		return result, nil
	case <-ctx.Done():
		interruptCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.client.Call(interruptCtx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID}, nil)
		return run.snapshot(domain.TaskInterrupted, ctx.Err().Error()), ctx.Err()
	}
}

func validateEffectiveSandbox(response map[string]any) error {
	profile, ok := response["activePermissionProfile"].(map[string]any)
	if !ok || profile["id"] != "bridge-readonly" || profile["extends"] != ":read-only" {
		return errors.New("Codex App Server effective sandbox profile does not match bridge read-only policy")
	}
	sandbox, ok := response["sandbox"].(map[string]any)
	if !ok || sandbox["type"] != "readOnly" || sandbox["networkAccess"] != false {
		return errors.New("Codex App Server effective sandbox mode does not match bridge read-only policy")
	}
	return nil
}

func (a *Adapter) callStart(method string, params, output any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return a.client.Call(ctx, method, params, output)
}

func (a *Adapter) onNotification(method string, params map[string]any) {
	threadID := stringValue(params["threadId"])
	a.mu.Lock()
	run := a.runs[threadID]
	a.mu.Unlock()
	if run == nil {
		return
	}
	turnID := stringValue(params["turnId"])
	if turnID == "" {
		turnID = nestedID(params, "turn")
	}
	currentTurnID := run.getTurnID()
	if currentTurnID != "" && turnID != "" && currentTurnID != turnID {
		return
	}
	if currentTurnID == "" && turnID != "" {
		run.setTurnID(turnID)
	}
	switch method {
	case "turn/started":
		run.progress("Codex turn started")
	case "item/started":
		run.progress("Codex started a work item")
	case "item/completed":
		item, _ := params["item"].(map[string]any)
		if item["type"] == "commandExecution" {
			if output := stringValue(item["aggregatedOutput"]); output != "" {
				run.progress("Codex command output: " + bounded(output, 1_800))
			}
		}
	case "item/agentMessage/delta":
		if delta := stringValue(params["delta"]); delta != "" {
			run.appendText(delta)
			run.progress(delta)
		}
	case "item/plan/delta":
		if delta := stringValue(params["delta"]); delta != "" {
			run.progress(delta)
		}
	case "turn/diff/updated":
		run.setDiff(stringValue(params["diff"]))
	case "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		items, _ := turn["items"].([]any)
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok || item["type"] != "commandExecution" {
				continue
			}
			if output := stringValue(item["aggregatedOutput"]); output != "" {
				run.progress("Codex command output: " + bounded(output, 1_800))
			}
		}
		status := domain.TaskStatus(stringValue(turn["status"]))
		if status != domain.TaskSucceeded && status != domain.TaskFailed && status != domain.TaskInterrupted {
			return
		}
		result := run.snapshot(status, "")
		if result.FinalText == "" {
			result.FinalText = completedText(turn, run.maxOutput)
		}
		if errValue, ok := turn["error"].(map[string]any); ok {
			result.Error = stringValue(errValue["message"])
		}
		run.finish(result)
	case "item/commandExecution/outputDelta", "command/exec/outputDelta", "process/outputDelta":
		if output := commandOutput(params); output != "" {
			run.progress("Codex command output: " + bounded(output, 1_800))
		}
	case "rawResponseItem/completed":
		item, _ := params["item"].(map[string]any)
		itemType := stringValue(item["type"])
		if itemType != "" {
			run.progress("Codex App Server raw response item completed: " + itemType)
		}
		switch itemType {
		case "local_shell_call":
			if item["status"] == "completed" {
				run.progress("Codex local shell call completed")
			}
		case "function_call_output":
			if output := commandOutput(item); output != "" {
				run.progress("Codex command output: " + bounded(output, 1_800))
			}
		}
	}
}

func commandOutput(params map[string]any) string {
	if delta := stringValue(params["delta"]); delta != "" {
		return delta
	}
	if output := stringValue(params["output"]); output != "" {
		return output
	}
	if chunks, ok := params["output"].([]any); ok {
		var output strings.Builder
		for _, raw := range chunks {
			chunk, _ := raw.(map[string]any)
			output.WriteString(stringValue(chunk["text"]))
		}
		if output.Len() > 0 {
			return output.String()
		}
	}
	encoded := stringValue(params["deltaBase64"])
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	return strings.ToValidUTF8(string(decoded), "�")
}

func (r *activeRun) progress(value string) {
	if r.request.OnProgress != nil {
		r.request.OnProgress(bounded(value, 2_000))
	}
}
func (r *activeRun) appendText(value string) {
	r.mu.Lock()
	r.text = appendBounded(r.text, value, r.maxOutput)
	r.mu.Unlock()
}
func (r *activeRun) setTurnID(value string) { r.mu.Lock(); r.turnID = value; r.mu.Unlock() }
func (r *activeRun) getTurnID() string      { r.mu.Lock(); defer r.mu.Unlock(); return r.turnID }
func (r *activeRun) setDiff(value string) {
	r.mu.Lock()
	r.diff = bounded(value, r.maxOutput)
	r.mu.Unlock()
}
func (r *activeRun) snapshot(status domain.TaskStatus, message string) TaskResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return TaskResult{ThreadID: r.threadID, TurnID: r.turnID, Status: status, FinalText: r.text, Diff: r.diff, Error: message}
}
func (r *activeRun) finish(value TaskResult) { r.once.Do(func() { r.terminal <- value }) }

func (a *Adapter) Close() error { return a.client.Close() }
func nestedID(value map[string]any, key string) string {
	nested, ok := value[key].(map[string]any)
	if !ok {
		return ""
	}
	return stringValue(nested["id"])
}
func stringValue(value any) string { result, _ := value.(string); return result }
func bounded(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[len(value)-max:]
}
func appendBounded(existing, next string, max int) string {
	if len(next) >= max {
		return next[len(next)-max:]
	}
	if len(existing)+len(next) > max {
		existing = existing[len(existing)+len(next)-max:]
	}
	return existing + next
}

func completedText(turn map[string]any, max int) string {
	items, _ := turn["items"].([]any)
	var result strings.Builder
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "agentMessage" {
			continue
		}
		if text := stringValue(item["text"]); text != "" {
			result.WriteString(text)
			continue
		}
		parts, _ := item["content"].([]any)
		for _, part := range parts {
			if object, ok := part.(map[string]any); ok {
				result.WriteString(stringValue(object["text"]))
			}
		}
	}
	return bounded(result.String(), max)
}
