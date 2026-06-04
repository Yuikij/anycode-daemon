package main

import "fmt"

func (s *Server) handleGitStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	context, err := s.resolveProjectRequestContext(getParams(req.Params), false)
	if err != nil {
		return nil, err
	}
	return getGitStatus(context.projectID)

}

func (s *Server) handleGitDiff(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	context, err := s.resolveProjectRequestContext(params, false)
	if err != nil {
		return nil, err
	}
	filePath := getParamString(params, "path")
	cwd := getParamString(params, "cwd")
	if cwd != "" {
		resolvedCwd, err := resolveProjectScopedCwd(context.projectID, cwd)
		if err != nil {
			return nil, err
		}
		return getGitDiffHead(resolvedCwd)
	}
	return getGitDiff(context.projectID, filePath)

}

func (s *Server) handleGitDiffStaged(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	context, err := s.resolveProjectRequestContext(params, false)
	if err != nil {
		return nil, err
	}
	filePath := getParamString(params, "path")
	return getGitDiffStaged(context.projectID, filePath)

}

func (s *Server) handleGitLog(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	context, err := s.resolveProjectRequestContext(params, false)
	if err != nil {
		return nil, err
	}
	count := getParamInt(params, "count", 20)
	return getGitLog(context.projectID, count)

}

func (s *Server) handleGitDiffCommit(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	context, err := s.resolveProjectRequestContext(params, false)
	if err != nil {
		return nil, err
	}
	commit := getParamString(params, "commit")
	filePath := getParamString(params, "path")
	if commit == "" {
		return nil, fmt.Errorf("commit hash is required")
	}
	return getGitFileDiff(context.projectID, commit, filePath)

}
