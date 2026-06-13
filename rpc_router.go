package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func (s *Server) recordCodexEvent(method string, params interface{}) {
	if id := extractThreadID(params); id != "" {
		s.persistCodexThreadState(id)
	}
	s.recordAgentOperationEvent("codex", method, params)
	if runtime := s.runtime.CodexRuntime(); runtime != nil {
		runtime.RecordEvent(method, params)
	}
}

// extractThreadID best-effort pulls a thread id out of a codex notification's
// params (either top-level `threadId` or nested `thread.id`).
func extractThreadID(params interface{}) string {
	m, ok := params.(map[string]interface{})
	if !ok {
		return ""
	}
	if id, ok := m["threadId"].(string); ok && id != "" {
		return id
	}
	if th, ok := m["thread"].(map[string]interface{}); ok {
		if id, ok := th["id"].(string); ok {
			return id
		}
	}
	return ""
}

func (s *Server) switchProject(newRoot string) {
	s.mu.Lock()
	s.projectRoot = newRoot
	s.projectGeneration++
	generation := s.projectGeneration
	s.mu.Unlock()
	if s.eventJournal != nil {
		if err := s.eventJournal.saveProjectState(newRoot, generation); err != nil {
			log.Printf("[server] failed to persist project state: %v", err)
		}
		if err := s.eventJournal.upsertProject(newRoot, filepath.Base(newRoot), nowUnixMilli()); err != nil {
			log.Printf("[server] failed to persist project registry entry: %v", err)
		}
	}
	s.gemini.SetCwd(newRoot)
	s.claude.SetCwd(newRoot)
	s.cron.Stop()
	s.cron.Start(newRoot)
	s.broadcastRecordedEvent("daemon", "project.changed", map[string]interface{}{
		"root":       newRoot,
		"projectId":  newRoot,
		"generation": generation,
	})
	log.Printf("[server] switched to project: %s", newRoot)
}

func (s *Server) handleRequest(req RpcRequest, client *wsClient) (interface{}, error) {
	if s.paramValidation && s.paramValidator != nil {
		if err := s.paramValidator.validate(req.Method, req.Params); err != nil {
			return nil, err
		}
	}
	if handler, ok := s.routes[req.Method]; ok {
		return handler(req, client)
	}
	if strings.HasPrefix(req.Method, "codex.") {
		return s.handleCodexDynamic(req, client)
	}
	return nil, fmt.Errorf("unknown method: %s", req.Method)
}

func (s *Server) initRoutes() {
	s.routes["daemon.version"] = s.handleDaemonVersion
	s.routes["client.hello"] = s.handleClientHello
	s.routes["daemon.configRead"] = s.handleDaemonConfigRead
	s.routes["daemon.configWrite"] = s.handleDaemonConfigWrite
	s.routes["share.create"] = s.handleShareCreate
	s.routes["fs.browse"] = s.handleFsBrowse
	s.routes["fs.readAbsolute"] = s.handleFsReadAbsolute
	s.routes["fs.writeAbsolute"] = s.handleFsWriteAbsolute
	s.routes["project.open"] = s.handleProjectOpen
	s.routes["fs.tree"] = s.handleFsTree
	s.routes["git.status"] = s.handleGitStatus
	s.routes["git.diff"] = s.handleGitDiff
	s.routes["git.log"] = s.handleGitLog
	s.routes["codex.start"] = s.handleCodexStart
	s.routes["codex.stop"] = s.handleCodexStop
	s.routes["codex.status"] = s.handleCodexStatus
	s.routes["codex.taskStatus"] = s.handleCodexTaskStatus
	s.routes["codex.configWrite"] = s.handleCodexConfigWrite
	s.routes["codex.respond"] = s.handleCodexRespond
	s.routes["codex.revertFileChanges"] = s.handleCodexRevertFileChanges
	s.routes["codex.applyFileChanges"] = s.handleCodexApplyFileChanges
	s.routes["gemini.start"] = s.handleGeminiStart
	s.routes["gemini.status"] = s.handleGeminiStatus
	s.routes["gemini.newSession"] = s.handleGeminiNewSession
	s.routes["gemini.loadSession"] = s.handleGeminiLoadSession
	s.routes["gemini.prompt"] = s.handleGeminiPrompt
	s.routes["gemini.cancel"] = s.handleGeminiCancel
	s.routes["gemini.taskStatus"] = s.handleGeminiTaskStatus
	s.routes["gemini.setMode"] = s.handleGeminiSetMode
	s.routes["gemini.setModel"] = s.handleGeminiSetModel
	s.routes["gemini.sessionList"] = s.handleGeminiSessionList
	s.routes["claude.start"] = s.handleClaudeStart
	s.routes["claude.status"] = s.handleClaudeStatus
	s.routes["claude.sessionList"] = s.handleClaudeSessionList
	s.routes["claude.loadSession"] = s.handleClaudeLoadSession
	s.routes["claude.newSession"] = s.handleClaudeNewSession
	s.routes["claude.sessionDelete"] = s.handleClaudeSessionDelete
	s.routes["claude.sessionRename"] = s.handleClaudeSessionRename
	s.routes["claude.setConfig"] = s.handleClaudeSetConfig
	s.routes["claude.prompt"] = s.handleClaudePrompt
	s.routes["claude.cancel"] = s.handleClaudeCancel
	s.routes["claude.stop"] = s.handleClaudeStop
	s.routes["claude.taskStatus"] = s.handleClaudeTaskStatus
	s.routes["claude.permission/respond"] = s.handleClaudePermissionRespond
	s.routes["cron.list"] = s.handleCronList
	s.routes["cron.create"] = s.handleCronCreate
	s.routes["cron.update"] = s.handleCronUpdate
	s.routes["cron.delete"] = s.handleCronDelete
}

func (s *Server) handleCodexDynamic(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	rpcMethod := codexMethodMap(req.Method)
	if params == nil {
		return s.codex.Send(rpcMethod, map[string]interface{}{})
	}
	normalizedParams, _, err := s.normalizeProjectScopedParams(params, false)
	if err != nil {
		return nil, err
	}
	if normalizedParams == nil {
		normalizedParams = map[string]interface{}{}
	}
	return s.codex.Send(rpcMethod, normalizedParams)

}

// codexDynamicMethods maps the AnyCode-facing `codex.*` method names that are
// served via handleCodexDynamic to their underlying app-server RPC names. These
// are part of the protocol catalog (protocol/methods.json) even though they are
// not registered in initRoutes, because they are dispatched dynamically.
var codexDynamicMethods = map[string]string{
	"codex.threadList":      "thread/list",
	"codex.threadRead":      "thread/read",
	"codex.threadStart":     "thread/start",
	"codex.threadResume":    "thread/resume",
	"codex.threadArchive":   "thread/archive",
	"codex.threadUnarchive": "thread/unarchive",
	"codex.threadRename":    "thread/name/set",
	"codex.threadRollback":  "thread/rollback",
	"codex.threadCompact":   "thread/compact",
	"codex.turnStart":       "turn/start",
	"codex.turnSteer":       "turn/steer",
	"codex.turnInterrupt":   "turn/interrupt",
	"codex.modelList":       "model/list",
	"codex.configRead":      "config/read",
}

func codexMethodMap(method string) string {
	if v, ok := codexDynamicMethods[method]; ok {
		return v
	}
	return method
}

// registeredMethodNames returns every RPC method the daemon can serve: the
// statically registered routes plus the named dynamic codex passthroughs. This
// is the runtime view that protocol/methods.json is validated against.
func (s *Server) registeredMethodNames() []string {
	names := make([]string, 0, len(s.routes)+len(codexDynamicMethods))
	for name := range s.routes {
		names = append(names, name)
	}
	for name := range codexDynamicMethods {
		names = append(names, name)
	}
	return names
}

func codexAppServerArgs() []string {
	return []string{
		"app-server",
		"--listen", "stdio://",
	}
}

func codexCommand() string {
	if command := strings.TrimSpace(os.Getenv("ANYCODE_CODEX_BIN")); command != "" {
		return command
	}
	const desktopCodex = "/Applications/Codex.app/Contents/Resources/codex"
	if _, err := os.Stat(desktopCodex); err == nil {
		return desktopCodex
	}
	return "codex"
}

func buildClaudeConfigPatch(params map[string]interface{}) ClaudeConfigPatch {
	patch := ClaudeConfigPatch{}
	if model, ok := getOptionalParamString(params, "model"); ok {
		if model == nil {
			value := "default"
			patch.Model = &value
		} else {
			patch.Model = model
		}
	}
	if effort, ok := getOptionalParamString(params, "effort"); ok {
		if effort == nil {
			value := "medium"
			patch.Effort = &value
		} else {
			patch.Effort = effort
		}
	}
	if mode, ok := getOptionalParamString(params, "permissionMode"); ok {
		if mode == nil {
			value := "default"
			patch.PermissionMode = &value
		} else {
			patch.PermissionMode = mode
		}
	}
	return patch
}

func handleFileChanges(params map[string]interface{}, projectRoot string, reverse bool) (interface{}, error) {
	changesRaw, ok := params["changes"]
	if !ok {
		return map[string]interface{}{"ok": true, "reverted": []string{}, "applied": []string{}}, nil
	}

	changesJSON, _ := json.Marshal(changesRaw)
	var rawChanges []struct {
		Path string                 `json:"path"`
		Kind map[string]interface{} `json:"kind"`
		Diff interface{}            `json:"diff"`
	}
	if err := json.Unmarshal(changesJSON, &rawChanges); err != nil {
		return nil, fmt.Errorf("invalid changes format: %w", err)
	}

	type fileChange struct {
		Path    string
		Kind    map[string]interface{}
		Diff    string
		OldText string
		NewText string
	}
	var changes []fileChange
	for _, rc := range rawChanges {
		c := fileChange{Path: rc.Path, Kind: rc.Kind}
		switch d := rc.Diff.(type) {
		case string:
			c.Diff = d
		case map[string]interface{}:
			if ot, ok := d["oldText"].(string); ok {
				c.OldText = ot
			}
			if nt, ok := d["newText"].(string); ok {
				c.NewText = nt
			}
		}
		changes = append(changes, c)
	}

	var processed []string
	var errors []string

	for _, change := range changes {
		resolvedPath, _, pathErr := resolveProjectPath(projectRoot, change.Path)
		if pathErr != nil {
			errors = append(errors, fmt.Sprintf("%s: %s", change.Path, pathErr.Error()))
			continue
		}

		kindType := ""
		if change.Kind != nil {
			if t, ok := change.Kind["type"].(string); ok {
				kindType = t
			}
		}
		if kindType == "" && change.Diff != "" {
			if strings.Contains(change.Diff, "@@") {
				kindType = "update"
			} else {
				kindType = "add"
			}
		}
		if kindType == "" {
			kindType = "add"
		}
		// Normalize kind variants across agents: Codex uses add/update/delete,
		// while claude-code-acp reports edits as "modify". Anything that isn't
		// an explicit add/delete is treated as an in-place update so undo/redo
		// reverts file edits regardless of the originating agent.
		switch kindType {
		case "add", "delete", "update":
		default:
			kindType = "update"
		}

		var err error
		if reverse {
			switch kindType {
			case "add":
				if _, serr := os.Stat(resolvedPath); serr == nil {
					err = os.Remove(resolvedPath)
				}
			case "update", "delete":
				if change.Diff != "" {
					unified := buildReversibleDiff(resolvedPath, change.Diff)
					err = gitApplyReverse(resolvedPath, unified)
				} else if change.OldText != "" || change.NewText != "" {
					content, rErr := os.ReadFile(resolvedPath)
					if rErr == nil {
						newContent := strings.Replace(string(content), change.NewText, change.OldText, -1)
						err = os.WriteFile(resolvedPath, []byte(newContent), 0644)
					} else {
						err = rErr
					}
				}
			}
		} else {
			switch kindType {
			case "add":
				if change.Diff != "" {
					lines := strings.Split(change.Diff, "\n")
					var content []string
					for _, l := range lines {
						if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
							content = append(content, l[1:])
						}
					}
					dir := filepath.Dir(resolvedPath)
					_ = os.MkdirAll(dir, 0755)
					err = os.WriteFile(resolvedPath, []byte(strings.Join(content, "\n")), 0644)
				} else if change.NewText != "" {
					dir := filepath.Dir(resolvedPath)
					_ = os.MkdirAll(dir, 0755)
					err = os.WriteFile(resolvedPath, []byte(change.NewText), 0644)
				}
			case "update":
				if change.Diff != "" {
					unified := buildReversibleDiff(resolvedPath, change.Diff)
					err = gitApplyForward(resolvedPath, unified)
				} else if change.OldText != "" || change.NewText != "" {
					content, rErr := os.ReadFile(resolvedPath)
					if rErr == nil {
						newContent := strings.Replace(string(content), change.OldText, change.NewText, -1)
						err = os.WriteFile(resolvedPath, []byte(newContent), 0644)
					} else {
						err = rErr
					}
				}
			case "delete":
				if _, serr := os.Stat(resolvedPath); serr == nil {
					err = os.Remove(resolvedPath)
				}
			}
		}

		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %s", change.Path, err.Error()))
		} else {
			processed = append(processed, change.Path)
		}
	}

	key := "reverted"
	if !reverse {
		key = "applied"
	}
	return map[string]interface{}{
		"ok":     len(errors) == 0,
		key:      processed,
		"errors": errors,
	}, nil
}
