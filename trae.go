package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// TraeBridge runs the Trae CLI through the standard Agent Client Protocol (ACP)
// using `traecli acp serve` (Trae's built-in ACP server mode). All the shared
// ACP plumbing (permission flow, current-turn replay buffer, session
// lifecycle) lives in the embedded acpChatBridge; Trae's genuine differences
// are the spawn command, that it advertises its own set of modes (rather than
// Cursor's fixed agent/plan/ask, so mode handling is generic pass-through
// driven by what the agent reports), and its on-disk session store used for
// session listing.
type TraeBridge struct {
	acpChatBridge
}

// traeCommand resolves the Trae CLI binary. The trae.cn installer exposes it as
// `traecli`. Override with ANYCODE_TRAE_BIN (mirrors ANYCODE_CURSOR_BIN /
// ANYCODE_CODEX_BIN).
func traeCommand() string {
	if v := strings.TrimSpace(os.Getenv("ANYCODE_TRAE_BIN")); v != "" {
		return v
	}
	candidates := []string{"traecli", "trae-cli", "trae-agent", "ta"}
	for _, candidate := range candidates {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	if path := resolveCommandInCommonAgentBins(candidates...); path != "" {
		return path
	}

	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		for _, candidate := range candidates {
			exePath := filepath.Join(localAppData, "trae-cli", "bin", candidate+".exe")
			if _, err := os.Stat(exePath); err == nil {
				return exePath
			}
		}
	}

	return "traecli"
}

func canonicalTraeModel(value string) string {
	return strings.TrimSpace(value)
}

// canonicalTraeMode normalizes a mode value. Unlike Cursor we don't pin a fixed
// vocabulary — Trae advertises its own modes via the ACP session response, so we
// only trim and pass through whatever the client selected.
func canonicalTraeMode(value string) string {
	return strings.TrimSpace(value)
}

func NewTraeBridge() *TraeBridge {
	t := &TraeBridge{}
	t.initAcpChatBridge(AcpAgentConfig{
		ID:          "trae",
		Label:       "Trae",
		Command:     traeCommand(),
		Args:        []string{"acp", "serve"},
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
		id:    "trae",
		label: "Trae",
		startUnavailableError: func() error {
			return fmt.Errorf("%s not found in PATH; install the Trae CLI from https://docs.trae.cn/cli (or set ANYCODE_TRAE_BIN)", traeCommand())
		},
		loadUnavailableError: func() error {
			return fmt.Errorf("%s not found in PATH; install the Trae CLI from https://docs.trae.cn/cli", traeCommand())
		},
		canonicalModel: canonicalTraeModel,
		canonicalMode:  canonicalTraeMode,
	})
	// The mode surfaced to the UI falls back to whatever the Trae agent
	// reported as current when the user hasn't picked one explicitly.
	t.hooks.effectiveMode = func(selected string) string {
		if selected != "" {
			return selected
		}
		return t.agent.CurrentModeID()
	}
	t.hooks.newSession = t.NewSession
	return t
}

// traeSessionsRoot returns the directory where the Trae CLI persists its
// session store. traecli writes one `<session-id>/session.json` per
// conversation under the OS cache dir. It returns the first candidate that
// exists on disk, falling back to the most-canonical path otherwise. An
// explicit override is supported for non-standard installs.
func traeSessionsRoot() string {
	if env := os.Getenv("ANYCODE_TRAE_SESSIONS_DIR"); env != "" {
		return env
	}
	candidates := traeSessionsCandidates()
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

// traeSessionsCandidates lists the directories where traecli may have written
// its session store, most-canonical first. traecli resolves this from the OS
// cache dir, which `os.UserCacheDir` mirrors: `~/Library/Caches/trae-cli` on
// macOS, `~/.cache/trae-cli` on Linux, and `%LocalAppData%\trae-cli` on Windows.
// On Windows we also probe the Roaming AppData tree as a fallback in case the
// install resolved its data dir there instead.
func traeSessionsCandidates() []string {
	var dirs []string
	add := func(base string) {
		if base != "" {
			dirs = append(dirs, filepath.Join(base, "trae-cli", "sessions"))
		}
	}
	if cache, err := os.UserCacheDir(); err == nil {
		add(cache)
	}
	if runtime.GOOS == "windows" {
		add(os.Getenv("LOCALAPPDATA"))
		add(os.Getenv("APPDATA"))
	} else if home, err := os.UserHomeDir(); err == nil {
		if runtime.GOOS == "darwin" {
			add(filepath.Join(home, "Library", "Caches"))
		} else {
			add(filepath.Join(home, ".cache"))
		}
	}
	return dirs
}

type traeSessionFile struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Metadata  struct {
		Cwd            string `json:"cwd"`
		ModelName      string `json:"model_name"`
		PermissionMode string `json:"permission_mode"`
		Title          string `json:"title"`
	} `json:"metadata"`
}

type traeSessionSummary struct {
	ID             string
	Title          string
	Preview        string
	Cwd            string
	Model          string
	PermissionMode string
	UpdatedAt      time.Time
	HasUpdatedAt   bool
	HasContent     bool
}

// ListSessions reports historical Trae conversations by reading the Trae CLI's
// on-disk session store. Unlike the Cursor CLI (which keeps no parseable
// transcript), traecli writes a `session.json` per conversation, so past
// sessions can be surfaced for resume via ACP `session/load`.
func (t *TraeBridge) ListSessions(cwd string) (map[string]interface{}, error) {
	root := traeSessionsRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]interface{}{"ok": true, "sessions": []map[string]interface{}{}}, nil
		}
		return nil, err
	}

	sessions := []map[string]interface{}{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		summary, ok := readTraeSessionSummary(dir, entry.Name())
		if !ok {
			continue
		}

		title := strings.TrimSpace(summary.Title)
		if title == "" {
			title = summary.ID
		}
		preview := strings.TrimSpace(summary.Preview)
		if preview == "" {
			preview = title
		}

		session := map[string]interface{}{
			"sessionId":      summary.ID,
			"title":          title,
			"preview":        preview,
			"cwd":            summary.Cwd,
			"model":          summary.Model,
			"permissionMode": summary.PermissionMode,
		}

		if summary.HasUpdatedAt {
			session["updatedAt"] = summary.UpdatedAt.UnixMilli()
			session["timeAgo"] = humanTimeAgo(summary.UpdatedAt)
		}

		sessions = append(sessions, session)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return int64Value(sessions[i]["updatedAt"]) > int64Value(sessions[j]["updatedAt"])
	})

	return map[string]interface{}{"ok": true, "sessions": sessions}, nil
}

func readTraeSessionSummary(dir, fallbackID string) (traeSessionSummary, bool) {
	summary := traeSessionSummary{ID: fallbackID}
	sessionPath := filepath.Join(dir, "session.json")
	if data, err := os.ReadFile(sessionPath); err == nil {
		var sf traeSessionFile
		if err := json.Unmarshal(data, &sf); err != nil {
			log.Printf("[trae.sessionList] parse %s: %v", filepath.Base(dir), err)
		} else {
			if sf.ID != "" {
				summary.ID = sf.ID
			}
			summary.HasContent = true
			summary.Title = strings.TrimSpace(sf.Metadata.Title)
			summary.Preview = summary.Title
			summary.Cwd = sf.Metadata.Cwd
			summary.Model = sf.Metadata.ModelName
			summary.PermissionMode = sf.Metadata.PermissionMode
			if updated, ok := parseClaudeTimestamp(sf.UpdatedAt); ok {
				summary.UpdatedAt = updated
				summary.HasUpdatedAt = true
			} else if created, ok := parseClaudeTimestamp(sf.CreatedAt); ok {
				summary.UpdatedAt = created
				summary.HasUpdatedAt = true
			}
		}
	}

	mergeTraeEventsSummary(filepath.Join(dir, "events.jsonl"), &summary)

	if !summary.HasUpdatedAt {
		if info, err := os.Stat(sessionPath); err == nil {
			summary.UpdatedAt = info.ModTime()
			summary.HasUpdatedAt = true
		} else if info, err := os.Stat(filepath.Join(dir, "events.jsonl")); err == nil {
			summary.UpdatedAt = info.ModTime()
			summary.HasUpdatedAt = true
		} else if info, err := os.Stat(filepath.Join(dir, "session.log")); err == nil {
			summary.UpdatedAt = info.ModTime()
			summary.HasUpdatedAt = true
		}
	}

	return summary, summary.ID != "" && summary.HasContent
}

func mergeTraeEventsSummary(path string, summary *traeSessionSummary) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if sid := firstString(event, "session_id", "sessionId"); sid != "" {
			summary.ID = sid
		}
		if ts, ok := parseClaudeTimestamp(firstString(event, "created_at", "createdAt")); ok {
			if !summary.HasUpdatedAt || ts.After(summary.UpdatedAt) {
				summary.UpdatedAt = ts
				summary.HasUpdatedAt = true
			}
		}
		mergeTraeStateUpdate(event, summary)
		mergeTraeUserInput(event, summary)
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[trae.sessionList] scan %s: %v", path, err)
	}
}

func mergeTraeStateUpdate(event map[string]interface{}, summary *traeSessionSummary) {
	stateUpdate, _ := event["state_update"].(map[string]interface{})
	updates, _ := stateUpdate["updates"].(map[string]interface{})
	if len(updates) == 0 {
		return
	}
	if summary.Cwd == "" {
		summary.Cwd = firstString(updates, "cwd")
	}
	if summary.Model == "" {
		summary.Model = firstString(updates, "model_name", "modelName", "model")
	}
	if summary.PermissionMode == "" {
		summary.PermissionMode = firstString(updates, "permission_mode", "permissionMode")
	}
	if summary.Title == "" {
		summary.Title = firstString(updates, "title")
		if summary.Title != "" {
			summary.HasContent = true
		}
	}
}

func mergeTraeUserInput(event map[string]interface{}, summary *traeSessionSummary) {
	if summary.Preview != "" && summary.Title != "" {
		return
	}
	message, _ := event["message"].(map[string]interface{})
	inner, _ := message["message"].(map[string]interface{})
	if firstString(inner, "role") != "user" {
		return
	}
	extra, _ := inner["extra"].(map[string]interface{})
	if original, ok := extra["is_original_user_input"].(bool); ok && !original {
		return
	}
	content := strings.TrimSpace(firstString(inner, "content"))
	if content == "" || strings.HasPrefix(content, "<system-reminder>") {
		return
	}
	if summary.Preview == "" {
		summary.Preview = content
	}
	if summary.Title == "" {
		summary.Title = content
	}
	summary.HasContent = true
}

func (t *TraeBridge) TaskStatus() map[string]interface{} {
	t.mu.Lock()
	delegate := t.permissionDelegate
	events := make([]cachedNotification, len(t.lastNotifications))
	copy(events, t.lastNotifications)
	config := map[string]interface{}{
		"model": canonicalTraeModel(t.selectedModel),
		"mode":  canonicalTraeMode(t.selectedMode),
	}
	capabilities := t.Capabilities()
	t.mu.Unlock()

	pendingPermList := []map[string]interface{}{}
	if delegate != nil {
		pendingPermList = delegate.Pending()
	}

	return map[string]interface{}{
		"ok":              true,
		"running":         t.taskRunning,
		"operationId":     t.currentOperationID,
		"sessionId":       t.currentSession,
		"startedAt":       t.taskStartedAt.UnixMilli(),
		"recentEvents":    events,
		"pendingPerms":    pendingPermList,
		"cwd":             t.cwd,
		"config":          config,
		"capabilities":    capabilities,
		"model":           config["model"],
		"mode":            config["mode"],
		"availableModels": agentModelList(t.agent),
	}
}

func (t *TraeBridge) emit(method string, params interface{}) {
	t.mu.Lock()
	payload := attachOperationID(params, t.currentOperationID)
	t.lastNotifications = append(t.lastNotifications, cachedNotification{
		Method: method,
		Params: payload,
		Time:   time.Now().UnixMilli(),
	})
	if len(t.lastNotifications) > maxCachedNotifications {
		t.lastNotifications = t.lastNotifications[len(t.lastNotifications)-maxCachedNotifications:]
	}
	t.mu.Unlock()

	if t.OnNotification != nil {
		t.OnNotification(method, payload)
	}
}
