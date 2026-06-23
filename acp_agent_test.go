package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestChooseAcpPermissionOption(t *testing.T) {
	params := map[string]interface{}{
		"options": []interface{}{
			map[string]interface{}{"kind": "reject", "optionId": "deny"},
			map[string]interface{}{"kind": "allow_once", "optionId": "allow-once"},
		},
	}

	optionId, ok := pickAcpAllowOption(params)
	if !ok {
		t.Fatalf("expected an allow option to be picked")
	}
	if optionId != "allow-once" {
		t.Fatalf("expected allow-once, got %q", optionId)
	}
}

func TestAcpAgentSetModelUnsupportedByCapability(t *testing.T) {
	agent := NewAcpAgent(AcpAgentConfig{ID: "test", Label: "Test", Command: "test"})
	err := agent.SetModel("session-1", "opus")
	if err == nil {
		t.Fatal("expected unsupported method error")
	}
	unsupported, ok := err.(*AcpUnsupportedMethodError)
	if !ok {
		t.Fatalf("expected AcpUnsupportedMethodError, got %T", err)
	}
	if unsupported.Method != "session/set_model" {
		t.Fatalf("unexpected method: %s", unsupported.Method)
	}
}

func TestAcpAgentSetModeUnsupportedByCapability(t *testing.T) {
	agent := NewAcpAgent(AcpAgentConfig{ID: "test", Label: "Test", Command: "test"})
	err := agent.SetMode("session-1", "auto")
	if err == nil {
		t.Fatal("expected unsupported method error")
	}
	unsupported, ok := err.(*AcpUnsupportedMethodError)
	if !ok {
		t.Fatalf("expected AcpUnsupportedMethodError, got %T", err)
	}
	if unsupported.Method != "session/set_mode" {
		t.Fatalf("unexpected method: %s", unsupported.Method)
	}
}

func TestAcpAgentSessionUpdateNotification(t *testing.T) {
	agent := NewAcpAgent(AcpAgentConfig{ID: "test", Label: "Test", Command: "test"})
	var gotMethod string
	var gotParams map[string]interface{}
	agent.OnNotification = func(method string, params interface{}) {
		gotMethod = method
		gotParams, _ = params.(map[string]interface{})
	}

	agent.handleNotification("session/update", map[string]interface{}{
		"sessionId": "session-1",
		"update": map[string]interface{}{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]interface{}{"text": "hello"},
		},
	})

	if gotMethod != "message/assistant" {
		t.Fatalf("expected message/assistant, got %q", gotMethod)
	}
	if gotParams["sessionId"] != "session-1" {
		t.Fatalf("unexpected sessionId: %#v", gotParams["sessionId"])
	}
	if gotParams["content"] != "hello" {
		t.Fatalf("unexpected content: %#v", gotParams["content"])
	}
	if gotParams["delta"] != true {
		t.Fatalf("unexpected delta: %#v", gotParams["delta"])
	}
}

func TestAcpAgentCaptureSessionModels(t *testing.T) {
	agent := NewAcpAgent(AcpAgentConfig{ID: "test", Label: "Test", Command: "test"})

	// Shape advertised by claude-code-acp / Cursor in session/new responses.
	agent.captureSessionModels(map[string]interface{}{
		"models": map[string]interface{}{
			"availableModels": []interface{}{
				map[string]interface{}{"modelId": "sonnet", "name": "Sonnet", "description": "Fast"},
				map[string]interface{}{"modelId": "opus", "name": "Opus"},
			},
			"currentModelId": "opus",
		},
	})

	models, err := agent.ListModels()
	if err != nil {
		t.Fatalf("ListModels error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d: %#v", len(models), models)
	}
	if models[0].ID != "sonnet" || models[0].Name != "Sonnet" || models[0].Description != "Fast" {
		t.Fatalf("unexpected first model: %#v", models[0])
	}
	if models[1].ID != "opus" {
		t.Fatalf("unexpected second model: %#v", models[1])
	}
	if agent.CurrentModelID() != "opus" {
		t.Fatalf("expected current model opus, got %q", agent.CurrentModelID())
	}

	// A response without a models block must not clobber the cache.
	agent.captureSessionModels(map[string]interface{}{"sessionId": "s1"})
	if got, _ := agent.ListModels(); len(got) != 2 {
		t.Fatalf("cache should be preserved, got %d models", len(got))
	}
}

func TestAcpAgentCaptureSessionModes(t *testing.T) {
	agent := NewAcpAgent(AcpAgentConfig{ID: "test", Label: "Test", Command: "test"})

	// Shape advertised by claude-code-acp in session/new responses.
	agent.captureSessionModels(map[string]interface{}{
		"modes": map[string]interface{}{
			"availableModes": []interface{}{
				map[string]interface{}{"id": "default", "name": "Default", "description": "Ask"},
				map[string]interface{}{"id": "plan", "name": "Plan Mode"},
			},
			"currentModeId": "plan",
		},
	})

	modes := agent.ListModes()
	if len(modes) != 2 {
		t.Fatalf("expected 2 modes, got %d: %#v", len(modes), modes)
	}
	if modes[0].ID != "default" || modes[1].ID != "plan" {
		t.Fatalf("unexpected modes: %#v", modes)
	}
	if agent.CurrentModeID() != "plan" {
		t.Fatalf("expected current mode plan, got %q", agent.CurrentModeID())
	}
}

func TestAcpAgentLoadedSessionState(t *testing.T) {
	agent := NewAcpAgent(AcpAgentConfig{ID: "test", Label: "Test", Command: "test"})
	if agent.IsSessionLoaded("session-1") {
		t.Fatal("new agent should not have a loaded session")
	}
	agent.markLoadedSession("session-1")
	if !agent.IsSessionLoaded("session-1") {
		t.Fatal("expected session-1 to be marked loaded")
	}
	if agent.IsSessionLoaded("session-2") {
		t.Fatal("different session should not be marked loaded")
	}
	agent.clearLoadedSession()
	if agent.IsSessionLoaded("session-1") {
		t.Fatal("loaded session should be cleared")
	}
}

func TestAgentBridgeIgnoresResponsesFromStaleGeneration(t *testing.T) {
	bridge := NewAgentBridge()
	ch := make(chan agentResult, 1)
	bridge.generation = 2
	bridge.pending[agentRequestKey{Generation: 2, ID: float64(1)}] = ch

	id := json.RawMessage("1")
	bridge.handleMessage(1, agentMessage{ID: &id, Result: map[string]interface{}{"ok": true}})
	select {
	case <-ch:
		t.Fatal("stale generation response should not be delivered")
	default:
	}

	bridge.handleMessage(2, agentMessage{ID: &id, Result: map[string]interface{}{"ok": true}})
	select {
	case res := <-ch:
		if result, ok := res.Result.(map[string]interface{}); !ok || result["ok"] != true {
			t.Fatalf("unexpected result: %#v", res.Result)
		}
	case <-time.After(time.Second):
		t.Fatal("current generation response was not delivered")
	}
}
