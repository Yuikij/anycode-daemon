package main

import (
	"fmt"
	"log"
)

func (s *Server) handleGeminiStart(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	cwd := getParamString(params, "cwd")
	if cwd == "" {
		cwd = s.projectRoot
	}
	s.gemini.SetCwd(cwd)
	available := s.gemini.CheckAvailable()
	return map[string]interface{}{
		"ok": true, "available": available, "cwd": cwd, "acpRunning": s.gemini.IsRunning(),
	}, nil

}

func (s *Server) handleGeminiStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return map[string]interface{}{
		"available": s.gemini.Available(),
		"running":   s.gemini.IsRunning(),
	}, nil

}

func (s *Server) handleGeminiNewSession(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
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

}

func (s *Server) handleGeminiLoadSession(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
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

}

func (s *Server) handleGeminiPrompt(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
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

}

func (s *Server) handleGeminiCancel(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	sessionId := getParamString(params, "sessionId")
	if sessionId != "" {
		_ = s.gemini.Cancel(sessionId)
	}
	return map[string]bool{"ok": true}, nil

}

func (s *Server) handleGeminiSetMode(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	sessionId := getParamString(params, "sessionId")
	modeId := getParamString(params, "modeId")
	if sessionId == "" || modeId == "" {
		return nil, fmt.Errorf("sessionId and modeId required")
	}
	if err := s.gemini.SetMode(sessionId, modeId); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil

}

func (s *Server) handleGeminiSetModel(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	sessionId := getParamString(params, "sessionId")
	modelId := getParamString(params, "modelId")
	if sessionId == "" || modelId == "" {
		return nil, fmt.Errorf("sessionId and modelId required")
	}
	if err := s.gemini.SetModel(sessionId, modelId); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil

}

func (s *Server) handleGeminiSessionList(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
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

	// 闁冲厜鍋撻柍鍏夊亾 Claude Code integration (ACP mode via claude-code-acp) 闁冲厜鍋撻柍鍏夊亾

}
