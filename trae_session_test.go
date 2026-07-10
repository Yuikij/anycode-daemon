package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTraeListSessionsReadsSessionStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ANYCODE_TRAE_SESSIONS_DIR", root)

	write := func(id, body string) {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	write("aaa", `{"id":"aaa","created_at":"2026-06-20T01:00:00+08:00","updated_at":"2026-06-20T01:00:00+08:00","metadata":{"cwd":"/repo","model_name":"Doubao-Seed-Code","permission_mode":"default","title":"older"}}`)
	write("bbb", `{"id":"bbb","created_at":"2026-06-23T01:43:13.70692+08:00","updated_at":"2026-06-23T01:43:13.70692+08:00","metadata":{"cwd":"/","model_name":"Doubao-Seed-Code","permission_mode":"default","title":"newer"}}`)
	// A directory without session.json (e.g. an aborted run) must be skipped.
	if err := os.MkdirAll(filepath.Join(root, "ccc"), 0755); err != nil {
		t.Fatal(err)
	}

	bridge := &TraeBridge{}
	result, err := bridge.ListSessions("/")
	if err != nil {
		t.Fatal(err)
	}

	sessions, ok := result["sessions"].([]map[string]interface{})
	if !ok {
		t.Fatalf("sessions has unexpected type %T", result["sessions"])
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	// Sorted by updatedAt descending: the newer session comes first.
	if sessions[0]["sessionId"] != "bbb" || sessions[0]["title"] != "newer" {
		t.Fatalf("first session = %#v, want bbb/newer", sessions[0])
	}
	if sessions[1]["cwd"] != "/repo" {
		t.Fatalf("second session cwd = %v, want /repo", sessions[1]["cwd"])
	}
	if sessions[0]["updatedAt"] == nil || sessions[0]["timeAgo"] == nil {
		t.Fatalf("expected updatedAt/timeAgo to be populated, got %#v", sessions[0])
	}
}

func TestTraeListSessionsMissingRoot(t *testing.T) {
	t.Setenv("ANYCODE_TRAE_SESSIONS_DIR", filepath.Join(t.TempDir(), "does-not-exist"))
	bridge := &TraeBridge{}
	result, err := bridge.ListSessions("/")
	if err != nil {
		t.Fatalf("missing session store should not error, got %v", err)
	}
	sessions, _ := result["sessions"].([]map[string]interface{})
	if len(sessions) != 0 {
		t.Fatalf("expected empty sessions, got %d", len(sessions))
	}
}
