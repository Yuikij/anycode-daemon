package main

import "testing"

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
