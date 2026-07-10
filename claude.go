package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"
)

// ClaudeBridge runs Claude Code through the standard Agent Client Protocol
// (ACP) using `claude-code-acp` (the Zed-maintained bridge) as the agent.
// The shared ACP plumbing lives in the embedded acpChatBridge; Claude's
// genuine differences are its model/effort/permissionMode config vocabulary,
// the session/set_mode pushes that make permission modes take effect in
// claude-code-acp, and its on-disk JSONL transcripts: session storage still
// lives in `~/.claude/projects/<cwd-key>/*.jsonl` because claude-code-acp
// delegates to the Claude SDK, so session listing and resume continue to read
// those JSONL files directly (see claude_session.go).
type ClaudeBridge struct {
	acpChatBridge

	// Selected Claude config from the UI (beyond the shared model/mode).
	// These are daemon-owned preferences / policies, not guaranteed live ACP
	// session mutations. Guarded by the embedded acpChatBridge mutex.
	selectedEffort string

	// Last metadata observed for the active session (typically parsed from
	// JSONL history or emitted by Claude), kept separate from the selected UI
	// config so loading an old session doesn't silently overwrite the picker.
	// Guarded by the embedded acpChatBridge mutex.
	sessionModel string
	sessionMode  string
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
		selectedEffort: "medium",
	}
	c.selectedMode = "default"
	claudeUnavailableError := func() error {
		return fmt.Errorf("%s not found in PATH; install with `npm install -g %s`", claudeAcpCommand(), claudeAcpPackage)
	}
	c.initAcpChatBridge(AcpAgentConfig{
		ID:          "claude",
		Label:       "Claude",
		Command:     claudeAcpCommand(),
		Args:        nil,
		Env:         []string{"TERM=dumb"},
		VersionArgs: []string{"--version"},
		Capabilities: AcpCapabilities{
			CanSetModel: true,
			CanSetMode:  true,
		},
		// We handle permission requests ourselves via OnPermissionRequest
		// (auto-approving for bypass/dontAsk modes, forwarding to the iOS
		// UI otherwise), so leave the blanket auto-approve off.
		AutoApprovePermissions: false,
	}, acpChatBridgeHooks{
		id:                    "claude",
		label:                 "Claude",
		ensureInstalled:       ensureClaudeAcp,
		startUnavailableError: claudeUnavailableError,
		loadUnavailableError:  claudeUnavailableError,
		canonicalModel:        canonicalClaudeModel,
		canonicalMode:         canonicalClaudePermissionMode,
		afterModelSetLocked: func(model string) {
			c.sessionModel = model
		},
	})
	c.hooks.newSession = c.NewSession
	return c
}

func (c *ClaudeBridge) SelectedEffort() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedEffort
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

// effectiveModel resolves the model to surface to the UI: the user's explicit
// selection when set, otherwise the model the ACP agent reported as current in
// its latest session response (so the picker highlights the active model even
// before the user picks one).
func (c *ClaudeBridge) effectiveModel(selected string) string {
	if selected != "" && selected != "default" {
		return selected
	}
	if id := c.agent.CurrentModelID(); id != "" {
		return id
	}
	return canonicalClaudeModel(selected)
}

// effectiveMode resolves the permission mode to surface to the UI: the user's
// explicit selection when set, otherwise the mode the ACP agent reported as
// current in its latest session response.
func (c *ClaudeBridge) effectiveMode(selected string) string {
	if selected != "" && selected != "default" {
		return selected
	}
	if id := c.agent.CurrentModeID(); id != "" {
		return id
	}
	return canonicalClaudePermissionMode(selected)
}

// ListModes returns the permission modes advertised by the ACP agent, ensuring
// the current session is loaded first so the cache is populated (mirrors
// ListModels).
func (c *ClaudeBridge) ListModes() ([]AgentModelOption, error) {
	c.mu.Lock()
	sessionID := c.currentSession
	cwd := c.cwd
	c.mu.Unlock()
	if sessionID != "" {
		if err := c.ensureAgentSessionLoaded(sessionID, cwd); err != nil {
			return nil, err
		}
	} else if !c.agent.IsRunning() {
		if err := c.Start(cwd); err != nil {
			return nil, err
		}
	}
	return c.agent.ListModes(), nil
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

// SetConfig stores the desired Claude config/policy. Model and effort are
// daemon/UI-owned preferences, but a permission-mode change is pushed to the
// live ACP session via session/set_mode so plan/acceptEdits actually take
// effect in claude-code-acp (not just the daemon's auto-approve broker).
func (c *ClaudeBridge) SetConfig(patch ClaudeConfigPatch) {
	c.mu.Lock()
	if patch.Model != nil {
		c.selectedModel = canonicalClaudeModel(*patch.Model)
	}
	if patch.Effort != nil {
		c.selectedEffort = canonicalClaudeEffort(*patch.Effort)
	}
	modeChanged := false
	if patch.PermissionMode != nil {
		newMode := canonicalClaudePermissionMode(*patch.PermissionMode)
		modeChanged = newMode != c.selectedMode
		c.selectedMode = newMode
	}
	sid := c.currentSession
	mode := c.selectedMode
	cwd := c.cwd
	c.mu.Unlock()

	if modeChanged && sid != "" && c.agent.Capabilities().CanSetMode {
		if err := c.ensureAgentSessionLoaded(sid, cwd); err != nil {
			log.Printf("[claude] set_mode ensureLoaded failed: %v", err)
			return
		}
		if err := c.agent.SetMode(sid, mode); err != nil {
			log.Printf("[claude] set_mode(%s) failed: %v", mode, err)
			return
		}
		c.mu.Lock()
		c.sessionMode = mode
		c.mu.Unlock()
	}
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

	selectedModel = c.effectiveModel(selectedModel)

	// Honor the user's permission-mode preference on the fresh session so
	// plan/acceptEdits/etc. apply from the first turn.
	if selectedMode != "" && selectedMode != "default" && c.agent.Capabilities().CanSetMode {
		if err := c.agent.SetMode(newSid, selectedMode); err != nil {
			log.Printf("[claude] new session set_mode(%s) failed: %v", selectedMode, err)
		}
	}
	selectedMode = c.effectiveMode(selectedMode)

	c.mu.Lock()
	c.currentSession = newSid
	c.selectedModel = selectedModel
	c.selectedMode = selectedMode
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
		"capabilities":    c.Capabilities(),
		"model":           selectedModel,
		"effort":          selectedEffort,
		"permissionMode":  selectedMode,
		"availableModes":  c.agent.ListModes(),
		"availableModels": agentModelList(c.agent),
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
	selectedModel := c.effectiveModel(c.selectedModel)
	selectedEffort := canonicalClaudeEffort(c.selectedEffort)
	selectedMode := canonicalClaudePermissionMode(c.selectedMode)
	c.selectedModel = selectedModel
	c.mu.Unlock()

	// Re-apply the user's permission-mode preference to the resumed session.
	if selectedMode != "" && selectedMode != "default" && c.agent.Capabilities().CanSetMode {
		if err := c.agent.SetMode(sessionId, selectedMode); err != nil {
			log.Printf("[claude] load session set_mode(%s) failed: %v", selectedMode, err)
		}
	}
	selectedMode = c.effectiveMode(selectedMode)
	c.mu.Lock()
	c.selectedMode = selectedMode
	c.sessionMode = selectedMode
	c.mu.Unlock()

	availableModes := c.agent.ListModes()
	availableModels := agentModelList(c.agent)
	c.emit("init", map[string]interface{}{
		"sessionId": sessionId,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model":          selectedModel,
			"effort":         selectedEffort,
			"permissionMode": selectedMode,
		},
		"capabilities":    c.Capabilities(),
		"model":           selectedModel,
		"effort":          selectedEffort,
		"permissionMode":  selectedMode,
		"availableModes":  availableModes,
		"availableModels": availableModels,
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
		"capabilities":    c.Capabilities(),
		"availableModes":  availableModes,
		"availableModels": availableModels,
		"agentLoaded":     c.agent.IsSessionLoaded(sessionId),
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
// reconnecting from background or a page refresh. It extends the shared
// acpChatBridge payload with Claude's effort/permissionMode config vocabulary,
// the session-observed model/mode, and the advertised permission modes.
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
	running := c.taskRunning
	operationID := c.currentOperationID
	sessionID := c.currentSession
	startedAt := c.taskStartedAt
	cwd := c.cwd
	sessionModel := c.sessionModel
	sessionMode := c.sessionMode
	c.mu.Unlock()

	pendingPermList := []map[string]interface{}{}
	if delegate != nil {
		pendingPermList = delegate.Pending()
	}

	return map[string]interface{}{
		"ok":              true,
		"running":         running,
		"operationId":     operationID,
		"sessionId":       sessionID,
		"startedAt":       startedAt.UnixMilli(),
		"recentEvents":    events,
		"pendingPerms":    pendingPermList,
		"cwd":             cwd,
		"config":          config,
		"capabilities":    capabilities,
		"model":           config["model"],
		"effort":          config["effort"],
		"permissionMode":  config["permissionMode"],
		"sessionModel":    sessionModel,
		"sessionMode":     sessionMode,
		"availableModes":  c.agent.ListModes(),
		"availableModels": agentModelList(c.agent),
	}
}
