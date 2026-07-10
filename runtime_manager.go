package main

import (
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"time"
)

type PromptRequest struct {
	SessionID string
	Text      string
	Images    []string
}

type PromptResponse struct {
	OperationID string
	Payload     map[string]interface{}
}

type RuntimeSnapshotOptions struct {
	LatestSeq      uint64
	Project        *ProjectInfo
	LastOperation  map[string]interface{}
	LastPermission map[string]interface{}
}

type RuntimeStartOptions struct {
	Available bool
	Cwd       string
	Error     error
}

type AgentRuntime interface {
	Name() string
	SetCwd(cwd string)
	CheckAvailable() bool
	Available() bool
	IsRunning() bool
	Stop()
	TaskStatus() map[string]interface{}
	SessionID() string
	RestoreSession(sessionID string)
	Start(cwd string) error
	LoadSession(sessionID, cwd string) (map[string]interface{}, error)
	NewSession(cwd string) (map[string]interface{}, error)
	Prompt(req PromptRequest) (PromptResponse, error)
	Cancel(sessionID string) error

	// Response/snapshot shaping. Each runtime owns the exact payload shape of
	// its RPC responses; the manager methods below simply dispatch to these.
	statusSnapshot() map[string]interface{}
	taskSnapshot() map[string]interface{}
	startResponse(options RuntimeStartOptions) map[string]interface{}
	sessionResponse(payload map[string]interface{}) map[string]interface{}
	promptAcceptedResponse(prompt PromptResponse) map[string]interface{}
	configUpdateResponse() map[string]interface{}
	actionResponse(payload map[string]interface{}) map[string]interface{}
}

type AgentRuntimeManager struct {
	runtimes map[string]AgentRuntime
}

func NewAgentRuntimeManager(runtimes ...AgentRuntime) *AgentRuntimeManager {
	manager := &AgentRuntimeManager{runtimes: make(map[string]AgentRuntime, len(runtimes))}
	for _, runtime := range runtimes {
		if runtime == nil {
			continue
		}
		manager.runtimes[runtime.Name()] = runtime
	}
	return manager
}

func (m *AgentRuntimeManager) Runtime(agent string) (AgentRuntime, error) {
	if m == nil {
		return nil, fmt.Errorf("runtime manager not initialized")
	}
	runtime, ok := m.runtimes[agent]
	if !ok {
		return nil, fmt.Errorf("unknown runtime: %s", agent)
	}
	return runtime, nil
}

func (m *AgentRuntimeManager) MustRuntime(agent string) AgentRuntime {
	runtime, err := m.Runtime(agent)
	if err != nil {
		panic(err)
	}
	return runtime
}

func (m *AgentRuntimeManager) StatusSnapshot(agent string) map[string]interface{} {
	return m.MustRuntime(agent).statusSnapshot()
}

func (m *AgentRuntimeManager) TaskSnapshot(agent string, options RuntimeSnapshotOptions) map[string]interface{} {
	snapshot := m.MustRuntime(agent).taskSnapshot()
	if snapshot == nil {
		snapshot = map[string]interface{}{}
	}
	snapshot["latestSeq"] = options.LatestSeq
	if options.Project != nil {
		snapshot["project"] = options.Project
	}
	if options.LastOperation != nil {
		snapshot["lastOperation"] = options.LastOperation
	}
	if options.LastPermission != nil {
		snapshot["lastPermission"] = options.LastPermission
	}
	return snapshot
}

func (m *AgentRuntimeManager) StartResponse(agent string, options RuntimeStartOptions) map[string]interface{} {
	return m.MustRuntime(agent).startResponse(options)
}

func (m *AgentRuntimeManager) SessionResponse(agent string, payload map[string]interface{}) map[string]interface{} {
	return m.MustRuntime(agent).sessionResponse(payload)
}

func (m *AgentRuntimeManager) PromptAcceptedResponse(agent string, prompt PromptResponse) map[string]interface{} {
	return m.MustRuntime(agent).promptAcceptedResponse(prompt)
}

func (m *AgentRuntimeManager) ConfigResponse(agent string) map[string]interface{} {
	return m.MustRuntime(agent).configUpdateResponse()
}

func (m *AgentRuntimeManager) ActionResponse(agent string, payload map[string]interface{}) map[string]interface{} {
	return m.MustRuntime(agent).actionResponse(payload)
}

func (m *AgentRuntimeManager) ClaudeRuntime() *ClaudeRuntime {
	runtime, _ := m.runtimes["claude"].(*ClaudeRuntime)
	return runtime
}

func (m *AgentRuntimeManager) CodexRuntime() *CodexRuntime {
	runtime, _ := m.runtimes["codex"].(*CodexRuntime)
	return runtime
}

func (m *AgentRuntimeManager) CursorRuntime() *CursorRuntime {
	runtime, _ := m.runtimes["cursor"].(*CursorRuntime)
	return runtime
}

func (m *AgentRuntimeManager) TraeRuntime() *TraeRuntime {
	runtime, _ := m.runtimes["trae"].(*TraeRuntime)
	return runtime
}

func cloneResponseMap(payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		return map[string]interface{}{}
	}
	clone := make(map[string]interface{}, len(payload))
	for key, value := range payload {
		clone[key] = value
	}
	return clone
}

func ensureOK(response map[string]interface{}) map[string]interface{} {
	if response == nil {
		return map[string]interface{}{"ok": true}
	}
	if _, exists := response["ok"]; !exists {
		response["ok"] = true
	}
	return response
}

func normalizeActionPayload(result interface{}) map[string]interface{} {
	switch payload := result.(type) {
	case nil:
		return nil
	case map[string]interface{}:
		return payload
	default:
		return map[string]interface{}{"result": payload}
	}
}

type AcpPermissionDelegate interface {
	HandleRequest(id interface{}, params map[string]interface{})
	Resolve(requestID, optionID string, cancelled bool) error
	Pending() []map[string]interface{}
	Clear(reason string) []map[string]interface{}
}

// AcpPermissionStore brokers interactive ACP `session/request_permission`
// prompts: it forwards each request to the client UI (emit) and resolves the
// original ACP request (respond) once the user decides. Modes returned by the
// `mode` callback that appear in `autoApprove` are answered automatically
// without prompting. It is intentionally decoupled from any concrete bridge so
// every ACP-based agent can reuse it.
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

type AcpPermissionStore struct {
	mu          sync.Mutex
	respond     func(id interface{}, result interface{}) error
	mode        func() string
	emit        func(method string, params interface{})
	autoApprove map[string]bool
	pending     map[string]*pendingPermission
}

func NewAcpPermissionStore(
	respond func(id interface{}, result interface{}) error,
	mode func() string,
	emit func(method string, params interface{}),
	autoApproveModes []string,
) *AcpPermissionStore {
	auto := make(map[string]bool, len(autoApproveModes))
	for _, m := range autoApproveModes {
		auto[m] = true
	}
	if mode == nil {
		mode = func() string { return "" }
	}
	return &AcpPermissionStore{
		respond:     respond,
		mode:        mode,
		emit:        emit,
		autoApprove: auto,
		pending:     make(map[string]*pendingPermission),
	}
}

func (s *AcpPermissionStore) HandleRequest(id interface{}, params map[string]interface{}) {
	if s.autoApprove[s.mode()] {
		if optionID, ok := pickAcpAllowOption(params); ok {
			_ = s.respond(id, map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome":  "selected",
					"optionId": optionID,
				},
			})
			return
		}
		_ = s.respond(id, map[string]interface{}{
			"outcome": map[string]interface{}{"outcome": "cancelled"},
		})
		return
	}

	requestID := fmt.Sprintf("perm-%d", time.Now().UnixNano())
	options, _ := params["options"].([]interface{})
	sessionID, _ := params["sessionId"].(string)
	toolCall, _ := params["toolCall"].(map[string]interface{})
	toolName, _ := toolCall["title"].(string)
	if toolName == "" {
		toolName, _ = toolCall["name"].(string)
	}

	createdAt := time.Now()
	pending := &pendingPermission{
		requestId: requestID,
		acpID:     id,
		options:   options,
		sessionId: sessionID,
		toolName:  toolName,
		toolCall:  toolCall,
		createdAt: createdAt,
	}
	pending.timer = time.AfterFunc(5*time.Minute, func() {
		_ = s.resolve(requestID, "", true, "timeout")
	})

	s.mu.Lock()
	s.pending[requestID] = pending
	s.mu.Unlock()

	s.emit("permission/request", map[string]interface{}{
		"requestId": requestID,
		"sessionId": sessionID,
		"toolName":  toolName,
		"toolCall":  toolCall,
		"options":   options,
		"createdAt": createdAt.UnixMilli(),
	})
}

func (s *AcpPermissionStore) Resolve(requestID, optionID string, cancelled bool) error {
	reason := "approved"
	if cancelled || optionID == "" {
		reason = "cancelled"
	}
	if !s.resolve(requestID, optionID, cancelled, reason) {
		return fmt.Errorf("no pending permission request: %s", requestID)
	}
	return nil
}

func (s *AcpPermissionStore) resolve(requestID, optionID string, cancelled bool, reason string) bool {
	s.mu.Lock()
	pending, ok := s.pending[requestID]
	if ok {
		delete(s.pending, requestID)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}

	paramsMap := map[string]interface{}{"options": pending.options}
	resolution := "approved"
	var outcome map[string]interface{}
	if cancelled || optionID == "" {
		resolution = reason
		if rejectID, ok := pickAcpRejectOption(paramsMap); ok {
			outcome = map[string]interface{}{"outcome": "selected", "optionId": rejectID}
		} else {
			outcome = map[string]interface{}{"outcome": "cancelled"}
		}
	} else {
		outcome = map[string]interface{}{"outcome": "selected", "optionId": optionID}
	}
	_ = s.respond(pending.acpID, map[string]interface{}{"outcome": outcome})
	s.emit("permission/resolved", map[string]interface{}{
		"requestId":  requestID,
		"sessionId":  pending.sessionId,
		"toolName":   pending.toolName,
		"toolCall":   pending.toolCall,
		"resolvedAs": resolution,
	})
	return true
}

func (s *AcpPermissionStore) Pending() []map[string]interface{} {
	s.mu.Lock()
	pending := make([]*pendingPermission, 0, len(s.pending))
	for requestID, permission := range s.pending {
		permission.requestId = requestID
		pending = append(pending, permission)
	}
	s.mu.Unlock()

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].createdAt.Before(pending[j].createdAt)
	})

	result := make([]map[string]interface{}, 0, len(pending))
	for _, permission := range pending {
		result = append(result, map[string]interface{}{
			"requestId": permission.requestId,
			"toolName":  permission.toolName,
			"sessionId": permission.sessionId,
			"options":   permission.options,
			"toolCall":  permission.toolCall,
			"createdAt": permission.createdAt.UnixMilli(),
		})
	}
	return result
}

func (s *AcpPermissionStore) Clear(reason string) []map[string]interface{} {
	s.mu.Lock()
	pending := make([]*pendingPermission, 0, len(s.pending))
	for requestID, permission := range s.pending {
		permission.requestId = requestID
		pending = append(pending, permission)
	}
	s.pending = make(map[string]*pendingPermission)
	s.mu.Unlock()

	resolved := make([]map[string]interface{}, 0, len(pending))
	for _, permission := range pending {
		if permission.timer != nil {
			permission.timer.Stop()
		}
		resolved = append(resolved, map[string]interface{}{
			"requestId":  permission.requestId,
			"sessionId":  permission.sessionId,
			"toolName":   permission.toolName,
			"toolCall":   permission.toolCall,
			"resolvedAs": reason,
		})
	}
	return resolved
}

// acpChatBridgeIface is the surface the shared acpRuntime needs from an
// ACP-based chat bridge. ClaudeBridge, CursorBridge and TraeBridge all
// implement it.
type acpChatBridgeIface interface {
	SetCwd(cwd string)
	CheckAvailable() bool
	Available() bool
	IsRunning() bool
	Stop()
	TaskStatus() map[string]interface{}
	SessionId() string
	RestoreSession(sessionID string)
	Start(cwd string) error
	LoadSession(sessionID, cwd string) (map[string]interface{}, error)
	NewSession(cwd string) (string, error)
	Prompt(text string, images []string) (string, error)
	Cancel()
	ConfigSnapshot() map[string]interface{}
	Capabilities() map[string]bool
}

// acpRuntime is the shared AgentRuntime implementation for ACP-based agents
// (Claude, Cursor, Trae). The concrete runtime types only differ in the
// wrapped bridge, the configResponse payload shape, and the permission-store
// wiring done in their constructors.
type acpRuntime struct {
	name            string
	bridge          acpChatBridgeIface
	permissionStore *AcpPermissionStore
	configResponse  func() map[string]interface{}
}

func (r *acpRuntime) Name() string                       { return r.name }
func (r *acpRuntime) SetCwd(cwd string)                  { r.bridge.SetCwd(cwd) }
func (r *acpRuntime) CheckAvailable() bool               { return r.bridge.CheckAvailable() }
func (r *acpRuntime) Available() bool                    { return r.bridge.Available() }
func (r *acpRuntime) IsRunning() bool                    { return r.bridge.IsRunning() }
func (r *acpRuntime) Stop()                              { r.bridge.Stop() }
func (r *acpRuntime) TaskStatus() map[string]interface{} { return r.bridge.TaskStatus() }
func (r *acpRuntime) SessionID() string                  { return r.bridge.SessionId() }
func (r *acpRuntime) RestoreSession(sessionID string)    { r.bridge.RestoreSession(sessionID) }
func (r *acpRuntime) Start(cwd string) error             { return r.bridge.Start(cwd) }
func (r *acpRuntime) LoadSession(sessionID, cwd string) (map[string]interface{}, error) {
	return r.bridge.LoadSession(sessionID, cwd)
}
func (r *acpRuntime) NewSession(cwd string) (map[string]interface{}, error) {
	sessionID, err := r.bridge.NewSession(cwd)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"sessionId": sessionID}, nil
}
func (r *acpRuntime) Prompt(req PromptRequest) (PromptResponse, error) {
	operationID, err := r.bridge.Prompt(req.Text, req.Images)
	if err != nil {
		return PromptResponse{}, err
	}
	return PromptResponse{OperationID: operationID}, nil
}
func (r *acpRuntime) Cancel(sessionID string) error {
	r.bridge.Cancel()
	return nil
}

func (r *acpRuntime) ResolvePermission(requestID, optionID string, cancelled bool) error {
	return r.permissionStore.Resolve(requestID, optionID, cancelled)
}

func (r *acpRuntime) PendingPermissions() []map[string]interface{} {
	return r.permissionStore.Pending()
}

// modelModeConfigResponse is the default configResponse for agents whose config
// is model+mode (Cursor, Trae). Claude overrides it with its
// effort/permissionMode/session* shape in NewClaudeRuntime.
func (r *acpRuntime) modelModeConfigResponse() map[string]interface{} {
	config := r.bridge.ConfigSnapshot()
	caps := r.bridge.Capabilities()
	return map[string]interface{}{
		"config":       config,
		"capabilities": caps,
		"model":        config["model"],
		"mode":         config["mode"],
	}
}

func (r *acpRuntime) startResponse(options RuntimeStartOptions) map[string]interface{} {
	response := r.configResponse()
	response["ok"] = true
	response["available"] = options.Available
	response["cwd"] = options.Cwd
	response["running"] = r.IsRunning()
	response["sessionId"] = r.SessionID()
	if options.Error != nil {
		response["running"] = false
		response["error"] = options.Error.Error()
	}
	return response
}

func (r *acpRuntime) sessionResponse(payload map[string]interface{}) map[string]interface{} {
	response := cloneResponseMap(payload)
	response["ok"] = true
	return response
}

func (r *acpRuntime) promptAcceptedResponse(prompt PromptResponse) map[string]interface{} {
	response := r.configResponse()
	response["ok"] = true
	response["operationId"] = prompt.OperationID
	response["sessionId"] = r.SessionID()
	return response
}

func (r *acpRuntime) configUpdateResponse() map[string]interface{} {
	response := r.statusSnapshot()
	response["ok"] = true
	return response
}

func (r *acpRuntime) actionResponse(payload map[string]interface{}) map[string]interface{} {
	return ensureOK(cloneResponseMap(payload))
}

func (r *acpRuntime) statusSnapshot() map[string]interface{} {
	response := r.configResponse()
	response["available"] = r.Available()
	response["running"] = r.IsRunning()
	response["sessionId"] = r.SessionID()
	return response
}

func (r *acpRuntime) taskSnapshot() map[string]interface{} {
	return r.bridge.TaskStatus()
}

type ClaudeRuntime struct {
	acpRuntime
}

func NewClaudeRuntime(bridge *ClaudeBridge) *ClaudeRuntime {
	runtime := &ClaudeRuntime{acpRuntime{name: "claude", bridge: bridge}}
	runtime.configResponse = func() map[string]interface{} {
		config := bridge.ConfigSnapshot()
		caps := bridge.Capabilities()
		return map[string]interface{}{
			"config":         config,
			"capabilities":   caps,
			"model":          config["model"],
			"effort":         config["effort"],
			"permissionMode": config["permissionMode"],
			"sessionModel":   bridge.SessionModel(),
			"sessionMode":    bridge.SessionMode(),
		}
	}
	runtime.permissionStore = NewAcpPermissionStore(
		bridge.agent.Respond,
		func() string { return canonicalClaudePermissionMode(bridge.SelectedMode()) },
		bridge.emit,
		// The permission mode is now pushed to claude-code-acp via
		// session/set_mode, so the SDK owns acceptEdits/plan/dontAsk semantics
		// (and usually won't forward a request for them). This broker only
		// needs to auto-approve the true bypass modes as a safety net.
		[]string{"bypass", "bypassPermissions"},
	)
	bridge.SetPermissionDelegate(runtime.permissionStore)
	return runtime
}

type CodexRuntime struct {
	bridge *AgentBridge

	mu          sync.Mutex
	events      []cachedNotification
	turnRunning bool
	threadID    string
	cwd         string
}

func NewCodexRuntime(bridge *AgentBridge) *CodexRuntime {
	return &CodexRuntime{bridge: bridge}
}

func (r *CodexRuntime) Name() string { return "codex" }

func (r *CodexRuntime) SetCwd(cwd string) {
	r.mu.Lock()
	r.cwd = cwd
	r.mu.Unlock()
}

func (r *CodexRuntime) CheckAvailable() bool {
	_, err := exec.LookPath(codexCommand())
	return err == nil
}

func (r *CodexRuntime) Available() bool { return r.CheckAvailable() }
func (r *CodexRuntime) IsRunning() bool { return r.bridge.IsRunning() }

func (r *CodexRuntime) Stop() {
	r.bridge.Stop()
	r.mu.Lock()
	r.events = nil
	r.turnRunning = false
	r.mu.Unlock()
}

func (r *CodexRuntime) TaskStatus() map[string]interface{} {
	return r.taskSnapshot()
}

func (r *CodexRuntime) taskSnapshot() map[string]interface{} {
	r.mu.Lock()
	events := make([]cachedNotification, len(r.events))
	copy(events, r.events)
	status := map[string]interface{}{
		"ok":           true,
		"running":      r.turnRunning,
		"codexRunning": r.bridge.IsRunning(),
		"threadId":     r.threadID,
		"recentEvents": events,
	}
	r.mu.Unlock()
	return status
}

func (r *CodexRuntime) statusSnapshot() map[string]interface{} {
	return map[string]interface{}{"running": r.IsRunning()}
}

func (r *CodexRuntime) startResponse(options RuntimeStartOptions) map[string]interface{} {
	return map[string]interface{}{"ok": options.Error == nil}
}

func (r *CodexRuntime) sessionResponse(payload map[string]interface{}) map[string]interface{} {
	response := cloneResponseMap(payload)
	response["ok"] = true
	return response
}

func (r *CodexRuntime) promptAcceptedResponse(prompt PromptResponse) map[string]interface{} {
	response := cloneResponseMap(prompt.Payload)
	response["ok"] = true
	if prompt.OperationID != "" {
		response["operationId"] = prompt.OperationID
	}
	return response
}

func (r *CodexRuntime) configUpdateResponse() map[string]interface{} {
	return map[string]interface{}{"ok": true}
}

func (r *CodexRuntime) actionResponse(payload map[string]interface{}) map[string]interface{} {
	return ensureOK(cloneResponseMap(payload))
}

func (r *CodexRuntime) SessionID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.threadID
}

func (r *CodexRuntime) RestoreSession(sessionID string) {
	r.mu.Lock()
	r.threadID = sessionID
	r.mu.Unlock()
}

func (r *CodexRuntime) Start(cwd string) error {
	if cwd == "" {
		r.mu.Lock()
		cwd = r.cwd
		r.mu.Unlock()
	} else {
		r.SetCwd(cwd)
	}
	return r.bridge.Start(codexCommand(), codexAppServerArgs(), cwd)
}

func (r *CodexRuntime) LoadSession(sessionID, cwd string) (map[string]interface{}, error) {
	return nil, fmt.Errorf("codex runtime does not support loadSession")
}

func (r *CodexRuntime) NewSession(cwd string) (map[string]interface{}, error) {
	return nil, fmt.Errorf("codex runtime does not support newSession")
}

func (r *CodexRuntime) Prompt(req PromptRequest) (PromptResponse, error) {
	return PromptResponse{}, fmt.Errorf("codex runtime does not support prompt")
}

func (r *CodexRuntime) Cancel(sessionID string) error {
	return fmt.Errorf("codex runtime does not support cancel")
}

func (r *CodexRuntime) ConfigWrite(params map[string]interface{}) (map[string]interface{}, error) {
	result, err := r.bridge.Send("config/value/write", params)
	if err != nil {
		return nil, err
	}
	return normalizeActionPayload(result), nil
}

func (r *CodexRuntime) Respond(requestID interface{}, result interface{}) error {
	return r.bridge.Respond(requestID, result)
}

func (r *CodexRuntime) RecordEvent(method string, params interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch method {
	case "turn/started":
		r.turnRunning = true
		r.events = r.events[:0]
	case "turn/completed", "turn/failed", "turn/aborted", "turn/interrupted":
		r.turnRunning = false
	case "thread/started":
		if id := extractThreadID(params); id != "" {
			r.threadID = id
		}
	}
	if id := extractThreadID(params); id != "" {
		r.threadID = id
	}

	r.events = append(r.events, cachedNotification{
		Method: method,
		Params: params,
		Time:   time.Now().UnixMilli(),
	})
	if len(r.events) > maxCachedNotifications {
		r.events = r.events[len(r.events)-maxCachedNotifications:]
	}
}

// CursorRuntime adapts the Cursor CLI (run as an ACP server via `agent acp`) to
// the AgentRuntime interface. Cursor's config is model+mode (agent/plan/ask)
// instead of Claude's model/effort/permissionMode.
type CursorRuntime struct {
	acpRuntime
}

func NewCursorRuntime(bridge *CursorBridge) *CursorRuntime {
	runtime := &CursorRuntime{acpRuntime{name: "cursor", bridge: bridge}}
	runtime.configResponse = runtime.modelModeConfigResponse
	// Cursor has no "auto-approve" permission mode of its own — every tool
	// request is surfaced to the UI for an explicit decision.
	runtime.permissionStore = NewAcpPermissionStore(
		bridge.agent.Respond,
		func() string { return "" },
		bridge.emit,
		nil,
	)
	bridge.SetPermissionDelegate(runtime.permissionStore)
	return runtime
}

// TraeRuntime adapts the Trae CLI (run as an ACP server via `traecli acp serve`)
// to the AgentRuntime interface. Trae's config is model+mode like Cursor, but
// its mode vocabulary is whatever the agent advertises rather than a fixed
// agent/plan/ask set.
type TraeRuntime struct {
	acpRuntime
}

func NewTraeRuntime(bridge *TraeBridge) *TraeRuntime {
	runtime := &TraeRuntime{acpRuntime{name: "trae", bridge: bridge}}
	runtime.configResponse = runtime.modelModeConfigResponse
	// Trae has no "auto-approve" permission mode of its own — every tool
	// request is surfaced to the UI for an explicit decision.
	runtime.permissionStore = NewAcpPermissionStore(
		bridge.agent.Respond,
		func() string { return "" },
		bridge.emit,
		nil,
	)
	bridge.SetPermissionDelegate(runtime.permissionStore)
	return runtime
}
