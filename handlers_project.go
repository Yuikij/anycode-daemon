package main

import (
	"fmt"
	"path/filepath"
)

func (s *Server) handleProjectInfo(req RpcRequest, client *wsClient) (interface{}, error) {
	return getProjectInfo(s.projectRoot), nil

}

func (s *Server) handleProjectOpen(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	newRoot := getParamString(params, "path")
	if newRoot == "" {
		return nil, fmt.Errorf("path is required")
	}
	abs, _ := filepath.Abs(newRoot)
	s.switchProject(abs)
	return getProjectInfo(s.projectRoot), nil

}

func (s *Server) handleProjectList(req RpcRequest, client *wsClient) (interface{}, error) {
	result := listProjectDirs()
	result["current"] = s.projectRoot
	return result, nil

}
