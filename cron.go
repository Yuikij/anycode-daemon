package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

type CronJobConfig struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Agent      string `json:"agent"`
	SessionID  string `json:"sessionId"`
	Prompt     string `json:"prompt"`
	Expression string `json:"expression"`
	Enabled    bool   `json:"enabled"`
	LastRun    int64  `json:"lastRun,omitempty"`
}

type CronManager struct {
	c       *cron.Cron
	server  *Server
	jobs    map[string]CronJobConfig
	entries map[string]cron.EntryID
	mu      sync.RWMutex
	cfgPath string
}

func NewCronManager(s *Server) *CronManager {
	return &CronManager{
		c:       cron.New(),
		server:  s,
		jobs:    make(map[string]CronJobConfig),
		entries: make(map[string]cron.EntryID),
	}
}

func (cm *CronManager) Start(projectRoot string) {
	cm.cfgPath = filepath.Join(projectRoot, ".anycode", "crons.json")
	_ = os.MkdirAll(filepath.Dir(cm.cfgPath), 0755)

	cm.loadJobs()
	cm.c.Start()
}

func (cm *CronManager) Stop() {
	if cm.c != nil {
		cm.c.Stop()
	}
}

func (cm *CronManager) loadJobs() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	data, err := os.ReadFile(cm.cfgPath)
	if err == nil {
		var loaded []CronJobConfig
		if err := json.Unmarshal(data, &loaded); err == nil {
			for _, job := range loaded {
				cm.jobs[job.ID] = job
				if job.Enabled {
					cm.scheduleJob(job)
				}
			}
		}
	}
}

func (cm *CronManager) saveJobs() {
	var list []CronJobConfig
	for _, j := range cm.jobs {
		list = append(list, j)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err == nil {
		_ = os.WriteFile(cm.cfgPath, data, 0644)
	}
}

func (cm *CronManager) scheduleJob(job CronJobConfig) {
	// Must be called with lock held
	if eid, ok := cm.entries[job.ID]; ok {
		cm.c.Remove(eid)
		delete(cm.entries, job.ID)
	}

	if !job.Enabled {
		return
	}

	eid, err := cm.c.AddFunc(job.Expression, func() {
		cm.executeJob(job.ID)
	})
	if err != nil {
		log.Printf("[cron] Error scheduling job %s: %v", job.ID, err)
		return
	}
	cm.entries[job.ID] = eid
}

func (cm *CronManager) executeJob(jobID string) {
	cm.mu.Lock()
	job, ok := cm.jobs[jobID]
	if !ok || !job.Enabled {
		cm.mu.Unlock()
		return
	}
	job.LastRun = time.Now().UnixMilli()
	cm.jobs[jobID] = job
	cm.saveJobs()
	cm.mu.Unlock()

	log.Printf("[cron] Executing job %s (%s) for agent %s", job.Name, job.ID, job.Agent)

	// Ensure the agent has a session
	sessionID := job.SessionID
	var err error

	switch job.Agent {
	case "gemini":
		if sessionID == "" {
			if !cm.server.gemini.IsRunning() {
				_ = cm.server.gemini.Start()
			}
			res, err := cm.server.gemini.NewSession(cm.server.projectRoot)
			if err == nil && res["sessionId"] != nil {
				sessionID = res["sessionId"].(string)
				cm.updateJobSession(jobID, sessionID)
			}
		}
		if sessionID != "" {
			_, err = cm.server.gemini.Prompt(sessionID, job.Prompt, nil)
		}

	case "claude":
		if sessionID == "" {
			if !cm.server.claude.IsRunning() {
				_ = cm.server.claude.Start(cm.server.projectRoot)
			}
			cm.server.claude.ClearSession()
			sessionID = cm.server.claude.SessionId()
			cm.updateJobSession(jobID, sessionID)
		}
		if sessionID != "" {
			if cm.server.claude.SessionId() != sessionID {
				_, _ = cm.server.claude.LoadSession(sessionID, cm.server.projectRoot)
			}
			err = cm.server.claude.Prompt(job.Prompt, nil)
		}

	case "codex":
		if sessionID == "" {
			if !cm.server.codex.IsRunning() {
				_ = cm.server.codex.Start(codexCommand(), codexAppServerArgs(), cm.server.projectRoot)
			}
			// Request new thread
			res, err := cm.server.codex.Send("thread/start", map[string]interface{}{"cwd": cm.server.projectRoot})
			if err == nil && res != nil {
				if rMap, ok := res.(map[string]interface{}); ok {
					if id, ok := rMap["id"].(string); ok {
						sessionID = id
						cm.updateJobSession(jobID, sessionID)
					}
				}
			}
		}
		if sessionID != "" {
			_, err = cm.server.codex.Send("turn/start", map[string]interface{}{
				"threadId": sessionID,
				"text":     job.Prompt,
			})
		}
	default:
		err = fmt.Errorf("unknown agent %s", job.Agent)
	}

	if err != nil {
		log.Printf("[cron] Error executing job %s: %v", jobID, err)
	}
}

func (cm *CronManager) updateJobSession(jobID, sessionID string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if job, ok := cm.jobs[jobID]; ok {
		job.SessionID = sessionID
		cm.jobs[jobID] = job
		cm.saveJobs()
	}
}

// RPC Methods

func (cm *CronManager) ListJobs() []CronJobConfig {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	var list []CronJobConfig
	for _, j := range cm.jobs {
		list = append(list, j)
	}
	return list
}

func (cm *CronManager) CreateJob(name, agent, sessionID, prompt, expression string, enabled bool) (CronJobConfig, error) {
	b := make([]byte, 4)
	rand.Read(b)
	id := hex.EncodeToString(b)

	job := CronJobConfig{
		ID:         id,
		Name:       name,
		Agent:      agent,
		SessionID:  sessionID,
		Prompt:     prompt,
		Expression: expression,
		Enabled:    enabled,
	}

	// Test expression
	_, err := cron.ParseStandard(expression)
	if err != nil {
		return job, fmt.Errorf("invalid cron expression: %w", err)
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.jobs[id] = job
	cm.saveJobs()
	cm.scheduleJob(job)

	// If the job was created with no session, and we are on Gemini or Claude,
	// maybe we pre-create it? Actually, wait, it's safer to create it on first run or right now.
	// We'll stick to creating it on first run as implemented in executeJob, but
	// user plan proposed creating it on creation.
	// Let's create it on creation to be consistent with the plan.
	if sessionID == "" {
		go cm.createSessionForJob(id, agent)
	}

	return job, nil
}

func (cm *CronManager) createSessionForJob(jobID, agent string) {
	// Runs in a goroutine
	sessionID := ""
	switch agent {
	case "gemini":
		if !cm.server.gemini.IsRunning() {
			_ = cm.server.gemini.Start()
		}
		res, err := cm.server.gemini.NewSession(cm.server.projectRoot)
		if err == nil && res["sessionId"] != nil {
			sessionID = res["sessionId"].(string)
		}
	case "claude":
		if !cm.server.claude.IsRunning() {
			_ = cm.server.claude.Start(cm.server.projectRoot)
		}
		cm.server.claude.ClearSession()
		sessionID = cm.server.claude.SessionId()
	case "codex":
		if !cm.server.codex.IsRunning() {
			_ = cm.server.codex.Start(codexCommand(), codexAppServerArgs(), cm.server.projectRoot)
		}
		res, err := cm.server.codex.Send("thread/start", map[string]interface{}{"cwd": cm.server.projectRoot})
		if err == nil && res != nil {
			if rMap, ok := res.(map[string]interface{}); ok {
				if id, ok := rMap["id"].(string); ok {
					sessionID = id
				}
			}
		}
	}
	if sessionID != "" {
		cm.updateJobSession(jobID, sessionID)
	}
}

func (cm *CronManager) UpdateJob(id, name, agent, sessionID, prompt, expression string, enabled bool) (CronJobConfig, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	job, ok := cm.jobs[id]
	if !ok {
		return job, fmt.Errorf("job not found")
	}

	if expression != "" {
		_, err := cron.ParseStandard(expression)
		if err != nil {
			return job, fmt.Errorf("invalid cron expression: %w", err)
		}
		job.Expression = expression
	}

	if name != "" {
		job.Name = name
	}
	if agent != "" {
		job.Agent = agent
	}
	// We allow sessionID to be empty, so we must always set it if passed
	job.SessionID = sessionID
	if prompt != "" {
		job.Prompt = prompt
	}
	job.Enabled = enabled

	cm.jobs[id] = job
	cm.saveJobs()
	cm.scheduleJob(job)

	if sessionID == "" && job.SessionID == "" {
		go cm.createSessionForJob(id, job.Agent)
	}

	return job, nil
}

func (cm *CronManager) DeleteJob(id string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if _, ok := cm.jobs[id]; !ok {
		return fmt.Errorf("job not found")
	}

	if eid, ok := cm.entries[id]; ok {
		cm.c.Remove(eid)
		delete(cm.entries, id)
	}
	delete(cm.jobs, id)
	cm.saveJobs()
	return nil
}
