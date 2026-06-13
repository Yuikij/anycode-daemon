package main

import (
	"fmt"
	"log"
)

func (s *Server) handleClaudeStart(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("claude")
	p, err := decodeParams[claudeStartParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
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
	p, err := decodeParams[claudeSessionListParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
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
	p, err := decodeParams[claudeLoadSessionParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	result, err := runtime.LoadSession(p.SessionID, context.cwd)
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("claude")
	return s.runtime.SessionResponse("claude", result), nil

}

func (s *Server) handleClaudeNewSession(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("claude")
	p, err := decodeParams[claudeNewSessionParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	result, err := runtime.NewSession(context.cwd)
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("claude")
	return s.runtime.SessionResponse("claude", result), nil

}

func (s *Server) handleClaudeSessionDelete(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[claudeSessionDeleteParams](req)
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
	if err := s.claude.DeleteSession(p.SessionID, context.cwd); err != nil {
		return nil, err
	}
	s.persistRuntimeState("claude")
	return s.runtime.ActionResponse("claude", nil), nil

}

func (s *Server) handleClaudeSessionRename(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[claudeSessionRenameParams](req)
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
	if err := s.claude.RenameSession(p.SessionID, p.Title, context.cwd); err != nil {
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
	p, err := decodeParams[claudePromptParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	text := firstNonEmpty(p.Prompt, p.Text)
	if text == "" {
		return nil, fmt.Errorf("prompt text is required")
	}
	// Config fields (model/effort/permissionMode) use a tri-state null-vs-absent
	// semantics that JSON structs can't express, so the patch still reads the
	// raw param map.
	s.claude.SetConfig(buildClaudeConfigPatch(getParams(req.Params)))
	if !runtime.IsRunning() {
		if err := runtime.Start(context.cwd); err != nil {
			return nil, fmt.Errorf("failed to start claude: %w", err)
		}
	} else {
		runtime.SetCwd(context.cwd)
	}
	result, err := runtime.Prompt(PromptRequest{Text: text, Images: p.Images})
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
	p, err := decodeParams[claudePermissionRespondParams](req)
	if err != nil {
		return nil, err
	}
	if p.RequestID == "" {
		return nil, fmt.Errorf("requestId is required")
	}
	if err := s.runtime.ClaudeRuntime().ResolvePermission(p.RequestID, p.OptionID, p.Cancelled); err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("claude", nil), nil

}
