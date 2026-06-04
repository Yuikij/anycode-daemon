package main

import (
	"fmt"
	"log"
)

func (s *Server) handleCodexStart(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
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

}

func (s *Server) handleCodexStop(req RpcRequest, client *wsClient) (interface{}, error) {
	s.codex.Stop()
	s.codexMu.Lock()
	s.codexEvents = s.codexEvents[:0]
	s.codexTurnRunning = false
	s.codexMu.Unlock()
	return map[string]bool{"ok": true}, nil

}

func (s *Server) handleCodexStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return map[string]bool{"running": s.codex.IsRunning()}, nil

	// Replay buffer for reconnecting clients: returns whether a turn is in
	// progress plus the current turn's buffered streaming events so the UI can
	// be rebuilt after a disconnect without waiting for the next delta.
}

func (s *Server) handleCodexTaskStatus(req RpcRequest, client *wsClient) (interface{}, error) {
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

}

func (s *Server) handleCodexConfigWrite(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	if params == nil {
		return nil, fmt.Errorf("params required")
	}
	return s.codex.Send("config/value/write", params)

}

func (s *Server) handleCodexRespond(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	reqID := params["requestId"]
	result := params["result"]
	if reqID == nil {
		return nil, fmt.Errorf("requestId is required")
	}
	return map[string]bool{"ok": true}, s.codex.Respond(reqID, result)

}

func (s *Server) handleCodexRevertFileChanges(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	return handleFileChanges(params, true)

}

func (s *Server) handleCodexApplyFileChanges(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	return handleFileChanges(params, false)

	// 闁冲厜鍋撻柍鍏夊亾 Gemini CLI integration (ACP mode) 闁冲厜鍋撻柍鍏夊亾

}
