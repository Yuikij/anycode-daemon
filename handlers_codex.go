package main

import (
	"fmt"
	"log"
)

func (s *Server) handleCodexStart(req RpcRequest, client *wsClient) (interface{}, error) {
	runtime := s.runtime.MustRuntime("codex")
	p, err := decodeParams[codexStartParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, true)
	if err != nil {
		return nil, err
	}
	cwd := context.cwd
	command := codexCommand()
	log.Printf("[codex.start] command=%s cwd=%s, already_running=%v", command, cwd, runtime.IsRunning())
	if err := runtime.Start(cwd); err != nil {
		log.Printf("[codex.start] error: %v", err)
		return nil, err
	}
	log.Printf("[codex.start] success")
	return s.runtime.StartResponse("codex", RuntimeStartOptions{Cwd: cwd}), nil

}

func (s *Server) handleCodexStop(req RpcRequest, client *wsClient) (interface{}, error) {
	s.runtime.MustRuntime("codex").Stop()
	return s.runtime.ActionResponse("codex", nil), nil

}

func (s *Server) handleCodexStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.StatusSnapshot("codex"), nil
}

// handleCodexTaskStatus returns the in-progress turn replay buffer so the UI
// can rebuild streaming state after reconnecting.
func (s *Server) handleCodexTaskStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.runtime.TaskSnapshot("codex", RuntimeSnapshotOptions{
		LatestSeq:     s.latestEventSeq(),
		Project:       s.currentProjectInfo(),
		LastOperation: s.latestOperationPayload("codex"),
	}), nil

}

func (s *Server) handleCodexConfigWrite(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	if params == nil {
		return nil, fmt.Errorf("params required")
	}
	result, err := s.runtime.CodexRuntime().ConfigWrite(params)
	if err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("codex", result), nil

}

func (s *Server) handleCodexRespond(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[codexRespondParams](req)
	if err != nil {
		return nil, err
	}
	if p.RequestID == nil {
		return nil, fmt.Errorf("requestId is required")
	}
	if err := s.runtime.CodexRuntime().Respond(p.RequestID, p.Result); err != nil {
		return nil, err
	}
	return s.runtime.ActionResponse("codex", nil), nil

}

func (s *Server) handleCodexRevertFileChanges(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	if err := s.validateExpectedProjectGeneration(params); err != nil {
		return nil, err
	}
	return handleFileChanges(params, s.projectRoot, true)

}

func (s *Server) handleCodexApplyFileChanges(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	if err := s.validateExpectedProjectGeneration(params); err != nil {
		return nil, err
	}
	return handleFileChanges(params, s.projectRoot, false)

}
