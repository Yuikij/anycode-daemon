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
// "闂傗偓閹稿孩顦ч梻鍌滅節缁楀骞欏鍕▕闁告艾姘︾换娑欑▔瀹ュ嫮鐟? symptom: the agent link is dead but never reconnects.
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
	port        int
	token       string
	projectRoot string

	codex  *AgentBridge
	gemini *GeminiBridge
	claude *ClaudeBridge

	cron *CronManager

	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	routes  map[string]func(req RpcRequest, client *wsClient) (interface{}, error)

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
		routes:      make(map[string]func(req RpcRequest, client *wsClient) (interface{}, error)),
	}
	s.initRoutes()

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
