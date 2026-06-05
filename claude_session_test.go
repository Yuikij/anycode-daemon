package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseClaudeSessionKeepsInitialCwd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-1.jsonl")
	content := `{"type":"user","sessionId":"session-1","cwd":"/repo","message":{"role":"user","content":"start"}}
{"type":"assistant","sessionId":"session-1","cwd":"/repo/ios","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}
{"type":"user","sessionId":"session-1","cwd":"/repo/ios","message":{"role":"user","content":[{"type":"tool_result","content":"done"}]}}
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	session, _, err := parseClaudeSessionFile(path, "session-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := session["cwd"]; got != "/repo" {
		t.Fatalf("cwd = %v, want /repo", got)
	}
}
