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
	mu sync.Mutex

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
	pendingPerms map[string]*pendingPermission

	// Task tracking: lets the iOS app query whether a task is in progress
	// after reconnecting from background.
	taskRunning   bool
	taskStartedAt time.Time

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
		pendingPerms:   make(map[string]*pendingPermission),
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
	mode := canonicalClaudePermissionMode(c.selectedMode)
	c.mu.Unlock()

	if mode == "bypass" || mode == "dontAsk" || mode == "auto" || mode == "acceptEdits" || mode == "bypassPermissions" {
		if optionId, ok := pickAcpAllowOption(params); ok {
			_ = c.agent.Respond(id, map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome":  "selected",
					"optionId": optionId,
				},
			})
			return
		}
		_ = c.agent.Respond(id, map[string]interface{}{
			"outcome": map[string]interface{}{"outcome": "cancelled"},
		})
		return
	}

	requestId := fmt.Sprintf("perm-%d", time.Now().UnixNano())
	options, _ := params["options"].([]interface{})
	sessionId, _ := params["sessionId"].(string)
	toolCall, _ := params["toolCall"].(map[string]interface{})
	toolName, _ := toolCall["title"].(string)
	if toolName == "" {
		toolName, _ = toolCall["name"].(string)
	}

	createdAt := time.Now()
	pending := &pendingPermission{
		requestId: requestId,
		acpID:     id,
		options:   options,
		sessionId: sessionId,
		toolName:  toolName,
		toolCall:  toolCall,
		createdAt: createdAt,
	}
	pending.timer = time.AfterFunc(5*time.Minute, func() {
		c.resolvePermission(requestId, "", true, "timeout")
	})

	c.mu.Lock()
	c.pendingPerms[requestId] = pending
	c.mu.Unlock()

	c.emit("permission/request", map[string]interface{}{
		"requestId": requestId,
		"sessionId": sessionId,
		"toolName":  toolName,
		"toolCall":  toolCall,
		"options":   options,
		"createdAt": createdAt.UnixMilli(),
	})
}

// RespondPermission resolves a previously-emitted permission/request. If
// `cancelled` is true the daemon picks a reject option (falling back to
// the ACP "cancelled" outcome when no reject option is offered).
func (c *ClaudeBridge) RespondPermission(requestId, optionId string, cancelled bool) error {
	reason := "approved"
	if cancelled || optionId == "" {
		reason = "cancelled"
	}
	if !c.resolvePermission(requestId, optionId, cancelled, reason) {
		return fmt.Errorf("no pending permission request: %s", requestId)
	}
	return nil
}

func (c *ClaudeBridge) resolvePermission(requestId, optionId string, cancelled bool, reason string) bool {
	c.mu.Lock()
	pending, ok := c.pendingPerms[requestId]
	if ok {
		delete(c.pendingPerms, requestId)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}

	paramsMap := map[string]interface{}{"options": pending.options}
	resolution := "approved"
	var outcome map[string]interface{}
	if cancelled || optionId == "" {
		resolution = reason
		if rejectId, ok := pickAcpRejectOption(paramsMap); ok {
			outcome = map[string]interface{}{"outcome": "selected", "optionId": rejectId}
		} else {
			outcome = map[string]interface{}{"outcome": "cancelled"}
		}
	} else {
		outcome = map[string]interface{}{"outcome": "selected", "optionId": optionId}
	}
	_ = c.agent.Respond(pending.acpID, map[string]interface{}{"outcome": outcome})
	c.emit("permission/resolved", map[string]interface{}{
		"requestId":  requestId,
		"sessionId":  pending.sessionId,
		"toolName":   pending.toolName,
		"toolCall":   pending.toolCall,
		"resolvedAs": resolution,
	})
	return true
}

func (c *ClaudeBridge) clearPendingPermissionsLocked(reason string) []map[string]interface{} {
	c.mu.Lock()
	pending := make([]*pendingPermission, 0, len(c.pendingPerms))
	for requestId, p := range c.pendingPerms {
		p.requestId = requestId
		pending = append(pending, p)
	}
	c.pendingPerms = make(map[string]*pendingPermission)
	c.mu.Unlock()

	resolved := make([]map[string]interface{}, 0, len(pending))
	for _, p := range pending {
		if p.timer != nil {
			p.timer.Stop()
		}
		resolved = append(resolved, map[string]interface{}{
			"requestId":  p.requestId,
			"sessionId":  p.sessionId,
			"toolName":   p.toolName,
			"toolCall":   p.toolCall,
			"resolvedAs": reason,
		})
	}
	return resolved
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
	return c.agent.Start()
}

func (c *ClaudeBridge) Stop() {
	c.agent.Stop()
	resolved := c.clearPendingPermissionsLocked("stopped")
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
func (c *ClaudeBridge) Prompt(text string, images []string) error {
	if !c.agent.IsRunning() {
		if !ensureClaudeAcp(c.agent.CheckAvailable) {
			return fmt.Errorf("%s not found in PATH; install with `npm install -g %s`", claudeAcpCommand(), claudeAcpPackage)
		}
		if err := c.agent.Start(); err != nil {
			return fmt.Errorf("start claude-code-acp: %w", err)
		}
	}

	c.mu.Lock()
	sid := c.currentSession
	cwd := c.cwd
	selectedModel := canonicalClaudeModel(c.selectedModel)
	selectedEffort := canonicalClaudeEffort(c.selectedEffort)
	selectedMode := canonicalClaudePermissionMode(c.selectedMode)
	c.mu.Unlock()

	if sid == "" {
		result, err := c.agent.NewSession(cwd)
		if err != nil {
			return fmt.Errorf("session/new: %w", err)
		}
		newSid, _ := result["sessionId"].(string)
		if newSid == "" {
			return fmt.Errorf("session/new returned no sessionId")
		}
		c.mu.Lock()
		c.currentSession = newSid
		c.sessionModel = selectedModel
		c.sessionMode = selectedMode
		c.mu.Unlock()
		sid = newSid

		c.emit("init", map[string]interface{}{
			"sessionId": sid,
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
	}

	c.mu.Lock()
	c.taskRunning = true
	c.taskStartedAt = time.Now()
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
	}()
	return nil
}

// LoadSession resumes a session by ID. It loads the conversation into the
// ACP agent (so further prompts continue in that session) AND returns the
// parsed items from the local JSONL so the UI can display history.
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
	busy := c.taskRunning && c.currentSession == sessionId
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

	if busy {
		log.Printf("[claude] resume: task in flight for %s; returning history only, not disturbing the running agent", sessionId)
	} else {
		c.agent.SetCwd(cwd)

		if !c.agent.IsRunning() && c.agent.CheckAvailable() {
			if err := c.agent.Start(); err != nil {
				log.Printf("[claude] resume: agent start failed: %v", err)
			}
		}
		if c.agent.IsRunning() {
			if _, lerr := c.agent.LoadSession(sessionId, cwd); lerr != nil {
				log.Printf("[claude] session/load best-effort failed: %v", lerr)
			}
		}
	}

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
	}, nil
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
	resolved := c.clearPendingPermissionsLocked("cleared")
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
	defer c.mu.Unlock()

	pending := make([]*pendingPermission, 0, len(c.pendingPerms))
	for reqId, p := range c.pendingPerms {
		p.requestId = reqId
		pending = append(pending, p)
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].createdAt.Before(pending[j].createdAt)
	})

	pendingPermList := make([]map[string]interface{}, 0, len(pending))
	for _, p := range pending {
		pendingPermList = append(pendingPermList, map[string]interface{}{
			"requestId": p.requestId,
			"toolName":  p.toolName,
			"sessionId": p.sessionId,
			"options":   p.options,
			"toolCall":  p.toolCall,
			"createdAt": p.createdAt.UnixMilli(),
		})
	}

	events := make([]cachedNotification, len(c.lastNotifications))
	copy(events, c.lastNotifications)

	config := map[string]interface{}{
		"model":          canonicalClaudeModel(c.selectedModel),
		"effort":         canonicalClaudeEffort(c.selectedEffort),
		"permissionMode": canonicalClaudePermissionMode(c.selectedMode),
	}
	capabilities := c.Capabilities()

	return map[string]interface{}{
		"ok":             true,
		"running":        c.taskRunning,
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
	c.lastNotifications = append(c.lastNotifications, cachedNotification{
		Method: method,
		Params: params,
		Time:   time.Now().UnixMilli(),
	})
	// Trim to ring buffer size.
	if len(c.lastNotifications) > maxCachedNotifications {
		c.lastNotifications = c.lastNotifications[len(c.lastNotifications)-maxCachedNotifications:]
	}
	c.mu.Unlock()

	if c.OnNotification != nil {
		c.OnNotification(method, params)
	}
}
