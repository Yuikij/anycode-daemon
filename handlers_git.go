package main

func (s *Server) handleGitStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[gitStatusParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, false)
	if err != nil {
		return nil, err
	}
	return getGitStatus(context.projectID)

}

func (s *Server) handleGitDiff(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[gitDiffParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, false)
	if err != nil {
		return nil, err
	}
	if p.Cwd != "" {
		resolvedCwd, err := resolveProjectScopedCwd(context.projectID, p.Cwd)
		if err != nil {
			return nil, err
		}
		return getGitDiffHead(resolvedCwd)
	}
	return getGitDiff(context.projectID, p.Path)

}

func (s *Server) handleGitLog(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[gitLogParams](req)
	if err != nil {
		return nil, err
	}
	context, err := s.resolveScope(p.projectScope, false)
	if err != nil {
		return nil, err
	}
	count := 20
	if p.Count != nil {
		count = *p.Count
	}
	return getGitLog(context.projectID, count)

}
