package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CursorBridge runs the Cursor CLI through the standard Agent Client Protocol
// (ACP) using `agent acp` (Cursor's built-in ACP server mode). All the shared
// ACP plumbing (permission flow, current-turn replay buffer, session
// lifecycle) lives in the embedded acpChatBridge; Cursor's genuine differences
// are the spawn command, the fixed agent/plan/ask mode vocabulary, and that it
// has no on-disk transcript so session listing degrades to an empty list.
type CursorBridge struct {
	acpChatBridge
}

// cursorCommand resolves the Cursor CLI binary. The cursor.com installer
// exposes it as `agent`; some installs expose `cursor-agent`. Override with
// ANYCODE_CURSOR_BIN (mirrors ANYCODE_CODEX_BIN / CLAUDE_ACP_COMMAND).
func cursorCommand() string {
	if v := strings.TrimSpace(os.Getenv("ANYCODE_CURSOR_BIN")); v != "" {
		return v
	}
	if _, err := exec.LookPath("agent"); err == nil {
		return "agent"
	}
	if _, err := exec.LookPath("cursor-agent"); err == nil {
		return "cursor-agent"
	}
	return "agent"
}

func canonicalCursorModel(value string) string {
	return strings.TrimSpace(value)
}

func canonicalCursorMode(value string) string {
	switch strings.TrimSpace(value) {
	case "plan":
		return "plan"
	case "ask":
		return "ask"
	default:
		return "agent"
	}
}

func NewCursorBridge() *CursorBridge {
	c := &CursorBridge{}
	c.selectedMode = "agent"
	c.initAcpChatBridge(AcpAgentConfig{
		ID:          "cursor",
		Label:       "Cursor",
		Command:     cursorCommand(),
		Args:        []string{"acp"},
		Env:         []string{"TERM=dumb"},
		VersionArgs: []string{"--version"},
		Capabilities: AcpCapabilities{
			CanSetModel: true,
			CanSetMode:  true,
		},
		// Permission requests are surfaced to the UI via OnPermissionRequest,
		// not blanket auto-approved.
		AutoApprovePermissions: false,
	}, acpChatBridgeHooks{
		id:    "cursor",
		label: "Cursor",
		startUnavailableError: func() error {
			return fmt.Errorf("%s not found in PATH; install the Cursor CLI from https://cursor.com/install (or set ANYCODE_CURSOR_BIN)", cursorCommand())
		},
		loadUnavailableError: func() error {
			return fmt.Errorf("%s not found in PATH; install the Cursor CLI from https://cursor.com/install", cursorCommand())
		},
		canonicalModel: canonicalCursorModel,
		canonicalMode:  canonicalCursorMode,
	})
	c.hooks.newSession = c.NewSession
	return c
}

// ListSessions reports historical Cursor conversations. The Cursor CLI only
// exposes session history through interactive TUI commands (`agent ls` /
// `agent resume`, both of which open a picker and block on stdin) — there is no
// non-interactive "print sessions as JSON" command. Invoking those here would
// hang the RPC read loop and, since a bare arg is treated as a prompt, risks
// spending agent credits. So session listing degrades to an empty list: the
// live session created via ACP (`session/new`) is still tracked and surfaced
// through taskStatus / notifications, which is what the clients rely on.
func (c *CursorBridge) ListSessions(cwd string) (map[string]interface{}, error) {
	return map[string]interface{}{
		"ok":       true,
		"sessions": []map[string]interface{}{},
	}, nil
}
