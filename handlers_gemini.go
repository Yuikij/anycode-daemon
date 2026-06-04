package main

import (
	"fmt"
	"log"
)

func (s *Server) handleGeminiStart(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("gemini")
	_, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	runtime.SetCwd(context.cwd)
	available := runtime.CheckAvailable()
	return s.runtime.StartResponse("gemini", RuntimeStartOptions{Available: available, Cwd: context.cwd}), nil

}

func (s *Server) handleGeminiStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.StatusSnapshot("gemini"), nil

}

func (s *Server) handleGeminiNewSession(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("gemini")
	_, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	if !runtime.IsRunning() {
		runtime.SetCwd(context.cwd)
		if err := runtime.Start(context.cwd); err != nil {
			return nil, err
		}
	}
	result, err := runtime.NewSession(context.cwd)
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("gemini")
	return s.runtime.SessionResponse("gemini", result), nil

}

func (s *Server) handleGeminiLoadSession(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("gemini")
	params, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	sessionId := getParamString(params, "sessionId")
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if !runtime.IsRunning() {
		runtime.SetCwd(context.cwd)
		if err := runtime.Start(context.cwd); err != nil {
			return nil, err
		}
	}
	result, err := runtime.LoadSession(sessionId, context.cwd)
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("gemini")
	return s.runtime.SessionResponse("gemini", result), nil

}

func (s *Server) handleGeminiPrompt(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("gemini")
	params, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
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
	runtime.SetCwd(context.cwd)
	var images []string
	if arr, ok := params["images"].([]interface{}); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				images = append(images, s)
			}
		}
	}
	promptResult, err := runtime.Prompt(PromptRequest{SessionID: sessionId, Text: text, Images: images})
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("gemini")
	s.persistAcceptedOperation("gemini", promptResult.OperationID)
	return s.runtime.PromptAcceptedResponse("gemini", promptResult), nil

}

func (s *Server) handleGeminiCancel(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("gemini")
	status := runtime.TaskStatus()
	params := getParams(req.Params)
	sessionId := getParamString(params, "sessionId")
	if sessionId != "" {
		_ = runtime.Cancel(sessionId)
	}
	s.persistRuntimeState("gemini")
	s.persistInterruptedOperation("gemini", status)
	return s.runtime.ActionResponse("gemini", nil), nil

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
	return s.runtime.ActionResponse("gemini", nil), nil

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
	return s.runtime.ActionResponse("gemini", nil), nil

}

func (s *Server) handleGeminiSessionList(req RpcRequest, client *wsClient) (interface{}, error) {
	params, context, err := s.normalizeProjectScopedParams(getParams(req.Params), true)
	if err != nil {
		return nil, err
	}
	_ = params
	s.gemini.SetCwd(context.cwd)
	output, err := s.gemini.ListSessions()
	sessions := ParseGeminiSessionList(output)
	result := map[string]interface{}{
		"ok": err == nil, "output": output, "sessions": sessions,
	}
	if err != nil {
		log.Printf("[gemini.sessionList] %v", err)
		result["error"] = err.Error()
	}
	return s.runtime.ActionResponse("gemini", result), nil

	// 闁冲厜鍋撻柍鍏夊亾 Claude Code integration (ACP mode via claude-code-acp) 闁冲厜鍋撻柍鍏夊亾

}

func (s *Server) handleGeminiTaskStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.TaskSnapshot("gemini", RuntimeSnapshotOptions{
		LatestSeq:     s.latestEventSeq(),
		Project:       s.currentProjectInfo(),
		LastOperation: s.latestOperationPayload("gemini"),
	}), nil
}
