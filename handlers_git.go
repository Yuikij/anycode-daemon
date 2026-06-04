package main

import "fmt"

func (s *Server) handleGitStatus(req RpcRequest, client *wsClient) (interface{}, error) {
	return getGitStatus(s.projectRoot)

}

func (s *Server) handleGitDiff(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	filePath := getParamString(params, "path")
	cwd := getParamString(params, "cwd")
	if cwd != "" {
		return getGitDiffHead(cwd)
	}
	return getGitDiff(s.projectRoot, filePath)

}

func (s *Server) handleGitDiffStaged(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	filePath := getParamString(params, "path")
	return getGitDiffStaged(s.projectRoot, filePath)

}

func (s *Server) handleGitLog(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	count := getParamInt(params, "count", 20)
	return getGitLog(s.projectRoot, count)

}

func (s *Server) handleGitDiffCommit(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	commit := getParamString(params, "commit")
	filePath := getParamString(params, "path")
	if commit == "" {
		return nil, fmt.Errorf("commit hash is required")
	}
	return getGitFileDiff(s.projectRoot, commit, filePath)

	// 闁冲厜鍋撻柍鍏夊亾 Codex agent integration 闁冲厜鍋撻柍鍏夊亾
}
