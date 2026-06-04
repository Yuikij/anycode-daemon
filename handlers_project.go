package main

import (
	"fmt"
	"path/filepath"
)

func (s *Server) handleProjectInfo(req RpcRequest, client *wsClient) (interface{}, error) {
	return s.currentProjectInfo(), nil

}

func (s *Server) handleProjectOpen(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	newRoot := getParamString(params, "path")
	if newRoot == "" {
		return nil, fmt.Errorf("path is required")
	}
	abs, _ := filepath.Abs(newRoot)
	s.switchProject(abs)
	return s.currentProjectInfo(), nil

}

func (s *Server) handleProjectList(req RpcRequest, client *wsClient) (interface{}, error) {
	result := listProjectDirs()
	if s.eventJournal != nil {
		persisted, err := s.eventJournal.listProjects()
		if err != nil {
			return nil, err
		}
		scanned, _ := result["projects"].([]map[string]string)
		if scanned == nil {
			scanned = make([]map[string]string, 0)
		}
		result["projects"] = mergeProjectListings(scanned, persisted)
	}
	currentRoot, generation := s.currentProjectState()
	result["current"] = currentRoot
	result["projectId"] = currentRoot
	result["generation"] = generation
	return result, nil

}
