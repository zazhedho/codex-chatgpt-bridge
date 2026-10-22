package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/domain"
)

const maxProtocolLine = 1 << 20

type Client struct {
	executable     string
	logger         *slog.Logger
	startMu        sync.Mutex
	mu             sync.Mutex
	writeMu        sync.Mutex
	cmd            *exec.Cmd
	executablePath string
	processDone    chan struct{}
	serverVersion  string
	stdin          io.WriteCloser
	pending        map[string]chan response
	notify         func(string, map[string]any)
	nextID         atomic.Uint64
	connected      bool
	done           chan struct{}
	closeOnce      sync.Once
}

type response struct {
	result json.RawMessage
	err    error
}

func NewClient(executable string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{executable: executable, logger: logger, pending: make(map[string]chan response)}
}

func (c *Client) Connected() bool        { c.mu.Lock(); defer c.mu.Unlock(); return c.connected }
func (c *Client) ExecutablePath() string { c.mu.Lock(); defer c.mu.Unlock(); return c.executablePath }
func (c *Client) SupportsPermissionProfiles() bool {
	c.mu.Lock()
	version := c.serverVersion
	c.mu.Unlock()
	return supportsPermissionProfiles(version)
}

func (c *Client) SetNotificationHandler(handler func(string, map[string]any)) {
	c.mu.Lock()
	c.notify = handler
	c.mu.Unlock()
}

func (c *Client) Start(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	return c.start(ctx)
}

func (c *Client) start(ctx context.Context) error {
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}
	if c.cmd != nil {
		c.mu.Unlock()
		return errors.New("Codex App Server is stopping")
	}
	c.mu.Unlock()
	path, err := exec.LookPath(c.executable)
	if err != nil {
		return errors.New("Codex executable is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}
	cmd := exec.Command(path, "app-server", "--stdio")
	cmd.Env = safeEnvironment(os.Environ())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return errors.New("Codex App Server stdin unavailable")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("Codex App Server stdout unavailable")
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return errors.New("Codex App Server stderr unavailable")
	}
	if err := cmd.Start(); err != nil {
		return errors.New("failed to start Codex App Server")
	}
	c.mu.Lock()
	c.cmd, c.stdin, c.done, c.processDone, c.connected, c.closeOnce = cmd, stdin, make(chan struct{}), make(chan struct{}), false, sync.Once{}
	c.executablePath = path
	done := c.done
	processDone := c.processDone
	c.mu.Unlock()
	go c.readLoop(stdout, done)
	go c.logStderr(stderr, done)
	go func() {
		err := cmd.Wait()
		close(processDone)
		c.disconnect(fmt.Errorf("Codex App Server exited: %w", err))
	}()
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var initialized map[string]any
	if err := c.call(initCtx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "codex-chatgpt-bridge", "title": "Codex ChatGPT Bridge", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &initialized); err != nil {
		_ = c.close()
		return fmt.Errorf("initialize Codex App Server: %w", err)
	}
	c.mu.Lock()
	c.serverVersion = appServerVersion(initialized["userAgent"])
	c.mu.Unlock()
	if err := c.notifyMessage("initialized", nil); err != nil {
		_ = c.close()
		return err
	}
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
	c.logger.Info("Codex App Server connected")
	return nil
}

func (c *Client) Call(ctx context.Context, method string, params any, output any) error {
	switch method {
	case "thread/start", "thread/resume", "thread/list", "thread/read", "turn/start", "turn/interrupt":
	default:
		return errors.New("Codex App Server method is not allowed")
	}
	return c.call(ctx, method, params, output)
}

// ListCLISessions uses the App Server catalog, scoped to one exact workspace path.
func (c *Client) ListCLISessions(ctx context.Context, workspacePath string) ([]domain.CodexSession, error) {
	var sessions []domain.CodexSession
	seen := make(map[string]struct{})
	var cursor string
	for page := 0; page < 10; page++ {
		result, err := c.ListCLISessionsPage(ctx, workspacePath, "", cursor, 100)
		if err != nil {
			return nil, err
		}
		for _, session := range result.Sessions {
			if _, duplicate := seen[session.ID]; duplicate {
				return nil, errors.New("Codex App Server repeated a thread across pages")
			}
			seen[session.ID] = struct{}{}
			sessions = append(sessions, session)
		}
		next := result.NextCursor
		if next == "" {
			return sessions, nil
		}
		if next == cursor {
			return nil, errors.New("Codex App Server repeated a thread list cursor")
		}
		cursor = next
	}
	return nil, errors.New("Codex App Server thread list exceeds the local discovery limit")
}

// ListCLISessionsPage returns only metadata from the installed App Server's thread/list API.
func (c *Client) ListCLISessionsPage(ctx context.Context, workspacePath, search, cursor string, limit int) (domain.CodexSessionPage, error) {
	if (workspacePath != "" && (!filepath.IsAbs(workspacePath) || len(workspacePath) > 4096)) || len(search) > 200 || len(cursor) > 8192 || limit < 1 || limit > 100 {
		return domain.CodexSessionPage{}, domain.ErrInvalidInput
	}
	if err := c.Start(ctx); err != nil {
		return domain.CodexSessionPage{}, err
	}
	params := map[string]any{
		"archived": false, "sourceKinds": []string{"cli", "vscode", "exec"},
		"sortKey": "updated_at", "sortDirection": "desc", "limit": limit, "useStateDbOnly": true,
	}
	if workspacePath != "" {
		params["cwd"] = []string{filepath.Clean(workspacePath)}
	}
	if search != "" {
		params["searchTerm"] = search
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var response map[string]any
	if err := c.Call(ctx, "thread/list", params, &response); err != nil {
		return domain.CodexSessionPage{}, err
	}
	data, ok := response["data"].([]any)
	if !ok {
		return domain.CodexSessionPage{}, errors.New("invalid Codex App Server thread list")
	}
	page := domain.CodexSessionPage{Sessions: make([]domain.CodexSession, 0, len(data))}
	for _, raw := range data {
		value, ok := raw.(map[string]any)
		if !ok {
			return domain.CodexSessionPage{}, errors.New("invalid Codex App Server thread metadata")
		}
		session := sessionFromAppServer(value)
		if session.ID == "" || session.Source == "" || session.WorkspacePath == "" {
			return domain.CodexSessionPage{}, errors.New("incomplete Codex App Server thread metadata")
		}
		if !session.IsCodexCLISession() || (workspacePath != "" && filepath.Clean(session.WorkspacePath) != filepath.Clean(workspacePath)) {
			continue
		}
		page.Sessions = append(page.Sessions, session)
	}
	page.NextCursor, _ = response["nextCursor"].(string)
	if len(page.NextCursor) > 8192 {
		return domain.CodexSessionPage{}, errors.New("invalid Codex App Server thread list cursor")
	}
	return page, nil
}

// ReadSessionMetadata requests thread metadata only; persisted turns stay inside Codex.
func (c *Client) ReadSessionMetadata(ctx context.Context, threadID string) (domain.CodexSession, error) {
	if threadID == "" || len(threadID) > 256 || strings.TrimSpace(threadID) != threadID {
		return domain.CodexSession{}, domain.ErrInvalidInput
	}
	if err := c.Start(ctx); err != nil {
		return domain.CodexSession{}, err
	}
	var response map[string]any
	if err := c.Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &response); err != nil {
		return domain.CodexSession{}, err
	}
	thread, ok := response["thread"].(map[string]any)
	if !ok {
		return domain.CodexSession{}, errors.New("invalid Codex App Server thread metadata")
	}
	session := sessionFromAppServer(thread)
	if session.ID != threadID || session.Source == "" || session.WorkspacePath == "" {
		return domain.CodexSession{}, errors.New("incomplete Codex App Server thread metadata")
	}
	return session, nil
}

func sessionFromAppServer(value map[string]any) domain.CodexSession {
	return domain.CodexSession{
		ID:            stringValue(value["id"]),
		WorkspacePath: stringValue(value["cwd"]),
		Source:        sessionSource(value["source"]),
		Originator:    stringValue(value["originator"]),
		Title:         stringValue(value["name"]),
		Preview:       stringValue(value["preview"]),
		Status:        sessionStatus(value["status"]),
		CreatedAt:     unixTime(value["createdAt"]),
		UpdatedAt:     unixTime(value["updatedAt"]),
	}
}

func sessionSource(value any) string {
	if source, ok := value.(string); ok {
		return source
	}
	if source, ok := value.(map[string]any); ok {
		if custom, ok := source["custom"].(string); ok {
			return "custom:" + custom
		}
	}
	return ""
}

func sessionStatus(value any) string {
	if status, ok := value.(string); ok {
		return status
	}
	if status, ok := value.(map[string]any); ok {
		return stringValue(status["type"])
	}
	return ""
}

func unixTime(value any) time.Time {
	seconds, ok := value.(float64)
	if !ok || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(seconds), 0).UTC()
}

func (c *Client) call(ctx context.Context, method string, params any, output any) error {
	c.mu.Lock()
	connected, done := c.connected, c.done
	c.mu.Unlock()
	// initialize is the only request permitted before the handshake completes.
	if !connected && method != "initialize" {
		return errors.New("Codex App Server is disconnected")
	}
	id := c.nextID.Add(1)
	idText := fmt.Sprint(id)
	resultCh := make(chan response, 1)
	c.mu.Lock()
	if c.pending == nil {
		c.pending = make(map[string]chan response)
	}
	c.pending[idText] = resultCh
	c.mu.Unlock()
	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := c.writeMessage(message); err != nil {
		c.removePending(idText)
		return err
	}
	select {
	case response := <-resultCh:
		if response.err != nil {
			return response.err
		}
		if output == nil {
			return nil
		}
		if err := json.Unmarshal(response.result, output); err != nil {
			return errors.New("invalid Codex App Server response")
		}
		return nil
	case <-ctx.Done():
		c.removePending(idText)
		return ctx.Err()
	case <-done:
		c.removePending(idText)
		return errors.New("Codex App Server disconnected")
	}
}

func (c *Client) notifyMessage(method string, params any) error {
	if params == nil {
		params = map[string]any{}
	}
	return c.writeMessage(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) writeMessage(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > maxProtocolLine {
		return errors.New("Codex App Server message too large")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()
	if stdin == nil {
		return errors.New("Codex App Server is disconnected")
	}
	encoded = append(encoded, '\n')
	if _, err := stdin.Write(encoded); err != nil {
		c.disconnect(errors.New("Codex App Server write failed"))
		return errors.New("Codex App Server write failed")
	}
	return nil
}

func (c *Client) readLoop(reader io.Reader, done <-chan struct{}) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxProtocolLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		msg, err := parseMessage(line)
		if err != nil {
			c.disconnect(errors.New("Codex App Server sent malformed JSON-RPC"))
			return
		}
		if msg.Method != "" {
			params := map[string]any{}
			if len(msg.Params) > 0 {
				if err := json.Unmarshal(msg.Params, &params); err != nil {
					c.disconnect(errors.New("Codex App Server sent malformed notification"))
					return
				}
			}
			if len(msg.ID) > 0 {
				c.denyServerRequest(msg)
				continue
			}
			c.mu.Lock()
			handler := c.notify
			c.mu.Unlock()
			if handler != nil {
				handler(msg.Method, params)
			}
			continue
		}
		key := strings.TrimSpace(string(msg.ID))
		c.mu.Lock()
		pending := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if pending == nil {
			continue
		}
		if msg.Error != nil {
			pending <- response{err: fmt.Errorf("Codex App Server error %d", msg.Error.Code)}
		} else {
			pending <- response{result: msg.Result}
		}
	}
	if err := scanner.Err(); err != nil {
		c.disconnect(errors.New("Codex App Server message read failed"))
		return
	}
	select {
	case <-done:
	default:
		c.disconnect(io.EOF)
	}
}

func (c *Client) logStderr(reader io.Reader, done <-chan struct{}) {
	buffer := make([]byte, 4096)
	for {
		_, err := reader.Read(buffer)
		if err != nil {
			return
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func (c *Client) denyServerRequest(msg message) {
	var decision any
	switch msg.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		decision = map[string]string{"decision": "decline"}
	case "applyPatchApproval", "execCommandApproval":
		decision = map[string]string{"decision": "abort"}
	}
	if decision != nil {
		if err := c.writeMessage(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "result": decision}); err != nil {
			c.logger.Warn("Denied Codex approval request could not be answered")
		}
		return
	}
	_ = c.writeMessage(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "error": map[string]any{"code": -32000, "message": "Bridge denies unhandled App Server requests"}})
}

func (c *Client) removePending(id string) { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }

func (c *Client) disconnect(reason error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		cmd := c.cmd
		c.connected = false
		if c.done != nil {
			close(c.done)
		}
		c.stdin = nil
		c.cmd = nil
		pending := c.pending
		c.pending = make(map[string]chan response)
		c.mu.Unlock()
		if cmd != nil && cmd.Process != nil && reason != nil && !errors.Is(reason, io.EOF) {
			_ = cmd.Process.Kill()
		}
		for _, ch := range pending {
			ch <- response{err: errors.New("Codex App Server disconnected")}
		}
		if reason != nil && !errors.Is(reason, io.EOF) {
			c.logger.Warn("Codex App Server disconnected", "error", reason.Error())
		}
	})
}

func (c *Client) Close() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	return c.close()
}

func (c *Client) close() error {
	c.mu.Lock()
	cmd, stdin, processDone := c.cmd, c.stdin, c.processDone
	c.mu.Unlock()
	if cmd == nil {
		return nil
	}
	c.disconnect(nil)
	if cmd.Process == nil {
		return nil
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-processDone:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-processDone
	}
	return nil
}

func safeEnvironment(env []string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "CODEX_HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true}
	var result []string
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if allowed[key] {
			result = append(result, entry)
		}
	}
	if !containsEnv(result, "PATH") {
		result = append(result, "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	}
	return result
}

func containsEnv(env []string, key string) bool {
	for _, item := range env {
		if strings.HasPrefix(item, key+"=") {
			return true
		}
	}
	return false
}

func appServerVersion(userAgent any) string {
	value, ok := userAgent.(string)
	if !ok {
		return ""
	}
	_, version, found := strings.Cut(value, "/")
	if !found {
		return ""
	}
	version, _, _ = strings.Cut(version, " ")
	return version
}

func supportsPermissionProfiles(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	patch, errPatch := strconv.Atoi(parts[2])
	if errMajor != nil || errMinor != nil || errPatch != nil || major < 0 || minor < 0 || patch < 0 {
		return false
	}
	return major > 0 || minor >= 138
}
