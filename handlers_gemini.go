package main

import (
	"fmt"
	"log"
)

func (s *Server) handleGeminiStart(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("gemini")
	p, err := decodeParams[geminiStartParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
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
	p, err := decodeParams[geminiNewSessionParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
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
	p, err := decodeParams[geminiLoadSessionParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	if p.SessionID == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if !runtime.IsRunning() {
		runtime.SetCwd(context.cwd)
		if err := runtime.Start(context.cwd); err != nil {
			return nil, err
		}
	}
	result, err := runtime.LoadSession(p.SessionID, context.cwd)
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("gemini")
	return s.runtime.SessionResponse("gemini", result), nil

}

func (s *Server) handleGeminiPrompt(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("gemini")
	p, err := decodeParams[geminiPromptParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	if p.SessionID == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	text := firstNonEmpty(p.Prompt, p.Text)
	if text == "" {
		return nil, fmt.Errorf("prompt text is required")
	}
	runtime.SetCwd(context.cwd)
	promptResult, err := runtime.Prompt(PromptRequest{SessionID: p.SessionID, Text: text, Images: p.Images})
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
	p, err := decodeParams[geminiCancelParams](req)
	if err != nil {
		return nil, err
	}
	if p.SessionID != "" {
		_ = runtime.Cancel(p.SessionID)
	}
	s.persistRuntimeState("gemini")
	s.persistInterruptedOperation("gemini", status)
	return s.runtime.ActionResponse("gemini", nil), nil

}

func (s *Server) handleGeminiSetMode(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[geminiSetModeParams](req)
	if err != nil {
		return nil, err
	}
	if p.SessionID == "" || p.ModeID == "" {
		return nil, fmt.Errorf("sessionId and modeId required")
	}
	if err := s.gemini.SetMode(p.SessionID, p.ModeID); err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("gemini", nil), nil

}

func (s *Server) handleGeminiSetModel(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[geminiSetModelParams](req)
	if err != nil {
		return nil, err
	}
	if p.SessionID == "" || p.ModelID == "" {
		return nil, fmt.Errorf("sessionId and modelId required")
	}
	if err := s.gemini.SetModel(p.SessionID, p.ModelID); err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("gemini", nil), nil

}

func (s *Server) handleGeminiSessionList(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[geminiSessionListParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
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
