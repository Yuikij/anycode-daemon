package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CursorBridge runs the Cursor CLI through the standard Agent Client Protocol
// (ACP) using `agent acp` (Cursor's built-in ACP server mode). It mirrors
// ClaudeBridge — both are ACP-based and share the interactive permission flow
// and current-turn replay buffer — but Cursor's config is model + mode
// (agent/plan/ask) rather than Claude's model/effort/permissionMode, and Cursor
// has no on-disk JSONL transcript so session listing is best-effort via the CLI.
type CursorBridge struct {
	mu      sync.Mutex
	agentMu sync.Mutex

	agent *AcpAgent

	cwd            string
	currentSession string
	selectedModel  string
	selectedMode   string

	permissionDelegate ClaudePermissionDelegate

	taskRunning        bool
	taskStartedAt      time.Time
	currentOperationID string
	lastNotifications  []cachedNotification

	OnNotification func(method string, params interface{})
}

// cursorCommand resolves the Cursor CLI binary. The cursor.com installer
// exposes it as `agent`; some installs expose `cursor-agent`. Override with
// ANYCODE_CURSOR_BIN (mirrors ANYCODE_CODEX_BIN / CLAUDE_ACP_COMMAND).
func cursorCommand() string {
	if v := strings.TrimSpace(os.Getenv("ANYCODE_CURSOR_BIN")); v != "" {
		return v
	}
	if _, err := exec.LookPath("agent"); err == nil {
		return "agent"
	}
	if _, err := exec.LookPath("cursor-agent"); err == nil {
		return "cursor-agent"
	}
	return "agent"
}

func canonicalCursorModel(value string) string {
	return strings.TrimSpace(value)
}

// effectiveModel resolves the model to surface to the UI: the user's explicit
// selection when set, otherwise the model the Cursor ACP agent reported as
// current in its latest session response.
func (c *CursorBridge) effectiveModel(selected string) string {
	if selected != "" {
		return selected
	}
	return c.agent.CurrentModelID()
}

func canonicalCursorMode(value string) string {
	switch strings.TrimSpace(value) {
	case "plan":
		return "plan"
	case "ask":
		return "ask"
	default:
		return "agent"
	}
}

func NewCursorBridge() *CursorBridge {
	c := &CursorBridge{
		selectedMode: "agent",
	}
	c.agent = NewAcpAgent(AcpAgentConfig{
		ID:          "cursor",
		Label:       "Cursor",
		Command:     cursorCommand(),
		Args:        []string{"acp"},
		Env:         []string{"TERM=dumb"},
		VersionArgs: []string{"--version"},
		Capabilities: AcpCapabilities{
			CanSetModel: true,
			CanSetMode:  true,
		},
		// Permission requests are surfaced to the UI via OnPermissionRequest,
		// not blanket auto-approved.
		AutoApprovePermissions: false,
	})
	c.agent.OnNotification = func(method string, params interface{}) {
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
		if method == "turn/completed" || method == "turn/failed" || method == "turn/aborted" || method == "turn/interrupted" {
			c.mu.Lock()
			c.taskRunning = false
			c.mu.Unlock()
		}
	}
	c.agent.OnPermissionRequest = c.handlePermissionRequest
	return c
}

func (c *CursorBridge) SetPermissionDelegate(delegate ClaudePermissionDelegate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.permissionDelegate = delegate
}

func (c *CursorBridge) handlePermissionRequest(id interface{}, params map[string]interface{}) {
	c.mu.Lock()
	delegate := c.permissionDelegate
	c.mu.Unlock()
	if delegate != nil {
		delegate.HandleRequest(id, params)
	}
}

func (c *CursorBridge) RespondPermission(requestId, optionId string, cancelled bool) error {
	c.mu.Lock()
	delegate := c.permissionDelegate
	c.mu.Unlock()
	if delegate == nil {
		return fmt.Errorf("permission delegate not configured")
	}
	return delegate.Resolve(requestId, optionId, cancelled)
}

func (c *CursorBridge) SetCwd(cwd string) {
	c.mu.Lock()
	c.cwd = cwd
	c.mu.Unlock()
	c.agent.SetCwd(cwd)
}

func (c *CursorBridge) IsRunning() bool { return c.agent.IsRunning() }
func (c *CursorBridge) Available() bool { return c.agent.Available() }

func (c *CursorBridge) SessionId() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentSession
}

func (c *CursorBridge) RestoreSession(sessionId string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentSession = sessionId
}

func (c *CursorBridge) SelectedModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedModel
}

func (c *CursorBridge) SelectedMode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedMode
}

func (c *CursorBridge) ConfigSnapshot() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]interface{}{
		"model": canonicalCursorModel(c.selectedModel),
		"mode":  canonicalCursorMode(c.selectedMode),
	}
}

func (c *CursorBridge) Capabilities() map[string]bool {
	caps := c.agent.Capabilities()
	return map[string]bool{
		"canSetModel": caps.CanSetModel,
		"canSetMode":  caps.CanSetMode,
	}
}

func (c *CursorBridge) CheckAvailable() bool {
	return c.agent.CheckAvailable()
}

func (c *CursorBridge) Start(cwd string) error {
	c.mu.Lock()
	if cwd != "" {
		c.cwd = cwd
	}
	c.mu.Unlock()
	if cwd != "" {
		c.agent.SetCwd(cwd)
	}
	if !c.agent.CheckAvailable() {
		return fmt.Errorf("%s not found in PATH; install the Cursor CLI from https://cursor.com/install (or set ANYCODE_CURSOR_BIN)", cursorCommand())
	}
	c.agentMu.Lock()
	err := c.agent.Start()
	c.agentMu.Unlock()
	return err
}

func (c *CursorBridge) Stop() {
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

func (c *CursorBridge) Cancel() {
	c.mu.Lock()
	sid := c.currentSession
	c.mu.Unlock()
	if sid != "" {
		_ = c.agent.Cancel(sid)
	}
}

// ListModels returns the models advertised by the Cursor ACP agent. Cursor (like
// claude-code-acp) only reports models in its session/new and session/load
// responses, so ensure the current session is loaded first to populate the cache.
func (c *CursorBridge) ListModels() ([]AgentModelOption, error) {
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
	return c.agent.ListModels()
}

func (c *CursorBridge) SetModel(model string) error {
	model = canonicalCursorModel(model)
	if model == "" {
		return fmt.Errorf("model is required")
	}
	c.mu.Lock()
	sid := c.currentSession
	cwd := c.cwd
	c.mu.Unlock()
	if sid == "" {
		return fmt.Errorf("no active Cursor session to set model")
	}
	if err := c.ensureAgentSessionLoaded(sid, cwd); err != nil {
		return err
	}
	c.agentMu.Lock()
	err := c.agent.SetModel(sid, model)
	c.agentMu.Unlock()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.selectedModel = model
	c.mu.Unlock()
	return nil
}

// SetConfig stores the desired mode. Model changes must go through SetModel so
// the UI only reports success after the backend ACP session accepts the change.
func (c *CursorBridge) SetConfig(model, mode *string) {
	c.mu.Lock()
	if mode != nil {
		c.selectedMode = canonicalCursorMode(*mode)
	}
	sid := c.currentSession
	selectedMode := c.selectedMode
	c.mu.Unlock()

	if sid == "" {
		return
	}
	if mode != nil {
		if err := c.agent.SetMode(sid, selectedMode); err != nil {
			log.Printf("[cursor] setMode failed: %v", err)
		}
	}
}

// Prompt sends a user turn to the active session, creating one if needed. It
// runs the ACP prompt asynchronously and synthesizes a turn/completed event on
// completion (ACP itself has no turn lifecycle notifications), matching Claude.
func (c *CursorBridge) Prompt(text string, images []string) (string, error) {
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
	opID := newOperationID("cursor")

	c.mu.Lock()
	c.taskRunning = true
	c.taskStartedAt = time.Now()
	c.currentOperationID = opID
	c.lastNotifications = nil
	c.mu.Unlock()

	go func() {
		result, err := c.agent.Prompt(sid, text, images)
		if err != nil {
			log.Printf("[cursor] session/prompt failed: %v", err)
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

func (c *CursorBridge) NewSession(cwd string) (string, error) {
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
	selectedModel := canonicalCursorModel(c.selectedModel)
	selectedMode := canonicalCursorMode(c.selectedMode)
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

	c.mu.Lock()
	c.currentSession = newSid
	c.selectedModel = selectedModel
	c.mu.Unlock()

	c.emit("init", map[string]interface{}{
		"sessionId": newSid,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model": selectedModel,
			"mode":  selectedMode,
		},
		"capabilities":    c.Capabilities(),
		"model":           selectedModel,
		"mode":            selectedMode,
		"availableModels": agentModelList(c.agent),
	})
	return newSid, nil
}

// LoadSession resumes a session via ACP session/load. Unlike Claude there is no
// local transcript to rebuild, so `items` is empty and the UI relies on the
// live ACP stream from the resumed session.
func (c *CursorBridge) LoadSession(sessionId, cwd string) (map[string]interface{}, error) {
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if cwd == "" {
		c.mu.Lock()
		cwd = c.cwd
		c.mu.Unlock()
	}

	c.mu.Lock()
	if c.taskRunning && c.currentSession != "" && c.currentSession != sessionId {
		runningSession := c.currentSession
		c.mu.Unlock()
		return nil, fmt.Errorf("cannot load session %s while session %s is running", sessionId, runningSession)
	}
	c.mu.Unlock()

	if err := c.ensureAgentSessionLoaded(sessionId, cwd); err != nil {
		return nil, fmt.Errorf("session/load: %w", err)
	}

	c.mu.Lock()
	c.currentSession = sessionId
	if cwd != "" {
		c.cwd = cwd
	}
	selectedModel := c.effectiveModel(canonicalCursorModel(c.selectedModel))
	selectedMode := canonicalCursorMode(c.selectedMode)
	c.selectedModel = selectedModel
	c.mu.Unlock()

	availableModels := agentModelList(c.agent)
	c.emit("init", map[string]interface{}{
		"sessionId": sessionId,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model": selectedModel,
			"mode":  selectedMode,
		},
		"capabilities":    c.Capabilities(),
		"model":           selectedModel,
		"mode":            selectedMode,
		"availableModels": availableModels,
	})

	return map[string]interface{}{
		"ok": true,
		"session": map[string]interface{}{
			"sessionId": sessionId,
			"cwd":       cwd,
		},
		"items": []interface{}{},
		"config": map[string]interface{}{
			"model": selectedModel,
			"mode":  selectedMode,
		},
		"capabilities":    c.Capabilities(),
		"availableModels": availableModels,
		"agentLoaded":     c.agent.IsSessionLoaded(sessionId),
	}, nil
}

func (c *CursorBridge) ensureAgentSessionLoaded(sessionId, cwd string) error {
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	if !c.agent.IsRunning() {
		if !c.agent.CheckAvailable() {
			return fmt.Errorf("%s not found in PATH; install the Cursor CLI from https://cursor.com/install", cursorCommand())
		}
	}
	return c.agent.EnsureLoaded(sessionId, cwd)
}

// ListSessions reports historical Cursor conversations. The Cursor CLI only
// exposes session history through interactive TUI commands (`agent ls` /
// `agent resume`, both of which open a picker and block on stdin) — there is no
// non-interactive "print sessions as JSON" command. Invoking those here would
// hang the RPC read loop and, since a bare arg is treated as a prompt, risks
// spending agent credits. So session listing degrades to an empty list: the
// live session created via ACP (`session/new`) is still tracked and surfaced
// through taskStatus / notifications, which is what the clients rely on.
func (c *CursorBridge) ListSessions(cwd string) (map[string]interface{}, error) {
	return map[string]interface{}{
		"ok":       true,
		"sessions": []map[string]interface{}{},
	}, nil
}

func (c *CursorBridge) TaskStatus() map[string]interface{} {
	c.mu.Lock()
	delegate := c.permissionDelegate
	events := make([]cachedNotification, len(c.lastNotifications))
	copy(events, c.lastNotifications)
	config := map[string]interface{}{
		"model": canonicalCursorModel(c.selectedModel),
		"mode":  canonicalCursorMode(c.selectedMode),
	}
	capabilities := c.Capabilities()
	c.mu.Unlock()

	pendingPermList := []map[string]interface{}{}
	if delegate != nil {
		pendingPermList = delegate.Pending()
	}

	return map[string]interface{}{
		"ok":           true,
		"running":      c.taskRunning,
		"operationId":  c.currentOperationID,
		"sessionId":    c.currentSession,
		"startedAt":    c.taskStartedAt.UnixMilli(),
		"recentEvents": events,
		"pendingPerms": pendingPermList,
		"cwd":          c.cwd,
		"config":       config,
		"capabilities":    capabilities,
		"model":           config["model"],
		"mode":            config["mode"],
		"availableModels": agentModelList(c.agent),
	}
}

func (c *CursorBridge) emit(method string, params interface{}) {
	c.mu.Lock()
	payload := attachOperationID(params, c.currentOperationID)
	c.lastNotifications = append(c.lastNotifications, cachedNotification{
		Method: method,
		Params: payload,
		Time:   time.Now().UnixMilli(),
	})
	if len(c.lastNotifications) > maxCachedNotifications {
		c.lastNotifications = c.lastNotifications[len(c.lastNotifications)-maxCachedNotifications:]
	}
	c.mu.Unlock()

	if c.OnNotification != nil {
		c.OnNotification(method, payload)
	}
}
