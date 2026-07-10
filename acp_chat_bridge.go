package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// maxCachedNotifications bounds the current-turn replay buffer we keep for
// reconnect recovery while an agent task is in flight.
const maxCachedNotifications = 300

// cachedNotification stores a single notification for replay after
// iOS app reconnect.
type cachedNotification struct {
	Method string      `json:"method"`
	Params interface{} `json:"params"`
	Time   int64       `json:"time"`
}

// agentModelList is a small helper so init/taskStatus payloads can carry the
// captured model options (ListModels never errors).
func agentModelList(a *AcpAgent) []AgentModelOption {
	models, _ := a.ListModels()
	return models
}

// acpChatBridgeHooks injects the per-agent behavior into the shared
// acpChatBridge core. Everything protocol-visible (error strings, config key
// canonicalization, mode fallbacks) is routed through these hooks so each
// agent keeps its exact wire behavior.
type acpChatBridgeHooks struct {
	// id is the log + operation-id prefix (e.g. "cursor"); label is the human
	// name used in error messages (e.g. "Cursor").
	id    string
	label string

	// ensureInstalled verifies (and possibly installs) the agent binary. It
	// receives the agent's CheckAvailable and returns false when the CLI is
	// genuinely unavailable. Defaults to just running the check.
	ensureInstalled func(check func() bool) bool

	// startUnavailableError / loadUnavailableError build the error returned
	// when the CLI is missing during Start / ensureAgentSessionLoaded.
	startUnavailableError func() error
	loadUnavailableError  func() error

	canonicalModel func(string) string
	canonicalMode  func(string) string

	// effectiveMode resolves the mode surfaced in session init payloads.
	// Defaults to the canonical selection as-is (Cursor); Trae falls back to
	// the mode the agent reported as current.
	effectiveMode func(selected string) string

	// newSession lets the shared Prompt create a session through the outer
	// bridge's NewSession (which Claude overrides), since Go embedding does
	// not virtually dispatch method calls.
	newSession func(cwd string) (string, error)

	// afterModelSetLocked runs under the bridge mutex after a successful
	// SetModel (Claude mirrors the selection into its sessionModel).
	afterModelSetLocked func(model string)
}

// acpChatBridge is the shared core for ACP-based chat bridges (Claude, Cursor,
// Trae). It owns the ACP agent handle, the current-session/task tracking, the
// current-turn replay buffer and the permission delegation plumbing. The
// concrete bridges embed it and keep only their genuine differences (spawn
// command, config vocabulary, session listing, Claude's JSONL transcript and
// permission-mode handling).
//
// Locking: mu guards the mutable fields below and must not be held across
// agent stdin writes; agentMu serializes agent lifecycle + session RPCs (the
// AgentBridge writeMu already serializes raw pipe writes).
type acpChatBridge struct {
	mu      sync.Mutex
	agentMu sync.Mutex

	agent *AcpAgent
	hooks acpChatBridgeHooks

	cwd            string
	currentSession string
	selectedModel  string
	selectedMode   string

	// Pending permission requests awaiting user decision via
	// <agent>.permission/respond, brokered by the runtime's AcpPermissionStore.
	permissionDelegate AcpPermissionDelegate

	// Task tracking: lets clients query whether a task is in progress after
	// reconnecting from background.
	taskRunning        bool
	taskStartedAt      time.Time
	currentOperationID string

	// Ring buffer of recent notifications for replay on reconnect.
	lastNotifications []cachedNotification

	OnNotification func(method string, params interface{})
}

// initAcpChatBridge wires the ACP agent and hook defaults. Concrete bridges
// call it from their constructors.
func (b *acpChatBridge) initAcpChatBridge(agentConfig AcpAgentConfig, hooks acpChatBridgeHooks) {
	b.hooks = hooks
	if b.hooks.ensureInstalled == nil {
		b.hooks.ensureInstalled = func(check func() bool) bool { return check() }
	}
	if b.hooks.effectiveMode == nil {
		b.hooks.effectiveMode = func(selected string) string { return selected }
	}
	b.agent = NewAcpAgent(agentConfig)
	b.agent.OnNotification = func(method string, params interface{}) {
		// Track sessionId from session updates so resume/cancel work.
		if p, ok := params.(map[string]interface{}); ok {
			if sid, _ := p["sessionId"].(string); sid != "" {
				b.mu.Lock()
				if b.currentSession == "" {
					b.currentSession = sid
				}
				b.mu.Unlock()
			}
		}
		b.emit(method, params)

		// Detect terminal turn states to clear taskRunning.
		if method == "turn/completed" || method == "turn/failed" || method == "turn/aborted" || method == "turn/interrupted" {
			b.mu.Lock()
			b.taskRunning = false
			b.mu.Unlock()
		}
	}
	b.agent.OnPermissionRequest = b.handlePermissionRequest
}

func (b *acpChatBridge) SetPermissionDelegate(delegate AcpPermissionDelegate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.permissionDelegate = delegate
}

// handlePermissionRequest is invoked when the ACP agent asks the client to
// approve/deny a tool invocation; it forwards to the permission store, which
// emits `<agent>.permission/request` to the UI and auto-rejects after 5
// minutes with no reply.
func (b *acpChatBridge) handlePermissionRequest(id interface{}, params map[string]interface{}) {
	b.mu.Lock()
	delegate := b.permissionDelegate
	b.mu.Unlock()
	if delegate != nil {
		delegate.HandleRequest(id, params)
	}
}

func (b *acpChatBridge) SetCwd(cwd string) {
	b.mu.Lock()
	b.cwd = cwd
	b.mu.Unlock()
	b.agent.SetCwd(cwd)
}

func (b *acpChatBridge) IsRunning() bool { return b.agent.IsRunning() }
func (b *acpChatBridge) Available() bool { return b.agent.Available() }

func (b *acpChatBridge) SessionId() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.currentSession
}

func (b *acpChatBridge) RestoreSession(sessionId string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.currentSession = sessionId
}

func (b *acpChatBridge) SelectedModel() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.selectedModel
}

func (b *acpChatBridge) SelectedMode() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.selectedMode
}

// effectiveModel resolves the model to surface to the UI: the user's explicit
// selection when set, otherwise the model the ACP agent reported as current in
// its latest session response. (Claude overrides this to treat "default" as
// unset.)
func (b *acpChatBridge) effectiveModel(selected string) string {
	if selected != "" {
		return selected
	}
	return b.agent.CurrentModelID()
}

// ConfigSnapshot reports the model+mode config vocabulary shared by Cursor and
// Trae. Claude overrides it with its model/effort/permissionMode shape.
func (b *acpChatBridge) ConfigSnapshot() map[string]interface{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]interface{}{
		"model": b.hooks.canonicalModel(b.selectedModel),
		"mode":  b.hooks.canonicalMode(b.selectedMode),
	}
}

func (b *acpChatBridge) Capabilities() map[string]bool {
	caps := b.agent.Capabilities()
	return map[string]bool{
		"canSetModel": caps.CanSetModel,
		"canSetMode":  caps.CanSetMode,
	}
}

func (b *acpChatBridge) CheckAvailable() bool {
	return b.agent.CheckAvailable()
}

// Start ensures the ACP agent subprocess is running. The caller can pass a
// cwd to update the working directory used for newly created sessions.
func (b *acpChatBridge) Start(cwd string) error {
	b.mu.Lock()
	if cwd != "" {
		b.cwd = cwd
	}
	b.mu.Unlock()
	if cwd != "" {
		b.agent.SetCwd(cwd)
	}
	if !b.hooks.ensureInstalled(b.agent.CheckAvailable) {
		return b.hooks.startUnavailableError()
	}
	b.agentMu.Lock()
	err := b.agent.Start()
	b.agentMu.Unlock()
	return err
}

func (b *acpChatBridge) Stop() {
	b.agent.Stop()
	b.mu.Lock()
	delegate := b.permissionDelegate
	b.mu.Unlock()
	resolved := []map[string]interface{}{}
	if delegate != nil {
		resolved = delegate.Clear("stopped")
	}
	b.mu.Lock()
	b.currentSession = ""
	b.taskRunning = false
	b.taskStartedAt = time.Time{}
	b.currentOperationID = ""
	b.lastNotifications = nil
	b.mu.Unlock()
	for _, notif := range resolved {
		b.emit("permission/resolved", notif)
	}
}

func (b *acpChatBridge) Cancel() {
	b.mu.Lock()
	sid := b.currentSession
	b.mu.Unlock()
	if sid != "" {
		_ = b.agent.Cancel(sid)
	}
}

// ListModels returns the models advertised by the ACP agent. Agents only
// report models in their session/new and session/load responses, so ensure the
// current session is loaded first to populate the cache.
func (b *acpChatBridge) ListModels() ([]AgentModelOption, error) {
	b.mu.Lock()
	sessionID := b.currentSession
	cwd := b.cwd
	b.mu.Unlock()
	if sessionID != "" {
		if err := b.ensureAgentSessionLoaded(sessionID, cwd); err != nil {
			return nil, err
		}
	} else if !b.agent.IsRunning() {
		if err := b.Start(cwd); err != nil {
			return nil, err
		}
	}
	return b.agent.ListModels()
}

func (b *acpChatBridge) SetModel(model string) error {
	model = b.hooks.canonicalModel(model)
	if model == "" {
		return fmt.Errorf("model is required")
	}
	b.mu.Lock()
	sid := b.currentSession
	cwd := b.cwd
	b.mu.Unlock()
	if sid == "" {
		return fmt.Errorf("no active %s session to set model", b.hooks.label)
	}
	if err := b.ensureAgentSessionLoaded(sid, cwd); err != nil {
		return err
	}
	b.agentMu.Lock()
	err := b.agent.SetModel(sid, model)
	b.agentMu.Unlock()
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.selectedModel = model
	if b.hooks.afterModelSetLocked != nil {
		b.hooks.afterModelSetLocked(model)
	}
	b.mu.Unlock()
	return nil
}

// SetConfig stores the desired mode. Model changes must go through SetModel so
// the UI only reports success after the backend ACP session accepts the change.
// (Claude shadows this with its ClaudeConfigPatch variant.)
func (b *acpChatBridge) SetConfig(model, mode *string) {
	b.mu.Lock()
	if mode != nil {
		b.selectedMode = b.hooks.canonicalMode(*mode)
	}
	sid := b.currentSession
	selectedMode := b.selectedMode
	b.mu.Unlock()

	if sid == "" {
		return
	}
	if mode != nil && selectedMode != "" {
		if err := b.agent.SetMode(sid, selectedMode); err != nil {
			log.Printf("[%s] setMode failed: %v", b.hooks.id, err)
		}
	}
}

// Prompt sends a user turn to the active session, creating one if needed. It
// runs the ACP prompt asynchronously and synthesizes a turn/completed event on
// completion (ACP itself has no turn lifecycle notifications).
func (b *acpChatBridge) Prompt(text string, images []string) (string, error) {
	b.mu.Lock()
	sid := b.currentSession
	cwd := b.cwd
	b.mu.Unlock()

	if sid == "" {
		newSid, err := b.hooks.newSession(cwd)
		if err != nil {
			return "", err
		}
		sid = newSid
	} else if err := b.ensureAgentSessionLoaded(sid, cwd); err != nil {
		return "", fmt.Errorf("session/load before prompt: %w", err)
	}
	opID := newOperationID(b.hooks.id)

	b.mu.Lock()
	b.taskRunning = true
	b.taskStartedAt = time.Now()
	b.currentOperationID = opID
	b.lastNotifications = nil
	b.mu.Unlock()

	go func() {
		result, err := b.agent.Prompt(sid, text, images)
		if err != nil {
			log.Printf("[%s] session/prompt failed: %v", b.hooks.id, err)
			b.emit("error", map[string]interface{}{
				"error":     err.Error(),
				"sessionId": sid,
			})
		}
		b.mu.Lock()
		b.taskRunning = false
		b.mu.Unlock()
		b.emit("turn/completed", map[string]interface{}{
			"sessionId": sid,
			"success":   err == nil,
			"result":    result,
		})
		b.mu.Lock()
		b.currentOperationID = ""
		b.mu.Unlock()
	}()
	return opID, nil
}

// NewSession creates a fresh ACP session and emits the synthetic `init`
// notification for the model+mode agents (Cursor, Trae). Claude shadows this
// with its own version carrying effort/permissionMode and set_mode pushes.
func (b *acpChatBridge) NewSession(cwd string) (string, error) {
	if cwd == "" {
		b.mu.Lock()
		cwd = b.cwd
		b.mu.Unlock()
	}
	if !b.agent.IsRunning() {
		if err := b.Start(cwd); err != nil {
			return "", err
		}
	} else if cwd != "" {
		b.SetCwd(cwd)
	}

	b.mu.Lock()
	selectedModel := b.hooks.canonicalModel(b.selectedModel)
	selectedMode := b.hooks.canonicalMode(b.selectedMode)
	b.mu.Unlock()

	b.agentMu.Lock()
	result, err := b.agent.NewSession(cwd)
	b.agentMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("session/new: %w", err)
	}
	newSid, _ := result["sessionId"].(string)
	if newSid == "" {
		return "", fmt.Errorf("session/new returned no sessionId")
	}

	selectedModel = b.effectiveModel(selectedModel)
	selectedMode = b.hooks.effectiveMode(selectedMode)

	b.mu.Lock()
	b.currentSession = newSid
	b.selectedModel = selectedModel
	b.mu.Unlock()

	b.emit("init", map[string]interface{}{
		"sessionId": newSid,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model": selectedModel,
			"mode":  selectedMode,
		},
		"capabilities":    b.Capabilities(),
		"model":           selectedModel,
		"mode":            selectedMode,
		"availableModels": agentModelList(b.agent),
	})
	return newSid, nil
}

// LoadSession resumes a session via ACP session/load for the model+mode agents
// (Cursor, Trae). There is no local transcript to rebuild, so `items` is empty
// and the UI relies on the live ACP stream from the resumed session. Claude
// shadows this with its JSONL-transcript-backed version.
func (b *acpChatBridge) LoadSession(sessionId, cwd string) (map[string]interface{}, error) {
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if cwd == "" {
		b.mu.Lock()
		cwd = b.cwd
		b.mu.Unlock()
	}

	b.mu.Lock()
	if b.taskRunning && b.currentSession != "" && b.currentSession != sessionId {
		runningSession := b.currentSession
		b.mu.Unlock()
		return nil, fmt.Errorf("cannot load session %s while session %s is running", sessionId, runningSession)
	}
	b.mu.Unlock()

	if err := b.ensureAgentSessionLoaded(sessionId, cwd); err != nil {
		return nil, fmt.Errorf("session/load: %w", err)
	}

	b.mu.Lock()
	b.currentSession = sessionId
	if cwd != "" {
		b.cwd = cwd
	}
	selectedModel := b.effectiveModel(b.hooks.canonicalModel(b.selectedModel))
	selectedMode := b.hooks.effectiveMode(b.hooks.canonicalMode(b.selectedMode))
	b.selectedModel = selectedModel
	b.mu.Unlock()

	availableModels := agentModelList(b.agent)
	b.emit("init", map[string]interface{}{
		"sessionId": sessionId,
		"cwd":       cwd,
		"config": map[string]interface{}{
			"model": selectedModel,
			"mode":  selectedMode,
		},
		"capabilities":    b.Capabilities(),
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
		"capabilities":    b.Capabilities(),
		"availableModels": availableModels,
		"agentLoaded":     b.agent.IsSessionLoaded(sessionId),
	}, nil
}

func (b *acpChatBridge) ensureAgentSessionLoaded(sessionId, cwd string) error {
	b.agentMu.Lock()
	defer b.agentMu.Unlock()
	if !b.agent.IsRunning() {
		if !b.hooks.ensureInstalled(b.agent.CheckAvailable) {
			return b.hooks.loadUnavailableError()
		}
	}
	return b.agent.EnsureLoaded(sessionId, cwd)
}

// TaskStatus returns the current task state for clients to recover after
// reconnecting from background or a page refresh, using the model+mode config
// vocabulary. Claude shadows this to add its effort/permissionMode/session*
// keys.
func (b *acpChatBridge) TaskStatus() map[string]interface{} {
	b.mu.Lock()
	delegate := b.permissionDelegate
	events := make([]cachedNotification, len(b.lastNotifications))
	copy(events, b.lastNotifications)
	config := map[string]interface{}{
		"model": b.hooks.canonicalModel(b.selectedModel),
		"mode":  b.hooks.canonicalMode(b.selectedMode),
	}
	capabilities := b.Capabilities()
	running := b.taskRunning
	operationID := b.currentOperationID
	sessionID := b.currentSession
	startedAt := b.taskStartedAt
	cwd := b.cwd
	b.mu.Unlock()

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
		"mode":            config["mode"],
		"availableModels": agentModelList(b.agent),
	}
}

func (b *acpChatBridge) emit(method string, params interface{}) {
	// Cache the notification for replay on reconnect.
	b.mu.Lock()
	payload := attachOperationID(params, b.currentOperationID)
	b.lastNotifications = append(b.lastNotifications, cachedNotification{
		Method: method,
		Params: payload,
		Time:   time.Now().UnixMilli(),
	})
	// Trim to ring buffer size.
	if len(b.lastNotifications) > maxCachedNotifications {
		b.lastNotifications = b.lastNotifications[len(b.lastNotifications)-maxCachedNotifications:]
	}
	b.mu.Unlock()

	if b.OnNotification != nil {
		b.OnNotification(method, payload)
	}
}
