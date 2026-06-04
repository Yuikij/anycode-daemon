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
	switch runtime := m.MustRuntime(agent).(type) {
	case *ClaudeRuntime:
		return runtime.statusSnapshot()
	case *GeminiRuntime:
		return runtime.statusSnapshot()
	case *CodexRuntime:
		return runtime.statusSnapshot()
	default:
		return map[string]interface{}{}
	}
}

func (m *AgentRuntimeManager) TaskSnapshot(agent string, options RuntimeSnapshotOptions) map[string]interface{} {
	var snapshot map[string]interface{}
	switch runtime := m.MustRuntime(agent).(type) {
	case *ClaudeRuntime:
		snapshot = runtime.taskSnapshot()
	case *GeminiRuntime:
		snapshot = runtime.taskSnapshot()
	case *CodexRuntime:
		snapshot = runtime.taskSnapshot()
	default:
		snapshot = map[string]interface{}{}
	}
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
	switch runtime := m.MustRuntime(agent).(type) {
	case *ClaudeRuntime:
		return runtime.startResponse(options)
	case *GeminiRuntime:
		return runtime.startResponse(options)
	case *CodexRuntime:
		return runtime.startResponse(options)
	default:
		return map[string]interface{}{"ok": options.Error == nil}
	}
}

func (m *AgentRuntimeManager) SessionResponse(agent string, payload map[string]interface{}) map[string]interface{} {
	switch runtime := m.MustRuntime(agent).(type) {
	case *ClaudeRuntime:
		return runtime.sessionResponse(payload)
	case *GeminiRuntime:
		return runtime.sessionResponse(payload)
	case *CodexRuntime:
		return runtime.sessionResponse(payload)
	default:
		return cloneResponseMap(payload)
	}
}

func (m *AgentRuntimeManager) PromptAcceptedResponse(agent string, prompt PromptResponse) map[string]interface{} {
	switch runtime := m.MustRuntime(agent).(type) {
	case *ClaudeRuntime:
		return runtime.promptAcceptedResponse(prompt)
	case *GeminiRuntime:
		return runtime.promptAcceptedResponse(prompt)
	case *CodexRuntime:
		return runtime.promptAcceptedResponse(prompt)
	default:
		response := cloneResponseMap(prompt.Payload)
		response["ok"] = true
		if prompt.OperationID != "" {
			response["operationId"] = prompt.OperationID
		}
		return response
	}
}

func (m *AgentRuntimeManager) ConfigResponse(agent string) map[string]interface{} {
	switch runtime := m.MustRuntime(agent).(type) {
	case *ClaudeRuntime:
		return runtime.configUpdateResponse()
	case *GeminiRuntime:
		return runtime.configUpdateResponse()
	case *CodexRuntime:
		return runtime.configUpdateResponse()
	default:
		return map[string]interface{}{"ok": true}
	}
}

func (m *AgentRuntimeManager) ActionResponse(agent string, payload map[string]interface{}) map[string]interface{} {
	switch runtime := m.MustRuntime(agent).(type) {
	case *ClaudeRuntime:
		return runtime.actionResponse(payload)
	case *GeminiRuntime:
		return runtime.actionResponse(payload)
	case *CodexRuntime:
		return runtime.actionResponse(payload)
	default:
		response := cloneResponseMap(payload)
		response["ok"] = true
		return response
	}
}

func (m *AgentRuntimeManager) ClaudeRuntime() *ClaudeRuntime {
	runtime, _ := m.runtimes["claude"].(*ClaudeRuntime)
	return runtime
}

func (m *AgentRuntimeManager) CodexRuntime() *CodexRuntime {
	runtime, _ := m.runtimes["codex"].(*CodexRuntime)
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

type ClaudePermissionDelegate interface {
	HandleRequest(id interface{}, params map[string]interface{})
	Resolve(requestID, optionID string, cancelled bool) error
	Pending() []map[string]interface{}
	Clear(reason string) []map[string]interface{}
}

type ClaudePermissionStore struct {
	mu      sync.Mutex
	bridge  *ClaudeBridge
	pending map[string]*pendingPermission
}

func NewClaudePermissionStore(bridge *ClaudeBridge) *ClaudePermissionStore {
	return &ClaudePermissionStore{
		bridge:  bridge,
		pending: make(map[string]*pendingPermission),
	}
}

func (s *ClaudePermissionStore) HandleRequest(id interface{}, params map[string]interface{}) {
	mode := canonicalClaudePermissionMode(s.bridge.SelectedMode())
	if mode == "bypass" || mode == "dontAsk" || mode == "auto" || mode == "acceptEdits" || mode == "bypassPermissions" {
		if optionID, ok := pickAcpAllowOption(params); ok {
			_ = s.bridge.agent.Respond(id, map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome":  "selected",
					"optionId": optionID,
				},
			})
			return
		}
		_ = s.bridge.agent.Respond(id, map[string]interface{}{
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

	s.bridge.emit("permission/request", map[string]interface{}{
		"requestId": requestID,
		"sessionId": sessionID,
		"toolName":  toolName,
		"toolCall":  toolCall,
		"options":   options,
		"createdAt": createdAt.UnixMilli(),
	})
}

func (s *ClaudePermissionStore) Resolve(requestID, optionID string, cancelled bool) error {
	reason := "approved"
	if cancelled || optionID == "" {
		reason = "cancelled"
	}
	if !s.resolve(requestID, optionID, cancelled, reason) {
		return fmt.Errorf("no pending permission request: %s", requestID)
	}
	return nil
}

func (s *ClaudePermissionStore) resolve(requestID, optionID string, cancelled bool, reason string) bool {
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
	_ = s.bridge.agent.Respond(pending.acpID, map[string]interface{}{"outcome": outcome})
	s.bridge.emit("permission/resolved", map[string]interface{}{
		"requestId":  requestID,
		"sessionId":  pending.sessionId,
		"toolName":   pending.toolName,
		"toolCall":   pending.toolCall,
		"resolvedAs": resolution,
	})
	return true
}

func (s *ClaudePermissionStore) Pending() []map[string]interface{} {
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

func (s *ClaudePermissionStore) Clear(reason string) []map[string]interface{} {
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

type ClaudeRuntime struct {
	bridge          *ClaudeBridge
	permissionStore *ClaudePermissionStore
}

func NewClaudeRuntime(bridge *ClaudeBridge) *ClaudeRuntime {
	runtime := &ClaudeRuntime{bridge: bridge}
	runtime.permissionStore = NewClaudePermissionStore(bridge)
	bridge.SetPermissionDelegate(runtime.permissionStore)
	return runtime
}

func (r *ClaudeRuntime) Name() string                       { return "claude" }
func (r *ClaudeRuntime) SetCwd(cwd string)                  { r.bridge.SetCwd(cwd) }
func (r *ClaudeRuntime) CheckAvailable() bool               { return r.bridge.CheckAvailable() }
func (r *ClaudeRuntime) Available() bool                    { return r.bridge.Available() }
func (r *ClaudeRuntime) IsRunning() bool                    { return r.bridge.IsRunning() }
func (r *ClaudeRuntime) Stop()                              { r.bridge.Stop() }
func (r *ClaudeRuntime) TaskStatus() map[string]interface{} { return r.bridge.TaskStatus() }
func (r *ClaudeRuntime) SessionID() string                  { return r.bridge.SessionId() }
func (r *ClaudeRuntime) RestoreSession(sessionID string)    { r.bridge.RestoreSession(sessionID) }
func (r *ClaudeRuntime) Start(cwd string) error             { return r.bridge.Start(cwd) }
func (r *ClaudeRuntime) LoadSession(sessionID, cwd string) (map[string]interface{}, error) {
	return r.bridge.LoadSession(sessionID, cwd)
}
func (r *ClaudeRuntime) NewSession(cwd string) (map[string]interface{}, error) {
	sessionID, err := r.bridge.NewSession(cwd)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"sessionId": sessionID}, nil
}
func (r *ClaudeRuntime) Prompt(req PromptRequest) (PromptResponse, error) {
	operationID, err := r.bridge.Prompt(req.Text, req.Images)
	if err != nil {
		return PromptResponse{}, err
	}
	return PromptResponse{OperationID: operationID}, nil
}
func (r *ClaudeRuntime) Cancel(sessionID string) error {
	r.bridge.Cancel()
	return nil
}

func (r *ClaudeRuntime) ResolvePermission(requestID, optionID string, cancelled bool) error {
	return r.permissionStore.Resolve(requestID, optionID, cancelled)
}

func (r *ClaudeRuntime) PendingPermissions() []map[string]interface{} {
	return r.permissionStore.Pending()
}

func (r *ClaudeRuntime) configResponse() map[string]interface{} {
	config := r.bridge.ConfigSnapshot()
	caps := r.bridge.Capabilities()
	return map[string]interface{}{
		"config":         config,
		"capabilities":   caps,
		"model":          config["model"],
		"effort":         config["effort"],
		"permissionMode": config["permissionMode"],
		"sessionModel":   r.bridge.SessionModel(),
		"sessionMode":    r.bridge.SessionMode(),
	}
}

func (r *ClaudeRuntime) startResponse(options RuntimeStartOptions) map[string]interface{} {
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

func (r *ClaudeRuntime) sessionResponse(payload map[string]interface{}) map[string]interface{} {
	response := cloneResponseMap(payload)
	response["ok"] = true
	return response
}

func (r *ClaudeRuntime) promptAcceptedResponse(prompt PromptResponse) map[string]interface{} {
	response := r.configResponse()
	response["ok"] = true
	response["operationId"] = prompt.OperationID
	response["sessionId"] = r.SessionID()
	return response
}

func (r *ClaudeRuntime) configUpdateResponse() map[string]interface{} {
	response := r.statusSnapshot()
	response["ok"] = true
	return response
}

func (r *ClaudeRuntime) actionResponse(payload map[string]interface{}) map[string]interface{} {
	return ensureOK(cloneResponseMap(payload))
}

func (r *ClaudeRuntime) statusSnapshot() map[string]interface{} {
	response := r.configResponse()
	response["available"] = r.Available()
	response["running"] = r.IsRunning()
	response["sessionId"] = r.SessionID()
	return response
}

func (r *ClaudeRuntime) taskSnapshot() map[string]interface{} {
	return r.bridge.TaskStatus()
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

type GeminiRuntime struct {
	bridge *GeminiBridge
}

func NewGeminiRuntime(bridge *GeminiBridge) *GeminiRuntime {
	return &GeminiRuntime{bridge: bridge}
}

func (r *GeminiRuntime) Name() string                       { return "gemini" }
func (r *GeminiRuntime) SetCwd(cwd string)                  { r.bridge.SetCwd(cwd) }
func (r *GeminiRuntime) CheckAvailable() bool               { return r.bridge.CheckAvailable() }
func (r *GeminiRuntime) Available() bool                    { return r.bridge.Available() }
func (r *GeminiRuntime) IsRunning() bool                    { return r.bridge.IsRunning() }
func (r *GeminiRuntime) Stop()                              { r.bridge.Stop() }
func (r *GeminiRuntime) TaskStatus() map[string]interface{} { return r.bridge.TaskStatus() }
func (r *GeminiRuntime) SessionID() string                  { return r.bridge.CurrentSessionID() }
func (r *GeminiRuntime) RestoreSession(sessionID string)    { r.bridge.RestoreSession(sessionID) }
func (r *GeminiRuntime) Start(cwd string) error {
	if cwd != "" {
		r.bridge.SetCwd(cwd)
	}
	return r.bridge.Start()
}
func (r *GeminiRuntime) LoadSession(sessionID, cwd string) (map[string]interface{}, error) {
	return r.bridge.LoadSession(sessionID, cwd)
}
func (r *GeminiRuntime) NewSession(cwd string) (map[string]interface{}, error) {
	return r.bridge.NewSession(cwd)
}
func (r *GeminiRuntime) Prompt(req PromptRequest) (PromptResponse, error) {
	payload, err := r.bridge.Prompt(req.SessionID, req.Text, req.Images)
	if err != nil {
		return PromptResponse{}, err
	}
	operationID, _ := payload["operationId"].(string)
	return PromptResponse{OperationID: operationID, Payload: payload}, nil
}
func (r *GeminiRuntime) Cancel(sessionID string) error {
	return r.bridge.Cancel(sessionID)
}

func (r *GeminiRuntime) statusSnapshot() map[string]interface{} {
	return map[string]interface{}{
		"available": r.Available(),
		"running":   r.IsRunning(),
	}
}

func (r *GeminiRuntime) taskSnapshot() map[string]interface{} {
	return r.bridge.TaskStatus()
}

func (r *GeminiRuntime) startResponse(options RuntimeStartOptions) map[string]interface{} {
	return map[string]interface{}{
		"ok":         options.Error == nil,
		"available":  options.Available,
		"cwd":        options.Cwd,
		"acpRunning": r.IsRunning(),
	}
}

func (r *GeminiRuntime) sessionResponse(payload map[string]interface{}) map[string]interface{} {
	response := cloneResponseMap(payload)
	response["ok"] = true
	return response
}

func (r *GeminiRuntime) promptAcceptedResponse(prompt PromptResponse) map[string]interface{} {
	response := cloneResponseMap(prompt.Payload)
	response["ok"] = true
	return response
}

func (r *GeminiRuntime) configUpdateResponse() map[string]interface{} {
	return map[string]interface{}{"ok": true}
}

func (r *GeminiRuntime) actionResponse(payload map[string]interface{}) map[string]interface{} {
	return ensureOK(cloneResponseMap(payload))
}
