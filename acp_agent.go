package main

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
)

type AcpCapabilities struct {
	CanSetModel bool
	CanSetMode  bool
}

type acpInitializeParams struct {
	ProtocolVersion    int                    `json:"protocolVersion"`
	ClientInfo         map[string]string      `json:"clientInfo"`
	ClientCapabilities map[string]interface{} `json:"clientCapabilities"`
}

type acpAuthenticateParams struct {
	MethodID string `json:"methodId"`
}

type acpSessionParams struct {
	SessionID  string        `json:"sessionId,omitempty"`
	Cwd        string        `json:"cwd,omitempty"`
	MCPServers []interface{} `json:"mcpServers"`
}

type acpPromptContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

type acpPromptParams struct {
	SessionID string             `json:"sessionId"`
	Prompt    []acpPromptContent `json:"prompt"`
}

type acpCancelParams struct {
	SessionID string `json:"sessionId"`
}

type acpSetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type acpSetModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

type AcpUnsupportedMethodError struct {
	Method string
}

func (e *AcpUnsupportedMethodError) Error() string {
	return fmt.Sprintf("ACP method unsupported: %s", e.Method)
}

type AcpAgentConfig struct {
	ID                     string
	Label                  string
	Command                string
	Args                   []string
	Env                    []string
	VersionArgs            []string
	AuthMethods            []string
	AutoApprovePermissions bool
	Capabilities           AcpCapabilities
}

// AcpAgent manages an ACP-compatible CLI process over stdio JSON-RPC.
type AcpAgent struct {
	mu            sync.Mutex
	config        AcpAgentConfig
	bridge        *AgentBridge
	cwd           string
	avail         bool
	loadedSession string

	OnNotification func(method string, params interface{})
	OnRequest      func(id interface{}, method string, params interface{})

	// OnPermissionRequest, if set, takes ownership of responding to an ACP
	// `session/request_permission` (a.k.a. `requestPermission`) request.
	// The handler must eventually call AcpAgent.Respond with the resulting
	// outcome. When nil, AcpAgent falls back to AutoApprovePermissions.
	OnPermissionRequest func(id interface{}, params map[string]interface{})
}

func NewAcpAgent(config AcpAgentConfig) *AcpAgent {
	if config.VersionArgs == nil {
		config.VersionArgs = []string{"--version"}
	}
	a := &AcpAgent{
		config: config,
		bridge: NewAgentBridge(),
	}
	a.bridge.OnNotification = a.handleNotification
	a.bridge.OnRequest = a.handleRequest
	return a
}

func (a *AcpAgent) SetCwd(cwd string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cwd = cwd
}

func (a *AcpAgent) Cwd() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cwd
}

func (a *AcpAgent) IsRunning() bool { return a.bridge.IsRunning() }

func (a *AcpAgent) Capabilities() AcpCapabilities {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.config.Capabilities
}

func (a *AcpAgent) Available() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.avail
}

func (a *AcpAgent) CheckAvailable() bool {
	cmd := exec.Command(a.config.Command, a.config.VersionArgs...)
	err := cmd.Run()
	a.mu.Lock()
	a.avail = err == nil
	available := a.avail
	a.mu.Unlock()
	return available
}

func (a *AcpAgent) Start() error {
	wasRunning := a.bridge.IsRunning()
	if wasRunning {
		return nil
	}

	cwd := a.Cwd()
	log.Printf("[%s] spawning: %s %v (cwd=%s)", a.config.ID, a.config.Command, a.config.Args, cwd)
	if err := a.bridge.StartProcess(a.config.Command, a.config.Args, cwd, a.config.Env); err != nil {
		return fmt.Errorf("spawn %s: %w", a.config.Label, err)
	}

	if err := a.initialize(); err != nil {
		return err
	}
	a.clearLoadedSession()
	return nil
}

func (a *AcpAgent) Stop() {
	a.bridge.Stop()
	a.clearLoadedSession()
}

func (a *AcpAgent) initialize() error {
	raw, err := a.bridge.Send("initialize", acpInitializeParams{
		ProtocolVersion:    1,
		ClientInfo:         map[string]string{"name": "AnyCode", "version": Version},
		ClientCapabilities: map[string]interface{}{},
	})
	if err != nil {
		return fmt.Errorf("ACP initialize: %w", err)
	}
	log.Printf("[%s] ACP initialized", a.config.ID)

	methodIds := extractAcpAuthMethodIDs(raw)
	for _, methodId := range a.config.AuthMethods {
		if len(methodIds) > 0 && !contains(methodIds, methodId) {
			continue
		}
		_, err := a.bridge.Send("authenticate", acpAuthenticateParams{MethodID: methodId})
		if err == nil {
			log.Printf("[%s] authenticated via %s", a.config.ID, methodId)
			return nil
		}
		log.Printf("[%s] auth %s failed: %v", a.config.ID, methodId, err)
	}
	if len(a.config.AuthMethods) > 0 {
		log.Printf("[%s] all auth methods failed, will retry on newSession", a.config.ID)
	}
	return nil
}

func extractAcpAuthMethodIDs(raw interface{}) []string {
	var methodIds []string
	if m, ok := raw.(map[string]interface{}); ok {
		if methods, ok := m["authMethods"].([]interface{}); ok {
			for _, item := range methods {
				if obj, ok := item.(map[string]interface{}); ok {
					if id, ok := obj["id"].(string); ok {
						methodIds = append(methodIds, id)
					}
				}
			}
		}
	}
	return methodIds
}

func (a *AcpAgent) NewSession(cwd string) (map[string]interface{}, error) {
	raw, err := a.bridge.Send("session/new", acpSessionParams{
		Cwd:        cwd,
		MCPServers: []interface{}{},
	})
	result := normalizeMapResult(raw)
	if err == nil {
		if sessionId, _ := result["sessionId"].(string); sessionId != "" {
			a.markLoadedSession(sessionId)
		}
	}
	return result, err
}

func (a *AcpAgent) LoadSession(sessionId, cwd string) (map[string]interface{}, error) {
	raw, err := a.bridge.Send("session/load", acpSessionParams{
		SessionID:  sessionId,
		Cwd:        cwd,
		MCPServers: []interface{}{},
	})
	result := normalizeMapResult(raw)
	if err == nil {
		a.markLoadedSession(sessionId)
	}
	return result, err
}

func (a *AcpAgent) IsSessionLoaded(sessionId string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return sessionId != "" && a.loadedSession == sessionId
}

func (a *AcpAgent) EnsureLoaded(sessionId, cwd string) error {
	if sessionId == "" {
		return fmt.Errorf("sessionId is required")
	}
	if a.IsSessionLoaded(sessionId) {
		return nil
	}
	if cwd != "" {
		a.SetCwd(cwd)
	}
	if !a.IsRunning() {
		if err := a.Start(); err != nil {
			return err
		}
	}
	if _, err := a.LoadSession(sessionId, cwd); err != nil {
		a.clearLoadedSession()
		return err
	}
	return nil
}

func (a *AcpAgent) markLoadedSession(sessionId string) {
	a.mu.Lock()
	a.loadedSession = sessionId
	a.mu.Unlock()
}

func (a *AcpAgent) clearLoadedSession() {
	a.mu.Lock()
	a.loadedSession = ""
	a.mu.Unlock()
}

func (a *AcpAgent) Prompt(sessionId, text string, images []string) (map[string]interface{}, error) {
	promptContent := []acpPromptContent{
		{Type: "text", Text: text},
	}
	for _, image := range images {
		promptContent = append(promptContent, acpImagePromptContent(image))
	}

	raw, err := a.bridge.Send("session/prompt", acpPromptParams{
		SessionID: sessionId,
		Prompt:    promptContent,
	})
	return normalizeMapResult(raw), err
}

func acpImagePromptContent(image string) acpPromptContent {
	mimeType := "image/jpeg"
	data := strings.TrimSpace(image)
	if strings.HasPrefix(data, "data:") {
		if header, payload, ok := strings.Cut(data, ","); ok {
			data = payload
			mediaType := strings.TrimPrefix(header, "data:")
			if idx := strings.Index(mediaType, ";"); idx >= 0 {
				mediaType = mediaType[:idx]
			}
			if strings.HasPrefix(mediaType, "image/") {
				mimeType = mediaType
			}
		}
	}
	return acpPromptContent{Type: "image", MimeType: mimeType, Data: data}
}

func (a *AcpAgent) Cancel(sessionId string) error {
	_, err := a.bridge.Send("session/cancel", acpCancelParams{SessionID: sessionId})
	return err
}

func (a *AcpAgent) SetMode(sessionId, modeId string) error {
	if !a.config.Capabilities.CanSetMode {
		return &AcpUnsupportedMethodError{Method: "session/setMode"}
	}
	_, err := a.bridge.Send("session/setMode", acpSetModeParams{SessionID: sessionId, ModeID: modeId})
	return err
}

func (a *AcpAgent) SetModel(sessionId, modelId string) error {
	if !a.config.Capabilities.CanSetModel {
		return &AcpUnsupportedMethodError{Method: "session/setModel"}
	}
	_, err := a.bridge.Send("unstable/session/setModel", acpSetModelParams{SessionID: sessionId, ModelID: modelId})
	if err == nil {
		return nil
	}
	_, fallbackErr := a.bridge.Send("session/setModel", acpSetModelParams{SessionID: sessionId, ModelID: modelId})
	if fallbackErr == nil {
		return nil
	}
	return err
}

func (a *AcpAgent) Respond(id interface{}, result interface{}) error {
	return a.bridge.Respond(id, result)
}

func normalizeMapResult(raw interface{}) map[string]interface{} {
	result, _ := raw.(map[string]interface{})
	if result == nil {
		result = map[string]interface{}{}
	}
	if _, ok := result["sessionId"]; !ok {
		for _, key := range []string{"session_id", "sessionID", "id"} {
			if sessionId, ok := result[key].(string); ok && sessionId != "" {
				result["sessionId"] = sessionId
				break
			}
		}
	}
	return result
}

func (a *AcpAgent) handleRequest(id interface{}, method string, params interface{}) {
	log.Printf("[%s] ACP request: %s", a.config.ID, method)

	// ACP permission methods: claude-code-acp / Gemini use the spec name
	// `session/request_permission`, some older bridges send `requestPermission`.
	if method == "session/request_permission" || method == "requestPermission" {
		paramsMap, _ := params.(map[string]interface{})
		if paramsMap == nil {
			paramsMap = map[string]interface{}{}
		}
		if a.OnPermissionRequest != nil {
			a.OnPermissionRequest(id, paramsMap)
			return
		}
		if a.config.AutoApprovePermissions {
			optionId, _ := pickAcpAllowOption(paramsMap)
			_ = a.bridge.Respond(id, map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome":  "selected",
					"optionId": optionId,
				},
			})
			return
		}
	}

	if a.OnRequest != nil {
		a.OnRequest(id, method, params)
		return
	}

	_ = a.bridge.Respond(id, map[string]interface{}{})
}

// pickAcpAllowOption inspects an ACP requestPermission params map and
// returns the first option whose `kind` indicates approval (preferring
// allow_once over allow_always). Returns ("", false) if no options exist.
func pickAcpAllowOption(params map[string]interface{}) (string, bool) {
	options, ok := params["options"].([]interface{})
	if !ok {
		return "", false
	}
	var allowOnce, allowAlways, fallback string
	for _, opt := range options {
		o, ok := opt.(map[string]interface{})
		if !ok {
			continue
		}
		oid, _ := o["optionId"].(string)
		if oid == "" {
			continue
		}
		if fallback == "" {
			fallback = oid
		}
		kind, _ := o["kind"].(string)
		switch kind {
		case "allow_once":
			if allowOnce == "" {
				allowOnce = oid
			}
		case "allow_always":
			if allowAlways == "" {
				allowAlways = oid
			}
		}
	}
	switch {
	case allowOnce != "":
		return allowOnce, true
	case allowAlways != "":
		return allowAlways, true
	case fallback != "":
		return fallback, true
	}
	return "", false
}

// pickAcpRejectOption returns the first reject-kind option's id, or
// ("", false) if none. Used when the user cancels a permission prompt.
func pickAcpRejectOption(params map[string]interface{}) (string, bool) {
	options, ok := params["options"].([]interface{})
	if !ok {
		return "", false
	}
	for _, opt := range options {
		o, ok := opt.(map[string]interface{})
		if !ok {
			continue
		}
		kind, _ := o["kind"].(string)
		oid, _ := o["optionId"].(string)
		if oid != "" && (kind == "reject_once" || kind == "reject_always") {
			return oid, true
		}
	}
	return "", false
}

func (a *AcpAgent) handleNotification(method string, params interface{}) {
	if method != "session/update" && method != "sessionUpdate" && method != "session_update" {
		return
	}
	p, ok := params.(map[string]interface{})
	if !ok {
		return
	}
	sessionId, _ := p["sessionId"].(string)
	if sessionId == "" {
		sessionId, _ = p["session_id"].(string)
	}
	update, ok := p["update"].(map[string]interface{})
	if !ok {
		return
	}

	updateType := acpStringField(update, "sessionUpdate", "session_update", "type")
	switch updateType {
	case "agent_message_chunk":
		if text := extractContentText(update); text != "" {
			a.emit("message/assistant", map[string]interface{}{
				"sessionId": sessionId, "content": text, "delta": true,
			})
		}

	case "user_message_chunk":
		if text := extractContentText(update); text != "" {
			a.emit("message/user", map[string]interface{}{
				"sessionId": sessionId, "content": text,
			})
		}

	case "agent_thought_chunk", "plan":
		if text := extractContentText(update); text != "" {
			a.emit("message/thought", map[string]interface{}{
				"sessionId": sessionId, "content": text, "delta": true,
			})
		}

	case "tool_call", "tool_call_update":
		a.emitToolUpdate(sessionId, updateType, update)

	case "available_commands_update":
		// ignored by AnyCode's current chat surface

	default:
		log.Printf("[%s] unknown ACP update: %s", a.config.ID, updateType)
	}
}

func (a *AcpAgent) emitToolUpdate(sessionId, updateType string, update map[string]interface{}) {
	toolCallId, _ := update["toolCallId"].(string)
	status, _ := update["status"].(string)
	title, _ := update["title"].(string)
	kind, _ := update["kind"].(string)

	notifMethod := "tool/started"
	if updateType == "tool_call_update" {
		notifMethod = "tool/completed"
	}

	notifParams := map[string]interface{}{
		"sessionId": sessionId,
		"toolId":    toolCallId,
		"toolName":  title,
		"status":    status,
		"kind":      kind,
	}

	// ACP carries the raw tool input (e.g. {"command":"ls -la"} for Bash,
	// {"file_path":"…"} for Read/Edit, {"pattern":"…"} for Grep, …) under
	// `rawInput`. Surface it so the UI can show meaningful tool detail
	// instead of just the tool name.
	if raw, ok := update["rawInput"].(map[string]interface{}); ok {
		notifParams["input"] = raw
		if detail := summarizeToolInput(title, raw); detail != "" {
			notifParams["detail"] = detail
		}
	}
	if loc, ok := update["locations"].([]interface{}); ok && len(loc) > 0 {
		notifParams["locations"] = loc
	}

	if contentArr, ok := update["content"].([]interface{}); ok {
		var outputs []string
		for _, item := range contentArr {
			c, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			cType, _ := c["type"].(string)
			switch cType {
			case "diff":
				diff := map[string]interface{}{
					"path":    c["path"],
					"oldText": c["oldText"],
					"newText": c["newText"],
				}
				if meta, ok := c["_meta"].(map[string]interface{}); ok {
					kind, _ := meta["kind"].(string)
					// Normalize Claude's "modify" to the Codex "update" vocabulary
					// that the frontend FileChangeCard natively expects.
					if kind == "modify" {
						kind = "update"
					}
					diff["kind"] = kind
				}
				notifParams["diff"] = diff
			case "content":
				if inner, ok := c["content"].(map[string]interface{}); ok {
					if text, _ := inner["text"].(string); text != "" {
						outputs = append(outputs, text)
					}
				}
			default:
				// Some ACP servers (claude-code-acp) emit content items
				// without an explicit `type` key — fall back to nested text.
				if inner, ok := c["content"].(map[string]interface{}); ok {
					if text, _ := inner["text"].(string); text != "" {
						outputs = append(outputs, text)
					}
				} else if text, _ := c["text"].(string); text != "" {
					outputs = append(outputs, text)
				}
			}
		}
		if len(outputs) > 0 {
			notifParams["output"] = strings.Join(outputs, "\n")
		}
	}

	a.emit(notifMethod, notifParams)
}

// summarizeToolInput returns a human-readable one-line summary of a tool's
// raw input map. It's intentionally lightweight — the UI can still inspect
// `input` for full detail.
func summarizeToolInput(title string, raw map[string]interface{}) string {
	if cmd, _ := raw["command"].(string); cmd != "" {
		return cmd
	}
	if path, _ := raw["file_path"].(string); path != "" {
		if pattern, _ := raw["pattern"].(string); pattern != "" {
			return fmt.Sprintf("%s  (pattern: %s)", path, pattern)
		}
		return path
	}
	if path, _ := raw["path"].(string); path != "" {
		return path
	}
	if pattern, _ := raw["pattern"].(string); pattern != "" {
		return pattern
	}
	if query, _ := raw["query"].(string); query != "" {
		return query
	}
	if url, _ := raw["url"].(string); url != "" {
		return url
	}
	return ""
}

func (a *AcpAgent) emit(method string, params interface{}) {
	if a.OnNotification != nil {
		a.OnNotification(method, params)
	}
}

func extractContentText(update map[string]interface{}) string {
	content, ok := update["content"].(map[string]interface{})
	if !ok {
		return ""
	}
	text, _ := content["text"].(string)
	return text
}

func acpStringField(obj map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if val, ok := obj[key]; ok {
			if s, ok := val.(string); ok {
				return s
			}
		}
	}
	return ""
}

func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}
