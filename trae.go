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

// TraeBridge runs the Trae CLI through the standard Agent Client Protocol (ACP)
// using `traecli acp serve` (Trae's built-in ACP server mode). It mirrors
// CursorBridge — both are pure ACP agents with no on-disk JSONL transcript and
// share the interactive permission flow and current-turn replay buffer. The
// only difference is the spawn command and that Trae advertises its own set of
// modes (rather than Cursor's fixed agent/plan/ask), so mode handling here is
// generic pass-through driven by what the agent reports in its session response.
type TraeBridge struct {
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

// traeCommand resolves the Trae CLI binary. The trae.cn installer exposes it as
// `traecli`. Override with ANYCODE_TRAE_BIN (mirrors ANYCODE_CURSOR_BIN /
// ANYCODE_CODEX_BIN).
func traeCommand() string {
	if v := strings.TrimSpace(os.Getenv("ANYCODE_TRAE_BIN")); v != "" {
		return v
	}
	for _, candidate := range []string{"traecli", "trae-cli", "trae-agent", "ta"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	return "traecli"
}

func canonicalTraeModel(value string) string {
	return strings.TrimSpace(value)
}

// effectiveModel resolves the model to surface to the UI: the user's explicit
// selection when set, otherwise the model the Trae ACP agent reported as current
// in its latest session response.
func (t *TraeBridge) effectiveModel(selected string) string {
	if selected != "" {
		return selected
	}
	return t.agent.CurrentModelID()
}

// canonicalTraeMode normalizes a mode value. Unlike Cursor we don't pin a fixed
// vocabulary — Trae advertises its own modes via the ACP session response, so we
// only trim and pass through whatever the client selected.
func canonicalTraeMode(value string) string {
	return strings.TrimSpace(value)
}

// effectiveMode resolves the mode to surface: the user's explicit selection when
// set, otherwise the mode the Trae ACP agent reported as current.
func (t *TraeBridge) effectiveMode(selected string) string {
	if selected != "" {
		return selected
	}
	return t.agent.CurrentModeID()
}

func NewTraeBridge() *TraeBridge {
	t := &TraeBridge{}
	t.agent = NewAcpAgent(AcpAgentConfig{
		ID:          "trae",
		Label:       "Trae",
		Command:     traeCommand(),
		Args:        []string{"acp", "serve"},
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
	t.agent.OnNotification = func(method string, params interface{}) {
		if p, ok := params.(map[string]interface{}); ok {
			if sid, _ := p["sessionId"].(string); sid != "" {
				t.mu.Lock()
				if t.currentSession == "" {
					t.currentSession = sid
				}
				t.mu.Unlock()
			}
		}
		t.emit(method, params)
		if method == "turn/completed" || method == "turn/failed" || method == "turn/aborted" || method == "turn/interrupted" {
			t.mu.Lock()
			t.taskRunning = false
			t.mu.Unlock()
		}
	}
	t.agent.OnPermissionRequest = t.handlePermissionRequest
	return t
}

func (t *TraeBridge) SetPermissionDelegate(delegate ClaudePermissionDelegate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.permissionDelegate = delegate
}

func (t *TraeBridge) handlePermissionRequest(id interface{}, params map[string]interface{}) {
	t.mu.Lock()
	delegate := t.permissionDelegate
	t.mu.Unlock()
	if delegate != nil {
		delegate.HandleRequest(id, params)
	}
}

func (t *TraeBridge) RespondPermission(requestId, optionId string, cancelled bool) error {
	t.mu.Lock()
	delegate := t.permissionDelegate
	t.mu.Unlock()
	if delegate == nil {
		return fmt.Errorf("permission delegate not configured")
	}
	return delegate.Resolve(requestId, optionId, cancelled)
}

func (t *TraeBridge) SetCwd(cwd string) {
	t.mu.Lock()
	t.cwd = cwd
	t.mu.Unlock()
	t.agent.SetCwd(cwd)
}

func (t *TraeBridge) IsRunning() bool { return t.agent.IsRunning() }
func (t *TraeBridge) Available() bool { return t.agent.Available() }

func (t *TraeBridge) SessionId() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.currentSession
}

func (t *TraeBridge) RestoreSession(sessionId string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currentSession = sessionId
}

func (t *TraeBridge) SelectedModel() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.selectedModel
}

func (t *TraeBridge) SelectedMode() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.selectedMode
}

func (t *TraeBridge) ConfigSnapshot() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]interface{}{
		"model": canonicalTraeModel(t.selectedModel),
		"mode":  canonicalTraeMode(t.selectedMode),
	}
}

func (t *TraeBridge) Capabilities() map[string]bool {
	caps := t.agent.Capabilities()
	return map[string]bool{
		"canSetModel": caps.CanSetModel,
		"canSetMode":  caps.CanSetMode,
	}
}

func (t *TraeBridge) CheckAvailable() bool {
	return t.agent.CheckAvailable()
}

func (t *TraeBridge) Start(cwd string) error {
	t.mu.Lock()
	if cwd != "" {
		t.cwd = cwd
	}
	t.mu.Unlock()
	if cwd != "" {
		t.agent.SetCwd(cwd)
	}
	if !t.agent.CheckAvailable() {
		return fmt.Errorf("%s not found in PATH; install the Trae CLI from https://docs.trae.cn/cli (or set ANYCODE_TRAE_BIN)", traeCommand())
	}
	t.agentMu.Lock()
	err := t.agent.Start()
	t.agentMu.Unlock()
	return err
}

func (t *TraeBridge) Stop() {
	t.agent.Stop()
	t.mu.Lock()
	delegate := t.permissionDelegate
	t.mu.Unlock()
	resolved := []map[string]interface{}{}
	if delegate != nil {
		resolved = delegate.Clear("stopped")
	}
	t.mu.Lock()
	t.currentSession = ""
	t.taskRunning = false
	t.taskStartedAt = time.Time{}
	t.currentOperationID = ""
	t.lastNotifications = nil
	t.mu.Unlock()
	for _, notif := range resolved {
		t.emit("permission/resolved", notif)
	}
}

func (t *TraeBridge) Cancel() {
	t.mu.Lock()
	sid := t.currentSession
	t.mu.Unlock()
	if sid != "" {
		_ = t.agent.Cancel(sid)
	}
}

// ListModels returns the models advertised by the Trae ACP agent. Trae (like
// Cursor / claude-code-acp) only reports models in its session/new and
// session/load responses, so ensure the current session is loaded first to
// populate the cache.
func (t *TraeBridge) ListModels() ([]AgentModelOption, error) {
	t.mu.Lock()
	sessionID := t.currentSession
	cwd := t.cwd
	t.mu.Unlock()
	if sessionID != "" {
		if err := t.ensureAgentSessionLoaded(sessionID, cwd); err != nil {
			return nil, err
		}
	} else if !t.agent.IsRunning() {
		if err := t.Start(cwd); err != nil {
			return nil, err
		}
	}
	return t.agent.ListModels()
}

func (t *TraeBridge) SetModel(model string) error {
	model = canonicalTraeModel(model)
	if model == "" {
		return fmt.Errorf("model is required")
	}
	t.mu.Lock()
	sid := t.currentSession
	cwd := t.cwd
	t.mu.Unlock()
	if sid == "" {
		return fmt.Errorf("no active Trae session to set model")
	}
	if err := t.ensureAgentSessionLoaded(sid, cwd); err != nil {
		return err
	}
	t.agentMu.Lock()
	err := t.agent.SetModel(sid, model)
	t.agentMu.Unlock()
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.selectedModel = model
	t.mu.Unlock()
	return nil
}

// SetConfig stores the desired mode. Model changes must go through SetModel so
// the UI only reports success after the backend ACP session accepts the change.
func (t *TraeBridge) SetConfig(model, mode *string) {
	t.mu.Lock()
	if mode != nil {
		t.selectedMode = canonicalTraeMode(*mode)
	}
	sid := t.currentSession
	selectedMode := t.selectedMode
	t.mu.Unlock()

	if sid == "" {
		return
	}
	if mode != nil && selectedMode != "" {
		if err := t.agent.SetMode(sid, selectedMode); err != nil {
			log.Printf("[trae] setMode failed: %v", err)
		}
	}
}

// Prompt sends a user turn to the active session, creating one if needed. It
// runs the ACP prompt asynchronously and synthesizes a turn/completed event on
// completion (ACP itself has no turn lifecycle notifications), matching Cursor.
func (t *TraeBridge) Prompt(text string, images []string) (string, error) {
	t.mu.Lock()
	sid := t.currentSession
	cwd := t.cwd
	t.mu.Unlock()

	if sid == "" {
		newSid, err := t.NewSession(cwd)
		if err != nil {
			return "", err
		}
		sid = newSid
	} else if err := t.ensureAgentSessionLoaded(sid, cwd); err != nil {
		return "", fmt.Errorf("session/load before prompt: %w", err)
	}
	opID := newOperationID("trae")

	t.mu.Lock()
	t.taskRunning = true
	t.taskStartedAt = time.Now()
	t.currentOperationID = opID
	t.lastNotifications = nil
	t.mu.Unlock()

	go func() {
		result, err := t.agent.Prompt(sid, text, images)
		if err != nil {
			log.Printf("[trae] session/prompt failed: %v", err)
			t.emit("error", map[string]interface{}{
				"error":     err.Error(),
				"sessionId": sid,
			})
		}
		t.mu.Lock()
		t.taskRunning = false
		t.mu.Unlock()
		t.emit("turn/completed", map[string]interface{}{
			"sessionId": sid,
			"success":   err == nil,
			"result":    result,
		})
		t.mu.Lock()
		t.currentOperationID = ""
		t.mu.Unlock()
	}()
	return opID, nil
}

func (t *TraeBridge) NewSession(cwd string) (string, error) {
	if cwd == "" {
		t.mu.Lock()
		cwd = t.cwd
		t.mu.Unlock()
	}
	if !t.agent.IsRunning() {
		if err := t.Start(cwd); err != nil {
			return "", err
		}
	} else if cwd != "" {
		t.SetCwd(cwd)
	}

	t.mu.Lock()
	selectedModel := canonicalTraeModel(t.selectedModel)
	selectedMode := canonicalTraeMode(t.selectedMode)
	t.mu.Unlock()

	t.agentMu.Lock()
	result, err := t.agent.NewSession(cwd)
	t.agentMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}
	newSid, _ := result["sessionId"].(string)
	if newSid == "" {
		return "", fmt.Errorf("session/new returned no sessionId")
	}

	selectedModel = t.effectiveModel(selectedModel)
	selectedMode = t.effectiveMode(selectedMode)

	t.mu.Lock()
	t.currentSession = newSid
	t.selectedModel = selectedModel
	t.mu.Unlock()

	t.emit("init", map[string]interface{}{
		"sessionId": newSid,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model": selectedModel,
			"mode":  selectedMode,
		},
		"capabilities":    t.Capabilities(),
		"model":           selectedModel,
		"mode":            selectedMode,
		"availableModels": agentModelList(t.agent),
	})
	return newSid, nil
}

// LoadSession resumes a session via ACP session/load. Unlike Claude there is no
// local transcript to rebuild, so `items` is empty and the UI relies on the live
// ACP stream from the resumed session.
func (t *TraeBridge) LoadSession(sessionId, cwd string) (map[string]interface{}, error) {
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if cwd == "" {
		t.mu.Lock()
		cwd = t.cwd
		t.mu.Unlock()
	}

	t.mu.Lock()
	if t.taskRunning && t.currentSession != "" && t.currentSession != sessionId {
		runningSession := t.currentSession
		t.mu.Unlock()
		return nil, fmt.Errorf("cannot load session %s while session %s is running", sessionId, runningSession)
	}
	t.mu.Unlock()

	if err := t.ensureAgentSessionLoaded(sessionId, cwd); err != nil {
		return nil, fmt.Errorf("session/load: %w", err)
	}

	t.mu.Lock()
	t.currentSession = sessionId
	if cwd != "" {
		t.cwd = cwd
	}
	selectedModel := t.effectiveModel(canonicalTraeModel(t.selectedModel))
	selectedMode := t.effectiveMode(canonicalTraeMode(t.selectedMode))
	t.selectedModel = selectedModel
	t.mu.Unlock()

	availableModels := agentModelList(t.agent)
	t.emit("init", map[string]interface{}{
		"sessionId": sessionId,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model": selectedModel,
			"mode":  selectedMode,
		},
		"capabilities":    t.Capabilities(),
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
		"capabilities":    t.Capabilities(),
		"availableModels": availableModels,
		"agentLoaded":     t.agent.IsSessionLoaded(sessionId),
	}, nil
}

func (t *TraeBridge) ensureAgentSessionLoaded(sessionId, cwd string) error {
	t.agentMu.Lock()
	defer t.agentMu.Unlock()
	if !t.agent.IsRunning() {
		if !t.agent.CheckAvailable() {
			return fmt.Errorf("%s not found in PATH; install the Trae CLI from https://docs.trae.cn/cli", traeCommand())
		}
	}
	return t.agent.EnsureLoaded(sessionId, cwd)
}

// ListSessions reports historical Trae conversations. Like the Cursor CLI, Trae
// exposes no non-interactive "print sessions as JSON" command, so session
// listing degrades to an empty list: the live session created via ACP
// (`session/new`) is still tracked and surfaced through taskStatus /
// notifications, which is what the clients rely on.
func (t *TraeBridge) ListSessions(cwd string) (map[string]interface{}, error) {
	return map[string]interface{}{
		"ok":       true,
		"sessions": []map[string]interface{}{},
	}, nil
}

func (t *TraeBridge) TaskStatus() map[string]interface{} {
	t.mu.Lock()
	delegate := t.permissionDelegate
	events := make([]cachedNotification, len(t.lastNotifications))
	copy(events, t.lastNotifications)
	config := map[string]interface{}{
		"model": canonicalTraeModel(t.selectedModel),
		"mode":  canonicalTraeMode(t.selectedMode),
	}
	capabilities := t.Capabilities()
	t.mu.Unlock()

	pendingPermList := []map[string]interface{}{}
	if delegate != nil {
		pendingPermList = delegate.Pending()
	}

	return map[string]interface{}{
		"ok":              true,
		"running":         t.taskRunning,
		"operationId":     t.currentOperationID,
		"sessionId":       t.currentSession,
		"startedAt":       t.taskStartedAt.UnixMilli(),
		"recentEvents":    events,
		"pendingPerms":    pendingPermList,
		"cwd":             t.cwd,
		"config":          config,
		"capabilities":    capabilities,
		"model":           config["model"],
		"mode":            config["mode"],
		"availableModels": agentModelList(t.agent),
	}
}

func (t *TraeBridge) emit(method string, params interface{}) {
	t.mu.Lock()
	payload := attachOperationID(params, t.currentOperationID)
	t.lastNotifications = append(t.lastNotifications, cachedNotification{
		Method: method,
		Params: payload,
		Time:   time.Now().UnixMilli(),
	})
	if len(t.lastNotifications) > maxCachedNotifications {
		t.lastNotifications = t.lastNotifications[len(t.lastNotifications)-maxCachedNotifications:]
	}
	t.mu.Unlock()

	if t.OnNotification != nil {
		t.OnNotification(method, payload)
	}
}
