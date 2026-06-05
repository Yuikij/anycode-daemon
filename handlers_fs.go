package main

import (
	"fmt"
	"os"
)

func (s *Server) handleFsBrowse(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	dirPath := getParamString(params, "path")
	if dirPath == "" {
		home, _ := os.UserHomeDir()
		dirPath = home
	}
	showHidden := getParamBool(params, "showHidden")
	return browseDirectory(dirPath, showHidden)

}

func (s *Server) handleFsReadAbsolute(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	filePath := getParamString(params, "path")
	if filePath == "" {
		return nil, fmt.Errorf("path is required")
	}
	return readAbsoluteFile(filePath)

}

func (s *Server) handleFsWriteAbsolute(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	if err := s.validateExpectedProjectGeneration(params); err != nil {
		return nil, err
	}
	filePath := getParamString(params, "path")
	content := getParamString(params, "content")
	if filePath == "" {
		return nil, fmt.Errorf("path is required")
	}
	err := os.WriteFile(filePath, []byte(content), 0644)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleFsTree(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	dirPath := getParamString(params, "path")
	depth := getParamInt(params, "depth", 3)
	fullPath, _, err := resolveProjectPath(s.projectRoot, dirPath)
	if err != nil {
		return nil, err
	}
	return getFileTree(fullPath, s.projectRoot, depth)

}
