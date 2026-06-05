package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, projectRoot string) *Server {
	t.Helper()
	t.Setenv("ANYCODE_STATE_DB_PATH", filepath.Join(t.TempDir(), "state.db"))
	server, err := NewServer(0, projectRoot, "secret-token")
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Fatalf("Server.Close failed: %v", err)
		}
	})
	return server
}

func openProjectForTest(t *testing.T, server *Server, root string) *ProjectInfo {
	t.Helper()
	encodedParams, err := json.Marshal(map[string]string{"path": root})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(encodedParams)
	result, err := server.handleProjectOpen(RpcRequest{Method: "project.open", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := result.(*ProjectInfo)
	if !ok {
		t.Fatalf("expected *ProjectInfo, got %T", result)
	}
	return info
}

func initGitRepoForTest(t *testing.T, root string) {
	t.Helper()
	cmd := exec.Command("git", "init")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s failed: %v\n%s", root, err, string(output))
	}
}

func TestRewriteTextResponseRewritesInlineCSSRootRefsInHTML(t *testing.T) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:3001", Path: "/"}
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body: io.NopCloser(strings.NewReader(
			`<html><head><style>@font-face{src:url(/_next/static/media/font.woff2)}</style></head></html>`,
		)),
	}

	if err := rewriteTextResponse(resp, target, "anycodeapp.com", "https"); err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	got := string(body)
	want := `url(/__anycode_proxy/http/127.0.0.1:3001/_next/static/media/font.woff2)`
	if !strings.Contains(got, want) {
		t.Fatalf("expected inline CSS URL to be proxied\nwant contains: %s\ngot: %s", want, got)
	}
}

func TestRelayProxyOpenDoesNotExposeDaemonTokenCookie(t *testing.T) {
	server := newTestServer(t, ".")
	result, err := server.handleRelayProxyFetch(map[string]interface{}{
		"method": "GET",
		"path":   "/__anycode_proxy/open?url=http%3A%2F%2F127.0.0.1%3A3001%2F",
	})
	if err != nil {
		t.Fatal(err)
	}

	headers := result.(map[string]interface{})["headers"].(map[string][]string)
	for _, value := range headers["Set-Cookie"] {
		if strings.HasPrefix(value, proxyTokenCookie+"=") {
			t.Fatalf("relay response exposed daemon token cookie: %s", value)
		}
	}
}

func TestStripProxyCookiesRemovesAnyCodePlatformCookies(t *testing.T) {
	req, err := http.NewRequest("GET", "http://127.0.0.1:3001/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", strings.Join([]string{
		proxyTokenCookie + "=secret",
		proxyOriginCookie + "=origin",
		sessionCookieName + "=session",
		relayDeviceCookie + "=device",
		relayAuthCookie + "=auth",
		"target_cookie=kept",
	}, "; "))

	stripProxyCookies(req)

	cookie := req.Header.Get("Cookie")
	if strings.Contains(cookie, proxyTokenCookie+"=") ||
		strings.Contains(cookie, proxyOriginCookie+"=") ||
		strings.Contains(cookie, sessionCookieName+"=") ||
		strings.Contains(cookie, relayDeviceCookie+"=") ||
		strings.Contains(cookie, relayAuthCookie+"=") {
		t.Fatalf("reserved cookie leaked to target: %s", cookie)
	}
	if !strings.Contains(cookie, "target_cookie=kept") {
		t.Fatalf("target cookie was not preserved: %s", cookie)
	}
}

func TestResolveProjectPathRejectsTraversalAndSiblingPrefix(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	sibling := filepath.Join(base, "repo2")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := resolveProjectPath(root, ".."); err == nil {
		t.Fatal("expected parent traversal to be rejected")
	}
	if _, _, err := resolveProjectPath(root, filepath.Join(sibling, "file.txt")); err == nil {
		t.Fatal("expected same-prefix sibling path to be rejected")
	}

	inside, rel, err := resolveProjectPath(root, filepath.Join("sub", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if rel != filepath.Join("sub", "file.txt") {
		t.Fatalf("unexpected relative path: %q", rel)
	}
	if !strings.HasPrefix(inside, root) {
		t.Fatalf("expected resolved path under root, got %q", inside)
	}
}

func TestHandleFileChangesRejectsPathsOutsideProjectRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside.txt")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}

	result, err := handleFileChanges(map[string]interface{}{
		"changes": []interface{}{
			map[string]interface{}{
				"path": outside,
				"kind": map[string]interface{}{"type": "add"},
				"diff": "+should not write",
			},
		},
	}, root, false)
	if err != nil {
		t.Fatal(err)
	}
	res := result.(map[string]interface{})
	if res["ok"].(bool) {
		t.Fatal("expected unsafe file change to be rejected")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside file should not be created, stat err=%v", err)
	}
}

func TestBuildClaudeConfigPatchUsesCanonicalDefaults(t *testing.T) {
	patch := buildClaudeConfigPatch(map[string]interface{}{
		"model":          "",
		"effort":         "",
		"permissionMode": nil,
	})
	bridge := NewClaudeBridge()
	bridge.SetConfig(patch)
	config := bridge.ConfigSnapshot()
	if config["model"] != "default" {
		t.Fatalf("expected default model, got %#v", config["model"])
	}
	if config["effort"] != "medium" {
		t.Fatalf("expected medium effort, got %#v", config["effort"])
	}
	if config["permissionMode"] != "default" {
		t.Fatalf("expected default permission mode, got %#v", config["permissionMode"])
	}
}

func TestClaudeTaskStatusSortsPendingPermissions(t *testing.T) {
	bridge := NewClaudeBridge()
	runtime := NewClaudeRuntime(bridge)
	runtime.permissionStore.pending["later"] = &pendingPermission{requestId: "later", toolName: "later", createdAt: time.Unix(20, 0)}
	runtime.permissionStore.pending["earlier"] = &pendingPermission{requestId: "earlier", toolName: "earlier", createdAt: time.Unix(10, 0)}
	status := bridge.TaskStatus()
	perms := status["pendingPerms"].([]map[string]interface{})
	if len(perms) != 2 {
		t.Fatalf("expected 2 pending perms, got %d", len(perms))
	}
	if perms[0]["requestId"] != "earlier" || perms[1]["requestId"] != "later" {
		t.Fatalf("unexpected order: %#v", perms)
	}
}

func TestRuntimeManagerStatusSnapshotForClaudeIncludesConfigAndSession(t *testing.T) {
	bridge := NewClaudeBridge()
	bridge.SetConfig(ClaudeConfigPatch{})
	bridge.RestoreSession("claude-session-1")
	manager := NewAgentRuntimeManager(NewClaudeRuntime(bridge))

	status := manager.StatusSnapshot("claude")
	if status["sessionId"] != "claude-session-1" {
		t.Fatalf("expected sessionId in status snapshot, got %#v", status)
	}
	if status["config"] == nil || status["capabilities"] == nil {
		t.Fatalf("expected config and capabilities in status snapshot, got %#v", status)
	}
	if status["permissionMode"] != "default" {
		t.Fatalf("expected default permissionMode, got %#v", status["permissionMode"])
	}
}

func TestRuntimeManagerTaskSnapshotAddsCommonFields(t *testing.T) {
	manager := NewAgentRuntimeManager(NewCodexRuntime(NewAgentBridge()))
	project := &ProjectInfo{ProjectID: "project-a", Generation: 2}
	lastOperation := map[string]interface{}{"operationId": "codex-op-1", "status": "interrupted"}

	status := manager.TaskSnapshot("codex", RuntimeSnapshotOptions{
		LatestSeq:     42,
		Project:       project,
		LastOperation: lastOperation,
	})

	if status["latestSeq"] != uint64(42) {
		t.Fatalf("expected latestSeq 42, got %#v", status["latestSeq"])
	}
	if status["project"] != project {
		t.Fatalf("expected project pointer to be preserved, got %#v", status["project"])
	}
	operation, ok := status["lastOperation"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected lastOperation map payload, got %#v", status["lastOperation"])
	}
	if operation["operationId"] != lastOperation["operationId"] || operation["status"] != lastOperation["status"] {
		t.Fatalf("expected lastOperation payload to be preserved, got %#v", status["lastOperation"])
	}
}

func TestRuntimeManagerStartResponseForClaudeIncludesConfigAndCwd(t *testing.T) {
	bridge := NewClaudeBridge()
	bridge.RestoreSession("claude-session-2")
	manager := NewAgentRuntimeManager(NewClaudeRuntime(bridge))

	response := manager.StartResponse("claude", RuntimeStartOptions{Available: true, Cwd: "/tmp/work"})
	if response["ok"] != true {
		t.Fatalf("expected ok=true, got %#v", response)
	}
	if response["cwd"] != "/tmp/work" || response["available"] != true {
		t.Fatalf("expected cwd and available in start response, got %#v", response)
	}
	if response["sessionId"] != "claude-session-2" || response["config"] == nil {
		t.Fatalf("expected Claude config/session fields in start response, got %#v", response)
	}
}

func TestRuntimeManagerPromptAcceptedResponseForGeminiPreservesPayload(t *testing.T) {
	manager := NewAgentRuntimeManager(NewGeminiRuntime(NewGeminiBridge()))
	response := manager.PromptAcceptedResponse("gemini", PromptResponse{
		OperationID: "gemini-op-1",
		Payload: map[string]interface{}{
			"sessionId":   "gemini-session-1",
			"operationId": "gemini-op-1",
			"accepted":    true,
		},
	})

	if response["ok"] != true {
		t.Fatalf("expected ok=true, got %#v", response)
	}
	if response["sessionId"] != "gemini-session-1" || response["accepted"] != true {
		t.Fatalf("expected Gemini payload to be preserved, got %#v", response)
	}
	if response["operationId"] != "gemini-op-1" {
		t.Fatalf("expected operationId to be preserved, got %#v", response)
	}
}

func TestRuntimeManagerActionResponsePreservesExplicitFailure(t *testing.T) {
	manager := NewAgentRuntimeManager(NewGeminiRuntime(NewGeminiBridge()))
	response := manager.ActionResponse("gemini", map[string]interface{}{
		"ok":    false,
		"error": "session list failed",
	})
	if response["ok"] != false {
		t.Fatalf("expected explicit ok=false to be preserved, got %#v", response)
	}
	if response["error"] != "session list failed" {
		t.Fatalf("expected error payload to be preserved, got %#v", response)
	}
}

func TestNormalizeActionPayload(t *testing.T) {
	mapPayload := normalizeActionPayload(map[string]interface{}{"applied": true})
	if mapPayload["applied"] != true {
		t.Fatalf("expected map payload to be preserved, got %#v", mapPayload)
	}

	scalarPayload := normalizeActionPayload("updated")
	if scalarPayload["result"] != "updated" {
		t.Fatalf("expected scalar payload to be wrapped, got %#v", scalarPayload)
	}

	if normalizeActionPayload(nil) != nil {
		t.Fatal("expected nil payload to remain nil")
	}
}

func TestEventsResumeReturnsSequencedEvents(t *testing.T) {
	server := newTestServer(t, ".")
	server.broadcastRecordedEvent("gemini", "gemini.message.delta", map[string]interface{}{"sessionId": "sess-1"})
	server.broadcastRecordedEvent("codex", "codex.turn/completed", map[string]interface{}{"threadId": "thread-1"})

	params := json.RawMessage([]byte(`{"afterSeq":0}`))
	result, err := server.handleEventsResume(RpcRequest{Method: "events.resume", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := result.(map[string]interface{})
	events := payload["events"].([]eventEnvelope)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("unexpected event seqs: %#v", events)
	}
	if events[0].Method != "gemini.message.delta" || events[1].Method != "codex.turn/completed" {
		t.Fatalf("unexpected methods: %#v", events)
	}
	if payload["latestSeq"].(uint64) != 2 {
		t.Fatalf("expected latestSeq 2, got %#v", payload["latestSeq"])
	}
	if payload["cursorExpired"].(bool) {
		t.Fatalf("expected cursorExpired false, got %#v", payload["cursorExpired"])
	}
	if _, ok := payload["snapshot"]; ok {
		t.Fatalf("expected no snapshot when cursor is current, got %#v", payload["snapshot"])
	}
}

func TestEventsResumeReturnsSnapshotWhenCursorExpired(t *testing.T) {
	server := newTestServer(t, ".")
	server.eventJournal.maxEntries = 2
	server.broadcastRecordedEvent("gemini", "gemini.message.delta", map[string]interface{}{"sessionId": "sess-1"})
	server.broadcastRecordedEvent("codex", "codex.turn/completed", map[string]interface{}{"threadId": "thread-1"})
	server.broadcastRecordedEvent("claude", "claude.turn/completed", map[string]interface{}{"sessionId": "sess-2"})
	server.broadcastRecordedEvent("daemon", "project.changed", map[string]interface{}{"projectId": ".", "generation": 1})

	params := json.RawMessage([]byte(`{"afterSeq":1}`))
	result, err := server.handleEventsResume(RpcRequest{Method: "events.resume", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := result.(map[string]interface{})
	if !payload["cursorExpired"].(bool) {
		t.Fatalf("expected cursorExpired true, got %#v", payload["cursorExpired"])
	}
	events := payload["events"].([]eventEnvelope)
	if len(events) != 0 {
		t.Fatalf("expected expired resume to omit partial events, got %#v", events)
	}
	snapshot, ok := payload["snapshot"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected snapshot payload, got %#v", payload["snapshot"])
	}
	if snapshot["latestSeq"].(uint64) != 4 {
		t.Fatalf("expected snapshot latestSeq 4, got %#v", snapshot["latestSeq"])
	}
	project, ok := snapshot["project"].(*ProjectInfo)
	if !ok {
		t.Fatalf("expected snapshot project info, got %#v", snapshot["project"])
	}
	if project.ProjectID == "" {
		t.Fatalf("expected snapshot project id, got %#v", project)
	}
	agents, ok := snapshot["agents"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected agent snapshots, got %#v", snapshot["agents"])
	}
	for _, agent := range []string{"codex", "claude", "gemini"} {
		if _, ok := agents[agent].(map[string]interface{}); !ok {
			t.Fatalf("expected %s snapshot, got %#v", agent, agents[agent])
		}
	}
}

func TestClientHelloReturnsServerHelloWithResumeReplay(t *testing.T) {
	server := newTestServer(t, ".")
	server.broadcastRecordedEvent("gemini", "gemini.message.delta", map[string]interface{}{"sessionId": "sess-1"})
	server.broadcastRecordedEvent("codex", "codex.turn/completed", map[string]interface{}{"threadId": "thread-1"})

	params := json.RawMessage([]byte(`{"clientId":"web-test","platform":"web","lastSeq":1,"capabilities":["client.hello","project.generation"]}`))
	result, err := server.handleClientHello(RpcRequest{Method: "client.hello", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := decodeResultJSON(t, result)
	if payload["protocolVersion"].(float64) != 1 {
		t.Fatalf("expected protocolVersion 1, got %#v", payload["protocolVersion"])
	}
	if payload["daemonVersion"].(string) != Version {
		t.Fatalf("expected daemon version %q, got %#v", Version, payload["daemonVersion"])
	}
	if payload["role"].(string) != "client" {
		t.Fatalf("expected role client, got %#v", payload["role"])
	}
	capabilities, ok := payload["capabilities"].([]interface{})
	if !ok {
		t.Fatalf("expected capabilities slice, got %#v", payload["capabilities"])
	}
	if len(capabilities) != 2 || capabilities[0].(string) != "client.hello" || capabilities[1].(string) != "project.generation" {
		t.Fatalf("expected minimal hello capabilities, got %#v", capabilities)
	}
	resume, ok := payload["resume"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected nested resume payload, got %#v", payload["resume"])
	}
	events := resume["events"].([]interface{})
	if len(events) != 1 || events[0].(map[string]interface{})["seq"].(float64) != 2 {
		t.Fatalf("expected replay after seq 1 to return one event, got %#v", events)
	}
	if resume["cursorExpired"].(bool) {
		t.Fatalf("expected non-expired hello resume, got %#v", resume)
	}
	if _, ok := events[0].(map[string]interface{})["projectGeneration"]; !ok {
		t.Fatalf("expected projectGeneration in replay event, got %#v", events[0])
	}
	agents, ok := payload["agents"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected agent status payload, got %#v", payload["agents"])
	}
	if _, ok := agents["claude"].(map[string]interface{}); !ok {
		t.Fatalf("expected claude agent status, got %#v", agents["claude"])
	}
}

func TestClientHelloWithoutCursorSkipsReplay(t *testing.T) {
	server := newTestServer(t, ".")
	server.broadcastRecordedEvent("gemini", "gemini.message.delta", map[string]interface{}{"sessionId": "sess-1"})

	params := json.RawMessage([]byte(`{"clientId":"ios-test","platform":"ios","capabilities":["client.hello","project.generation"]}`))
	result, err := server.handleClientHello(RpcRequest{Method: "client.hello", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := decodeResultJSON(t, result)
	resume := payload["resume"].(map[string]interface{})
	if resume["cursorExpired"].(bool) {
		t.Fatalf("expected no-cursor hello to avoid expired fallback, got %#v", resume)
	}
	events := resume["events"].([]interface{})
	if len(events) != 0 {
		t.Fatalf("expected no-cursor hello to skip replay, got %#v", events)
	}
	if resume["latestSeq"].(float64) != 1 {
		t.Fatalf("expected latestSeq to reflect current journal, got %#v", resume["latestSeq"])
	}
}

func TestClientHelloNegotiatesCapabilities(t *testing.T) {
	server := newTestServer(t, ".")

	params := json.RawMessage([]byte(`{"clientId":"web-test","platform":"web","capabilities":["client.hello","events.resume","unknown.capability"]}`))
	result, err := server.handleClientHello(RpcRequest{Method: "client.hello", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := decodeResultJSON(t, result)
	capabilities, ok := payload["capabilities"].([]interface{})
	if !ok {
		t.Fatalf("expected capabilities slice, got %#v", payload["capabilities"])
	}
	if len(capabilities) != 1 || capabilities[0].(string) != "client.hello" {
		t.Fatalf("expected only supported requested capabilities, got %#v", capabilities)
	}
}

func TestClientHelloOmitsProjectGenerationWithoutCapability(t *testing.T) {
	server := newTestServer(t, ".")
	server.broadcastRecordedEvent("gemini", "gemini.message.delta", map[string]interface{}{"sessionId": "sess-1"})

	params := json.RawMessage([]byte(`{"clientId":"legacy-web","platform":"web","lastSeq":0,"capabilities":["client.hello"]}`))
	result, err := server.handleClientHello(RpcRequest{Method: "client.hello", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := decodeResultJSON(t, result)
	project := payload["project"].(map[string]interface{})
	if _, ok := project["generation"]; ok {
		t.Fatalf("expected hello project to omit generation without capability, got %#v", project)
	}
	resume := payload["resume"].(map[string]interface{})
	resumeProject := resume["project"].(map[string]interface{})
	if _, ok := resumeProject["generation"]; ok {
		t.Fatalf("expected resume project to omit generation without capability, got %#v", resumeProject)
	}
	events := resume["events"].([]interface{})
	if len(events) != 0 {
		t.Fatalf("expected no replay events for afterSeq 0, got %#v", events)
	}
}

func decodeResultJSON(t *testing.T, result interface{}) map[string]interface{} {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode result json: %v", err)
	}
	return payload
}

func TestAttachEventMetaIncludesOperationMetadata(t *testing.T) {
	event := eventEnvelope{
		Seq:               7,
		Agent:             "claude",
		ProjectID:         "/tmp/project",
		ProjectGeneration: 3,
		OperationID:       "claude-op-7",
		Timestamp:         123456789,
	}
	payload := attachEventMeta(map[string]interface{}{
		"sessionId":   "sess-1",
		"operationId": "claude-op-7",
	}, event).(map[string]interface{})

	if payload["operationId"] != "claude-op-7" {
		t.Fatalf("expected top-level operationId to survive, got %#v", payload["operationId"])
	}
	meta, ok := payload["_anycode"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected _anycode metadata, got %#v", payload)
	}
	if meta["seq"].(uint64) != 7 {
		t.Fatalf("unexpected seq in metadata: %#v", meta)
	}
	if meta["agent"] != "claude" || meta["projectId"] != "/tmp/project" {
		t.Fatalf("unexpected agent/project metadata: %#v", meta)
	}
	if meta["projectGeneration"].(uint64) != 3 {
		t.Fatalf("unexpected project generation metadata: %#v", meta)
	}
	if meta["operationId"] != "claude-op-7" {
		t.Fatalf("unexpected operation metadata: %#v", meta)
	}
	if meta["ts"].(int64) != 123456789 {
		t.Fatalf("unexpected timestamp metadata: %#v", meta)
	}
}

func TestReplayEventsRetainsOperationIDInParamsAndEnvelope(t *testing.T) {
	server := newTestServer(t, ".")
	server.broadcastRecordedEvent("gemini", "gemini.message/assistant", map[string]interface{}{
		"sessionId":   "sess-1",
		"operationId": "gemini-op-1",
		"content":     "hello",
	})

	events, latestSeq := server.replayEvents(0, "")
	if latestSeq != 1 || len(events) != 1 {
		t.Fatalf("expected one replay event, latest=%d events=%d", latestSeq, len(events))
	}
	if events[0].OperationID != "gemini-op-1" {
		t.Fatalf("expected envelope operationId, got %#v", events[0])
	}
	params := events[0].Params.(map[string]interface{})
	if params["operationId"] != "gemini-op-1" {
		t.Fatalf("expected params operationId to survive replay, got %#v", params)
	}
}

func TestEventsResumePersistsAcrossServerRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, ".", "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Fatalf("first Server.Close failed: %v", err)
		}
	})
	first.broadcastRecordedEvent("claude", "claude.turn/completed", map[string]interface{}{
		"sessionId":   "sess-1",
		"operationId": "claude-op-1",
	})

	second, err := NewServer(0, ".", "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	events, latestSeq := second.replayEvents(0, "")
	if latestSeq != 1 {
		t.Fatalf("expected latest seq 1 after restart, got %d", latestSeq)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 persisted event after restart, got %d", len(events))
	}
	if events[0].Method != "claude.turn/completed" {
		t.Fatalf("unexpected replayed method: %#v", events[0])
	}
	params, ok := events[0].Params.(map[string]interface{})
	if !ok {
		t.Fatalf("expected persisted params map, got %#v", events[0].Params)
	}
	if params["operationId"] != "claude-op-1" {
		t.Fatalf("expected persisted operationId, got %#v", params)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("expected state db to exist: %v", err)
	}
}

func TestServerRestoresProjectStateAcrossRestart(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, rootA, "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	first.switchProject(rootB)
	if err := first.Close(); err != nil {
		t.Fatalf("first Server.Close failed: %v", err)
	}

	second, err := NewServer(0, rootA, "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	projectRoot, generation := second.currentProjectState()
	if projectRoot != rootB {
		t.Fatalf("expected restored project root %q, got %q", rootB, projectRoot)
	}
	if generation != 2 {
		t.Fatalf("expected restored generation 2, got %d", generation)
	}
	if second.gemini.agent.Cwd() != rootB {
		t.Fatalf("expected gemini cwd %q, got %q", rootB, second.gemini.agent.Cwd())
	}
	if second.claude.agent.Cwd() != rootB {
		t.Fatalf("expected claude cwd %q, got %q", rootB, second.claude.agent.Cwd())
	}
	info := second.currentProjectInfo()
	if info.ProjectID != rootB || info.Generation != 2 {
		t.Fatalf("unexpected restored project info: %#v", info)
	}
}

func TestTaskStatusRestoresPersistedAgentSessionsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	first.persistAgentSessionState("claude", "claude-session-1")
	first.persistAgentSessionState("gemini", "gemini-session-1")
	if err := first.Close(); err != nil {
		t.Fatalf("first Server.Close failed: %v", err)
	}

	second, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	claudeStatus, err := second.handleClaudeTaskStatus(RpcRequest{Method: "claude.taskStatus"}, nil)
	if err != nil {
		t.Fatalf("claude task status failed: %v", err)
	}
	if claudeStatus.(map[string]interface{})["sessionId"] != "claude-session-1" {
		t.Fatalf("expected restored claude session, got %#v", claudeStatus)
	}

	geminiStatus, err := second.handleGeminiTaskStatus(RpcRequest{Method: "gemini.taskStatus"}, nil)
	if err != nil {
		t.Fatalf("gemini task status failed: %v", err)
	}
	if geminiStatus.(map[string]interface{})["sessionId"] != "gemini-session-1" {
		t.Fatalf("expected restored gemini session, got %#v", geminiStatus)
	}
	if claudeStatus.(map[string]interface{})["running"].(bool) || geminiStatus.(map[string]interface{})["running"].(bool) {
		t.Fatalf("expected restored task status to remain non-running after restart")
	}
}

func TestClaudeTaskStatusRestoresOperationAndPermissionSnapshotsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	first.persistAgentSessionState("claude", "claude-session-1")
	if err := first.eventJournal.upsertOperation(persistedOperation{
		OperationID: "claude-op-1",
		Agent:       "claude",
		SessionID:   "claude-session-1",
		Status:      "running",
		StartedAt:   100,
		UpdatedAt:   100,
	}); err != nil {
		t.Fatalf("persist operation failed: %v", err)
	}
	if err := first.eventJournal.upsertPermission(persistedPermission{
		RequestID:   "perm-1",
		Agent:       "claude",
		SessionID:   "claude-session-1",
		ToolName:    "Bash",
		Status:      "pending",
		PayloadJSON: `{"toolName":"Bash"}`,
		CreatedAt:   200,
	}); err != nil {
		t.Fatalf("persist permission failed: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Server.Close failed: %v", err)
	}

	second, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	result, err := second.handleClaudeTaskStatus(RpcRequest{Method: "claude.taskStatus"}, nil)
	if err != nil {
		t.Fatalf("claude task status failed: %v", err)
	}
	status := result.(map[string]interface{})
	lastOperation, ok := status["lastOperation"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected lastOperation payload, got %#v", status)
	}
	if lastOperation["operationId"] != "claude-op-1" || lastOperation["status"] != "interrupted" {
		t.Fatalf("unexpected restored operation snapshot: %#v", lastOperation)
	}
	lastPermission, ok := status["lastPermission"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected lastPermission payload, got %#v", status)
	}
	if lastPermission["requestId"] != "perm-1" || lastPermission["status"] != "expired" {
		t.Fatalf("unexpected restored permission snapshot: %#v", lastPermission)
	}
}

func TestGeminiTaskStatusRestoresLastOperationAcrossRestart(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	first.persistAgentSessionState("gemini", "gemini-session-1")
	if err := first.eventJournal.upsertOperation(persistedOperation{
		OperationID: "gemini-op-1",
		Agent:       "gemini",
		SessionID:   "gemini-session-1",
		Status:      "running",
		StartedAt:   100,
		UpdatedAt:   100,
	}); err != nil {
		t.Fatalf("persist operation failed: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Server.Close failed: %v", err)
	}

	second, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	result, err := second.handleGeminiTaskStatus(RpcRequest{Method: "gemini.taskStatus"}, nil)
	if err != nil {
		t.Fatalf("gemini task status failed: %v", err)
	}
	status := result.(map[string]interface{})
	lastOperation, ok := status["lastOperation"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected lastOperation payload, got %#v", status)
	}
	if lastOperation["operationId"] != "gemini-op-1" || lastOperation["status"] != "interrupted" {
		t.Fatalf("unexpected restored operation snapshot: %#v", lastOperation)
	}
}

func TestCodexTaskStatusRestoresPersistedThreadAcrossRestart(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	first.persistCodexThreadState("thread-1")
	if err := first.eventJournal.upsertOperation(persistedOperation{
		OperationID: "codex-turn-1",
		Agent:       "codex",
		ThreadID:    "thread-1",
		Status:      "running",
		StartedAt:   100,
		UpdatedAt:   100,
	}); err != nil {
		t.Fatalf("persist codex operation failed: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Server.Close failed: %v", err)
	}

	second, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	result, err := second.handleCodexTaskStatus(RpcRequest{Method: "codex.taskStatus"}, nil)
	if err != nil {
		t.Fatalf("codex task status failed: %v", err)
	}
	status := result.(map[string]interface{})
	if status["threadId"] != "thread-1" {
		t.Fatalf("expected restored codex thread, got %#v", status)
	}
	lastOperation, ok := status["lastOperation"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected lastOperation payload, got %#v", status)
	}
	if lastOperation["operationId"] != "codex-turn-1" || lastOperation["status"] != "interrupted" {
		t.Fatalf("unexpected restored codex operation snapshot: %#v", lastOperation)
	}
	if status["running"].(bool) {
		t.Fatalf("expected codex task status to remain non-running after restart")
	}
}

func TestEventsResumeReportsCurrentProjectAfterSwitch(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	server := newTestServer(t, rootA)
	server.broadcastRecordedEvent("gemini", "gemini.message.delta", map[string]interface{}{"sessionId": "sess-a"})
	server.switchProject(rootB)

	params := json.RawMessage([]byte(`{"afterSeq":0}`))
	result, err := server.handleEventsResume(RpcRequest{Method: "events.resume", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}

	payload := result.(map[string]interface{})
	project := payload["project"].(*ProjectInfo)
	if project.ProjectID != rootB {
		t.Fatalf("expected current project %q, got %q", rootB, project.ProjectID)
	}
	if project.Generation != 2 {
		t.Fatalf("expected current generation 2, got %d", project.Generation)
	}
	events := payload["events"].([]eventEnvelope)
	if len(events) != 2 {
		t.Fatalf("expected original event and project.changed, got %d", len(events))
	}
	if events[1].Method != "project.changed" || events[1].ProjectID != rootB {
		t.Fatalf("expected project.changed for new project, got %#v", events[1])
	}
}

func TestProjectOpenReturnsProjectGeneration(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	server := newTestServer(t, rootA)

	encodedParams, err := json.Marshal(map[string]string{"path": rootB})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(encodedParams)
	result, err := server.handleProjectOpen(RpcRequest{Method: "project.open", Params: &params}, nil)
	if err != nil {
		t.Fatal(err)
	}

	info := result.(*ProjectInfo)
	if info.ProjectID != rootB {
		t.Fatalf("expected project id %q, got %q", rootB, info.ProjectID)
	}
	if info.Generation != 2 {
		t.Fatalf("expected generation 2, got %d", info.Generation)
	}

	events, latestSeq := server.replayEvents(0, rootB)
	if latestSeq == 0 || len(events) != 1 {
		t.Fatalf("expected one project change event, latest=%d events=%d", latestSeq, len(events))
	}
	if events[0].Method != "project.changed" {
		t.Fatalf("unexpected project event: %#v", events[0])
	}
}

func TestProjectListIncludesPersistedProjectsAcrossRestart(t *testing.T) {
	rootA := t.TempDir()
	rootB := filepath.Join(t.TempDir(), "nested", "repo-b")
	if err := os.MkdirAll(rootB, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, rootA, "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	first.switchProject(rootB)
	if err := first.Close(); err != nil {
		t.Fatalf("first Server.Close failed: %v", err)
	}

	second, err := NewServer(0, rootA, "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	result, err := second.handleProjectList(RpcRequest{Method: "project.list"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	projects, ok := result.(map[string]interface{})["projects"].([]map[string]interface{})
	if !ok {
		t.Fatalf("expected merged project list, got %#v", result)
	}

	found := false
	for _, project := range projects {
		if project["path"] == rootB {
			found = true
			if project["name"] != filepath.Base(rootB) {
				t.Fatalf("unexpected persisted project name: %#v", project)
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected persisted project %q in project.list, got %#v", rootB, projects)
	}
}

func TestClaudeTaskStatusRestoresPersistedSessionAcrossRestart(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	first.persistAgentSessionState("claude", "claude-session-1")
	if err := first.Close(); err != nil {
		t.Fatalf("first Server.Close failed: %v", err)
	}

	second, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	status, err := second.handleClaudeTaskStatus(RpcRequest{Method: "claude.taskStatus"}, nil)
	if err != nil {
		t.Fatalf("claude task status failed: %v", err)
	}
	payload := status.(map[string]interface{})
	if payload["sessionId"] != "claude-session-1" {
		t.Fatalf("expected restored claude session, got %#v", payload)
	}
	if payload["running"].(bool) {
		t.Fatalf("expected restored claude session to be idle, got %#v", payload)
	}
}

func TestGeminiTaskStatusRestoresPersistedSessionAcrossRestart(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ANYCODE_STATE_DB_PATH", statePath)

	first, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("first NewServer failed: %v", err)
	}
	first.persistAgentSessionState("gemini", "gemini-session-1")
	if err := first.Close(); err != nil {
		t.Fatalf("first Server.Close failed: %v", err)
	}

	second, err := NewServer(0, root, "secret-token")
	if err != nil {
		t.Fatalf("second NewServer failed: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Server.Close failed: %v", err)
		}
	})

	status, err := second.handleGeminiTaskStatus(RpcRequest{Method: "gemini.taskStatus"}, nil)
	if err != nil {
		t.Fatalf("gemini task status failed: %v", err)
	}
	payload := status.(map[string]interface{})
	if payload["sessionId"] != "gemini-session-1" {
		t.Fatalf("expected restored gemini session, got %#v", payload)
	}
	if payload["running"].(bool) {
		t.Fatalf("expected restored gemini session to be idle, got %#v", payload)
	}
}

func TestFsWriteAbsoluteRejectsStaleProjectGeneration(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	target := filepath.Join(rootA, "note.txt")
	server := newTestServer(t, rootA)
	server.switchProject(rootB)

	encodedParams, err := json.Marshal(map[string]interface{}{
		"path":                      target,
		"content":                   "hello",
		"expectedProjectGeneration": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(encodedParams)
	if _, err := server.handleFsWriteAbsolute(RpcRequest{Method: "fs.writeAbsolute", Params: &params}, nil); err == nil {
		t.Fatal("expected stale generation write to fail")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected file to remain absent, stat err=%v", err)
	}
}

func TestGitStatusRejectsStaleProjectGeneration(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	server := newTestServer(t, rootA)
	server.switchProject(rootB)

	encodedParams, err := json.Marshal(map[string]interface{}{
		"expectedProjectGeneration": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(encodedParams)
	if _, err := server.handleGitStatus(RpcRequest{Method: "git.status", Params: &params}, nil); err == nil {
		t.Fatal("expected stale generation git.status to fail")
	}
}



func TestClaudeNewSessionRejectsStaleProjectGeneration(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	server := newTestServer(t, rootA)
	server.switchProject(rootB)

	encodedParams, err := json.Marshal(map[string]interface{}{
		"cwd":                       rootA,
		"expectedProjectGeneration": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(encodedParams)
	if _, err := server.handleClaudeNewSession(RpcRequest{Method: "claude.newSession", Params: &params}, nil); err == nil {
		t.Fatal("expected stale generation claude.newSession to fail")
	}
}

func TestGeminiStartRejectsStaleProjectGeneration(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	server := newTestServer(t, rootA)
	server.switchProject(rootB)

	encodedParams, err := json.Marshal(map[string]interface{}{
		"cwd":                       rootA,
		"expectedProjectGeneration": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(encodedParams)
	if _, err := server.handleGeminiStart(RpcRequest{Method: "gemini.start", Params: &params}, nil); err == nil {
		t.Fatal("expected stale generation gemini.start to fail")
	}
}

func TestCodexApplyFileChangesRejectsStaleProjectGeneration(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	target := filepath.Join(rootA, "created.txt")
	server := newTestServer(t, rootA)
	server.switchProject(rootB)

	encodedParams, err := json.Marshal(map[string]interface{}{
		"expectedProjectGeneration": 1,
		"changes": []map[string]interface{}{
			{
				"path": target,
				"kind": map[string]interface{}{"type": "add"},
				"diff": "+hello",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(encodedParams)
	if _, err := server.handleCodexApplyFileChanges(RpcRequest{Method: "codex.applyFileChanges", Params: &params}, nil); err == nil {
		t.Fatal("expected stale generation file apply to fail")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected file to remain absent, stat err=%v", err)
	}
}

func TestCodexDynamicRejectsStaleProjectGeneration(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	server := newTestServer(t, rootA)
	server.switchProject(rootB)

	tests := []struct {
		name   string
		method string
		params map[string]interface{}
	}{
		{
			name:   "threadRead",
			method: "codex.threadRead",
			params: map[string]interface{}{"threadId": "thread-1", "includeTurns": true},
		},
		{
			name:   "threadResume",
			method: "codex.threadResume",
			params: map[string]interface{}{"threadId": "thread-1"},
		},
		{
			name:   "turnStart",
			method: "codex.turnStart",
			params: map[string]interface{}{"threadId": "thread-1", "input": []map[string]interface{}{{"type": "text", "text": "hello"}}},
		},
		{
			name:   "turnInterrupt",
			method: "codex.turnInterrupt",
			params: map[string]interface{}{"threadId": "thread-1", "expectedTurnId": "turn-1"},
		},
		{
			name:   "respond",
			method: "codex.respond",
			params: map[string]interface{}{"requestId": 1, "result": map[string]interface{}{"decision": "accept"}},
		},
		{
			name:   "threadArchive",
			method: "codex.threadArchive",
			params: map[string]interface{}{"threadId": "thread-1"},
		},
		{
			name:   "threadRename",
			method: "codex.threadRename",
			params: map[string]interface{}{"threadId": "thread-1", "name": "renamed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paramsMap := make(map[string]interface{}, len(tt.params)+1)
			for key, value := range tt.params {
				paramsMap[key] = value
			}
			paramsMap["expectedProjectGeneration"] = 1

			encodedParams, err := json.Marshal(paramsMap)
			if err != nil {
				t.Fatal(err)
			}
			params := json.RawMessage(encodedParams)
			if _, err := server.handleCodexDynamic(RpcRequest{Method: tt.method, Params: &params}, nil); err == nil {
				t.Fatalf("expected %s to fail on stale project generation", tt.method)
			}
		})
	}
}



func TestCodexThreadReadRejectsMismatchedProjectID(t *testing.T) {
	root := t.TempDir()
	server := newTestServer(t, root)

	encodedParams, err := json.Marshal(map[string]interface{}{
		"threadId": "thread-1",
		"projectId": filepath.Join(root, "other-project"),
	})
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(encodedParams)
	if _, err := server.handleCodexDynamic(RpcRequest{Method: "codex.threadRead", Params: &params}, nil); err == nil {
		t.Fatal("expected codex.threadRead with mismatched projectId to fail")
	}
}

func TestProjectOpenRejectsOldGitBindingAndAcceptsNewBinding(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	initGitRepoForTest(t, rootA)
	initGitRepoForTest(t, rootB)

	server := newTestServer(t, rootA)
	oldProject := server.currentProjectInfo()
	newProject := openProjectForTest(t, server, rootB)

	oldEncodedParams, err := json.Marshal(map[string]interface{}{
		"projectId":                 oldProject.ProjectID,
		"expectedProjectGeneration": oldProject.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldParams := json.RawMessage(oldEncodedParams)
	if _, err := server.handleGitStatus(RpcRequest{Method: "git.status", Params: &oldParams}, nil); err == nil {
		t.Fatal("expected git.status with old project binding to fail after project.open")
	}

	newEncodedParams, err := json.Marshal(map[string]interface{}{
		"projectId":                 newProject.ProjectID,
		"expectedProjectGeneration": newProject.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	newParams := json.RawMessage(newEncodedParams)
	result, err := server.handleGitStatus(RpcRequest{Method: "git.status", Params: &newParams}, nil)
	if err != nil {
		t.Fatalf("expected git.status with current project binding to pass, got %v", err)
	}
	status, ok := result.(*GitStatus)
	if !ok {
		t.Fatalf("expected *GitStatus, got %T", result)
	}
	if status.Branch == "" {
		t.Fatalf("expected git branch in status, got %#v", status)
	}
}

func TestProjectOpenRejectsOldAgentBindingsAndAcceptsNewBindings(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	server := newTestServer(t, rootA)
	oldProject := server.currentProjectInfo()
	newProject := openProjectForTest(t, server, rootB)

	tests := []struct {
		name        string
		call        func(req RpcRequest) (interface{}, error)
		method      string
		wantCwdPath string
	}{
		{
			name:        "claude.start",
			call:        func(req RpcRequest) (interface{}, error) { return server.handleClaudeStart(req, nil) },
			method:      "claude.start",
			wantCwdPath: rootB,
		},
		{
			name:        "gemini.start",
			call:        func(req RpcRequest) (interface{}, error) { return server.handleGeminiStart(req, nil) },
			method:      "gemini.start",
			wantCwdPath: rootB,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldEncodedParams, err := json.Marshal(map[string]interface{}{
				"cwd":                       rootA,
				"projectId":                 oldProject.ProjectID,
				"expectedProjectGeneration": oldProject.Generation,
			})
			if err != nil {
				t.Fatal(err)
			}
			oldParams := json.RawMessage(oldEncodedParams)
			if _, err := tt.call(RpcRequest{Method: tt.method, Params: &oldParams}); err == nil {
				t.Fatalf("expected %s with old project binding to fail after project.open", tt.method)
			}

			newEncodedParams, err := json.Marshal(map[string]interface{}{
				"cwd":                       rootB,
				"projectId":                 newProject.ProjectID,
				"expectedProjectGeneration": newProject.Generation,
			})
			if err != nil {
				t.Fatal(err)
			}
			newParams := json.RawMessage(newEncodedParams)
			result, err := tt.call(RpcRequest{Method: tt.method, Params: &newParams})
			if err != nil {
				t.Fatalf("expected %s with current project binding to pass, got %v", tt.method, err)
			}
			payload, ok := result.(map[string]interface{})
			if !ok {
				t.Fatalf("expected response map, got %T", result)
			}
			if payload["cwd"] != tt.wantCwdPath {
				t.Fatalf("expected %s cwd %q, got %#v", tt.method, tt.wantCwdPath, payload["cwd"])
			}
		})
	}
}

func TestGeminiTaskStatusIncludesRecentEvents(t *testing.T) {
	bridge := NewGeminiBridge()
	bridge.taskRunning = true
	bridge.taskStartedAt = time.Unix(30, 0)
	bridge.currentOperationID = "gemini-op-1"
	bridge.emit("session/init", map[string]interface{}{"sessionId": "sess-1"})
	bridge.emit("message/assistant", map[string]interface{}{"sessionId": "sess-1", "content": "hi"})

	status := bridge.TaskStatus()
	if !status["running"].(bool) {
		t.Fatal("expected gemini task to be running")
	}
	if status["sessionId"].(string) != "sess-1" {
		t.Fatalf("unexpected session id: %#v", status["sessionId"])
	}
	if status["operationId"].(string) != "gemini-op-1" {
		t.Fatalf("unexpected operation id: %#v", status["operationId"])
	}
	events := status["recentEvents"].([]cachedNotification)
	if len(events) != 2 {
		t.Fatalf("expected 2 cached gemini events, got %d", len(events))
	}
	if events[0].Params.(map[string]interface{})["operationId"] != "gemini-op-1" {
		t.Fatalf("expected operationId on cached event, got %#v", events[0].Params)
	}
	bridge.emit("turn/completed", map[string]interface{}{"sessionId": "sess-1"})
	if bridge.TaskStatus()["running"].(bool) {
		t.Fatal("expected gemini task to stop after turn/completed")
	}
}
