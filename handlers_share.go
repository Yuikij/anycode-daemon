package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) handleShareCreate(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[shareCreateParams](req)
	if err != nil {
		return nil, err
	}
	html := p.HTML
	if html == "" {
		return nil, fmt.Errorf("html content is required")
	}

	// The HTML stays on THIS machine 闁?we never upload it to the cloud.
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

	// 闁冲厜鍋撻柍鍏夊亾 Browse any directory (absolute paths) 闁冲厜鍋撻柍鍏夊亾
}
