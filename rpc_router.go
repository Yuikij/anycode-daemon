package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) recordCodexEvent(method string, params interface{}) {
	s.codexMu.Lock()
	defer s.codexMu.Unlock()

	switch method {
	case "turn/started":
		s.codexTurnRunning = true
		s.codexEvents = s.codexEvents[:0]
	case "turn/completed", "turn/failed", "turn/aborted", "turn/interrupted":
		s.codexTurnRunning = false
	case "thread/started":
		if id := extractThreadID(params); id != "" {
			s.codexThreadID = id
		}
	}
	if id := extractThreadID(params); id != "" {
		s.codexThreadID = id
	}

	s.codexEvents = append(s.codexEvents, cachedNotification{
		Method: method, Params: params, Time: time.Now().UnixMilli(),
	})
	if len(s.codexEvents) > maxCachedNotifications {
		s.codexEvents = s.codexEvents[len(s.codexEvents)-maxCachedNotifications:]
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
	s.projectRoot = newRoot
	s.cron.Stop()
	s.cron.Start(newRoot)
	log.Printf("[server] switched to project: %s", newRoot)
}

func (s *Server) handleRequest(req RpcRequest, client *wsClient) (interface{}, error) {
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
	s.routes["daemon.configRead"] = s.handleDaemonConfigRead
	s.routes["daemon.configWrite"] = s.handleDaemonConfigWrite
	s.routes["share.create"] = s.handleShareCreate
	s.routes["fs.browse"] = s.handleFsBrowse
	s.routes["fs.readAbsolute"] = s.handleFsReadAbsolute
	s.routes["fs.writeAbsolute"] = s.handleFsWriteAbsolute
	s.routes["project.info"] = s.handleProjectInfo
	s.routes["project.open"] = s.handleProjectOpen
	s.routes["project.list"] = s.handleProjectList
	s.routes["fs.list"] = s.handleFsList
	s.routes["fs.tree"] = s.handleFsTree
	s.routes["fs.read"] = s.handleFsRead
	s.routes["git.status"] = s.handleGitStatus
	s.routes["git.diff"] = s.handleGitDiff
	s.routes["git.diff.staged"] = s.handleGitDiffStaged
	s.routes["git.log"] = s.handleGitLog
	s.routes["git.diff.commit"] = s.handleGitDiffCommit
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
	rpcMethod := strings.TrimPrefix(req.Method, "codex.")
	rpcMethod = strings.Replace(rpcMethod, "thread", "thread/", 1)
	rpcMethod = strings.Replace(rpcMethod, "turn", "turn/", 1)
	rpcMethod = strings.Replace(rpcMethod, "model", "model/", 1)
	rpcMethod = strings.Replace(rpcMethod, "config", "config/", 1)
	// Reconstruct the correct method names
	rpcMethod = codexMethodMap(req.Method)
	if params == nil {
		return s.codex.Send(rpcMethod, map[string]interface{}{})
	}
	return s.codex.Send(rpcMethod, params)

}

func codexMethodMap(method string) string {
	m := map[string]string{
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
	if v, ok := m[method]; ok {
		return v
	}
	return method
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

func buildClaudeConfigResponse(claude *ClaudeBridge) map[string]interface{} {
	config := claude.ConfigSnapshot()
	caps := claude.Capabilities()
	return map[string]interface{}{
		"config":         config,
		"capabilities":   caps,
		"model":          config["model"],
		"effort":         config["effort"],
		"permissionMode": config["permissionMode"],
		"sessionModel":   claude.SessionModel(),
		"sessionMode":    claude.SessionMode(),
	}
}

func handleFileChanges(params map[string]interface{}, reverse bool) (interface{}, error) {
	changesRaw, ok := params["changes"]
	if !ok {
		return map[string]interface{}{"ok": true, "reverted": []string{}, "applied": []string{}}, nil
	}

	changesJSON, _ := json.Marshal(changesRaw)
	var changes []struct {
		Path string                 `json:"path"`
		Kind map[string]interface{} `json:"kind"`
		Diff string                 `json:"diff"`
	}
	if err := json.Unmarshal(changesJSON, &changes); err != nil {
		return nil, fmt.Errorf("invalid changes format: %w", err)
	}

	var processed []string
	var errors []string

	for _, change := range changes {
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
				if _, serr := os.Stat(change.Path); serr == nil {
					err = os.Remove(change.Path)
				}
			case "update", "delete":
				if change.Diff != "" {
					unified := buildReversibleDiff(change.Path, change.Diff)
					err = gitApplyReverse(change.Path, unified)
				}
			}
		} else {
			switch kindType {
			case "add":
				lines := strings.Split(change.Diff, "\n")
				var content []string
				for _, l := range lines {
					if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
						content = append(content, l[1:])
					}
				}
				dir := filepath.Dir(change.Path)
				_ = os.MkdirAll(dir, 0755)
				err = os.WriteFile(change.Path, []byte(strings.Join(content, "\n")), 0644)
			case "update":
				if change.Diff != "" {
					unified := buildReversibleDiff(change.Path, change.Diff)
					err = gitApplyForward(change.Path, unified)
				}
			case "delete":
				if _, serr := os.Stat(change.Path); serr == nil {
					err = os.Remove(change.Path)
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
