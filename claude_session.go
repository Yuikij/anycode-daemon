package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func getClaudeProjectsRoot() string {
	if env := os.Getenv("CLAUDE_PROJECTS_DIR"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func findSessionFilePath(sessionId, cwd string) (string, error) {
	if sessionId == "" {
		return "", fmt.Errorf("sessionId is required")
	}
	if cwd != "" {
		primary := filepath.Join(claudeProjectDir(cwd), sessionId+".jsonl")
		if _, err := os.Stat(primary); err == nil {
			return primary, nil
		}
	}
	root := getClaudeProjectsRoot()
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, entry.Name(), sessionId+".jsonl")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("session file not found: %s", sessionId)
}

func readSessionFile(sessionId, cwd string) (map[string]interface{}, []map[string]interface{}, error) {
	primary := filepath.Join(claudeProjectDir(cwd), sessionId+".jsonl")
	if _, err := os.Stat(primary); err == nil {
		return parseClaudeSessionFile(primary, sessionId, true)
	}
	root := getClaudeProjectsRoot()
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
	clean := filepath.Clean(cwd)
	key := strings.ReplaceAll(clean, string(os.PathSeparator), "-")
	return filepath.Join(getClaudeProjectsRoot(), key)
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
	root := getClaudeProjectsRoot()
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

type ClaudeLogLine struct {
	Type           string          `json:"type"`
	SessionId      string          `json:"sessionId"`
	Cwd            string          `json:"cwd"`
	PermissionMode string          `json:"permissionMode"`
	Timestamp      json.RawMessage `json:"timestamp"`
	AiTitle        string          `json:"aiTitle"`
	LastPrompt     string          `json:"lastPrompt"`
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
		b := scanner.Bytes()
		var base ClaudeLogLine
		if err := json.Unmarshal(b, &base); err != nil {
			continue
		}

		if base.SessionId != "" {
			session["sessionId"] = base.SessionId
		}
		if base.Cwd != "" {
			session["cwd"] = base.Cwd
		}
		if base.PermissionMode != "" {
			session["permissionMode"] = base.PermissionMode
		}
		if len(base.Timestamp) > 0 {
			var tsStr string
			if err := json.Unmarshal(base.Timestamp, &tsStr); err == nil {
				if ts, ok := parseClaudeTimestamp(tsStr); ok {
					session["updatedAt"] = ts.UnixMilli()
					session["timeAgo"] = humanTimeAgo(ts)
				}
			}
		}

		switch base.Type {
		case "ai-title":
			if base.AiTitle != "" {
				title = base.AiTitle
			}
		case "last-prompt":
			if base.LastPrompt != "" {
				lastPrompt = base.LastPrompt
			}
		case "user":
			if !includeItems && firstUser != "" {
				continue
			}
			var obj map[string]interface{}
			if err := json.Unmarshal(b, &obj); err == nil {
				text := claudeMessageText(obj["message"])
				if text != "" && firstUser == "" {
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
			}
		case "assistant":
			if !includeItems {
				if strings.Contains(string(b), `"model":`) {
					var obj map[string]interface{}
					if err := json.Unmarshal(b, &obj); err == nil {
						if msg, ok := obj["message"].(map[string]interface{}); ok {
							if model, _ := msg["model"].(string); model != "" {
								session["model"] = model
							}
						}
					}
				}
				continue
			}
			var obj map[string]interface{}
			if err := json.Unmarshal(b, &obj); err == nil {
				msg, _ := obj["message"].(map[string]interface{})
				if model, _ := msg["model"].(string); model != "" {
					session["model"] = model
				}
				text, thoughts := claudeAssistantParts(msg)
				if !seenClaudeUUID(obj, seen) {
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
