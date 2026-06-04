package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type wsClient struct {
	conn     *websocket.Conn
	sendChan chan []byte
	authed   bool
	host     string
}

func (c *wsClient) send(data []byte) {
	select {
	case c.sendChan <- data:
	default:
		c.conn.Close() // slow client, disconnect
	}
}

func (c *wsClient) writePump(done chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case <-done:
			return
		case data, ok := <-c.sendChan:
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(ctrlWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
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
// (relay.go), so the relay connection is treated exactly like a local client 闁?
// the remote client still performs the `auth` handshake end-to-end.
func (s *Server) serveConn(conn *websocket.Conn, hostLabel string) {
	log.Println("[server] client connected")

	conn.SetReadLimit(10 * 1024 * 1024)

	client := &wsClient{
		conn:     conn,
		host:     hostLabel,
		sendChan: make(chan []byte, 256),
	}

	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	// Keepalive: expire the read if no frame (pong/RPC/heartbeat) arrives in
	// time, and refresh the deadline on every pong.
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	done := make(chan struct{})
	go client.writePump(done)

	defer func() {
		close(done)
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
		// Any inbound frame proves the link is alive; extend the read window.
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))

		var req RpcRequest
		if err := json.Unmarshal(data, &req); err != nil {
			reply, _ := json.Marshal(makeError(0, -32700, "Parse error"))
			client.send(reply)
			continue
		}

		// Application-level heartbeat from the web client (the browser DOM
		// WebSocket API can't send protocol pings). Reply with a `pong`
		// notification so the client can detect a dead link and reconnect.
		// It carries no id, so handle it before the id validation below.
		if req.Method == "ping" {
			reply, _ := json.Marshal(makeNotification("pong", nil))
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
