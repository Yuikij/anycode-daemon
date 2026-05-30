package main

import (
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// StartRelay opens (and keeps re-opening) an outbound WebSocket to the AnyCode
// relay, registering this machine as the "agent" for its device. The relay
// (a Durable Object) forwards client traffic onto this connection, which we
// then serve with the exact same JSON-RPC loop used for local clients.
func (s *Server) StartRelay(relayURL, deviceToken string) {
	go s.relayLoop(relayURL, deviceToken)
}

func relayWSURL(relayURL, deviceToken string) (string, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/relay/agent"
	q := u.Query()
	q.Set("token", deviceToken)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *Server) relayLoop(relayURL, deviceToken string) {
	wsURL, err := relayWSURL(relayURL, deviceToken)
	if err != nil {
		log.Printf("[relay] invalid relay url: %v", err)
		return
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		log.Printf("[relay] connecting to %s", relayURL)
		dialer := websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
			ReadBufferSize:   10 * 1024 * 1024,
			WriteBufferSize:  10 * 1024 * 1024,
		}
		conn, resp, err := dialer.Dial(wsURL, nil)
		if err != nil {
			status := ""
			if resp != nil {
				status = resp.Status
			}
			log.Printf("[relay] connect failed: %v %s (retry in %s)", err, status, backoff)
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		log.Printf("[relay] connected; ready for clients")
		backoff = time.Second

		// Blocks until the relay link drops; treats the link like a local client.
		s.serveConn(conn, "relay")

		log.Printf("[relay] disconnected; reconnecting in %s", backoff)
		time.Sleep(backoff)
	}
}
