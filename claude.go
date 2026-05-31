package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxCachedNotifications bounds the ring buffer of recent notifications we
// keep for replay when the iOS app reconnects mid-turn (e.g. after a
// force-quit). Sized generously so a long in-progress turn can still be
// reconstructed on the client.
const maxCachedNotifications = 300

// cachedNotification stores a single notification for replay after
// iOS app reconnect.
type cachedNotification struct {
	Method string      `json:"method"`
	Params interface{} `json:"params"`
	Time   int64       `json:"time"`
}

// ClaudeBridge runs Claude Code through the standard Agent Client Protocol
// (ACP) using `claude-code-acp` (the Zed-maintained bridge) as the agent.
// Session storage still lives in `~/.claude/projects/<cwd-key>/*.jsonl`
// because claude-code-acp delegates to the Claude SDK, so session listing
// and resume continue to read those JSONL files directly.
type ClaudeBridge struct {
	mu sync.Mutex

	agent *AcpAgent

	// Per-session config (mirrors what the iOS UI exposes). These are
	// applied via session/setModel + session/setMode when a prompt is
	// dispatched, mirroring how GeminiBridge handles config.
	cwd            string
	currentSession string
	model          string
	effort         string // kept for UI compat (claude SDK uses "max_thinking_tokens" etc; surfaced as info only)
	mode           string

	// Pending permission requests awaiting user decision via
	// claude.permission/respond. Keyed by requestId (the daemon-generated
	// id we surface to the iOS UI, not the ACP id).
	pendingPerms map[string]*pendingPermission

	// Task tracking: lets the iOS app query whether a task is in progress
	// after reconnecting from background.
	taskRunning   bool
	taskStartedAt time.Time

	// Ring buffer of recent notifications for replay on reconnect.
	lastNotifications []cachedNotification

	OnNotification func(method string, params interface{})
}

// pendingPermission tracks one ACP permission request that's waiting on
// the user's decision in the app. We keep both the original ACP request
// id (so we know who to respond to) and the parsed option list (so we
// can validate the user's selection).
type pendingPermission struct {
	acpID     interface{}
	options   []interface{}
	timer     *time.Timer
	sessionId string
	toolName  string
	toolCall  map[string]interface{}
}

// acpCommand is the ACP bridge binary. It can be overridden via the
// CLAUDE_ACP_COMMAND env var, useful for local development.
func claudeAcpCommand() string {
	if v := os.Getenv("CLAUDE_ACP_COMMAND"); v != "" {
		return v
	}
	return "claude-code-acp"
}

func NewClaudeBridge() *ClaudeBridge {
	c := &ClaudeBridge{
		mode:         "default",
		effort:       "medium",
		pendingPerms: make(map[string]*pendingPermission),
	}
	c.agent = NewAcpAgent(AcpAgentConfig{
		ID:          "claude",
		Label:       "Claude",
		Command:     claudeAcpCommand(),
		Args:        nil,
		Env:         []string{"TERM=dumb"},
		VersionArgs: []string{"--version"},
		// We handle permission requests ourselves via OnPermissionRequest
		// (auto-approving for bypass/dontAsk modes, forwarding to the iOS
		// UI otherwise), so leave the blanket auto-approve off.
		AutoApprovePermissions: false,
	})
	c.agent.OnNotification = func(method string, params interface{}) {
		// Track sessionId from session updates so resume/cancel work.
		if p, ok := params.(map[string]interface{}); ok {
			if sid, _ := p["sessionId"].(string); sid != "" {
				c.mu.Lock()
				if c.currentSession == "" {
					c.currentSession = sid
				}
				c.mu.Unlock()
			}
		}
		c.emit(method, params)

		// Detect turn/completed to clear taskRunning flag.
		if method == "turn/completed" {
			c.mu.Lock()
			c.taskRunning = false
			c.mu.Unlock()
		}
	}
	c.agent.OnPermissionRequest = c.handlePermissionRequest
	return c
}

// handlePermissionRequest is invoked when claude-code-acp asks the client
// to approve/deny a tool invocation. Behavior depends on the configured
// permission mode:
//
//   - bypass / dontAsk: auto-approve immediately using the first allow
//     option. (The Claude SDK's own bypassPermissions/acceptEdits modes
//     normally short-circuit this, but the bridge still occasionally
//     forwards a request — handle it anyway.)
//   - Everything else: emit `claude.permission/request` to the iOS UI
//     and wait for `claude.permission/respond`. After 5 minutes with no
//     reply we auto-reject so Claude doesn't hang forever.
func (c *ClaudeBridge) handlePermissionRequest(id interface{}, params map[string]interface{}) {
	c.mu.Lock()
	mode := c.mode
	c.mu.Unlock()

	if mode == "bypass" || mode == "dontAsk" || mode == "auto" || mode == "acceptEdits" || mode == "bypassPermissions" {
		if optionId, ok := pickAcpAllowOption(params); ok {
			_ = c.agent.Respond(id, map[string]interface{}{
				"outcome": map[string]interface{}{
					"outcome":  "selected",
					"optionId": optionId,
				},
			})
			return
		}
		// No options at all — best-effort cancel so claude-code-acp
		// doesn't deadlock waiting on a reply.
		_ = c.agent.Respond(id, map[string]interface{}{
			"outcome": map[string]interface{}{"outcome": "cancelled"},
		})
		return
	}

	// Forward to the iOS UI.
	requestId := fmt.Sprintf("perm-%d", time.Now().UnixNano())
	options, _ := params["options"].([]interface{})
	sessionId, _ := params["sessionId"].(string)
	toolCall, _ := params["toolCall"].(map[string]interface{})
	toolName, _ := toolCall["title"].(string)
	if toolName == "" {
		toolName, _ = toolCall["name"].(string)
	}

	pending := &pendingPermission{
		acpID:     id,
		options:   options,
		sessionId: sessionId,
		toolName:  toolName,
		toolCall:  toolCall,
	}
	pending.timer = time.AfterFunc(5*time.Minute, func() {
		c.resolvePermission(requestId, "", true)
	})

	c.mu.Lock()
	c.pendingPerms[requestId] = pending
	c.mu.Unlock()

	notif := map[string]interface{}{
		"requestId": requestId,
		"sessionId": sessionId,
		"toolName":  toolName,
		"toolCall":  toolCall,
		"options":   options,
	}
	c.emit("permission/request", notif)
}

// RespondPermission resolves a previously-emitted permission/request. If
// `cancelled` is true the daemon picks a reject option (falling back to
// the ACP "cancelled" outcome when no reject option is offered).
func (c *ClaudeBridge) RespondPermission(requestId, optionId string, cancelled bool) error {
	if !c.resolvePermission(requestId, optionId, cancelled) {
		return fmt.Errorf("no pending permission request: %s", requestId)
	}
	return nil
}

func (c *ClaudeBridge) resolvePermission(requestId, optionId string, cancelled bool) bool {
	c.mu.Lock()
	pending, ok := c.pendingPerms[requestId]
	if ok {
		delete(c.pendingPerms, requestId)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}

	paramsMap := map[string]interface{}{"options": pending.options}

	var outcome map[string]interface{}
	if cancelled || optionId == "" {
		if rejectId, ok := pickAcpRejectOption(paramsMap); ok {
			outcome = map[string]interface{}{"outcome": "selected", "optionId": rejectId}
		} else {
			outcome = map[string]interface{}{"outcome": "cancelled"}
		}
	} else {
		outcome = map[string]interface{}{"outcome": "selected", "optionId": optionId}
	}
	_ = c.agent.Respond(pending.acpID, map[string]interface{}{"outcome": outcome})
	return true
}

func (c *ClaudeBridge) SetCwd(cwd string) {
	c.mu.Lock()
	c.cwd = cwd
	c.mu.Unlock()
	c.agent.SetCwd(cwd)
}

func (c *ClaudeBridge) IsRunning() bool { return c.agent.IsRunning() }
func (c *ClaudeBridge) Available() bool { return c.agent.Available() }

func (c *ClaudeBridge) SessionId() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentSession
}
func (c *ClaudeBridge) Model() string  { c.mu.Lock(); defer c.mu.Unlock(); return c.model }
func (c *ClaudeBridge) Effort() string { c.mu.Lock(); defer c.mu.Unlock(); return c.effort }
func (c *ClaudeBridge) Mode() string   { c.mu.Lock(); defer c.mu.Unlock(); return c.mode }

func (c *ClaudeBridge) CheckAvailable() bool {
	return c.agent.CheckAvailable()
}

// Start ensures the ACP agent subprocess is running. The caller can pass a
// cwd to update the working directory used for newly created sessions.
func (c *ClaudeBridge) Start(cwd string) error {
	c.mu.Lock()
	if cwd != "" {
		c.cwd = cwd
	}
	c.mu.Unlock()
	if cwd != "" {
		c.agent.SetCwd(cwd)
	}

	if !ensureClaudeAcp(c.agent.CheckAvailable) {
		return fmt.Errorf("%s not found in PATH; install with `npm install -g %s`", claudeAcpCommand(), claudeAcpPackage)
	}
	return c.agent.Start()
}

func (c *ClaudeBridge) Stop() {
	c.agent.Stop()
	c.mu.Lock()
	c.currentSession = ""
	// Cancel any in-flight permission prompts so the iOS UI doesn't keep
	// a stale sheet open after a manual stop.
	for _, p := range c.pendingPerms {
		if p.timer != nil {
			p.timer.Stop()
		}
	}
	c.pendingPerms = make(map[string]*pendingPermission)
	c.mu.Unlock()
}

func (c *ClaudeBridge) Cancel() {
	c.mu.Lock()
	sid := c.currentSession
	c.mu.Unlock()
	if sid != "" {
		_ = c.agent.Cancel(sid)
	}
}

// SetConfig stores the desired model/effort/mode. They are applied to the
// active session lazily on the next prompt (or now if a session is open).
func (c *ClaudeBridge) SetConfig(model, effort, mode string) {
	c.mu.Lock()
	if model != "" {
		c.model = model
	}
	if effort != "" {
		c.effort = effort
	}
	if mode != "" {
		c.mode = mode
	}
	sid := c.currentSession
	desiredModel := c.model
	desiredMode := c.mode
	c.mu.Unlock()

	if sid == "" || !c.agent.IsRunning() {
		return
	}
	if desiredModel != "" && desiredModel != "default" {
		_ = c.agent.SetModel(sid, desiredModel)
	}
	if desiredMode != "" && desiredMode != "default" {
		_ = c.agent.SetMode(sid, desiredMode)
	}
}

// Prompt sends a user turn to the active session. If no session is open,
// a new one is created (and a synthetic claude.init notification is fired
// so the iOS UI mirrors the previous stream-json behavior).
func (c *ClaudeBridge) Prompt(text string, images []string) error {
	if !c.agent.IsRunning() {
		if !ensureClaudeAcp(c.agent.CheckAvailable) {
			return fmt.Errorf("%s not found in PATH; install with `npm install -g %s`", claudeAcpCommand(), claudeAcpPackage)
		}
		if err := c.agent.Start(); err != nil {
			return fmt.Errorf("start claude-code-acp: %w", err)
		}
	}

	c.mu.Lock()
	sid := c.currentSession
	cwd := c.cwd
	desiredModel := c.model
	desiredMode := c.mode
	c.mu.Unlock()

	if sid == "" {
		result, err := c.agent.NewSession(cwd)
		if err != nil {
			return fmt.Errorf("session/new: %w", err)
		}
		newSid, _ := result["sessionId"].(string)
		if newSid == "" {
			return fmt.Errorf("session/new returned no sessionId")
		}
		c.mu.Lock()
		c.currentSession = newSid
		c.mu.Unlock()
		sid = newSid

		if desiredModel != "" && desiredModel != "default" {
			_ = c.agent.SetModel(sid, desiredModel)
		}
		if desiredMode != "" && desiredMode != "default" {
			_ = c.agent.SetMode(sid, desiredMode)
		}

		c.emit("init", map[string]interface{}{
			"sessionId": sid,
			"model":     desiredModel,
		})
	}

	// Mark the task as running before spawning the goroutine.
	c.mu.Lock()
	c.taskRunning = true
	c.taskStartedAt = time.Now()
	c.mu.Unlock()

	// Run the prompt in the background so the WS RPC returns promptly
	// while the ACP agent streams session/update notifications.
	go func() {
		_, err := c.agent.Prompt(sid, text, images)
		if err != nil {
			log.Printf("[claude] session/prompt failed: %v", err)
			c.emit("error", map[string]interface{}{
				"error":     err.Error(),
				"sessionId": sid,
			})
		}
		c.mu.Lock()
		c.taskRunning = false
		c.mu.Unlock()
		c.emit("turn/completed", map[string]interface{}{
			"sessionId": sid,
			"success":   err == nil,
		})
	}()
	return nil
}

// LoadSession resumes a session by ID. It loads the conversation into the
// ACP agent (so further prompts continue in that session) AND returns the
// parsed items from the local JSONL so the UI can display history.
func (c *ClaudeBridge) LoadSession(sessionId, cwd string) (map[string]interface{}, error) {
	if sessionId == "" {
		return nil, fmt.Errorf("sessionId is required")
	}
	if cwd == "" {
		c.mu.Lock()
		cwd = c.cwd
		c.mu.Unlock()
	}

	session, items, err := c.readSessionFile(sessionId, cwd)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	// If a turn is already in flight for this exact session, the live
	// agent already has it loaded in memory. This happens when the iOS
	// app reconnects (e.g. after a force-quit) while the background agent
	// is still working. Touching the agent now (SetCwd / session/load)
	// could disturb the running turn, so we only read the JSONL history
	// for display and leave the agent untouched.
	busy := c.taskRunning && c.currentSession == sessionId
	c.currentSession = sessionId
	if cwd != "" {
		c.cwd = cwd
	}
	if model, _ := session["model"].(string); model != "" {
		c.model = model
	}
	if mode, _ := session["permissionMode"].(string); mode != "" {
		c.mode = mode
	}
	c.mu.Unlock()

	if busy {
		log.Printf("[claude] resume: task in flight for %s; returning history only, not disturbing the running agent", sessionId)
	} else {
		c.agent.SetCwd(cwd)

		// Best-effort: start the ACP agent and tell it to resume so future
		// prompts continue in this session. We don't fail the RPC if either
		// step errors out — the UI can still browse the JSONL items, and
		// the next Prompt() call will retry the start.
		if !c.agent.IsRunning() && c.agent.CheckAvailable() {
			if err := c.agent.Start(); err != nil {
				log.Printf("[claude] resume: agent start failed: %v", err)
			}
		}
		if c.agent.IsRunning() {
			if _, lerr := c.agent.LoadSession(sessionId, cwd); lerr != nil {
				log.Printf("[claude] session/load best-effort failed: %v", lerr)
			}
		}
	}

	c.emit("init", map[string]interface{}{
		"sessionId": sessionId,
		"model":     session["model"],
		"cwd":       cwd,
	})

	return map[string]interface{}{
		"ok":      true,
		"session": session,
		"items":   items,
	}, nil
}

// ListSessions returns Claude sessions across all projects (matching the
// behavior of `claude /resume`). Reads directly from `~/.claude/projects/`.
func (c *ClaudeBridge) ListSessions(cwd string) (map[string]interface{}, error) {
	sessions, err := listAllClaudeSessions()
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	sort.Slice(sessions, func(i, j int) bool {
		return int64Value(sessions[i]["updatedAt"]) > int64Value(sessions[j]["updatedAt"])
	})

	return map[string]interface{}{"ok": true, "sessions": sessions}, nil
}

// ClearSession forgets the current session id so the next Prompt() starts
// fresh.
func (c *ClaudeBridge) ClearSession() {
	c.mu.Lock()
	c.currentSession = ""
	c.mu.Unlock()
}

// TaskStatus returns the current task state for the iOS app to query
// after reconnecting from background.
func (c *ClaudeBridge) TaskStatus() map[string]interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Build pending permission list for recovery
	var pendingPermList []map[string]interface{}
	for reqId, p := range c.pendingPerms {
		pendingPermList = append(pendingPermList, map[string]interface{}{
			"requestId": reqId,
			"toolName":  p.toolName,
			"sessionId": p.sessionId,
			"options":   p.options,
			"toolCall":  p.toolCall,
		})
	}

	// Copy cached notifications for response
	events := make([]cachedNotification, len(c.lastNotifications))
	copy(events, c.lastNotifications)

	return map[string]interface{}{
		"ok":            true,
		"running":       c.taskRunning,
		"sessionId":     c.currentSession,
		"startedAt":     c.taskStartedAt.UnixMilli(),
		"recentEvents":  events,
		"pendingPerms":  pendingPermList,
		"model":         c.model,
		"cwd":           c.cwd,
	}
}

func (c *ClaudeBridge) emit(method string, params interface{}) {
	// Cache the notification for replay on reconnect.
	c.mu.Lock()
	c.lastNotifications = append(c.lastNotifications, cachedNotification{
		Method: method,
		Params: params,
		Time:   time.Now().UnixMilli(),
	})
	// Trim to ring buffer size.
	if len(c.lastNotifications) > maxCachedNotifications {
		c.lastNotifications = c.lastNotifications[len(c.lastNotifications)-maxCachedNotifications:]
	}
	c.mu.Unlock()

	if c.OnNotification != nil {
		c.OnNotification(method, params)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Session file parsing (kept verbatim from stream-json mode — these
// JSONL files are written by the Claude SDK whether you invoke it via
// stream-json or claude-code-acp, so list/load logic is shared.)
// ────────────────────────────────────────────────────────────────────────

func (c *ClaudeBridge) readSessionFile(sessionId, cwd string) (map[string]interface{}, []map[string]interface{}, error) {
	// Try the project dir derived from cwd first.
	primary := filepath.Join(claudeProjectDir(cwd), sessionId+".jsonl")
	if _, err := os.Stat(primary); err == nil {
		return parseClaudeSessionFile(primary, sessionId, true)
	}
	// Fall back to scanning all project dirs since the same sessionId is
	// globally unique — Claude can store it under a different cwd key
	// than what the iOS app passed in.
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".claude", "projects")
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, entry.Name(), sessionId+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return parseClaudeSessionFile(candidate, sessionId, true)
		}
	}
	return nil, nil, fmt.Errorf("session file not found: %s", sessionId)
}

func claudeProjectDir(cwd string) string {
	home, _ := os.UserHomeDir()
	clean := filepath.Clean(cwd)
	key := strings.ReplaceAll(clean, string(os.PathSeparator), "-")
	return filepath.Join(home, ".claude", "projects", key)
}

func listClaudeSessionsInDir(projectDir string) ([]map[string]interface{}, error) {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return nil, err
	}

	sessions := []map[string]interface{}{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		sessionId := strings.TrimSuffix(entry.Name(), ".jsonl")
		session, _, err := parseClaudeSessionFile(filepath.Join(projectDir, entry.Name()), sessionId, false)
		if err != nil {
			log.Printf("[claude.sessionList] parse %s: %v", entry.Name(), err)
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func listAllClaudeSessions() ([]map[string]interface{}, error) {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	sessions := []map[string]interface{}{}
	seen := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectSessions, err := listClaudeSessionsInDir(filepath.Join(root, entry.Name()))
		if err != nil {
			log.Printf("[claude.sessionList] scan %s: %v", entry.Name(), err)
			continue
		}
		for _, session := range projectSessions {
			sessionId, _ := session["sessionId"].(string)
			if sessionId == "" || seen[sessionId] {
				continue
			}
			seen[sessionId] = true
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func parseClaudeSessionFile(path, fallbackSessionId string, includeItems bool) (map[string]interface{}, []map[string]interface{}, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	info, _ := file.Stat()
	session := map[string]interface{}{
		"sessionId": fallbackSessionId,
		"title":     fallbackSessionId,
		"preview":   "",
		"cwd":       "",
		"model":     "",
	}
	if info != nil {
		updated := info.ModTime()
		session["updatedAt"] = updated.UnixMilli()
		session["timeAgo"] = humanTimeAgo(updated)
	}

	items := []map[string]interface{}{}
	seen := map[string]bool{}
	firstUser := ""
	lastPrompt := ""
	title := ""

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		var obj map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &obj); err != nil {
			continue
		}

		if sid, _ := obj["sessionId"].(string); sid != "" {
			session["sessionId"] = sid
		}
		if cwd, _ := obj["cwd"].(string); cwd != "" {
			session["cwd"] = cwd
		}
		if mode, _ := obj["permissionMode"].(string); mode != "" {
			session["permissionMode"] = mode
		}
		if ts, ok := parseClaudeTimestamp(obj["timestamp"]); ok {
			session["updatedAt"] = ts.UnixMilli()
			session["timeAgo"] = humanTimeAgo(ts)
		}

		switch obj["type"] {
		case "ai-title":
			if t, _ := obj["aiTitle"].(string); t != "" {
				title = t
			}
		case "last-prompt":
			if p, _ := obj["lastPrompt"].(string); p != "" {
				lastPrompt = p
			}
		case "user":
			text := claudeMessageText(obj["message"])
			if text == "" {
				continue
			}
			if firstUser == "" {
				firstUser = text
			}
			if includeItems && !seenClaudeUUID(obj, seen) {
				items = append(items, map[string]interface{}{
					"id":      firstString(obj, "uuid", "promptId"),
					"type":    "userMessage",
					"text":    text,
					"content": []map[string]string{{"type": "text", "text": text}},
				})
			}
		case "assistant":
			msg, _ := obj["message"].(map[string]interface{})
			if model, _ := msg["model"].(string); model != "" {
				session["model"] = model
			}
			text, thoughts := claudeAssistantParts(msg)
			if includeItems && !seenClaudeUUID(obj, seen) {
				if thoughts != "" {
					items = append(items, map[string]interface{}{
						"id":          fmt.Sprintf("%s-thought", firstString(obj, "uuid")),
						"type":        "reasoning",
						"summaryText": thoughts,
						"text":        thoughts,
					})
				}
				if text != "" {
					items = append(items, map[string]interface{}{
						"id":   firstString(obj, "uuid"),
						"type": "agentMessage",
						"text": text,
					})
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}

	preview := firstNonEmpty(lastPrompt, firstUser, title, fallbackSessionId)
	session["title"] = firstNonEmpty(title, preview)
	session["preview"] = preview
	return session, items, nil
}

func claudeMessageText(raw interface{}) string {
	msg, _ := raw.(map[string]interface{})
	content := msg["content"]
	if text, ok := content.(string); ok {
		return text
	}
	arr, _ := content.([]interface{})
	parts := []string{}
	for _, item := range arr {
		block, _ := item.(map[string]interface{})
		if text, _ := block["text"].(string); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func claudeAssistantParts(msg map[string]interface{}) (string, string) {
	arr, _ := msg["content"].([]interface{})
	textParts := []string{}
	thoughtParts := []string{}
	for _, item := range arr {
		block, _ := item.(map[string]interface{})
		switch block["type"] {
		case "text":
			if text, _ := block["text"].(string); text != "" {
				textParts = append(textParts, text)
			}
		case "thinking":
			if thought, _ := block["thinking"].(string); thought != "" {
				thoughtParts = append(thoughtParts, thought)
			}
		case "tool_use":
			if name, _ := block["name"].(string); name != "" {
				textParts = append(textParts, fmt.Sprintf("Used tool: %s", name))
			}
		}
	}
	return strings.Join(textParts, "\n\n"), strings.Join(thoughtParts, "\n\n")
}

func seenClaudeUUID(obj map[string]interface{}, seen map[string]bool) bool {
	uuid := firstString(obj, "uuid")
	if uuid == "" {
		return false
	}
	if seen[uuid] {
		return true
	}
	seen[uuid] = true
	return false
}

func parseClaudeTimestamp(raw interface{}) (time.Time, bool) {
	s, _ := raw.(string)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

func humanTimeAgo(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	if d < 7*24*time.Hour {
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Format("Jan 2")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func int64Value(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
