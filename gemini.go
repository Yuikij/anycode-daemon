package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type GeminiBridge struct {
	mu             sync.Mutex
	agent          *AcpAgent
	cachedSessions string
	lastCacheTime  time.Time

	OnNotification func(method string, params interface{})
}

func (g *GeminiBridge) invalidateSessionCache() {
	g.mu.Lock()
	g.cachedSessions = ""
	g.mu.Unlock()
}

func NewGeminiBridge() *GeminiBridge {
	g := &GeminiBridge{
		agent: NewAcpAgent(AcpAgentConfig{
			ID:                     "gemini",
			Label:                  "Gemini",
			Command:                "gemini",
			Args:                   []string{"--acp", "--skip-trust"},
			Env:                    []string{"GEMINI_CLI_TRUST_WORKSPACE=true", "TERM=dumb"},
			AuthMethods:            []string{"oauth-personal", "gemini-api-key", "vertex-ai", "gateway"},
			AutoApprovePermissions: true,
			Capabilities: AcpCapabilities{
				CanSetModel: true,
				CanSetMode:  true,
			},
		}),
	}
	g.agent.OnNotification = g.emit
	return g
}

func (g *GeminiBridge) SetCwd(cwd string) { g.agent.SetCwd(cwd) }
func (g *GeminiBridge) IsRunning() bool   { return g.agent.IsRunning() }
func (g *GeminiBridge) Available() bool   { return g.agent.Available() }

func (g *GeminiBridge) CheckAvailable() bool {
	return g.agent.CheckAvailable()
}

func (g *GeminiBridge) Start() error {
	g.invalidateSessionCache()
	return g.agent.Start()
}

func (g *GeminiBridge) Stop() {
	g.agent.Stop()
}

func (g *GeminiBridge) NewSession(cwd string) (map[string]interface{}, error) {
	g.invalidateSessionCache()
	return g.agent.NewSession(cwd)
}

func (g *GeminiBridge) LoadSession(sessionId, cwd string) (map[string]interface{}, error) {
	return g.agent.LoadSession(sessionId, cwd)
}

func (g *GeminiBridge) Prompt(sessionId, text string, images []string) (map[string]interface{}, error) {
	g.invalidateSessionCache()
	return g.agent.Prompt(sessionId, text, images)
}

func (g *GeminiBridge) Cancel(sessionId string) error {
	return g.agent.Cancel(sessionId)
}

func (g *GeminiBridge) SetMode(sessionId, modeId string) error {
	return g.agent.SetMode(sessionId, modeId)
}

func (g *GeminiBridge) SetModel(sessionId, modelId string) error {
	return g.agent.SetModel(sessionId, modelId)
}

func (g *GeminiBridge) ListSessions() (string, error) {
	g.mu.Lock()
	cached := g.cachedSessions
	lastTime := g.lastCacheTime
	g.mu.Unlock()

	if cached != "" && time.Since(lastTime) < 15*time.Second {
		return cached, nil
	}

	res, err := runGeminiSessionList(g.agent.Cwd(), true)
	if err != nil || strings.TrimSpace(res) == "" {
		fallback, fallbackErr := runGeminiSessionList(g.agent.Cwd(), false)
		if strings.TrimSpace(fallback) != "" || fallbackErr == nil {
			res = fallback
			err = fallbackErr
		}
	}
	if err != nil {
		return res, err
	}

	g.mu.Lock()
	g.cachedSessions = res
	g.lastCacheTime = time.Now()
	g.mu.Unlock()

	return res, nil
}

func runGeminiSessionList(cwd string, preferJSON bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	args := []string{"--list-sessions"}
	if preferJSON {
		args = append(args, "--output-format", "json")
	}
	cmd := exec.CommandContext(ctx, "gemini", args...)
	cmd.Dir = cwd
	cmd.Env = append(cmd.Environ(), "TERM=dumb")

	out, err := cmd.CombinedOutput()
	res := string(out)
	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("gemini --list-sessions timed out")
	}
	if err != nil {
		msg := strings.TrimSpace(res)
		if msg == "" {
			return res, err
		}
		return res, fmt.Errorf("%w: %s", err, msg)
	}
	return res, nil
}

func (g *GeminiBridge) emit(method string, params interface{}) {
	if g.OnNotification != nil {
		g.OnNotification(method, params)
	}
}

var (
	sessionLineRe       = regexp.MustCompile(`^\s*(\d+)[\.\)]\s+(.+)$`)
	bracketedIDRe       = regexp.MustCompile(`\s+\[([A-Za-z0-9_.:-]{6,})\]\s*$`)
	trailingUUIDRe      = regexp.MustCompile(`\s+([A-Fa-f0-9]{8,}(?:-[A-Fa-f0-9]{4,}){2,})\s*$`)
	parenthesizedIDRe   = regexp.MustCompile(`\s+\(([A-Za-z0-9_.:-]{12,})\)\s*$`)
	parenthesizedTimeRe = regexp.MustCompile(`\s+\(([^()]*)\)\s*$`)
	trailingTimeAgoRe   = regexp.MustCompile(`(?i)\s+((?:just now)|today|yesterday|(?:\d+\s+(?:second|minute|hour|day|week|month|year)s?\s+ago))\s*$`)
	ansiEscapeRe        = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

func ParseGeminiSessionList(output string) []map[string]interface{} {
	if sessions := parseGeminiSessionJSON(output); len(sessions) > 0 {
		return sessions
	}

	sessions := []map[string]interface{}{}
	for _, line := range strings.Split(output, "\n") {
		m := sessionLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		title, timeAgo, sessionID := parseGeminiSessionLine(m[2])
		if sessionID == "" {
			continue
		}
		if title == "" {
			title = fmt.Sprintf("Session %d", idx)
		}
		sessions = append(sessions, map[string]interface{}{
			"index":     idx,
			"title":     title,
			"timeAgo":   timeAgo,
			"sessionId": sessionID,
			"ageMs":     parseGeminiAgeMs(timeAgo),
		})
	}
	return sessions
}

func parseGeminiSessionJSON(output string) []map[string]interface{} {
	trimmed := strings.TrimSpace(ansiEscapeRe.ReplaceAllString(output, ""))
	if trimmed == "" {
		return nil
	}

	var raw interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		start := strings.IndexAny(trimmed, "[{")
		endObj := strings.LastIndex(trimmed, "}")
		endArr := strings.LastIndex(trimmed, "]")
		end := endObj
		if endArr > end {
			end = endArr
		}
		if start < 0 || end <= start {
			return nil
		}
		if err := json.Unmarshal([]byte(trimmed[start:end+1]), &raw); err != nil {
			return nil
		}
	}

	items := []interface{}{}
	switch v := raw.(type) {
	case []interface{}:
		items = v
	case map[string]interface{}:
		for _, key := range []string{"sessions", "items", "data"} {
			if arr, ok := v[key].([]interface{}); ok {
				items = arr
				break
			}
		}
	}

	sessions := []map[string]interface{}{}
	for i, item := range items {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if session := normalizeGeminiSessionObject(obj, i+1); session != nil {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

func normalizeGeminiSessionObject(obj map[string]interface{}, fallbackIndex int) map[string]interface{} {
	sessionID := firstString(obj, "sessionId", "sessionID", "session_id", "id")
	if sessionID == "" {
		return nil
	}

	title := firstString(obj, "title", "preview", "description", "name")
	if title == "" {
		title = sessionID
	}
	timeAgo := firstString(obj, "timeAgo", "time_ago", "updatedAt", "updated_at", "createdAt", "created_at", "lastUsed", "last_used")
	index := firstInt(obj, fallbackIndex, "index", "idx")

	return map[string]interface{}{
		"index":     index,
		"title":     title,
		"timeAgo":   timeAgo,
		"sessionId": sessionID,
		"ageMs":     parseGeminiAgeMs(timeAgo),
	}
}

func parseGeminiSessionLine(line string) (string, string, string) {
	rest := strings.TrimSpace(line)
	sessionID := ""

	if m := bracketedIDRe.FindStringSubmatch(rest); m != nil {
		sessionID = m[1]
		rest = strings.TrimSpace(bracketedIDRe.ReplaceAllString(rest, ""))
	} else if m := trailingUUIDRe.FindStringSubmatch(rest); m != nil {
		sessionID = m[1]
		rest = strings.TrimSpace(trailingUUIDRe.ReplaceAllString(rest, ""))
	} else if m := parenthesizedIDRe.FindStringSubmatch(rest); m != nil && looksLikeSessionID(m[1]) {
		sessionID = m[1]
		rest = strings.TrimSpace(parenthesizedIDRe.ReplaceAllString(rest, ""))
	}

	timeAgo := ""
	if m := parenthesizedTimeRe.FindStringSubmatch(rest); m != nil {
		candidate := strings.TrimSpace(m[1])
		if looksLikeTimeAgo(candidate) {
			timeAgo = candidate
			rest = strings.TrimSpace(parenthesizedTimeRe.ReplaceAllString(rest, ""))
		}
	}
	if timeAgo == "" {
		if m := trailingTimeAgoRe.FindStringSubmatch(rest); m != nil {
			timeAgo = strings.TrimSpace(m[1])
			rest = strings.TrimSpace(trailingTimeAgoRe.ReplaceAllString(rest, ""))
		}
	}

	return rest, timeAgo, sessionID
}

func looksLikeTimeAgo(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	return value == "just now" || value == "today" || value == "yesterday" || trailingTimeAgoRe.MatchString(" "+value)
}

func parseGeminiAgeMs(timeAgo string) int64 {
	value := strings.TrimSpace(strings.ToLower(timeAgo))
	switch value {
	case "", "just now", "today":
		return 0
	case "yesterday":
		return int64(24 * time.Hour / time.Millisecond)
	}

	m := regexp.MustCompile(`^(\d+)\s+(second|minute|hour|day|week|month|year)s?\s+ago$`).FindStringSubmatch(value)
	if m == nil {
		return 0
	}
	amount, _ := strconv.Atoi(m[1])
	unit := m[2]
	durations := map[string]time.Duration{
		"second": time.Second,
		"minute": time.Minute,
		"hour":   time.Hour,
		"day":    24 * time.Hour,
		"week":   7 * 24 * time.Hour,
		"month":  30 * 24 * time.Hour,
		"year":   365 * 24 * time.Hour,
	}
	return int64(time.Duration(amount) * durations[unit] / time.Millisecond)
}

func looksLikeSessionID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 12 {
		return false
	}
	return strings.ContainsAny(value, "-_:") || regexp.MustCompile(`^[A-Fa-f0-9]+$`).MatchString(value)
}
