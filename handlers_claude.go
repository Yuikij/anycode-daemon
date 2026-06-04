package main

import (
	"fmt"
	"log"
)

func (s *Server) handleClaudeStart(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("claude")
	_, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	cwd := context.cwd
	available := runtime.CheckAvailable()
	if available && !runtime.IsRunning() {
		if err := runtime.Start(cwd); err != nil {
			log.Printf("[claude] start failed: %v", err)
			return s.runtime.StartResponse("claude", RuntimeStartOptions{Available: available, Cwd: cwd, Error: err}), nil
		}
	} else if cwd != "" {
		runtime.SetCwd(cwd)
	}
	return s.runtime.StartResponse("claude", RuntimeStartOptions{Available: available, Cwd: cwd}), nil

}

func (s *Server) handleClaudeStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.StatusSnapshot("claude"), nil

}

func (s *Server) handleClaudeSessionList(req RpcRequest, client *wsClient) (interface{}, error) {
	_, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	result, err := s.claude.ListSessions(context.cwd)
	if err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("claude", result), nil

}

func (s *Server) handleClaudeLoadSession(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("claude")
	params, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	sessionId := getParamString(params, "sessionId")
	result, err := runtime.LoadSession(sessionId, context.cwd)
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("claude")
	return s.runtime.SessionResponse("claude", result), nil

}

func (s *Server) handleClaudeNewSession(req RpcRequest, client *wsClient) (interface{}, error) {
	_, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	s.claude.SetCwd(context.cwd)
	s.claude.ClearSession()
	s.persistRuntimeState("claude")
	return s.runtime.SessionResponse("claude", nil), nil

}

func (s *Server) handleClaudeSessionDelete(req RpcRequest, client *wsClient) (interface{}, error) {
	params, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	sessionId := getParamString(params, "sessionId")
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if err := s.claude.DeleteSession(sessionId, context.cwd); err != nil {
		return nil, err
	}
	s.persistRuntimeState("claude")
	return s.runtime.ActionResponse("claude", nil), nil

}

func (s *Server) handleClaudeSessionRename(req RpcRequest, client *wsClient) (interface{}, error) {
	params, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	sessionId := getParamString(params, "sessionId")
	title := getParamString(params, "title")
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if err := s.claude.RenameSession(sessionId, title, context.cwd); err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("claude", nil), nil

}

func (s *Server) handleClaudeSetConfig(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	s.claude.SetConfig(buildClaudeConfigPatch(params))
	return s.runtime.ConfigResponse("claude"), nil

}

func (s *Server) handleClaudePrompt(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("claude")
	params, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	text := getParamString(params, "prompt")
	if text == "" {
		text = getParamString(params, "text")
	}
	if text == "" {
		return nil, fmt.Errorf("prompt text is required")
	}
	s.claude.SetConfig(buildClaudeConfigPatch(params))
	if !runtime.IsRunning() {
		if err := runtime.Start(context.cwd); err != nil {
			return nil, fmt.Errorf("failed to start claude: %w", err)
		}
	} else {
		runtime.SetCwd(context.cwd)
	}
	var images []string
	if arr, ok := params["images"].([]interface{}); ok {
		for _, v := range arr {
			if str, ok := v.(string); ok {
				images = append(images, str)
			}
		}
	}
	result, err := runtime.Prompt(PromptRequest{Text: text, Images: images})
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("claude")
	s.persistAcceptedOperation("claude", result.OperationID)
	return s.runtime.PromptAcceptedResponse("claude", result), nil

}

func (s *Server) handleClaudeCancel(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("claude")
	status := runtime.TaskStatus()
	_ = runtime.Cancel(runtime.SessionID())
	s.persistRuntimeState("claude")
	s.persistInterruptedOperation("claude", status)
	return s.runtime.ActionResponse("claude", nil), nil

}

func (s *Server) handleClaudeStop(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("claude")
	status := runtime.TaskStatus()
	runtime.Stop()
	s.persistRuntimeState("claude")
	s.persistInterruptedOperation("claude", status)
	return s.runtime.ActionResponse("claude", nil), nil

}

func (s *Server) handleClaudeTaskStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.TaskSnapshot("claude", RuntimeSnapshotOptions{
		LatestSeq:      s.latestEventSeq(),
		Project:        s.currentProjectInfo(),
		LastOperation:  s.latestOperationPayload("claude"),
		LastPermission: s.latestPermissionPayload("claude"),
	}), nil

}

func (s *Server) handleClaudePermissionRespond(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	requestId := getParamString(params, "requestId")
	if requestId == "" {
		return nil, fmt.Errorf("requestId is required")
	}
	optionId := getParamString(params, "optionId")
	cancelled, _ := params["cancelled"].(bool)
	if err := s.runtime.ClaudeRuntime().ResolvePermission(requestId, optionId, cancelled); err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("claude", nil), nil

}
