package main

import (
	"fmt"
	"os"
)

func (s *Server) handleFsBrowse(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[fsBrowseParams](req)
	if err != nil {
		return nil, err
	}
	dirPath := p.Path
	if dirPath == "" {
		home, _ := os.UserHomeDir()
		dirPath = home
	}
	return browseDirectory(dirPath, p.ShowHidden)

}

func (s *Server) handleFsReadAbsolute(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[fsReadAbsoluteParams](req)
	if err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	return readAbsoluteFile(p.Path)

}

func (s *Server) handleFsWriteAbsolute(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[fsWriteAbsoluteParams](req)
	if err != nil {
		return nil, err
	}
	if err := s.checkProjectGeneration(p.ExpectedProjectGeneration); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if err := os.WriteFile(p.Path, []byte(p.Content), 0644); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

func (s *Server) handleFsTree(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[fsTreeParams](req)
	if err != nil {
		return nil, err
	}
	depth := 3
	if p.Depth != nil {
		depth = *p.Depth
	}
	fullPath, _, err := resolveProjectPath(s.projectRoot, p.Path)
	if err != nil {
		return nil, err
	}
	return getFileTree(fullPath, s.projectRoot, depth)

}
