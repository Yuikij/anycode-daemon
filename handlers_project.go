package main

import (
	"fmt"
	"path/filepath"
)

func (s *Server) handleProjectOpen(req RpcRequest, client *wsClient) (interface{}, error) {
	p, err := decodeParams[projectOpenParams](req)
	if err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	abs, _ := filepath.Abs(p.Path)
	s.switchProject(abs)
	return s.currentProjectInfo(), nil

}

