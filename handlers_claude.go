package main

import (
	"fmt"
	"log"
)

func (s *Server) handleClaudeStart(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	cwd := getParamString(params, "cwd")
	if cwd == "" {
		cwd = s.projectRoot
	}
	available := s.claude.CheckAvailable()
	if available && !s.claude.IsRunning() {
		if err := s.claude.Start(cwd); err != nil {
			log.Printf("[claude] start failed: %v", err)
			resp := buildClaudeConfigResponse(s.claude)
			resp["ok"] = true
			resp["available"] = available
			resp["cwd"] = cwd
			resp["running"] = false
			resp["sessionId"] = s.claude.SessionId()
			resp["error"] = err.Error()
			return resp, nil
		}
	} else if cwd != "" {
		s.claude.SetCwd(cwd)
	}
	resp := buildClaudeConfigResponse(s.claude)
	resp["ok"] = true
	resp["available"] = available
	resp["cwd"] = cwd
	resp["running"] = s.claude.IsRunning()
	resp["sessionId"] = s.claude.SessionId()
	return resp, nil

}

func (s *Server) handleClaudeStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	resp := buildClaudeConfigResponse(s.claude)
	resp["available"] = s.claude.Available()
	resp["running"] = s.claude.IsRunning()
	resp["sessionId"] = s.claude.SessionId()
	return resp, nil

}

func (s *Server) handleClaudeSessionList(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	cwd := getParamString(params, "cwd")
	if cwd == "" {
		cwd = s.projectRoot
	}
	return s.claude.ListSessions(cwd)

}

func (s *Server) handleClaudeLoadSession(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	sessionId := getParamString(params, "sessionId")
	cwd := getParamString(params, "cwd")
	if cwd == "" {
		cwd = s.projectRoot
	}
	return s.claude.LoadSession(sessionId, cwd)

}

func (s *Server) handleClaudeNewSession(req RpcRequest, client *wsClient) (interface{}, error) {
	s.claude.ClearSession()
	return map[string]bool{"ok": true}, nil

}

func (s *Server) handleClaudeSessionDelete(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	sessionId := getParamString(params, "sessionId")
	cwd := getParamString(params, "cwd")
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if err := s.claude.DeleteSession(sessionId, cwd); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil

}

func (s *Server) handleClaudeSessionRename(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
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

}

func (s *Server) handleClaudeSetConfig(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	s.claude.SetConfig(buildClaudeConfigPatch(params))
	resp := buildClaudeConfigResponse(s.claude)
	resp["ok"] = true
	return resp, nil

}

func (s *Server) handleClaudePrompt(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	text := getParamString(params, "prompt")
	if text == "" {
		text = getParamString(params, "text")
	}
	if text == "" {
		return nil, fmt.Errorf("prompt text is required")
	}
	s.claude.SetConfig(buildClaudeConfigPatch(params))
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
	resp := buildClaudeConfigResponse(s.claude)
	resp["ok"] = true
	resp["sessionId"] = s.claude.SessionId()
	return resp, nil

}

func (s *Server) handleClaudeCancel(req RpcRequest, client *wsClient) (interface{}, error) {
	s.claude.Cancel()
	return map[string]bool{"ok": true}, nil

}

func (s *Server) handleClaudeStop(req RpcRequest, client *wsClient) (interface{}, error) {
	s.claude.Stop()
	return map[string]bool{"ok": true}, nil

}

func (s *Server) handleClaudeTaskStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.claude.TaskStatus(), nil

}

func (s *Server) handleClaudePermissionRespond(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
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

}
