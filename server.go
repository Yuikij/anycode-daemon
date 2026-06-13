package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	proxyPrefix       = "/__anycode_proxy/"
	proxyOpenPath     = "/__anycode_proxy/open"
	proxyTokenCookie  = "anycode_proxy_token"
	proxyOriginCookie = "anycode_proxy_origin"
	sessionCookieName = "AUTH_SESSION"
	relayDeviceCookie = "anycode_relay_proxy_device"
	relayAuthCookie   = "anycode_relay_proxy_auth"
)

// WebSocket keepalive. The relay link (daemon 闁?Cloudflare Durable Object) and
// local browser/app clients can silently die (NAT/idle timeouts, dropped
// Wi-Fi) without ever delivering a TCP FIN. Without a read deadline the daemon
// would block forever in ReadMessage and never notice 闁?which is exactly the
// classic symptom: the agent link is dead but never reconnects.
//
// We send a WebSocket ping every pingPeriod and require *some* inbound frame
// (a pong, an RPC, or the client heartbeat) within pongWait, otherwise the read
// fails and the connection loop exits 闁?triggering relayLoop's reconnect.
//
// Cloudflare's runtime auto-responds to ping frames with pongs, and both
// browsers (DOM WebSocket) and iOS (URLSessionWebSocketTask) auto-pong, so this
// is transparent to every existing client.
const (
	pongWait   = 70 * time.Second
	pingPeriod = 30 * time.Second
	ctrlWait   = 30 * time.Second
)

type Server struct {
	port              int
	token             string
	projectRoot       string
	projectGeneration uint64

	codex   *AgentBridge
	gemini  *GeminiBridge
	claude  *ClaudeBridge
	runtime *AgentRuntimeManager

	cron *CronManager

	mu           sync.RWMutex
	clients      map[*wsClient]struct{}
	routes       map[string]func(req RpcRequest, client *wsClient) (interface{}, error)
	eventJournal *eventJournal

	// paramValidator validates incoming RPC params against the embedded protocol
	// method catalog at the dispatch boundary (see protocol_params.go). It is
	// always loaded; paramValidation is on by default and only disabled via the
	// ANYCODE_DISABLE_PARAM_VALIDATION escape hatch.
	paramValidator  *protocolValidator
	paramValidation bool
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  10 * 1024 * 1024,
	WriteBufferSize: 10 * 1024 * 1024,
}

func NewServer(port int, projectRoot, token string) (*Server, error) {
	journal, err := openEventJournal(stateDBPath(), maxJournaledEvents)
	if err != nil {
		return nil, err
	}
	projectRoot, projectGeneration, err := journal.loadProjectState(projectRoot, 1)
	if err != nil {
		_ = journal.close()
		return nil, err
	}
	if err := journal.interruptRunningOperations("daemon restarted before operation completed"); err != nil {
		_ = journal.close()
		return nil, err
	}
	if err := journal.expirePendingPermissions("expired"); err != nil {
		_ = journal.close()
		return nil, err
	}

	s := &Server{
		port:              port,
		token:             token,
		projectRoot:       projectRoot,
		projectGeneration: projectGeneration,
		codex:             NewAgentBridge(),
		gemini:            NewGeminiBridge(),
		claude:            NewClaudeBridge(),
		clients:           make(map[*wsClient]struct{}),
		routes:            make(map[string]func(req RpcRequest, client *wsClient) (interface{}, error)),
		eventJournal:      journal,
	}
	s.runtime = NewAgentRuntimeManager(
		NewCodexRuntime(s.codex),
		NewClaudeRuntime(s.claude),
		NewGeminiRuntime(s.gemini),
	)
	if err := journal.upsertProject(projectRoot, filepath.Base(projectRoot), nowUnixMilli()); err != nil {
		_ = journal.close()
		return nil, err
	}
	s.initRoutes()
	s.paramValidator = mustLoadEmbeddedValidator()
	// Boundary param validation is on by default (the catalog is embedded, so it
	// works in production too). ANYCODE_DISABLE_PARAM_VALIDATION is an emergency
	// escape hatch only.
	s.paramValidation = os.Getenv("ANYCODE_DISABLE_PARAM_VALIDATION") == ""

	s.cron = NewCronManager(s)
	s.cron.Start(s.projectRoot)

	s.codex.OnNotification = func(method string, params interface{}) {
		s.recordCodexEvent(method, params)
		s.broadcastRecordedEvent("codex", "codex."+method, params)
	}

	s.codex.OnRequest = func(id interface{}, method string, params interface{}) {
		s.broadcast(makeNotification("codex.serverRequest", map[string]interface{}{
			"id": id, "method": method, "params": params,
		}))
	}

	s.gemini.SetCwd(s.projectRoot)
	s.gemini.OnNotification = func(method string, params interface{}) {
		s.recordAgentOperationEvent("gemini", method, params)
		s.broadcastRecordedEvent("gemini", "gemini."+method, params)
	}

	s.claude.SetCwd(s.projectRoot)
	s.claude.OnNotification = func(method string, params interface{}) {
		s.recordAgentOperationEvent("claude", method, params)
		s.recordClaudePermissionEvent(method, params)
		s.broadcastRecordedEvent("claude", "claude."+method, params)
	}
	if err := s.restorePersistedAgentSessions(); err != nil {
		_ = journal.close()
		return nil, err
	}

	return s, nil
}

func (s *Server) Close() error {
	if s == nil || s.eventJournal == nil {
		return nil
	}
	return s.eventJournal.close()
}

func (s *Server) persistAgentSessionState(agent, sessionID string) {
	s.persistAgentRuntimeState(agent, sessionID, "")
}

func (s *Server) persistRuntimeState(agent string) {
	runtime := s.runtime.MustRuntime(agent)
	id := runtime.SessionID()
	if agent == "codex" {
		s.persistCodexThreadState(id)
		return
	}
	s.persistAgentSessionState(agent, id)
}

func (s *Server) persistAcceptedOperation(agent string, operationID string) {
	if operationID == "" {
		return
	}
	runtime := s.runtime.MustRuntime(agent)
	sessionID := runtime.SessionID()
	threadID := ""
	if agent == "codex" {
		threadID = sessionID
		sessionID = ""
	}
	s.persistOperationSnapshot(persistedOperation{
		OperationID: operationID,
		Agent:       agent,
		SessionID:   sessionID,
		ThreadID:    threadID,
		Status:      "running",
		StartedAt:   nowUnixMilli(),
		UpdatedAt:   nowUnixMilli(),
	})
}

func (s *Server) persistInterruptedOperation(agent string, status map[string]interface{}) {
	operationID, _ := status["operationId"].(string)
	if operationID == "" {
		return
	}
	runtime := s.runtime.MustRuntime(agent)
	sessionID := runtime.SessionID()
	threadID := ""
	if agent == "codex" {
		threadID = sessionID
		sessionID = ""
	}
	s.persistOperationSnapshot(persistedOperation{
		OperationID: operationID,
		Agent:       agent,
		SessionID:   sessionID,
		ThreadID:    threadID,
		Status:      "interrupted",
		UpdatedAt:   nowUnixMilli(),
	})
}

func (s *Server) persistAgentRuntimeState(agent, sessionID, threadID string) {
	if s == nil || s.eventJournal == nil {
		return
	}
	if err := s.eventJournal.saveAgentState(agent, sessionID, threadID, nowUnixMilli()); err != nil {
		log.Printf("[server] failed to persist %s session state: %v", agent, err)
	}
}

func (s *Server) persistCodexThreadState(threadID string) {
	if threadID == "" {
		return
	}
	s.persistAgentRuntimeState("codex", "", threadID)
}

func (s *Server) persistOperationSnapshot(snapshot persistedOperation) {
	if s == nil || s.eventJournal == nil {
		return
	}
	if err := s.eventJournal.upsertOperation(snapshot); err != nil {
		log.Printf("[server] failed to persist %s operation snapshot: %v", snapshot.Agent, err)
	}
}

func (s *Server) persistPermissionSnapshot(snapshot persistedPermission) {
	if s == nil || s.eventJournal == nil {
		return
	}
	if err := s.eventJournal.upsertPermission(snapshot); err != nil {
		log.Printf("[server] failed to persist %s permission snapshot: %v", snapshot.Agent, err)
	}
}

func (s *Server) latestOperationPayload(agent string) map[string]interface{} {
	if s == nil || s.eventJournal == nil {
		return nil
	}
	op, err := s.eventJournal.latestOperation(agent)
	if err != nil {
		log.Printf("[server] failed to load %s operation snapshot: %v", agent, err)
		return nil
	}
	if op == nil {
		return nil
	}
	return map[string]interface{}{
		"operationId": op.OperationID,
		"sessionId":   op.SessionID,
		"threadId":    op.ThreadID,
		"status":      op.Status,
		"startedAt":   op.StartedAt,
		"updatedAt":   op.UpdatedAt,
		"completedAt": op.CompletedAt,
		"error":       op.ErrorMessage,
	}
}

func (s *Server) latestPermissionPayload(agent string) map[string]interface{} {
	if s == nil || s.eventJournal == nil {
		return nil
	}
	permission, err := s.eventJournal.latestPermission(agent)
	if err != nil {
		log.Printf("[server] failed to load %s permission snapshot: %v", agent, err)
		return nil
	}
	if permission == nil {
		return nil
	}
	return map[string]interface{}{
		"requestId":  permission.RequestID,
		"sessionId":  permission.SessionID,
		"toolName":   permission.ToolName,
		"status":     permission.Status,
		"createdAt":  permission.CreatedAt,
		"resolvedAt": permission.ResolvedAt,
	}
}

func (s *Server) restorePersistedAgentSessions() error {
	if s == nil || s.eventJournal == nil {
		return nil
	}
	states, err := s.eventJournal.listAgentStates()
	if err != nil {
		return err
	}
	for _, state := range states {
		switch state.Agent {
		case "claude":
			s.runtime.MustRuntime("claude").RestoreSession(state.SessionID)
		case "gemini":
			s.runtime.MustRuntime("gemini").RestoreSession(state.SessionID)
		case "codex":
			s.runtime.MustRuntime("codex").RestoreSession(state.ThreadID)
		}
	}
	return nil
}

func (s *Server) recordAgentOperationEvent(agent, method string, params interface{}) {
	if sessionID := stringParam(params, "sessionId"); sessionID != "" && agent != "codex" {
		s.persistAgentRuntimeState(agent, sessionID, "")
	}
	status := operationStatusForMethod(method, params)
	if status == "" {
		return
	}
	opID := extractOperationID(params)
	if opID == "" {
		return
	}
	s.persistOperationSnapshot(persistedOperation{
		OperationID:  opID,
		Agent:        agent,
		SessionID:    stringParam(params, "sessionId"),
		ThreadID:     extractThreadID(params),
		Status:       status,
		UpdatedAt:    nowUnixMilli(),
		ErrorMessage: operationErrorMessage(params),
	})
}

func (s *Server) recordClaudePermissionEvent(method string, params interface{}) {
	if !strings.HasPrefix(method, "permission/") {
		return
	}
	requestID := requestIDParam(params)
	if requestID == "" {
		return
	}
	status := "pending"
	resolvedAt := int64(0)
	if method == "permission/resolved" {
		status = stringParam(params, "resolvedAs")
		if status == "" {
			status = "resolved"
		}
		resolvedAt = nowUnixMilli()
	}
	payloadJSON, err := marshalEventParams(params)
	if err != nil {
		payloadJSON = "{}"
	}
	s.persistPermissionSnapshot(persistedPermission{
		RequestID:   requestID,
		Agent:       "claude",
		SessionID:   stringParam(params, "sessionId"),
		ToolName:    stringParam(params, "toolName"),
		Status:      status,
		PayloadJSON: payloadJSON,
		CreatedAt:   nowUnixMilli(),
		ResolvedAt:  resolvedAt,
	})
}

func operationStatusForMethod(method string, params interface{}) string {
	switch {
	case strings.Contains(method, "turn/started"):
		return "running"
	case strings.Contains(method, "turn/completed"):
		if success, ok := boolParam(params, "success"); ok && !success {
			return "failed"
		}
		return "completed"
	case strings.Contains(method, "turn/failed"), method == "error":
		return "failed"
	case strings.Contains(method, "turn/aborted"):
		return "aborted"
	case strings.Contains(method, "turn/interrupted"), strings.Contains(method, "turn/cancelled"), strings.Contains(method, "stop"):
		return "interrupted"
	default:
		return ""
	}
}

func operationErrorMessage(params interface{}) string {
	if message := stringParam(params, "error"); message != "" {
		return message
	}
	if message := stringParam(params, "message"); message != "" {
		return message
	}
	return ""
}

func requestIDParam(params interface{}) string {
	m, ok := params.(map[string]interface{})
	if !ok {
		return ""
	}
	value, ok := m["requestId"]
	if !ok || value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func boolParam(params interface{}, key string) (bool, bool) {
	m, ok := params.(map[string]interface{})
	if !ok {
		return false, false
	}
	value, ok := m[key]
	if !ok || value == nil {
		return false, false
	}
	flag, ok := value.(bool)
	return flag, ok
}

func stringParam(params interface{}, key string) string {
	m, ok := params.(map[string]interface{})
	if !ok {
		return ""
	}
	value, ok := m[key]
	if !ok || value == nil {
		return ""
	}
	if str, ok := value.(string); ok {
		return str
	}
	return fmt.Sprint(value)
}

func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc(proxyOpenPath, s.handleProxyOpen)
	mux.HandleFunc(proxyPrefix, s.handleProxyPrefix)
	mux.HandleFunc("/share/", s.handleShare)
	mux.HandleFunc("/", s.handleRoot)

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[server] listening on %s", addr)
	return http.ListenAndServe(addr, mux)
}

// sharesDir is a stable, project-independent location for share HTML so a
// share survives `project.open` switches.
func sharesDir() string {
	return filepath.Join(configDir(), "shares")
}

// readShare returns the stored HTML for a share id. Used both by the local
// HTTP handler and by the relay (via the pre-auth `share.read` RPC) so a
// public share link can be served live from this machine.
func (s *Server) readShare(id string) (interface{}, error) {
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return nil, fmt.Errorf("invalid share id")
	}
	content, err := os.ReadFile(filepath.Join(sharesDir(), id+".html"))
	if err != nil {
		return nil, fmt.Errorf("share not found")
	}
	return map[string]interface{}{"html": string(content)}, nil
}

func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	// Accept both /share/<id> and /share/<deviceId>/<id> 闁?the id is the last
	// path segment.
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/share/"), "/")
	segs := strings.Split(trimmed, "/")
	id := segs[len(segs)-1]
	if id == "" || strings.Contains(id, ".") {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile(filepath.Join(sharesDir(), id+".html"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if isWebSocketUpgrade(r) {
		if r.URL.Path == "/" {
			s.handleWS(w, r)
			return
		}
		s.handleProxyFallback(w, r)
		return
	}

	if s.hasProxyAuth(r) {
		s.handleProxyFallback(w, r)
		return
	}

	http.NotFound(w, r)
}
