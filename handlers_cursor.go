package main

import (
	"fmt"
	"log"
)

func (s *Server) handleCursorStart(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("cursor")
	p, err := decodeParams[cursorStartParams](req)
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
			log.Printf("[cursor] start failed: %v", err)
			return s.runtime.StartResponse("cursor", RuntimeStartOptions{Available: available, Cwd: cwd, Error: err}), nil
		}
	} else if cwd != "" {
		runtime.SetCwd(cwd)
	}
	return s.runtime.StartResponse("cursor", RuntimeStartOptions{Available: available, Cwd: cwd}), nil
}

func (s *Server) handleCursorStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.StatusSnapshot("cursor"), nil
}

func (s *Server) handleCursorSessionList(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[cursorSessionListParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	result, err := s.cursor.ListSessions(context.cwd)
	if err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("cursor", result), nil
}

func (s *Server) handleCursorLoadSession(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("cursor")
	p, err := decodeParams[cursorLoadSessionParams](req)
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
	result, err := runtime.LoadSession(p.SessionID, context.cwd)
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("cursor")
	return s.runtime.SessionResponse("cursor", result), nil
}

func (s *Server) handleCursorNewSession(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("cursor")
	p, err := decodeParams[cursorNewSessionParams](req)
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
	s.persistRuntimeState("cursor")
	return s.runtime.SessionResponse("cursor", result), nil
}

func (s *Server) handleCursorSetConfig(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	model, mode := buildCursorConfigPatch(params)
	s.cursor.SetConfig(model, mode)
	return s.runtime.ConfigResponse("cursor"), nil
}

func (s *Server) handleCursorPrompt(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("cursor")
	p, err := decodeParams[cursorPromptParams](req)
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
	// model/mode use tri-state null-vs-absent semantics that JSON structs can't
	// express, so the config patch still reads the raw param map.
	model, mode := buildCursorConfigPatch(getParams(req.Params))
	s.cursor.SetConfig(model, mode)
	if !runtime.IsRunning() {
		if err := runtime.Start(context.cwd); err != nil {
			return nil, fmt.Errorf("failed to start cursor: %w", err)
		}
	} else {
		runtime.SetCwd(context.cwd)
	}
	result, err := runtime.Prompt(PromptRequest{Text: text, Images: p.Images})
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("cursor")
	s.persistAcceptedOperation("cursor", result.OperationID)
	return s.runtime.PromptAcceptedResponse("cursor", result), nil
}

func (s *Server) handleCursorCancel(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("cursor")
	status := runtime.TaskStatus()
	_ = runtime.Cancel(runtime.SessionID())
	s.persistRuntimeState("cursor")
	s.persistInterruptedOperation("cursor", status)
	return s.runtime.ActionResponse("cursor", nil), nil
}

func (s *Server) handleCursorStop(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("cursor")
	status := runtime.TaskStatus()
	runtime.Stop()
	s.persistRuntimeState("cursor")
	s.persistInterruptedOperation("cursor", status)
	return s.runtime.ActionResponse("cursor", nil), nil
}

func (s *Server) handleCursorTaskStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.TaskSnapshot("cursor", RuntimeSnapshotOptions{
		LatestSeq:     s.latestEventSeq(),
		Project:       s.currentProjectInfo(),
		LastOperation: s.latestOperationPayload("cursor"),
	}), nil
}

func (s *Server) handleCursorPermissionRespond(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[cursorPermissionRespondParams](req)
	if err != nil {
		return nil, err
	}
	if p.RequestID == "" {
		return nil, fmt.Errorf("requestId is required")
	}
	if err := s.runtime.CursorRuntime().ResolvePermission(p.RequestID, p.OptionID, p.Cancelled); err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("cursor", nil), nil
}

// buildCursorConfigPatch reads model/mode with tri-state semantics: an absent
// key leaves the value unchanged (nil), while an explicit null resets it to the
// daemon default ("default" model / "agent" mode).
func buildCursorConfigPatch(params map[string]interface{}) (*string, *string) {
	var model, mode *string
	if v, ok := getOptionalParamString(params, "model"); ok {
		if v == nil {
			d := "default"
			model = &d
		} else {
			model = v
		}
	}
	if v, ok := getOptionalParamString(params, "mode"); ok {
		if v == nil {
			d := "agent"
			mode = &d
		} else {
			mode = v
		}
	}
	return model, mode
}
