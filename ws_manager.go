package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
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
		return
	default:
	}
	// Buffer full: the writer is briefly behind (typical during an agent streaming
	// burst over the higher-latency relay link). Apply bounded backpressure instead
	// of instantly dropping the connection; only close a genuinely stuck/dead link.
	select {
	case c.sendChan <- data:
	case <-time.After(sendBlockTimeout):
		log.Printf("[server] send buffer stuck for %s on %q; closing link", sendBlockTimeout, c.host)
		c.conn.Close()
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

// relayKeepalive sends an app-level relay.ping over the relay link and drops the
// connection if no relay.pong (answered in JS by the relay Durable Object) comes
// back in time. See the relayPingPeriod/relayPongWait comment for why protocol
// ping/pong is insufficient for the relay path.
func (s *Server) relayKeepalive(client *wsClient, conn *websocket.Conn, done chan struct{}, lastPong, pongSeen *int64) {
	ticker := time.NewTicker(relayPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// Only enforce the dead-link timeout once we've seen at least one
			// relay.pong on this connection. This feature-detects a relay DO that
			// understands relay.ping, so the daemon can ship before the DO is
			// updated without churning against an old DO (which would never answer
			// and look permanently dead). A healthy updated DO replies on the first
			// ping, so enforcement activates within one interval.
			if atomic.LoadInt64(pongSeen) != 0 {
				last := time.Unix(0, atomic.LoadInt64(lastPong))
				if time.Since(last) > relayPongWait {
					log.Printf("[relay] no relay.pong within %s; dropping dead relay link to reconnect", relayPongWait)
					conn.Close() // unblocks ReadMessage -> serveConn returns -> relayLoop reconnects
					return
				}
			}
			client.send(relayPingFrame)
		}
	}
}

func (s *Server) broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	// Snapshot the authed clients under the lock, then send outside it: send() may
	// now block (bounded backpressure), and we must not hold s.mu while it does,
	// or a slow link would stall connects/disconnects and other broadcasts.
	s.mu.RLock()
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		if c.authed {
			clients = append(clients, c)
		}
	}
	s.mu.RUnlock()
	for _, c := range clients {
		c.send(data)
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

	// The relay link multiplexes every remote client and rides the higher-latency
	// Cloudflare path, so it needs a much larger send buffer to absorb agent
	// streaming bursts without dropping.
	isRelay := hostLabel == "relay"
	sendBuffer := clientSendBuffer
	if isRelay {
		sendBuffer = relaySendBuffer
	}
	client := &wsClient{
		conn:     conn,
		host:     hostLabel,
		sendChan: make(chan []byte, sendBuffer),
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

	// The relay link needs app-level keepalive (its protocol pongs are faked by
	// the Cloudflare edge, so they can't detect a dead DO path). Direct local
	// clients are unaffected.
	var lastRelayPong int64
	var relayPongSeen int64
	if isRelay {
		atomic.StoreInt64(&lastRelayPong, time.Now().UnixNano())
		go s.relayKeepalive(client, conn, done, &lastRelayPong, &relayPongSeen)
	}

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

		// relay.pong answers our relay.ping liveness probe (see relayKeepalive).
		// It carries no id; handle it before id validation and don't reply.
		if req.Method == "relay.pong" {
			if isRelay {
				atomic.StoreInt64(&lastRelayPong, time.Now().UnixNano())
				atomic.StoreInt64(&relayPongSeen, 1)
			}
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

		// Dispatch the (potentially slow) handler in its own goroutine. Handlers
		// can block for a long time or even hang — e.g. an external agent CLI that
		// is unresponsive (a slow agent CLI subprocess can stall for tens of seconds).
		// If we ran it inline on this read loop, that single call would: (1) starve
		// relay.pong processing so the keepalive watchdog wrongly kills the link,
		// (2) stall every other client multiplexed over the same relay link, and
		// (3) prevent serveConn from ever returning, so the relay link can never
		// reconnect. Agent bridges serialize internally and JSON-RPC responses are
		// matched by id, so concurrent handling is safe.
		go func(req RpcRequest, id interface{}) {
			result, herr := s.handleRequest(req, client)
			var reply []byte
			if herr != nil {
				reply, _ = json.Marshal(makeError(id, -32000, herr.Error()))
			} else {
				reply, _ = json.Marshal(makeResponse(id, result))
			}
			client.send(reply)
		}(req, id)
	}
}
