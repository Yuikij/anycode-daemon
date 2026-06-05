package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxCachedNotifications bounds the current-turn replay buffer we keep for
// reconnect recovery while a Claude task is in flight.
const maxCachedNotifications = 300

// cachedNotification stores a single notification for replay after
// iOS app reconnect.
type cachedNotification struct {
	Method string      `json:"method"`
	Params interface{} `json:"params"`
	Time   int64       `json:"time"`
}

// ClaudeBridge runs Claude Code through the standard Agent Client Protocol
// (ACP) using `claude-code-acp` (the Zed-maintained bridge) as the agent.
// Session storage still lives in `~/.claude/projects/<cwd-key>/*.jsonl`
// because claude-code-acp delegates to the Claude SDK, so session listing
// and resume continue to read those JSONL files directly.
type ClaudeBridge struct {
	mu      sync.Mutex
	agentMu sync.Mutex

	agent *AcpAgent

	// Selected Claude config from the UI. These are daemon-owned preferences /
	// policies, not guaranteed live ACP session mutations.
	cwd            string
	currentSession string
	selectedModel  string
	selectedEffort string
	selectedMode   string

	// Last metadata observed for the active session (typically parsed from
	// JSONL history or emitted by Claude), kept separate from the selected UI
	// config so loading an old session doesn't silently overwrite the picker.
	sessionModel string
	sessionMode  string

	// Pending permission requests awaiting user decision via
	// claude.permission/respond. Keyed by requestId (the daemon-generated
	// id we surface to the iOS UI, not the ACP id).
	permissionDelegate ClaudePermissionDelegate

	// Task tracking: lets the iOS app query whether a task is in progress
	// after reconnecting from background.
	taskRunning        bool
	taskStartedAt      time.Time
	currentOperationID string

	// Ring buffer of recent notifications for replay on reconnect.
	lastNotifications []cachedNotification

	OnNotification func(method string, params interface{})
}

// pendingPermission tracks one ACP permission request that's waiting on
// the user's decision in the app. We keep both the original ACP request
// id (so we know who to respond to) and the parsed option list (so we
// can validate the user's selection).
type pendingPermission struct {
	requestId string
	acpID     interface{}
	options   []interface{}
	timer     *time.Timer
	sessionId string
	toolName  string
	toolCall  map[string]interface{}
	createdAt time.Time
}

// acpCommand is the ACP bridge binary. It can be overridden via the
// CLAUDE_ACP_COMMAND env var, useful for local development.
func claudeAcpCommand() string {
	if v := os.Getenv("CLAUDE_ACP_COMMAND"); v != "" {
		return v
	}
	return "claude-code-acp"
}

func NewClaudeBridge() *ClaudeBridge {
	c := &ClaudeBridge{
		selectedMode:   "default",
		selectedEffort: "medium",
	}
	c.agent = NewAcpAgent(AcpAgentConfig{
		ID:          "claude",
		Label:       "Claude",
		Command:     claudeAcpCommand(),
		Args:        nil,
		Env:         []string{"TERM=dumb"},
		VersionArgs: []string{"--version"},
		Capabilities: AcpCapabilities{
			CanSetModel: false,
			CanSetMode:  false,
		},
		// We handle permission requests ourselves via OnPermissionRequest
		// (auto-approving for bypass/dontAsk modes, forwarding to the iOS
		// UI otherwise), so leave the blanket auto-approve off.
		AutoApprovePermissions: false,
	})
	c.agent.OnNotification = func(method string, params interface{}) {
		// Track sessionId from session updates so resume/cancel work.
		if p, ok := params.(map[string]interface{}); ok {
			if sid, _ := p["sessionId"].(string); sid != "" {
				c.mu.Lock()
				if c.currentSession == "" {
					c.currentSession = sid
				}
				c.mu.Unlock()
			}
		}
		c.emit(method, params)

		// Detect terminal turn states to clear taskRunning.
		if method == "turn/completed" || method == "turn/failed" || method == "turn/aborted" || method == "turn/interrupted" {
			c.mu.Lock()
			c.taskRunning = false
			c.mu.Unlock()
		}
	}
	c.agent.OnPermissionRequest = c.handlePermissionRequest
	return c
}

func (c *ClaudeBridge) SetPermissionDelegate(delegate ClaudePermissionDelegate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.permissionDelegate = delegate
}

// handlePermissionRequest is invoked when claude-code-acp asks the client
// to approve/deny a tool invocation. Behavior depends on the configured
// permission mode:
//
//   - bypass / dontAsk: auto-approve immediately using the first allow
//     option. (The Claude SDK's own bypassPermissions/acceptEdits modes
//     normally short-circuit this, but the bridge still occasionally
//     forwards a request — handle it anyway.)
//   - Everything else: emit `claude.permission/request` to the iOS UI
//     and wait for `claude.permission/respond`. After 5 minutes with no
//     reply we auto-reject so Claude doesn't hang forever.
func (c *ClaudeBridge) handlePermissionRequest(id interface{}, params map[string]interface{}) {
	c.mu.Lock()
	delegate := c.permissionDelegate
	c.mu.Unlock()
	if delegate != nil {
		delegate.HandleRequest(id, params)
	}
}

// RespondPermission resolves a previously-emitted permission/request. If
// `cancelled` is true the daemon picks a reject option (falling back to
// the ACP "cancelled" outcome when no reject option is offered).
func (c *ClaudeBridge) RespondPermission(requestId, optionId string, cancelled bool) error {
	c.mu.Lock()
	delegate := c.permissionDelegate
	c.mu.Unlock()
	if delegate == nil {
		return fmt.Errorf("permission delegate not configured")
	}
	return delegate.Resolve(requestId, optionId, cancelled)
}

func (c *ClaudeBridge) SetCwd(cwd string) {
	c.mu.Lock()
	c.cwd = cwd
	c.mu.Unlock()
	c.agent.SetCwd(cwd)
}

func (c *ClaudeBridge) IsRunning() bool { return c.agent.IsRunning() }
func (c *ClaudeBridge) Available() bool { return c.agent.Available() }

func (c *ClaudeBridge) SessionId() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentSession
}

func (c *ClaudeBridge) RestoreSession(sessionId string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentSession = sessionId
}

func (c *ClaudeBridge) SelectedModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedModel
}

func (c *ClaudeBridge) SelectedEffort() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedEffort
}

func (c *ClaudeBridge) SelectedMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedMode
}

func (c *ClaudeBridge) SessionModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionModel
}

func (c *ClaudeBridge) SessionMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionMode
}

func (c *ClaudeBridge) ConfigSnapshot() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]interface{}{
		"model":          canonicalClaudeModel(c.selectedModel),
		"effort":         canonicalClaudeEffort(c.selectedEffort),
		"permissionMode": canonicalClaudePermissionMode(c.selectedMode),
	}
}

func (c *ClaudeBridge) Capabilities() map[string]bool {
	caps := c.agent.Capabilities()
	return map[string]bool{
		"canSetModel": caps.CanSetModel,
		"canSetMode":  caps.CanSetMode,
	}
}

func (c *ClaudeBridge) CheckAvailable() bool {
	return c.agent.CheckAvailable()
}

// Start ensures the ACP agent subprocess is running. The caller can pass a
// cwd to update the working directory used for newly created sessions.
func (c *ClaudeBridge) Start(cwd string) error {
	c.mu.Lock()
	if cwd != "" {
		c.cwd = cwd
	}
	c.mu.Unlock()
	if cwd != "" {
		c.agent.SetCwd(cwd)
	}

	if !ensureClaudeAcp(c.agent.CheckAvailable) {
		return fmt.Errorf("%s not found in PATH; install with `npm install -g %s`", claudeAcpCommand(), claudeAcpPackage)
	}
	c.agentMu.Lock()
	err := c.agent.Start()
	c.agentMu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (c *ClaudeBridge) Stop() {
	c.agent.Stop()
	c.mu.Lock()
	delegate := c.permissionDelegate
	c.mu.Unlock()
	resolved := []map[string]interface{}{}
	if delegate != nil {
		resolved = delegate.Clear("stopped")
	}
	c.mu.Lock()
	c.currentSession = ""
	c.taskRunning = false
	c.taskStartedAt = time.Time{}
	c.currentOperationID = ""
	c.lastNotifications = nil
	c.mu.Unlock()
	for _, notif := range resolved {
		c.emit("permission/resolved", notif)
	}
}

func (c *ClaudeBridge) Cancel() {
	c.mu.Lock()
	sid := c.currentSession
	c.mu.Unlock()
	if sid != "" {
		_ = c.agent.Cancel(sid)
	}
}

type ClaudeConfigPatch struct {
	Model          *string
	Effort         *string
	PermissionMode *string
}

func canonicalClaudeModel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	return value
}

func canonicalClaudeEffort(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "medium"
	}
	return value
}

func canonicalClaudePermissionMode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	return value
}

// SetConfig stores the desired Claude config/policy. These values are owned by
// the daemon/UI and are not assumed to map to live ACP session mutations.
func (c *ClaudeBridge) SetConfig(patch ClaudeConfigPatch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if patch.Model != nil {
		c.selectedModel = canonicalClaudeModel(*patch.Model)
	}
	if patch.Effort != nil {
		c.selectedEffort = canonicalClaudeEffort(*patch.Effort)
	}
	if patch.PermissionMode != nil {
		c.selectedMode = canonicalClaudePermissionMode(*patch.PermissionMode)
	}
}

// Prompt sends a user turn to the active session. If no session is open,
// a new one is created (and a synthetic claude.init notification is fired
// so the iOS UI mirrors the previous stream-json behavior).
func (c *ClaudeBridge) Prompt(text string, images []string) (string, error) {
	c.mu.Lock()
	sid := c.currentSession
	cwd := c.cwd
	c.mu.Unlock()

	if sid == "" {
		newSid, err := c.NewSession(cwd)
		if err != nil {
			return "", err
		}
		sid = newSid
	} else if err := c.ensureAgentSessionLoaded(sid, cwd); err != nil {
		return "", fmt.Errorf("session/load before prompt: %w", err)
	}
	opID := newOperationID("claude")

	c.mu.Lock()
	c.taskRunning = true
	c.taskStartedAt = time.Now()
	c.currentOperationID = opID
	c.lastNotifications = nil
	c.mu.Unlock()

	go func() {
		result, err := c.agent.Prompt(sid, text, images)
		if err != nil {
			log.Printf("[claude] session/prompt failed: %v", err)
			c.emit("error", map[string]interface{}{
				"error":     err.Error(),
				"sessionId": sid,
			})
		}
		c.mu.Lock()
		c.taskRunning = false
		c.mu.Unlock()
		c.emit("turn/completed", map[string]interface{}{
			"sessionId": sid,
			"success":   err == nil,
			"result":    result,
		})
		c.mu.Lock()
		c.currentOperationID = ""
		c.mu.Unlock()
	}()
	return opID, nil
}

func (c *ClaudeBridge) NewSession(cwd string) (string, error) {
	if cwd == "" {
		c.mu.Lock()
		cwd = c.cwd
		c.mu.Unlock()
	}
	if !c.agent.IsRunning() {
		if err := c.Start(cwd); err != nil {
			return "", err
		}
	} else if cwd != "" {
		c.SetCwd(cwd)
	}

	c.mu.Lock()
	selectedModel := canonicalClaudeModel(c.selectedModel)
	selectedEffort := canonicalClaudeEffort(c.selectedEffort)
	selectedMode := canonicalClaudePermissionMode(c.selectedMode)
	c.mu.Unlock()

	c.agentMu.Lock()
	result, err := c.agent.NewSession(cwd)
	c.agentMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}
	newSid, _ := result["sessionId"].(string)
	if newSid == "" {
		return "", fmt.Errorf("session/new returned no sessionId")
	}

	c.mu.Lock()
	c.currentSession = newSid
	c.sessionModel = selectedModel
	c.sessionMode = selectedMode
	c.mu.Unlock()

	c.emit("init", map[string]interface{}{
		"sessionId": newSid,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model":          selectedModel,
			"effort":         selectedEffort,
			"permissionMode": selectedMode,
		},
		"capabilities":   c.Capabilities(),
		"model":          selectedModel,
		"effort":         selectedEffort,
		"permissionMode": selectedMode,
	})
	return newSid, nil
}

// LoadSession resumes a session by ID. This is the single canonical load path:
// read the local transcript, load that session into ACP, then expose the parsed
// items to the UI. If ACP cannot load the session, return the error directly.
func (c *ClaudeBridge) LoadSession(sessionId, cwd string) (map[string]interface{}, error) {
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if cwd == "" {
		c.mu.Lock()
		cwd = c.cwd
		c.mu.Unlock()
	}

	session, items, err := readSessionFile(sessionId, cwd)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.taskRunning && c.currentSession != "" && c.currentSession != sessionId {
		runningSession := c.currentSession
		c.mu.Unlock()
		return nil, fmt.Errorf("cannot load session %s while session %s is running", sessionId, runningSession)
	}
	c.mu.Unlock()

	if sessionCwd, _ := session["cwd"].(string); sessionCwd != "" {
		cwd = sessionCwd
	}
	if err := c.ensureAgentSessionLoaded(sessionId, cwd); err != nil {
		return nil, fmt.Errorf("session/load: %w", err)
	}

	c.mu.Lock()
	c.currentSession = sessionId
	if cwd != "" {
		c.cwd = cwd
	}
	if model, _ := session["model"].(string); model != "" {
		c.sessionModel = model
	}
	if mode, _ := session["permissionMode"].(string); mode != "" {
		c.sessionMode = mode
	}
	selectedModel := canonicalClaudeModel(c.selectedModel)
	selectedEffort := canonicalClaudeEffort(c.selectedEffort)
	selectedMode := canonicalClaudePermissionMode(c.selectedMode)
	c.mu.Unlock()

	c.emit("init", map[string]interface{}{
		"sessionId": sessionId,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model":          selectedModel,
			"effort":         selectedEffort,
			"permissionMode": selectedMode,
		},
		"capabilities":   c.Capabilities(),
		"model":          selectedModel,
		"effort":         selectedEffort,
		"permissionMode": selectedMode,
	})

	return map[string]interface{}{
		"ok":      true,
		"session": session,
		"items":   items,
		"config": map[string]interface{}{
			"model":          selectedModel,
			"effort":         selectedEffort,
			"permissionMode": selectedMode,
		},
		"capabilities": c.Capabilities(),
		"agentLoaded":  c.agent.IsSessionLoaded(sessionId),
	}, nil
}

func (c *ClaudeBridge) ensureAgentSessionLoaded(sessionId, cwd string) error {
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	if !c.agent.IsRunning() {
		if !ensureClaudeAcp(c.agent.CheckAvailable) {
			return fmt.Errorf("%s not found in PATH; install with `npm install -g %s`", claudeAcpCommand(), claudeAcpPackage)
		}
	}
	return c.agent.EnsureLoaded(sessionId, cwd)
}

// ListSessions returns Claude sessions across all projects (matching the
// behavior of `claude /resume`). Reads directly from `~/.claude/projects/`.
func (c *ClaudeBridge) ListSessions(cwd string) (map[string]interface{}, error) {
	sessions, err := listAllClaudeSessions()
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	sort.Slice(sessions, func(i, j int) bool {
		return int64Value(sessions[i]["updatedAt"]) > int64Value(sessions[j]["updatedAt"])
	})

	return map[string]interface{}{"ok": true, "sessions": sessions}, nil
}

// ClearSession forgets the current session id so the next Prompt() starts
// fresh and clears any pending permission prompts tied to the old turn.
func (c *ClaudeBridge) ClearSession() {
	c.mu.Lock()
	delegate := c.permissionDelegate
	c.mu.Unlock()
	resolved := []map[string]interface{}{}
	if delegate != nil {
		resolved = delegate.Clear("cleared")
	}
	c.mu.Lock()
	c.currentSession = ""
	c.taskRunning = false
	c.taskStartedAt = time.Time{}
	c.lastNotifications = nil
	c.mu.Unlock()
	for _, notif := range resolved {
		c.emit("permission/resolved", notif)
	}
}

// DeleteSession removes a session's JSONL file from disk.
func (c *ClaudeBridge) DeleteSession(sessionId, cwd string) error {
	path, err := findSessionFilePath(sessionId, cwd)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	c.mu.Lock()
	if c.currentSession == sessionId {
		c.currentSession = ""
	}
	c.mu.Unlock()
	return nil
}

// RenameSession sets a custom title by appending an `ai-title` record to the
// session's JSONL. `parseClaudeSessionFile` already treats the last seen
// ai-title as the session title, so this overrides any auto-generated name.
func (c *ClaudeBridge) RenameSession(sessionId, title, cwd string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("title is required")
	}
	path, err := findSessionFilePath(sessionId, cwd)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]interface{}{
		"type":      "ai-title",
		"aiTitle":   title,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// TaskStatus returns the current task state for clients to recover after
// reconnecting from background or a page refresh.
func (c *ClaudeBridge) TaskStatus() map[string]interface{} {
	c.mu.Lock()
	delegate := c.permissionDelegate
	events := make([]cachedNotification, len(c.lastNotifications))
	copy(events, c.lastNotifications)

	config := map[string]interface{}{
		"model":          canonicalClaudeModel(c.selectedModel),
		"effort":         canonicalClaudeEffort(c.selectedEffort),
		"permissionMode": canonicalClaudePermissionMode(c.selectedMode),
	}
	capabilities := c.Capabilities()
	c.mu.Unlock()

	pendingPermList := []map[string]interface{}{}
	if delegate != nil {
		pendingPermList = delegate.Pending()
	}

	return map[string]interface{}{
		"ok":             true,
		"running":        c.taskRunning,
		"operationId":    c.currentOperationID,
		"sessionId":      c.currentSession,
		"startedAt":      c.taskStartedAt.UnixMilli(),
		"recentEvents":   events,
		"pendingPerms":   pendingPermList,
		"cwd":            c.cwd,
		"config":         config,
		"capabilities":   capabilities,
		"model":          config["model"],
		"effort":         config["effort"],
		"permissionMode": config["permissionMode"],
		"sessionModel":   c.sessionModel,
		"sessionMode":    c.sessionMode,
	}
}

func (c *ClaudeBridge) emit(method string, params interface{}) {
	// Cache the notification for replay on reconnect.
	c.mu.Lock()
	payload := attachOperationID(params, c.currentOperationID)
	c.lastNotifications = append(c.lastNotifications, cachedNotification{
		Method: method,
		Params: payload,
		Time:   time.Now().UnixMilli(),
	})
	// Trim to ring buffer size.
	if len(c.lastNotifications) > maxCachedNotifications {
		c.lastNotifications = c.lastNotifications[len(c.lastNotifications)-maxCachedNotifications:]
	}
	c.mu.Unlock()

	if c.OnNotification != nil {
		c.OnNotification(method, payload)
	}
}
