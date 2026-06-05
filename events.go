package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

var operationCounter uint64

const maxJournaledEvents = 1000

type eventEnvelope struct {
	Seq               uint64      `json:"seq"`
	Agent             string      `json:"agent"`
	Method            string      `json:"method"`
	Params            interface{} `json:"params,omitempty"`
	ProjectID         string      `json:"projectId"`
	ProjectGeneration uint64      `json:"projectGeneration"`
	OperationID       string      `json:"operationId,omitempty"`
	Timestamp         int64       `json:"ts"`
}

func (s *Server) currentProjectState() (string, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.projectRoot, s.projectGeneration
}

func (s *Server) currentProjectInfo() *ProjectInfo {
	root, generation := s.currentProjectState()
	info := getProjectInfo(root)
	info.ProjectID = root
	info.Generation = generation
	return info
}

func (s *Server) latestEventSeq() uint64 {
	if s.eventJournal == nil {
		return 0
	}
	return s.eventJournal.latestSeq()
}

func (s *Server) recordEvent(agent, method string, params interface{}) eventEnvelope {
	projectID, generation := s.currentProjectState()
	event := eventEnvelope{
		Agent:             agent,
		Method:            method,
		Params:            params,
		ProjectID:         projectID,
		ProjectGeneration: generation,
		OperationID:       extractOperationID(params),
		Timestamp:         nowUnixMilli(),
	}

	if s.eventJournal == nil {
		return event
	}
	stored, err := s.eventJournal.append(event)
	if err != nil {
		panic(fmt.Sprintf("append event: %v", err))
	}

	return stored
}

func (s *Server) broadcastRecordedEvent(agent, method string, params interface{}) {
	event := s.recordEvent(agent, method, params)
	s.broadcast(makeNotification(method, attachEventMeta(params, event)))
}

func (s *Server) replayEvents(afterSeq uint64, projectID string) ([]eventEnvelope, uint64) {
	if s.eventJournal == nil {
		return nil, 0
	}
	return s.eventJournal.replay(afterSeq, projectID)
}

func (s *Server) resumeResult(afterSeq uint64, projectID string, allowReplay bool) map[string]interface{} {
	latestSeq := s.latestEventSeq()
	result := map[string]interface{}{
		"ok":            true,
		"events":         []eventEnvelope{},
		"latestSeq":      latestSeq,
		"project":        s.currentProjectInfo(),
		"cursorExpired":  false,
	}

	if !allowReplay {
		return result
	}

	cursorExpired := s.isResumeCursorExpired(afterSeq, projectID)
	events, replayLatestSeq := s.replayEvents(afterSeq, projectID)
	result["latestSeq"] = replayLatestSeq
	result["cursorExpired"] = cursorExpired
	result["events"] = events
	if cursorExpired {
		result["events"] = []eventEnvelope{}
		result["snapshot"] = s.resumeSnapshot(replayLatestSeq)
	}
	return result
}

func (s *Server) helloAgentStatus(includeProjectGeneration bool) map[string]interface{} {
	options := RuntimeSnapshotOptions{
		LatestSeq: s.latestEventSeq(),
		Project:   s.currentProjectInfo(),
	}
	statuses := map[string]interface{}{
		"codex": s.runtime.TaskSnapshot("codex", RuntimeSnapshotOptions{
			LatestSeq:     options.LatestSeq,
			Project:       options.Project,
			LastOperation: s.latestOperationPayload("codex"),
		}),
		"claude": s.runtime.TaskSnapshot("claude", RuntimeSnapshotOptions{
			LatestSeq:      options.LatestSeq,
			Project:        options.Project,
			LastOperation:  s.latestOperationPayload("claude"),
			LastPermission: s.latestPermissionPayload("claude"),
		}),
		"gemini": s.runtime.TaskSnapshot("gemini", RuntimeSnapshotOptions{
			LatestSeq:     options.LatestSeq,
			Project:       options.Project,
			LastOperation: s.latestOperationPayload("gemini"),
		}),
	}
	shaped := shapeSnapshotPayload(map[string]interface{}{"agents": statuses}, includeProjectGeneration)
	if agents, ok := shaped["agents"].(map[string]interface{}); ok {
		return agents
	}
	return statuses
}

func (s *Server) negotiatedHelloCapabilities(requested []string) []string {
	supported := []string{"client.hello", "project.generation"}
	if len(requested) == 0 {
		return append([]string(nil), supported...)
	}
	negotiated := make([]string, 0, len(supported))
	for _, capability := range supported {
		if contains(requested, capability) {
			negotiated = append(negotiated, capability)
		}
	}
	return negotiated
}

func projectInfoPayload(project *ProjectInfo, includeGeneration bool) map[string]interface{} {
	if project == nil {
		return nil
	}
	payload := map[string]interface{}{
		"name":      project.Name,
		"root":      project.Root,
		"isGit":     project.IsGit,
		"fileCount": project.FileCount,
	}
	if project.ProjectID != "" {
		payload["projectId"] = project.ProjectID
	}
	if includeGeneration && project.Generation > 0 {
		payload["generation"] = project.Generation
	}
	return payload
}

func eventEnvelopePayload(event eventEnvelope, includeGeneration bool) map[string]interface{} {
	payload := map[string]interface{}{
		"seq":       event.Seq,
		"agent":     event.Agent,
		"method":    event.Method,
		"projectId": event.ProjectID,
		"ts":        event.Timestamp,
	}
	if event.Params != nil {
		payload["params"] = event.Params
	}
	if event.OperationID != "" {
		payload["operationId"] = event.OperationID
	}
	if includeGeneration && event.ProjectGeneration > 0 {
		payload["projectGeneration"] = event.ProjectGeneration
	}
	return payload
}

func shapeProjectValue(value interface{}, includeGeneration bool) interface{} {
	switch project := value.(type) {
	case *ProjectInfo:
		return projectInfoPayload(project, includeGeneration)
	case ProjectInfo:
		projectCopy := project
		return projectInfoPayload(&projectCopy, includeGeneration)
	case map[string]interface{}:
		clone := cloneResponseMap(project)
		if !includeGeneration {
			delete(clone, "generation")
		}
		return clone
	default:
		return value
	}
}

func shapeSnapshotPayload(snapshot map[string]interface{}, includeGeneration bool) map[string]interface{} {
	if snapshot == nil {
		return nil
	}
	clone := cloneResponseMap(snapshot)
	if project, ok := clone["project"]; ok {
		clone["project"] = shapeProjectValue(project, includeGeneration)
	}
	agents, ok := clone["agents"].(map[string]interface{})
	if !ok {
		return clone
	}
	agentClones := make(map[string]interface{}, len(agents))
	for agent, raw := range agents {
		if payload, ok := raw.(map[string]interface{}); ok {
			agentClone := cloneResponseMap(payload)
			if project, ok := agentClone["project"]; ok {
				agentClone["project"] = shapeProjectValue(project, includeGeneration)
			}
			agentClones[agent] = agentClone
			continue
		}
		agentClones[agent] = raw
	}
	clone["agents"] = agentClones
	return clone
}

func shapeResumePayload(result map[string]interface{}, includeGeneration bool) map[string]interface{} {
	clone := cloneResponseMap(result)
	if project, ok := clone["project"]; ok {
		clone["project"] = shapeProjectValue(project, includeGeneration)
	}
	if events, ok := clone["events"].([]eventEnvelope); ok {
		payloads := make([]map[string]interface{}, 0, len(events))
		for _, event := range events {
			payloads = append(payloads, eventEnvelopePayload(event, includeGeneration))
		}
		clone["events"] = payloads
	}
	if snapshot, ok := clone["snapshot"].(map[string]interface{}); ok {
		clone["snapshot"] = shapeSnapshotPayload(snapshot, includeGeneration)
	}
	return clone
}

func (s *Server) handleClientHello(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	afterSeq := uint64(getParamInt(params, "lastSeq", 0))
	projectID := getParamString(params, "projectId")
	requestedCapabilities := getParamStringSlice(params, "capabilities")
	negotiatedCapabilities := s.negotiatedHelloCapabilities(requestedCapabilities)
	includeProjectGeneration := contains(negotiatedCapabilities, "project.generation")
	allowReplay := afterSeq > 0
	resume := shapeResumePayload(s.resumeResult(afterSeq, projectID, allowReplay), includeProjectGeneration)

	return map[string]interface{}{
		"protocolVersion": 1,
		"daemonVersion":   Version,
		"role":            "client",
		"capabilities":    negotiatedCapabilities,
		"project":         projectInfoPayload(s.currentProjectInfo(), includeProjectGeneration),
		"agents":          s.helloAgentStatus(includeProjectGeneration),
		"latestSeq":       s.latestEventSeq(),
		"resume":          resume,
	}, nil
}

func (s *Server) isResumeCursorExpired(afterSeq uint64, projectID string) bool {
	if s.eventJournal == nil || afterSeq == 0 {
		return false
	}
	earliestSeq := s.eventJournal.earliestSeq(projectID)
	if earliestSeq <= 1 {
		return false
	}
	return afterSeq < earliestSeq-1
}

func (s *Server) agentResumeSnapshot(agent string, latestSeq uint64, project *ProjectInfo) map[string]interface{} {
	options := RuntimeSnapshotOptions{
		LatestSeq: latestSeq,
		Project:   project,
	}

	switch agent {
	case "claude":
		options.LastOperation = s.latestOperationPayload("claude")
		options.LastPermission = s.latestPermissionPayload("claude")
	case "codex":
		options.LastOperation = s.latestOperationPayload("codex")
	case "gemini":
		options.LastOperation = s.latestOperationPayload("gemini")
	}

	return s.runtime.TaskSnapshot(agent, options)
}

func (s *Server) resumeSnapshot(latestSeq uint64) map[string]interface{} {
	project := s.currentProjectInfo()
	return map[string]interface{}{
		"latestSeq": latestSeq,
		"project":   project,
		"agents": map[string]interface{}{
			"codex":  s.agentResumeSnapshot("codex", latestSeq, project),
			"claude": s.agentResumeSnapshot("claude", latestSeq, project),
			"gemini": s.agentResumeSnapshot("gemini", latestSeq, project),
		},
	}
}

func (s *Server) handleEventsResume(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	afterSeq := uint64(getParamInt(params, "afterSeq", 0))
	projectID := getParamString(params, "projectId")
	return s.resumeResult(afterSeq, projectID, true), nil
}

func attachEventMeta(params interface{}, event eventEnvelope) interface{} {
	payload, ok := params.(map[string]interface{})
	if !ok {
		return params
	}

	clone := make(map[string]interface{}, len(payload)+1)
	for key, value := range payload {
		clone[key] = value
	}
	clone["_anycode"] = map[string]interface{}{
		"seq":               event.Seq,
		"agent":             event.Agent,
		"projectId":         event.ProjectID,
		"projectGeneration": event.ProjectGeneration,
		"operationId":       event.OperationID,
		"ts":                event.Timestamp,
	}
	return clone
}

func extractOperationID(params interface{}) string {
	payload, ok := params.(map[string]interface{})
	if !ok {
		return ""
	}

	keys := []string{"operationId", "requestId", "sessionId", "threadId", "turnId"}
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}

	if turn, ok := payload["turn"].(map[string]interface{}); ok {
		if value, ok := turn["id"].(string); ok && value != "" {
			return value
		}
	}

	if thread, ok := payload["thread"].(map[string]interface{}); ok {
		if value, ok := thread["id"].(string); ok && value != "" {
			return value
		}
	}

	return ""
}

func nowUnixMilli() int64 {
	return time.Now().UnixMilli()
}

func newOperationID(agent string) string {
	return fmt.Sprintf("%s-op-%d-%d", agent, time.Now().UnixMilli(), atomic.AddUint64(&operationCounter, 1))
}

func attachOperationID(params interface{}, operationID string) interface{} {
	if operationID == "" {
		return params
	}
	payload, ok := params.(map[string]interface{})
	if !ok {
		return params
	}
	if existing, ok := payload["operationId"].(string); ok && existing != "" {
		return params
	}
	clone := make(map[string]interface{}, len(payload)+1)
	for key, value := range payload {
		clone[key] = value
	}
	clone["operationId"] = operationID
	return clone
}

func (s *Server) validateExpectedProjectGeneration(params map[string]interface{}) error {
	expected := getParamInt(params, "expectedProjectGeneration", 0)
	if expected <= 0 {
		return nil
	}

	_, generation := s.currentProjectState()
	if uint64(expected) != generation {
		return fmt.Errorf("project changed: expected generation %d, current %d", expected, generation)
	}
	return nil
}
