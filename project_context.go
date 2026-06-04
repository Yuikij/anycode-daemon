package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

type projectRequestContext struct {
	projectID  string
	generation uint64
	cwd        string
}

func (s *Server) currentProjectContext() projectRequestContext {
	projectID, generation := s.currentProjectState()
	return projectRequestContext{
		projectID:  projectID,
		generation: generation,
		cwd:        projectID,
	}
}

func (s *Server) validateRequestedProjectID(params map[string]interface{}) error {
	if params == nil {
		return nil
	}
	requestedProjectID := getParamString(params, "projectId")
	if requestedProjectID == "" {
		return nil
	}
	currentProjectID, _ := s.currentProjectState()
	if requestedProjectID != currentProjectID {
		return fmt.Errorf("stale project id: request bound to %q but current project is %q", requestedProjectID, currentProjectID)
	}
	return nil
}

func resolveProjectScopedCwd(projectRoot, requestedCwd string) (string, error) {
	absProjectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	if requestedCwd == "" {
		return absProjectRoot, nil
	}
	absRequestedCwd := requestedCwd
	if !filepath.IsAbs(absRequestedCwd) {
		absRequestedCwd = filepath.Join(absProjectRoot, absRequestedCwd)
	}
	absRequestedCwd, err = filepath.Abs(absRequestedCwd)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absProjectRoot, absRequestedCwd)
	if err != nil {
		return "", err
	}
	if rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
		return absRequestedCwd, nil
	}
	return "", fmt.Errorf("cwd %q is outside current project %q", absRequestedCwd, absProjectRoot)
}

func (s *Server) resolveProjectRequestContext(params map[string]interface{}, defaultToProjectRoot bool) (projectRequestContext, error) {
	context := s.currentProjectContext()
	if err := s.validateExpectedProjectGeneration(params); err != nil {
		return context, err
	}
	if err := s.validateRequestedProjectID(params); err != nil {
		return context, err
	}
	requestedCwd := getParamString(params, "cwd")
	if defaultToProjectRoot || requestedCwd != "" {
		resolvedCwd, err := resolveProjectScopedCwd(context.projectID, requestedCwd)
		if err != nil {
			return context, err
		}
		context.cwd = resolvedCwd
	}
	return context, nil
}

func (s *Server) normalizeProjectScopedParams(params map[string]interface{}, defaultToProjectRoot bool) (map[string]interface{}, projectRequestContext, error) {
	context, err := s.resolveProjectRequestContext(params, defaultToProjectRoot)
	if err != nil {
		return nil, context, err
	}
	if params == nil {
		if !defaultToProjectRoot {
			return nil, context, nil
		}
		return map[string]interface{}{"cwd": context.cwd}, context, nil
	}
	clone := make(map[string]interface{}, len(params)+1)
	for key, value := range params {
		clone[key] = value
	}
	delete(clone, "expectedProjectGeneration")
	delete(clone, "projectId")
	if defaultToProjectRoot || getParamString(params, "cwd") != "" {
		clone["cwd"] = context.cwd
	}
	return clone, context, nil
}
