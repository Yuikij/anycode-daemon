package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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
)

type wsClient struct {
	conn   *websocket.Conn
	wmu    sync.Mutex // protects writes
	authed bool
	host   string
}

func (c *wsClient) send(data []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.WriteMessage(websocket.TextMessage, data)
}

type Server struct {
	port        int
	token       string
	projectRoot string

	codex  *AgentBridge
	gemini *GeminiBridge
	claude *ClaudeBridge

	cron *CronManager

	mu      sync.RWMutex
	clients map[*wsClient]struct{}

	// Codex notification replay buffer. Codex (unlike Claude) has no daemon
	// session store, so streaming deltas were lost when a client disconnected
	// mid-turn. We keep the current turn's events so a reconnecting client can
	// replay them via `codex.taskStatus` and rebuild the in-progress UI.
	codexMu          sync.Mutex
	codexEvents      []cachedNotification
	codexTurnRunning bool
	codexThreadID    string
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  10 * 1024 * 1024,
	WriteBufferSize: 10 * 1024 * 1024,
}

func NewServer(port int, projectRoot, token string) *Server {
	s := &Server{
		port:        port,
		token:       token,
		projectRoot: projectRoot,
		codex:       NewAgentBridge(),
		gemini:      NewGeminiBridge(),
		claude:      NewClaudeBridge(),
		clients:     make(map[*wsClient]struct{}),
	}

	s.cron = NewCronManager(s)
	s.cron.Start(s.projectRoot)

	s.codex.OnNotification = func(method string, params interface{}) {
		s.recordCodexEvent(method, params)
		s.broadcast(makeNotification("codex."+method, params))
	}

	s.codex.OnRequest = func(id interface{}, method string, params interface{}) {
		s.broadcast(makeNotification("codex.serverRequest", map[string]interface{}{
			"id": id, "method": method, "params": params,
		}))
	}

	s.gemini.SetCwd(projectRoot)
	s.gemini.OnNotification = func(method string, params interface{}) {
		s.broadcast(makeNotification("gemini."+method, params))
	}

	s.claude.SetCwd(projectRoot)
	s.claude.OnNotification = func(method string, params interface{}) {
		s.broadcast(makeNotification("claude."+method, params))
	}

	return s
}

func (s *Server) broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for c := range s.clients {
		if c.authed {
			c.send(data)
		}
	}
}

// recordCodexEvent buffers a codex notification for replay on reconnect and
// tracks whether a turn is currently running. The buffer is reset at the start
// of each turn so it only holds the in-progress turn's events.
func (s *Server) recordCodexEvent(method string, params interface{}) {
	s.codexMu.Lock()
	defer s.codexMu.Unlock()

	switch method {
	case "turn/started":
		s.codexTurnRunning = true
		s.codexEvents = s.codexEvents[:0]
	case "turn/completed", "turn/failed", "turn/aborted", "turn/interrupted":
		s.codexTurnRunning = false
	case "thread/started":
		if id := extractThreadID(params); id != "" {
			s.codexThreadID = id
		}
	}
	if id := extractThreadID(params); id != "" {
		s.codexThreadID = id
	}

	s.codexEvents = append(s.codexEvents, cachedNotification{
		Method: method, Params: params, Time: time.Now().UnixMilli(),
	})
	if len(s.codexEvents) > maxCachedNotifications {
		s.codexEvents = s.codexEvents[len(s.codexEvents)-maxCachedNotifications:]
	}
}

// extractThreadID best-effort pulls a thread id out of a codex notification's
// params (either top-level `threadId` or nested `thread.id`).
func extractThreadID(params interface{}) string {
	m, ok := params.(map[string]interface{})
	if !ok {
		return ""
	}
	if id, ok := m["threadId"].(string); ok && id != "" {
		return id
	}
	if th, ok := m["thread"].(map[string]interface{}); ok {
		if id, ok := th["id"].(string); ok {
			return id
		}
	}
	return ""
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[server] upgrade error: %v", err)
		return
	}
	s.serveConn(conn, r.Host)
}

// serveConn runs the JSON-RPC message loop for a single websocket connection.
// It is shared by inbound clients (handleWS) and the outbound relay agent link
// (relay.go), so the relay connection is treated exactly like a local client —
// the remote client still performs the `auth` handshake end-to-end.
func (s *Server) serveConn(conn *websocket.Conn, hostLabel string) {
	log.Println("[server] client connected")

	conn.SetReadLimit(10 * 1024 * 1024)

	client := &wsClient{
		conn: conn,
		host: hostLabel,
	}

	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
		conn.Close()
		log.Println("[server] client disconnected")
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("[server] read error: %v", err)
			break
		}

		var req RpcRequest
		if err := json.Unmarshal(data, &req); err != nil {
			reply, _ := json.Marshal(makeError(0, -32700, "Parse error"))
			client.send(reply)
			continue
		}

		id := parseID(req.ID)
		if req.JSONRPC == "" || req.Method == "" || id == nil {
			reply, _ := json.Marshal(makeError(0, -32600, "Invalid request"))
			client.send(reply)
			continue
		}

		// Auth
		if req.Method == "auth" {
			params := getParams(req.Params)
			clientToken := getParamString(params, "token")
			if clientToken == s.token {
				s.mu.Lock()
				client.authed = true
				s.mu.Unlock()
				reply, _ := json.Marshal(makeResponse(id, map[string]interface{}{
					"ok": true, "codexAvailable": s.codex.IsRunning(),
				}))
				client.send(reply)
			} else {
				reply, _ := json.Marshal(makeError(id, 401, "Invalid token"))
				client.send(reply)
			}
			continue
		}

		// share.read is allowed pre-auth: it only returns HTML the user
		// explicitly shared, keyed by an unguessable id. The relay calls this
		// to serve a public share link by fetching live from this daemon.
		if req.Method == "share.read" {
			p := getParams(req.Params)
			result, herr := s.readShare(getParamString(p, "id"))
			var reply []byte
			if herr != nil {
				reply, _ = json.Marshal(makeError(id, -32000, herr.Error()))
			} else {
				reply, _ = json.Marshal(makeResponse(id, result))
			}
			client.send(reply)
			continue
		}

		// proxy.fetch is called by the authenticated relay worker to serve the
		// built-in browser over the agent WebSocket. The underlying HTTP proxy
		// handlers still validate the daemon token from query/cookies.
		if req.Method == "proxy.fetch" {
			p := getParams(req.Params)
			result, herr := s.handleRelayProxyFetch(p)
			var reply []byte
			if herr != nil {
				reply, _ = json.Marshal(makeError(id, -32000, herr.Error()))
			} else {
				reply, _ = json.Marshal(makeResponse(id, result))
			}
			client.send(reply)
			continue
		}

		// Check auth
		s.mu.RLock()
		authed := client.authed
		s.mu.RUnlock()
		if !authed {
			reply, _ := json.Marshal(makeError(id, 401, "Not authenticated"))
			client.send(reply)
			continue
		}

		// Handle request
		result, herr := s.handleRequest(req, client)
		var reply []byte
		if herr != nil {
			reply, _ = json.Marshal(makeError(id, -32000, herr.Error()))
		} else {
			reply, _ = json.Marshal(makeResponse(id, result))
		}
		client.send(reply)
	}
}

func (s *Server) switchProject(newRoot string) {
	s.projectRoot = newRoot
	s.cron.Stop()
	s.cron.Start(newRoot)
	log.Printf("[server] switched to project: %s", newRoot)
}

func (s *Server) handleRequest(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)

	switch req.Method {

	case "daemon.version":
		return map[string]string{"version": Version}, nil

	case "daemon.configRead":
		cfg := LoadConfig()
		return map[string]interface{}{
			"ok":    true,
			"proxy": cfg.Proxy,
		}, nil

	case "daemon.configWrite":
		cfg := LoadConfig()
		proxy := getParamString(params, "proxy")
		cfg.Proxy = proxy
		if err := cfg.Save(); err != nil {
			return nil, err
		}
		// Apply immediately to current process
		if cfg.Proxy != "" {
			os.Setenv("HTTP_PROXY", cfg.Proxy)
			os.Setenv("HTTPS_PROXY", cfg.Proxy)
			os.Setenv("ALL_PROXY", cfg.Proxy)
			os.Setenv("http_proxy", cfg.Proxy)
			os.Setenv("https_proxy", cfg.Proxy)
			os.Setenv("all_proxy", cfg.Proxy)
		} else {
			os.Unsetenv("HTTP_PROXY")
			os.Unsetenv("HTTPS_PROXY")
			os.Unsetenv("ALL_PROXY")
			os.Unsetenv("http_proxy")
			os.Unsetenv("https_proxy")
			os.Unsetenv("all_proxy")
		}
		return map[string]interface{}{"ok": true}, nil

	case "share.create":
		html := getParamString(params, "html")
		if html == "" {
			return nil, fmt.Errorf("html content is required")
		}

		// The HTML stays on THIS machine — we never upload it to the cloud.
		b := make([]byte, 6)
		rand.Read(b)
		id := hex.EncodeToString(b)
		dir := sharesDir()
		_ = os.MkdirAll(dir, 0755)
		if err := os.WriteFile(filepath.Join(dir, id+".html"), []byte(html), 0644); err != nil {
			return nil, fmt.Errorf("failed to save share: %w", err)
		}

		// Public link on the app domain. The relay routes
		// /share/<deviceId>/<id> to this device and fetches the HTML live over
		// the existing agent connection (see `share.read`), so the cloud only
		// forwards bytes and stores nothing.
		cfg := LoadConfig()
		if cfg.DeviceID != "" && cfg.RelayURL != "" {
			urlStr := fmt.Sprintf("%s/share/%s/%s", strings.TrimRight(cfg.RelayURL, "/"), cfg.DeviceID, id)
			return map[string]interface{}{"ok": true, "id": id, "url": urlStr}, nil
		}

		// LAN/direct fallback: served by this daemon's own HTTP server. Avoid
		// the unusable "relay" host label by falling back to localhost.
		host := client.host
		if host == "" || host == "relay" {
			host = fmt.Sprintf("localhost:%d", s.port)
		}
		urlStr := fmt.Sprintf("http://%s/share/%s", host, id)
		return map[string]interface{}{"ok": true, "id": id, "url": urlStr}, nil

	// ── Browse any directory (absolute paths) ──
	case "fs.browse":
		dirPath := getParamString(params, "path")
		if dirPath == "" {
			home, _ := os.UserHomeDir()
			dirPath = home
		}
		showHidden := getParamBool(params, "showHidden")
		return browseDirectory(dirPath, showHidden)

	case "fs.readAbsolute":
		filePath := getParamString(params, "path")
		if filePath == "" {
			return nil, fmt.Errorf("path is required")
		}
		return readAbsoluteFile(filePath)

	case "fs.writeAbsolute":
		filePath := getParamString(params, "path")
		content := getParamString(params, "content")
		if filePath == "" {
			return nil, fmt.Errorf("path is required")
		}
		err := os.WriteFile(filePath, []byte(content), 0644)
		if err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	// ── Project-scoped operations ──
	case "project.info":
		return getProjectInfo(s.projectRoot), nil

	case "project.open":
		newRoot := getParamString(params, "path")
		if newRoot == "" {
			return nil, fmt.Errorf("path is required")
		}
		abs, _ := filepath.Abs(newRoot)
		s.switchProject(abs)
		return getProjectInfo(s.projectRoot), nil

	case "project.list":
		result := listProjectDirs()
		result["current"] = s.projectRoot
		return result, nil

	case "fs.list":
		dirPath := getParamString(params, "path")
		fullPath := s.projectRoot
		if dirPath != "" {
			fullPath = filepath.Join(s.projectRoot, dirPath)
		}
		return listDirectory(fullPath, s.projectRoot)

	case "fs.tree":
		dirPath := getParamString(params, "path")
		depth := getParamInt(params, "depth", 3)
		fullPath := s.projectRoot
		if dirPath != "" {
			fullPath = filepath.Join(s.projectRoot, dirPath)
		}
		return getFileTree(fullPath, s.projectRoot, depth)

	case "fs.read":
		filePath := getParamString(params, "path")
		if filePath == "" {
			return nil, fmt.Errorf("path is required")
		}
		return readFileContent(filePath, s.projectRoot)

	case "git.status":
		return getGitStatus(s.projectRoot)

	case "git.diff":
		filePath := getParamString(params, "path")
		cwd := getParamString(params, "cwd")
		if cwd != "" {
			return getGitDiffHead(cwd)
		}
		return getGitDiff(s.projectRoot, filePath)

	case "git.diff.staged":
		filePath := getParamString(params, "path")
		return getGitDiffStaged(s.projectRoot, filePath)

	case "git.log":
		count := getParamInt(params, "count", 20)
		return getGitLog(s.projectRoot, count)

	case "git.diff.commit":
		commit := getParamString(params, "commit")
		filePath := getParamString(params, "path")
		if commit == "" {
			return nil, fmt.Errorf("commit hash is required")
		}
		return getGitFileDiff(s.projectRoot, commit, filePath)

	// ── Codex agent integration ──
	case "codex.start":
		cwd := getParamString(params, "cwd")
		if cwd == "" {
			cwd = s.projectRoot
		}
		command := codexCommand()
		log.Printf("[codex.start] command=%s cwd=%s, already_running=%v", command, cwd, s.codex.IsRunning())
		if err := s.codex.Start(command, codexAppServerArgs(), cwd); err != nil {
			log.Printf("[codex.start] error: %v", err)
			return nil, err
		}
		log.Printf("[codex.start] success")
		return map[string]bool{"ok": true}, nil

	case "codex.stop":
		s.codex.Stop()
		s.codexMu.Lock()
		s.codexEvents = s.codexEvents[:0]
		s.codexTurnRunning = false
		s.codexMu.Unlock()
		return map[string]bool{"ok": true}, nil

	case "codex.status":
		return map[string]bool{"running": s.codex.IsRunning()}, nil

	// Replay buffer for reconnecting clients: returns whether a turn is in
	// progress plus the current turn's buffered streaming events so the UI can
	// be rebuilt after a disconnect without waiting for the next delta.
	case "codex.taskStatus":
		s.codexMu.Lock()
		events := make([]cachedNotification, len(s.codexEvents))
		copy(events, s.codexEvents)
		running := s.codexTurnRunning
		threadID := s.codexThreadID
		s.codexMu.Unlock()
		return map[string]interface{}{
			"ok":           true,
			"running":      running,
			"codexRunning": s.codex.IsRunning(),
			"threadId":     threadID,
			"recentEvents": events,
		}, nil

	case "codex.threadList", "codex.threadRead", "codex.threadStart",
		"codex.threadResume", "codex.threadArchive", "codex.threadUnarchive",
		"codex.threadRename", "codex.threadRollback",
		"codex.threadCompact",
		"codex.turnStart", "codex.turnSteer", "codex.turnInterrupt",
		"codex.modelList", "codex.configRead":
		rpcMethod := strings.TrimPrefix(req.Method, "codex.")
		rpcMethod = strings.Replace(rpcMethod, "thread", "thread/", 1)
		rpcMethod = strings.Replace(rpcMethod, "turn", "turn/", 1)
		rpcMethod = strings.Replace(rpcMethod, "model", "model/", 1)
		rpcMethod = strings.Replace(rpcMethod, "config", "config/", 1)
		// Reconstruct the correct method names
		rpcMethod = codexMethodMap(req.Method)
		if params == nil {
			return s.codex.Send(rpcMethod, map[string]interface{}{})
		}
		return s.codex.Send(rpcMethod, params)

	case "codex.configWrite":
		if params == nil {
			return nil, fmt.Errorf("params required")
		}
		return s.codex.Send("config/value/write", params)

	case "codex.respond":
		reqID := params["requestId"]
		result := params["result"]
		if reqID == nil {
			return nil, fmt.Errorf("requestId is required")
		}
		return map[string]bool{"ok": true}, s.codex.Respond(reqID, result)

	case "codex.revertFileChanges":
		return handleFileChanges(params, true)

	case "codex.applyFileChanges":
		return handleFileChanges(params, false)

	// ── Gemini CLI integration (ACP mode) ──

	case "gemini.start":
		cwd := getParamString(params, "cwd")
		if cwd == "" {
			cwd = s.projectRoot
		}
		s.gemini.SetCwd(cwd)
		available := s.gemini.CheckAvailable()
		return map[string]interface{}{
			"ok": true, "available": available, "cwd": cwd, "acpRunning": s.gemini.IsRunning(),
		}, nil

	case "gemini.status":
		return map[string]interface{}{
			"available": s.gemini.Available(),
			"running":   s.gemini.IsRunning(),
		}, nil

	case "gemini.newSession":
		cwd := getParamString(params, "cwd")
		if cwd == "" {
			cwd = s.projectRoot
		}
		if !s.gemini.IsRunning() {
			s.gemini.SetCwd(cwd)
			if err := s.gemini.Start(); err != nil {
				return nil, err
			}
		}
		result, err := s.gemini.NewSession(cwd)
		if err != nil {
			return nil, err
		}
		result["ok"] = true
		return result, nil

	case "gemini.loadSession":
		sessionId := getParamString(params, "sessionId")
		cwd := getParamString(params, "cwd")
		if sessionId == "" {
			return nil, fmt.Errorf("sessionId is required")
		}
		if cwd == "" {
			cwd = s.projectRoot
		}
		if !s.gemini.IsRunning() {
			s.gemini.SetCwd(cwd)
			if err := s.gemini.Start(); err != nil {
				return nil, err
			}
		}
		result, err := s.gemini.LoadSession(sessionId, cwd)
		if err != nil {
			return nil, err
		}
		result["ok"] = true
		return result, nil

	case "gemini.prompt":
		sessionId := getParamString(params, "sessionId")
		text := getParamString(params, "prompt")
		if text == "" {
			text = getParamString(params, "text")
		}
		if sessionId == "" {
			return nil, fmt.Errorf("sessionId is required")
		}
		if text == "" {
			return nil, fmt.Errorf("prompt text is required")
		}
		var images []string
		if arr, ok := params["images"].([]interface{}); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok {
					images = append(images, s)
				}
			}
		}
		result, err := s.gemini.Prompt(sessionId, text, images)
		if err != nil {
			return nil, err
		}
		result["ok"] = true
		return result, nil

	case "gemini.cancel":
		sessionId := getParamString(params, "sessionId")
		if sessionId != "" {
			_ = s.gemini.Cancel(sessionId)
		}
		return map[string]bool{"ok": true}, nil

	case "gemini.setMode":
		sessionId := getParamString(params, "sessionId")
		modeId := getParamString(params, "modeId")
		if sessionId == "" || modeId == "" {
			return nil, fmt.Errorf("sessionId and modeId required")
		}
		if err := s.gemini.SetMode(sessionId, modeId); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	case "gemini.setModel":
		sessionId := getParamString(params, "sessionId")
		modelId := getParamString(params, "modelId")
		if sessionId == "" || modelId == "" {
			return nil, fmt.Errorf("sessionId and modelId required")
		}
		if err := s.gemini.SetModel(sessionId, modelId); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	case "gemini.sessionList":
		cwd := getParamString(params, "cwd")
		if cwd != "" {
			s.gemini.SetCwd(cwd)
		}
		output, err := s.gemini.ListSessions()
		sessions := ParseGeminiSessionList(output)
		result := map[string]interface{}{
			"ok": err == nil, "output": output, "sessions": sessions,
		}
		if err != nil {
			log.Printf("[gemini.sessionList] %v", err)
			result["error"] = err.Error()
		}
		return result, nil

	// ── Claude Code integration (ACP mode via claude-code-acp) ──

	case "claude.start":
		cwd := getParamString(params, "cwd")
		if cwd == "" {
			cwd = s.projectRoot
		}
		available := s.claude.CheckAvailable()
		if available && !s.claude.IsRunning() {
			if err := s.claude.Start(cwd); err != nil {
				log.Printf("[claude] start failed: %v", err)
				return map[string]interface{}{
					"ok": true, "available": available, "cwd": cwd, "running": false,
					"sessionId": s.claude.SessionId(), "model": s.claude.Model(),
					"effort": s.claude.Effort(), "permissionMode": s.claude.Mode(),
					"error": err.Error(),
				}, nil
			}
		} else if cwd != "" {
			s.claude.SetCwd(cwd)
		}
		return map[string]interface{}{
			"ok": true, "available": available, "cwd": cwd, "running": s.claude.IsRunning(),
			"sessionId": s.claude.SessionId(), "model": s.claude.Model(),
			"effort": s.claude.Effort(), "permissionMode": s.claude.Mode(),
		}, nil

	case "claude.status":
		return map[string]interface{}{
			"available":      s.claude.Available(),
			"running":        s.claude.IsRunning(),
			"sessionId":      s.claude.SessionId(),
			"model":          s.claude.Model(),
			"effort":         s.claude.Effort(),
			"permissionMode": s.claude.Mode(),
		}, nil

	case "claude.sessionList":
		cwd := getParamString(params, "cwd")
		if cwd == "" {
			cwd = s.projectRoot
		}
		return s.claude.ListSessions(cwd)

	case "claude.loadSession":
		sessionId := getParamString(params, "sessionId")
		cwd := getParamString(params, "cwd")
		if cwd == "" {
			cwd = s.projectRoot
		}
		return s.claude.LoadSession(sessionId, cwd)

	case "claude.newSession":
		s.claude.ClearSession()
		return map[string]bool{"ok": true}, nil

	case "claude.sessionDelete":
		sessionId := getParamString(params, "sessionId")
		cwd := getParamString(params, "cwd")
		if sessionId == "" {
			return nil, fmt.Errorf("sessionId is required")
		}
		if err := s.claude.DeleteSession(sessionId, cwd); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	case "claude.sessionRename":
		sessionId := getParamString(params, "sessionId")
		title := getParamString(params, "title")
		cwd := getParamString(params, "cwd")
		if sessionId == "" {
			return nil, fmt.Errorf("sessionId is required")
		}
		if err := s.claude.RenameSession(sessionId, title, cwd); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	case "claude.setConfig":
		model := getParamString(params, "model")
		effort := getParamString(params, "effort")
		mode := getParamString(params, "permissionMode")
		s.claude.SetConfig(model, effort, mode)
		return map[string]interface{}{
			"ok": true, "model": s.claude.Model(), "effort": s.claude.Effort(), "permissionMode": s.claude.Mode(),
		}, nil

	case "claude.prompt":
		text := getParamString(params, "prompt")
		if text == "" {
			text = getParamString(params, "text")
		}
		if text == "" {
			return nil, fmt.Errorf("prompt text is required")
		}
		s.claude.SetConfig(
			getParamString(params, "model"),
			getParamString(params, "effort"),
			getParamString(params, "permissionMode"),
		)
		if !s.claude.IsRunning() {
			cwd := getParamString(params, "cwd")
			if cwd == "" {
				cwd = s.projectRoot
			}
			if err := s.claude.Start(cwd); err != nil {
				return nil, fmt.Errorf("failed to start claude: %w", err)
			}
		}
		var images []string
		if arr, ok := params["images"].([]interface{}); ok {
			for _, v := range arr {
				if str, ok := v.(string); ok {
					images = append(images, str)
				}
			}
		}
		if err := s.claude.Prompt(text, images); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "sessionId": s.claude.SessionId()}, nil

	case "claude.cancel":
		s.claude.Cancel()
		return map[string]bool{"ok": true}, nil

	case "claude.stop":
		s.claude.Stop()
		return map[string]bool{"ok": true}, nil

	case "claude.taskStatus":
		return s.claude.TaskStatus(), nil

	case "claude.permission/respond":
		requestId := getParamString(params, "requestId")
		if requestId == "" {
			return nil, fmt.Errorf("requestId is required")
		}
		optionId := getParamString(params, "optionId")
		cancelled, _ := params["cancelled"].(bool)
		if err := s.claude.RespondPermission(requestId, optionId, cancelled); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	// ── Cron Integration ──

	case "cron.list":
		return map[string]interface{}{"ok": true, "crons": s.cron.ListJobs()}, nil

	case "cron.create":
		name := getParamString(params, "name")
		agent := getParamString(params, "agent")
		sessionId := getParamString(params, "sessionId")
		prompt := getParamString(params, "prompt")
		expression := getParamString(params, "expression")
		enabled := getParamBool(params, "enabled")

		if name == "" || agent == "" || prompt == "" || expression == "" {
			return nil, fmt.Errorf("name, agent, prompt and expression are required")
		}

		job, err := s.cron.CreateJob(name, agent, sessionId, prompt, expression, enabled)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "cron": job}, nil

	case "cron.update":
		id := getParamString(params, "id")
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		name := getParamString(params, "name")
		agent := getParamString(params, "agent")
		sessionId := getParamString(params, "sessionId")
		prompt := getParamString(params, "prompt")
		expression := getParamString(params, "expression")
		enabled := getParamBool(params, "enabled")

		job, err := s.cron.UpdateJob(id, name, agent, sessionId, prompt, expression, enabled)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "cron": job}, nil

	case "cron.delete":
		id := getParamString(params, "id")
		if id == "" {
			return nil, fmt.Errorf("id is required")
		}
		if err := s.cron.DeleteJob(id); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil

	default:
		return nil, fmt.Errorf("unknown method: %s", req.Method)
	}
}

func codexMethodMap(method string) string {
	m := map[string]string{
		"codex.threadList":      "thread/list",
		"codex.threadRead":      "thread/read",
		"codex.threadStart":     "thread/start",
		"codex.threadResume":    "thread/resume",
		"codex.threadArchive":   "thread/archive",
		"codex.threadUnarchive": "thread/unarchive",
		"codex.threadRename":    "thread/name/set",
		"codex.threadRollback":  "thread/rollback",
		"codex.threadCompact":   "thread/compact",
		"codex.turnStart":       "turn/start",
		"codex.turnSteer":       "turn/steer",
		"codex.turnInterrupt":   "turn/interrupt",
		"codex.modelList":       "model/list",
		"codex.configRead":      "config/read",
	}
	if v, ok := m[method]; ok {
		return v
	}
	return method
}

func codexAppServerArgs() []string {
	return []string{
		"app-server",
		"--listen", "stdio://",
	}
}

func codexCommand() string {
	if command := strings.TrimSpace(os.Getenv("ANYCODE_CODEX_BIN")); command != "" {
		return command
	}
	const desktopCodex = "/Applications/Codex.app/Contents/Resources/codex"
	if _, err := os.Stat(desktopCodex); err == nil {
		return desktopCodex
	}
	return "codex"
}

func handleFileChanges(params map[string]interface{}, reverse bool) (interface{}, error) {
	changesRaw, ok := params["changes"]
	if !ok {
		return map[string]interface{}{"ok": true, "reverted": []string{}, "applied": []string{}}, nil
	}

	changesJSON, _ := json.Marshal(changesRaw)
	var changes []struct {
		Path string                 `json:"path"`
		Kind map[string]interface{} `json:"kind"`
		Diff string                 `json:"diff"`
	}
	if err := json.Unmarshal(changesJSON, &changes); err != nil {
		return nil, fmt.Errorf("invalid changes format: %w", err)
	}

	var processed []string
	var errors []string

	for _, change := range changes {
		kindType := ""
		if change.Kind != nil {
			if t, ok := change.Kind["type"].(string); ok {
				kindType = t
			}
		}
		if kindType == "" && change.Diff != "" {
			if strings.Contains(change.Diff, "@@") {
				kindType = "update"
			} else {
				kindType = "add"
			}
		}
		if kindType == "" {
			kindType = "add"
		}
		// Normalize kind variants across agents: Codex uses add/update/delete,
		// while claude-code-acp reports edits as "modify". Anything that isn't
		// an explicit add/delete is treated as an in-place update so undo/redo
		// reverts file edits regardless of the originating agent.
		switch kindType {
		case "add", "delete", "update":
		default:
			kindType = "update"
		}

		var err error
		if reverse {
			switch kindType {
			case "add":
				if _, serr := os.Stat(change.Path); serr == nil {
					err = os.Remove(change.Path)
				}
			case "update", "delete":
				if change.Diff != "" {
					unified := buildReversibleDiff(change.Path, change.Diff)
					err = gitApplyReverse(change.Path, unified)
				}
			}
		} else {
			switch kindType {
			case "add":
				lines := strings.Split(change.Diff, "\n")
				var content []string
				for _, l := range lines {
					if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
						content = append(content, l[1:])
					}
				}
				dir := filepath.Dir(change.Path)
				_ = os.MkdirAll(dir, 0755)
				err = os.WriteFile(change.Path, []byte(strings.Join(content, "\n")), 0644)
			case "update":
				if change.Diff != "" {
					unified := buildReversibleDiff(change.Path, change.Diff)
					err = gitApplyForward(change.Path, unified)
				}
			case "delete":
				if _, serr := os.Stat(change.Path); serr == nil {
					err = os.Remove(change.Path)
				}
			}
		}

		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %s", change.Path, err.Error()))
		} else {
			processed = append(processed, change.Path)
		}
	}

	key := "reverted"
	if !reverse {
		key = "applied"
	}
	return map[string]interface{}{
		"ok":     len(errors) == 0,
		key:      processed,
		"errors": errors,
	}, nil
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
	// Accept both /share/<id> and /share/<deviceId>/<id> — the id is the last
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

func (s *Server) handleProxyOpen(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token != s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rawURL := r.URL.Query().Get("url")
	target, err := parseProxyTarget(rawURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	setProxyCookie(w, proxyTokenCookie, token)
	setProxyCookie(w, proxyOriginCookie, encodeCookieValue(target.Scheme+"://"+target.Host))
	http.Redirect(w, r, proxyPathForTarget(target), http.StatusFound)
}

func (s *Server) handleProxyPrefix(w http.ResponseWriter, r *http.Request) {
	if !s.hasProxyAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	target, err := targetFromProxyPath(r.URL.Path, r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	setProxyCookie(w, proxyOriginCookie, encodeCookieValue(target.Scheme+"://"+target.Host))
	s.proxyToTarget(w, r, target)
}

func (s *Server) handleProxyFallback(w http.ResponseWriter, r *http.Request) {
	if !s.hasProxyAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	originCookie, err := r.Cookie(proxyOriginCookie)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	origin, err := decodeCookieValue(originCookie.Value)
	if err != nil {
		http.Error(w, "invalid proxy origin", http.StatusBadRequest)
		return
	}

	baseURL, err := parseProxyTarget(origin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	target := *baseURL
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery
	s.proxyToTarget(w, r, &target)
}

func (s *Server) handleRelayProxyFetch(params map[string]interface{}) (interface{}, error) {
	method := getParamString(params, "method")
	if method == "" {
		method = http.MethodGet
	}
	proxyPath := getParamString(params, "path")
	if proxyPath == "" || !strings.HasPrefix(proxyPath, "/") {
		return nil, fmt.Errorf("invalid proxy path")
	}

	var body []byte
	if encoded := getParamString(params, "bodyBase64"); encoded != "" {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid body: %w", err)
		}
		body = decoded
	}

	req, err := http.NewRequest(method, "https://relay.internal"+proxyPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Host = "anycodeapp.com"

	if rawHeaders, ok := params["headers"].(map[string]interface{}); ok {
		for key, rawValue := range rawHeaders {
			switch values := rawValue.(type) {
			case []interface{}:
				for _, value := range values {
					if s, ok := value.(string); ok {
						req.Header.Add(key, s)
					}
				}
			case []string:
				for _, value := range values {
					req.Header.Add(key, value)
				}
			case string:
				req.Header.Add(key, values)
			}
		}
	}
	if forwardedHost := req.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		req.Host = forwardedHost
	}

	rec := httptest.NewRecorder()
	if req.URL.Path == proxyOpenPath {
		s.handleProxyOpen(rec, req)
	} else if strings.HasPrefix(req.URL.Path, proxyPrefix) {
		s.handleProxyPrefix(rec, req)
	} else if s.hasProxyAuth(req) {
		s.handleProxyFallback(rec, req)
	} else {
		http.NotFound(rec, req)
	}

	resp := rec.Result()
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	headers := make(map[string][]string)
	for key, values := range resp.Header {
		headers[key] = append([]string(nil), values...)
	}

	return map[string]interface{}{
		"status":     resp.StatusCode,
		"headers":    headers,
		"bodyBase64": base64.StdEncoding.EncodeToString(respBody),
	}, nil
}

func (s *Server) proxyToTarget(w http.ResponseWriter, r *http.Request, target *url.URL) {
	origin := &url.URL{Scheme: target.Scheme, Host: target.Host}
	proxy := httputil.NewSingleHostReverseProxy(origin)

	targetPath := target.EscapedPath()
	if targetPath == "" {
		targetPath = "/"
	}
	targetQuery := target.RawQuery
	incomingHost := r.Host
	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		incomingHost = forwardedHost
	}
	incomingScheme := "http"
	if r.TLS != nil {
		incomingScheme = "https"
	}
	if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto == "http" || forwardedProto == "https" {
		incomingScheme = forwardedProto
	}

	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = targetPath
		req.URL.RawPath = target.RawPath
		req.URL.RawQuery = targetQuery
		req.Host = target.Host
		req.RequestURI = ""
		req.Header.Del("Accept-Encoding")
		stripProxyCookies(req)
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		rewriteLocationHeader(resp, target, incomingHost)
		rewriteSetCookieHeaders(resp)
		return rewriteTextResponse(resp, target, incomingHost, incomingScheme)
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[proxy] %s -> %s error: %v", r.URL.String(), target.String(), err)
		http.Error(w, err.Error(), http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}

func parseProxyTarget(rawURL string) (*url.URL, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("target URL must include scheme and host")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("only http and https targets are supported")
	}
	if target.Path == "" {
		target.Path = "/"
	}
	return target, nil
}

func targetFromProxyPath(path, rawQuery string) (*url.URL, error) {
	rest := strings.TrimPrefix(path, proxyPrefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid proxy path")
	}

	scheme := parts[0]
	host, err := url.PathUnescape(parts[1])
	if err != nil {
		return nil, err
	}
	targetPath := "/"
	if len(parts) == 3 && parts[2] != "" {
		targetPath = "/" + parts[2]
	}

	return parseProxyTarget((&url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     targetPath,
		RawQuery: rawQuery,
	}).String())
}

func proxyPathForTarget(target *url.URL) string {
	targetPath := target.EscapedPath()
	if targetPath == "" {
		targetPath = "/"
	}

	proxyURL := &url.URL{
		Path:     proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host) + targetPath,
		RawQuery: target.RawQuery,
	}
	return proxyURL.String()
}

func (s *Server) hasProxyAuth(r *http.Request) bool {
	if r.URL.Query().Get("token") == s.token {
		return true
	}
	cookie, err := r.Cookie(proxyTokenCookie)
	return err == nil && cookie.Value == s.token
}

func setProxyCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int((24 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func encodeCookieValue(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCookieValue(value string) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func stripProxyCookies(req *http.Request) {
	cookies := req.Cookies()
	if len(cookies) == 0 {
		return
	}

	kept := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie.Name == proxyTokenCookie || cookie.Name == proxyOriginCookie {
			continue
		}
		kept = append(kept, cookie.Name+"="+cookie.Value)
	}

	if len(kept) == 0 {
		req.Header.Del("Cookie")
		return
	}
	req.Header.Set("Cookie", strings.Join(kept, "; "))
}

func rewriteLocationHeader(resp *http.Response, target *url.URL, incomingHost string) {
	location := resp.Header.Get("Location")
	if location == "" {
		return
	}

	locURL, err := url.Parse(location)
	if err != nil {
		return
	}

	if locURL.IsAbs() && locURL.Host == target.Host && locURL.Scheme == target.Scheme {
		resp.Header.Set("Location", proxyPathForTarget(locURL))
		return
	}

	if strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
		relURL, err := url.Parse(location)
		if err != nil {
			return
		}
		locURL := *target
		locURL.Path = relURL.Path
		locURL.RawPath = relURL.RawPath
		locURL.RawQuery = relURL.RawQuery
		resp.Header.Set("Location", proxyPathForTarget(&locURL))
		return
	}

	if locURL.IsAbs() && incomingHost != "" {
		if locURL.Scheme == "ws" || locURL.Scheme == "wss" {
			proxyScheme := "http"
			if locURL.Scheme == "wss" {
				proxyScheme = "https"
			}
			locURL.Scheme = proxyScheme
			resp.Header.Set("Location", proxyPathForTarget(locURL))
		}
	}
}

func rewriteSetCookieHeaders(resp *http.Response) {
	values := resp.Header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}

	resp.Header.Del("Set-Cookie")
	for _, value := range values {
		parts := strings.Split(value, ";")
		kept := parts[:0]
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToLower(trimmed), "domain=") {
				continue
			}
			kept = append(kept, part)
		}
		resp.Header.Add("Set-Cookie", strings.Join(kept, ";"))
	}
}

func rewriteTextResponse(resp *http.Response, target *url.URL, incomingHost, incomingScheme string) error {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !isRewriteableContent(contentType) || resp.Body == nil {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	origin := target.Scheme + "://" + target.Host
	proxyOrigin := incomingScheme + "://" + incomingHost + proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)

	text := string(body)
	if incomingHost != "" {
		wsProxyOrigin := "ws://" + incomingHost + proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)
		if incomingScheme == "https" {
			wsProxyOrigin = "wss://" + incomingHost + proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)
		}
		text = strings.ReplaceAll(text, "ws://"+target.Host, wsProxyOrigin)
		text = strings.ReplaceAll(text, "wss://"+target.Host, wsProxyOrigin)
	}
	text = strings.ReplaceAll(text, origin, proxyOrigin)
	text = strings.ReplaceAll(text, "//"+target.Host, proxyOrigin)
	if strings.Contains(contentType, "text/html") {
		text = injectBaseTag(text, proxyOrigin+"/")
		text = rewriteHTMLRootRelativeRefs(text, target)
	} else if strings.Contains(contentType, "text/css") {
		text = rewriteCSSRootRelativeRefs(text, target)
	} else if strings.Contains(contentType, "javascript") {
		text = rewriteJSPublicPath(text, proxyOrigin+"/")
	}

	rewritten := []byte(text)
	resp.Body = io.NopCloser(strings.NewReader(text))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	return nil
}

func isRewriteableContent(contentType string) bool {
	return strings.Contains(contentType, "text/html") ||
		strings.Contains(contentType, "text/css") ||
		strings.Contains(contentType, "javascript") ||
		strings.Contains(contentType, "application/json") ||
		strings.Contains(contentType, "text/plain")
}

func rewriteHTMLRootRelativeRefs(text string, target *url.URL) string {
	proxyBase := proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)
	replacements := [][2]string{
		{`href="/`, `href="` + proxyBase + `/`},
		{`src="/`, `src="` + proxyBase + `/`},
		{`action="/`, `action="` + proxyBase + `/`},
		{`poster="/`, `poster="` + proxyBase + `/`},
		{`href='/`, `href='` + proxyBase + `/`},
		{`src='/`, `src='` + proxyBase + `/`},
		{`action='/`, `action='` + proxyBase + `/`},
		{`poster='/`, `poster='` + proxyBase + `/`},
	}
	for _, pair := range replacements {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}

// injectBaseTag inserts <base href="baseHref"> as the first child of <head>.
// This fixes root-relative refs that the string rewriter can't catch: inline
// @font-face url() in <style> blocks, preload links, and browser-resolved paths.
// It does NOT fix webpack __webpack_public_path__ (handled by rewriteJSPublicPath).
func injectBaseTag(html, baseHref string) string {
	tag := `<base href="` + baseHref + `">`
	for _, marker := range []string{"<head>", "<head ", "<HEAD>", "<HEAD "} {
		if idx := strings.Index(html, marker); idx >= 0 {
			end := strings.Index(html[idx:], ">")
			if end < 0 {
				break
			}
			pos := idx + end + 1
			return html[:pos] + tag + html[pos:]
		}
	}
	// No <head> tag — prepend to body as fallback.
	if idx := strings.Index(html, "<body"); idx >= 0 {
		return html[:idx] + "<head>" + tag + "</head>" + html[idx:]
	}
	return html
}

func rewriteJSPublicPath(text, proxyBase string) string {
	quoted := `"` + proxyBase + `"`
	for _, pat := range []string{
		`__webpack_public_path__="/"`,
		`__webpack_public_path__='/'`,
		`__webpack_public_path__ = "/"`,
		`__webpack_public_path__ = '/'`,
	} {
		text = strings.ReplaceAll(text, pat, `__webpack_public_path__=`+quoted)
	}
	return text
}

func rewriteCSSRootRelativeRefs(text string, target *url.URL) string {
	proxyBase := proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)
	replacements := [][2]string{
		{`url("/`, `url("` + proxyBase + `/`},
		{`url('/`, `url('` + proxyBase + `/`},
		{`url(/`, `url(` + proxyBase + `/`},
	}
	for _, pair := range replacements {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}
