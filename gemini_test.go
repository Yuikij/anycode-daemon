package main

import "testing"

func TestParseGeminiSessionListText(t *testing.T) {
	output := `
1. Build mobile UI (2 hours ago) [A1B2C3D4-E5F6-7890-ABCD-1234567890AB]
2) Fix daemon reconnect (yesterday) 12345678-1234-1234-1234-1234567890ab
3. Add Hermes support 2 days ago [abcdef12-1234-4567-9999-abcdefabcdef]
`

	sessions := ParseGeminiSessionList(output)
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(sessions))
	}
	if sessions[0]["title"] != "Build mobile UI" {
		t.Fatalf("unexpected title: %#v", sessions[0]["title"])
	}
	if sessions[0]["timeAgo"] != "2 hours ago" {
		t.Fatalf("unexpected timeAgo: %#v", sessions[0]["timeAgo"])
	}
	if sessions[1]["sessionId"] != "12345678-1234-1234-1234-1234567890ab" {
		t.Fatalf("unexpected sessionId: %#v", sessions[1]["sessionId"])
	}
	if sessions[2]["title"] != "Add Hermes support" {
		t.Fatalf("unexpected title: %#v", sessions[2]["title"])
	}
	if sessions[2]["timeAgo"] != "2 days ago" {
		t.Fatalf("unexpected timeAgo: %#v", sessions[2]["timeAgo"])
	}
	if sessions[2]["ageMs"] != int64(48*60*60*1000) {
		t.Fatalf("unexpected ageMs: %#v", sessions[2]["ageMs"])
	}
}

func TestParseGeminiSessionListJSON(t *testing.T) {
	output := `{"sessions":[{"id":"session-123456","name":"Plan work","updatedAt":"today"}]}`

	sessions := ParseGeminiSessionList(output)
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0]["sessionId"] != "session-123456" {
		t.Fatalf("unexpected sessionId: %#v", sessions[0]["sessionId"])
	}
	if sessions[0]["title"] != "Plan work" {
		t.Fatalf("unexpected title: %#v", sessions[0]["title"])
	}
}
