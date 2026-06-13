package main

import (
	"fmt"
	"path/filepath"
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

// projectScope is the typed view of the common project-scope request fields
// (projectId / expectedProjectGeneration / cwd). Handlers embed it in their
// request structs and resolve it via resolveScope, replacing ad-hoc map digging.
type projectScope struct {
	ProjectID                 string `json:"projectId"`
	ExpectedProjectGeneration int    `json:"expectedProjectGeneration"`
	Cwd                       string `json:"cwd"`
}

func projectScopeFromParams(params map[string]interface{}) projectScope {
	return projectScope{
		ProjectID:                 getParamString(params, "projectId"),
		ExpectedProjectGeneration: getParamInt(params, "expectedProjectGeneration", 0),
		Cwd:                       getParamString(params, "cwd"),
	}
}

// resolveScope validates the expected generation and project id, then resolves
// the effective cwd. It is the single implementation behind both the typed
// handler path and the legacy map-based resolveProjectRequestContext.
func (s *Server) resolveScope(scope projectScope, defaultToProjectRoot bool) (projectRequestContext, error) {
	context := s.currentProjectContext()
	if err := s.checkProjectGeneration(scope.ExpectedProjectGeneration); err != nil {
		return context, err
	}
	if scope.ProjectID != "" {
		currentProjectID, _ := s.currentProjectState()
		if scope.ProjectID != currentProjectID {
			return context, fmt.Errorf("stale project id: request bound to %q but current project is %q", scope.ProjectID, currentProjectID)
		}
	}
	if defaultToProjectRoot || scope.Cwd != "" {
		resolvedCwd, err := resolveProjectScopedCwd(context.projectID, scope.Cwd)
		if err != nil {
			return context, err
		}
		context.cwd = resolvedCwd
	}
	return context, nil
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

	// Disable strict bounds checking to allow operating outside project root.
	return absRequestedCwd, nil
}

func (s *Server) resolveProjectRequestContext(params map[string]interface{}, defaultToProjectRoot bool) (projectRequestContext, error) {
	return s.resolveScope(projectScopeFromParams(params), defaultToProjectRoot)
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
