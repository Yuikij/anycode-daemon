package main

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

func (s *Server) handleGitLog(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	context, err := s.resolveProjectRequestContext(params, false)
	if err != nil {
		return nil, err
	}
	count := getParamInt(params, "count", 20)
	return getGitLog(context.projectID, count)

}
