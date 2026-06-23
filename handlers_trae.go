package main

import (
	"fmt"
	"log"
)

func (s *Server) handleTraeStart(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("trae")
	p, err := decodeParams[traeStartParams](req)
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
			log.Printf("[trae] start failed: %v", err)
			return s.runtime.StartResponse("trae", RuntimeStartOptions{Available: available, Cwd: cwd, Error: err}), nil
		}
	} else if cwd != "" {
		runtime.SetCwd(cwd)
	}
	return s.runtime.StartResponse("trae", RuntimeStartOptions{Available: available, Cwd: cwd}), nil
}

func (s *Server) handleTraeStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.StatusSnapshot("trae"), nil
}

func (s *Server) handleTraeSessionList(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[traeSessionListParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	result, err := s.trae.ListSessions(context.cwd)
	if err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("trae", result), nil
}

func (s *Server) handleTraeLoadSession(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("trae")
	p, err := decodeParams[traeLoadSessionParams](req)
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
	s.persistRuntimeState("trae")
	return s.runtime.SessionResponse("trae", result), nil
}

func (s *Server) handleTraeNewSession(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("trae")
	p, err := decodeParams[traeNewSessionParams](req)
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
	s.persistRuntimeState("trae")
	return s.runtime.SessionResponse("trae", result), nil
}

func (s *Server) handleTraeSetConfig(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	model, mode := buildTraeConfigPatch(params)
	s.trae.SetConfig(model, mode)
	return s.runtime.ConfigResponse("trae"), nil
}

func (s *Server) handleTraeModelList(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("trae")
	p, err := decodeParams[traeModelListParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	if !runtime.IsRunning() {
		if err := runtime.Start(context.cwd); err != nil {
			return nil, err
		}
	} else {
		runtime.SetCwd(context.cwd)
	}
	models, err := s.trae.ListModels()
	if err != nil {
		log.Printf("[trae] model/list failed: %v", err)
		return map[string]interface{}{"ok": true, "data": []AgentModelOption{}}, nil
	}
	return map[string]interface{}{"ok": true, "data": models}, nil
}

func (s *Server) handleTraeSetModel(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[traeSetModelParams](req)
	if err != nil {
		return nil, err
	}
	if p.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if err := s.trae.SetModel(p.Model); err != nil {
		return nil, err
	}
	return s.runtime.ConfigResponse("trae"), nil
}

func (s *Server) handleTraePrompt(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("trae")
	p, err := decodeParams[traePromptParams](req)
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
	// mode uses tri-state null-vs-absent semantics that JSON structs can't
	// express, so the config patch still reads the raw param map.
	model, mode := buildTraeConfigPatch(getParams(req.Params))
	s.trae.SetConfig(model, mode)
	if !runtime.IsRunning() {
		if err := runtime.Start(context.cwd); err != nil {
			return nil, fmt.Errorf("failed to start trae: %w", err)
		}
	} else {
		runtime.SetCwd(context.cwd)
	}
	result, err := runtime.Prompt(PromptRequest{Text: text, Images: p.Images})
	if err != nil {
		return nil, err
	}
	s.persistRuntimeState("trae")
	s.persistAcceptedOperation("trae", result.OperationID)
	return s.runtime.PromptAcceptedResponse("trae", result), nil
}

func (s *Server) handleTraeCancel(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("trae")
	status := runtime.TaskStatus()
	_ = runtime.Cancel(runtime.SessionID())
	s.persistRuntimeState("trae")
	s.persistInterruptedOperation("trae", status)
	return s.runtime.ActionResponse("trae", nil), nil
}

func (s *Server) handleTraeStop(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("trae")
	status := runtime.TaskStatus()
	runtime.Stop()
	s.persistRuntimeState("trae")
	s.persistInterruptedOperation("trae", status)
	return s.runtime.ActionResponse("trae", nil), nil
}

func (s *Server) handleTraeTaskStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.TaskSnapshot("trae", RuntimeSnapshotOptions{
		LatestSeq:     s.latestEventSeq(),
		Project:       s.currentProjectInfo(),
		LastOperation: s.latestOperationPayload("trae"),
	}), nil
}

func (s *Server) handleTraePermissionRespond(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[traePermissionRespondParams](req)
	if err != nil {
		return nil, err
	}
	if p.RequestID == "" {
		return nil, fmt.Errorf("requestId is required")
	}
	if err := s.runtime.TraeRuntime().ResolvePermission(p.RequestID, p.OptionID, p.Cancelled); err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("trae", nil), nil
}

// buildTraeConfigPatch reads mode with tri-state semantics: an absent key leaves
// the value unchanged (nil), while an explicit null resets it to the daemon
// default (empty, i.e. let the agent pick). Model changes must use trae.setModel.
func buildTraeConfigPatch(params map[string]interface{}) (*string, *string) {
	var model, mode *string
	if v, ok := getOptionalParamString(params, "mode"); ok {
		if v == nil {
			d := ""
			mode = &d
		} else {
			mode = v
		}
	}
	return model, mode
}
