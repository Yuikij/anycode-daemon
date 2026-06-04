package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

type eventJournal struct {
	mu         sync.Mutex
	db         *sql.DB
	maxEntries int
}

type persistedProject struct {
	Root         string
	Name         string
	LastOpenedAt int64
}

type persistedAgentState struct {
	Agent     string
	SessionID string
	ThreadID  string
	UpdatedAt int64
}

type persistedOperation struct {
	OperationID  string
	Agent        string
	SessionID    string
	ThreadID     string
	Status       string
	StartedAt    int64
	UpdatedAt    int64
	CompletedAt  int64
	ErrorMessage string
}

type persistedPermission struct {
	RequestID   string
	Agent       string
	SessionID   string
	ToolName    string
	Status      string
	PayloadJSON string
	CreatedAt   int64
	ResolvedAt  int64
}

func openEventJournal(path string, maxEntries int) (*eventJournal, error) {
	if maxEntries <= 0 {
		maxEntries = maxJournaledEvents
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	journal := &eventJournal{db: db, maxEntries: maxEntries}
	if err := journal.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return journal, nil
}

func (j *eventJournal) close() error {
	if j == nil || j.db == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	err := j.db.Close()
	j.db = nil
	return err
}

func (j *eventJournal) init() error {
	if _, err := j.db.Exec(`
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
CREATE TABLE IF NOT EXISTS daemon_state (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	project_root TEXT NOT NULL,
	project_generation INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS projects (
	root TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	last_opened_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_state (
	agent TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
	operation_id TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	session_id TEXT NOT NULL,
	thread_id TEXT NOT NULL,
	status TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	completed_at INTEGER NOT NULL,
	error_message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_operations_agent_updated ON operations(agent, updated_at DESC);
CREATE TABLE IF NOT EXISTS permissions (
	request_id TEXT PRIMARY KEY,
	agent TEXT NOT NULL,
	session_id TEXT NOT NULL,
	tool_name TEXT NOT NULL,
	status TEXT NOT NULL,
	payload_json TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	resolved_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_permissions_agent_created ON permissions(agent, created_at DESC);
CREATE TABLE IF NOT EXISTS events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	agent TEXT NOT NULL,
	method TEXT NOT NULL,
	params_json TEXT,
	project_id TEXT NOT NULL,
	project_generation INTEGER NOT NULL,
	operation_id TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_project_seq ON events(project_id, seq);
`); err != nil {
		return fmt.Errorf("init event journal: %w", err)
	}
	if err := j.ensureColumnExists("agent_state", "thread_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

func (j *eventJournal) ensureColumnExists(tableName, columnName, definition string) error {
	rows, err := j.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", tableName, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			return fmt.Errorf("scan %s schema: %w", tableName, err)
		}
		if strings.EqualFold(name, columnName) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s schema: %w", tableName, err)
	}
	if _, err := j.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, columnName, definition)); err != nil {
		return fmt.Errorf("alter %s add %s: %w", tableName, columnName, err)
	}
	return nil
}

func (j *eventJournal) upsertProject(root, name string, lastOpenedAt int64) error {
	if root == "" {
		return nil
	}
	if name == "" {
		name = filepath.Base(root)
	}
	if lastOpenedAt <= 0 {
		lastOpenedAt = nowUnixMilli()
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.db.Exec(
		`INSERT INTO projects (root, name, last_opened_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(root) DO UPDATE SET
		   name = excluded.name,
		   last_opened_at = excluded.last_opened_at`,
		root,
		name,
		lastOpenedAt,
	); err != nil {
		return fmt.Errorf("upsert project: %w", err)
	}
	return nil
}

func (j *eventJournal) listProjects() ([]persistedProject, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	rows, err := j.db.Query(`SELECT root, name, last_opened_at FROM projects ORDER BY last_opened_at DESC, name ASC, root ASC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	projects := make([]persistedProject, 0)
	for rows.Next() {
		var project persistedProject
		if err := rows.Scan(&project.Root, &project.Name, &project.LastOpenedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return projects, nil
}

func (j *eventJournal) saveAgentSession(agent, sessionID string, updatedAt int64) error {
	return j.saveAgentState(agent, sessionID, "", updatedAt)
}

func (j *eventJournal) saveAgentState(agent, sessionID, threadID string, updatedAt int64) error {
	if agent == "" {
		return nil
	}
	if updatedAt <= 0 {
		updatedAt = nowUnixMilli()
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.db.Exec(
		`INSERT INTO agent_state (agent, session_id, thread_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(agent) DO UPDATE SET
		   session_id = excluded.session_id,
		   thread_id = excluded.thread_id,
		   updated_at = excluded.updated_at`,
		agent,
		sessionID,
		threadID,
		updatedAt,
	); err != nil {
		return fmt.Errorf("save agent session: %w", err)
	}
	return nil
}

func (j *eventJournal) listAgentStates() ([]persistedAgentState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	rows, err := j.db.Query(`SELECT agent, session_id, thread_id, updated_at FROM agent_state ORDER BY agent ASC`)
	if err != nil {
		return nil, fmt.Errorf("list agent state: %w", err)
	}
	defer rows.Close()

	states := make([]persistedAgentState, 0)
	for rows.Next() {
		var state persistedAgentState
		if err := rows.Scan(&state.Agent, &state.SessionID, &state.ThreadID, &state.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan agent state: %w", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent state: %w", err)
	}
	return states, nil
}

func (j *eventJournal) upsertOperation(op persistedOperation) error {
	if op.Agent == "" || op.OperationID == "" {
		return nil
	}
	if op.Status == "" {
		op.Status = "unknown"
	}
	if op.UpdatedAt <= 0 {
		op.UpdatedAt = nowUnixMilli()
	}
	if op.StartedAt <= 0 {
		op.StartedAt = op.UpdatedAt
	}
	if isTerminalOperationStatus(op.Status) && op.CompletedAt <= 0 {
		op.CompletedAt = op.UpdatedAt
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.db.Exec(
		`INSERT INTO operations (operation_id, agent, session_id, thread_id, status, started_at, updated_at, completed_at, error_message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(operation_id) DO UPDATE SET
		   agent = excluded.agent,
		   session_id = excluded.session_id,
		   thread_id = excluded.thread_id,
		   status = excluded.status,
		   started_at = CASE
		     WHEN operations.started_at > 0 THEN operations.started_at
		     ELSE excluded.started_at
		   END,
		   updated_at = excluded.updated_at,
		   completed_at = CASE
		     WHEN excluded.completed_at > 0 THEN excluded.completed_at
		     ELSE operations.completed_at
		   END,
		   error_message = excluded.error_message`,
		op.OperationID,
		op.Agent,
		op.SessionID,
		op.ThreadID,
		op.Status,
		op.StartedAt,
		op.UpdatedAt,
		op.CompletedAt,
		op.ErrorMessage,
	); err != nil {
		return fmt.Errorf("upsert operation: %w", err)
	}
	return nil
}

func (j *eventJournal) latestOperation(agent string) (*persistedOperation, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	var op persistedOperation
	err := j.db.QueryRow(
		`SELECT operation_id, agent, session_id, thread_id, status, started_at, updated_at, completed_at, error_message
		 FROM operations
		 WHERE agent = ?
		 ORDER BY updated_at DESC, operation_id DESC
		 LIMIT 1`,
		agent,
	).Scan(&op.OperationID, &op.Agent, &op.SessionID, &op.ThreadID, &op.Status, &op.StartedAt, &op.UpdatedAt, &op.CompletedAt, &op.ErrorMessage)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest operation: %w", err)
	}
	return &op, nil
}

func (j *eventJournal) interruptRunningOperations(errorMessage string) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	updatedAt := nowUnixMilli()
	if _, err := j.db.Exec(
		`UPDATE operations
		 SET status = 'interrupted',
		     updated_at = ?,
		     completed_at = CASE WHEN completed_at <= 0 THEN ? ELSE completed_at END,
		     error_message = CASE
		       WHEN error_message = '' THEN ?
		       ELSE error_message
		     END
		 WHERE status = 'running'`,
		updatedAt,
		updatedAt,
		errorMessage,
	); err != nil {
		return fmt.Errorf("interrupt running operations: %w", err)
	}
	return nil
}

func (j *eventJournal) upsertPermission(permission persistedPermission) error {
	if permission.RequestID == "" || permission.Agent == "" {
		return nil
	}
	if permission.Status == "" {
		permission.Status = "pending"
	}
	if permission.CreatedAt <= 0 {
		permission.CreatedAt = nowUnixMilli()
	}
	if permission.PayloadJSON == "" {
		permission.PayloadJSON = "{}"
	}
	if permission.Status != "pending" && permission.ResolvedAt <= 0 {
		permission.ResolvedAt = nowUnixMilli()
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.db.Exec(
		`INSERT INTO permissions (request_id, agent, session_id, tool_name, status, payload_json, created_at, resolved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(request_id) DO UPDATE SET
		   agent = excluded.agent,
		   session_id = excluded.session_id,
		   tool_name = excluded.tool_name,
		   status = excluded.status,
		   payload_json = excluded.payload_json,
		   created_at = CASE
		     WHEN permissions.created_at > 0 THEN permissions.created_at
		     ELSE excluded.created_at
		   END,
		   resolved_at = excluded.resolved_at`,
		permission.RequestID,
		permission.Agent,
		permission.SessionID,
		permission.ToolName,
		permission.Status,
		permission.PayloadJSON,
		permission.CreatedAt,
		permission.ResolvedAt,
	); err != nil {
		return fmt.Errorf("upsert permission: %w", err)
	}
	return nil
}

func (j *eventJournal) latestPermission(agent string) (*persistedPermission, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	var permission persistedPermission
	err := j.db.QueryRow(
		`SELECT request_id, agent, session_id, tool_name, status, payload_json, created_at, resolved_at
		 FROM permissions
		 WHERE agent = ?
		 ORDER BY created_at DESC, request_id DESC
		 LIMIT 1`,
		agent,
	).Scan(&permission.RequestID, &permission.Agent, &permission.SessionID, &permission.ToolName, &permission.Status, &permission.PayloadJSON, &permission.CreatedAt, &permission.ResolvedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest permission: %w", err)
	}
	return &permission, nil
}

func (j *eventJournal) expirePendingPermissions(status string) error {
	if status == "" {
		status = "expired"
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	resolvedAt := nowUnixMilli()
	if _, err := j.db.Exec(
		`UPDATE permissions
		 SET status = ?,
		     resolved_at = CASE WHEN resolved_at <= 0 THEN ? ELSE resolved_at END
		 WHERE status = 'pending'`,
		status,
		resolvedAt,
	); err != nil {
		return fmt.Errorf("expire pending permissions: %w", err)
	}
	return nil
}

func isTerminalOperationStatus(status string) bool {
	switch status {
	case "completed", "failed", "interrupted", "aborted", "cancelled":
		return true
	default:
		return false
	}
}

func mergeProjectListings(scanned []map[string]string, persisted []persistedProject) []map[string]interface{} {
	type projectRow struct {
		Name         string
		Path         string
		LastOpenedAt int64
	}

	merged := make(map[string]projectRow, len(scanned)+len(persisted))
	for _, project := range scanned {
		path := project["path"]
		if path == "" {
			continue
		}
		merged[path] = projectRow{Name: project["name"], Path: path}
	}
	for _, project := range persisted {
		if project.Root == "" {
			continue
		}
		row := merged[project.Root]
		if project.Name != "" {
			row.Name = project.Name
		}
		row.Path = project.Root
		row.LastOpenedAt = project.LastOpenedAt
		merged[project.Root] = row
	}

	rows := make([]projectRow, 0, len(merged))
	for _, row := range merged {
		if row.Name == "" {
			row.Name = filepath.Base(row.Path)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].LastOpenedAt != rows[j].LastOpenedAt {
			return rows[i].LastOpenedAt > rows[j].LastOpenedAt
		}
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Path < rows[j].Path
	})

	projects := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, map[string]interface{}{
			"name":         row.Name,
			"path":         row.Path,
			"lastOpenedAt": row.LastOpenedAt,
		})
	}
	return projects
}

func (j *eventJournal) loadProjectState(defaultRoot string, defaultGeneration uint64) (string, uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	var (
		projectRoot       string
		projectGeneration int64
	)
	err := j.db.QueryRow(`SELECT project_root, project_generation FROM daemon_state WHERE id = 1`).Scan(&projectRoot, &projectGeneration)
	if err == nil {
		if projectRoot == "" {
			projectRoot = defaultRoot
		}
		if projectGeneration <= 0 {
			projectGeneration = int64(defaultGeneration)
		}
		return projectRoot, uint64(projectGeneration), nil
	}
	if err != sql.ErrNoRows {
		return "", 0, fmt.Errorf("load project state: %w", err)
	}
	if defaultGeneration == 0 {
		defaultGeneration = 1
	}
	if _, err := j.db.Exec(
		`INSERT INTO daemon_state (id, project_root, project_generation) VALUES (1, ?, ?)`,
		defaultRoot,
		defaultGeneration,
	); err != nil {
		return "", 0, fmt.Errorf("seed project state: %w", err)
	}
	return defaultRoot, defaultGeneration, nil
}

func (j *eventJournal) saveProjectState(projectRoot string, generation uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	if generation == 0 {
		generation = 1
	}
	if _, err := j.db.Exec(
		`INSERT INTO daemon_state (id, project_root, project_generation)
		 VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   project_root = excluded.project_root,
		   project_generation = excluded.project_generation`,
		projectRoot,
		generation,
	); err != nil {
		return fmt.Errorf("save project state: %w", err)
	}
	return nil
}

func (j *eventJournal) latestSeq() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()

	var latest int64
	if err := j.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&latest); err != nil || latest < 0 {
		return 0
	}
	return uint64(latest)
}

func (j *eventJournal) append(event eventEnvelope) (eventEnvelope, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	paramsJSON, err := marshalEventParams(event.Params)
	if err != nil {
		return eventEnvelope{}, err
	}

	tx, err := j.db.Begin()
	if err != nil {
		return eventEnvelope{}, fmt.Errorf("begin event tx: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	result, err := tx.Exec(
		`INSERT INTO events (agent, method, params_json, project_id, project_generation, operation_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.Agent,
		event.Method,
		paramsJSON,
		event.ProjectID,
		event.ProjectGeneration,
		event.OperationID,
		event.Timestamp,
	)
	if err != nil {
		return eventEnvelope{}, fmt.Errorf("insert event: %w", err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return eventEnvelope{}, fmt.Errorf("event seq: %w", err)
	}
	event.Seq = uint64(seq)

	if j.maxEntries > 0 {
		if _, err := tx.Exec(
			`DELETE FROM events
			 WHERE seq <= COALESCE((SELECT MAX(seq) - ? FROM events), 0)`,
			j.maxEntries,
		); err != nil {
			return eventEnvelope{}, fmt.Errorf("prune events: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return eventEnvelope{}, fmt.Errorf("commit event tx: %w", err)
	}
	tx = nil
	return event, nil
}

func (j *eventJournal) earliestSeq(projectID string) uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()

	query := `SELECT COALESCE(MIN(seq), 0) FROM events`
	args := []interface{}{}
	if projectID != "" {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}

	var earliest int64
	if err := j.db.QueryRow(query, args...).Scan(&earliest); err != nil || earliest < 0 {
		return 0
	}
	return uint64(earliest)
}

func (j *eventJournal) replay(afterSeq uint64, projectID string) ([]eventEnvelope, uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()

	query := `SELECT seq, agent, method, params_json, project_id, project_generation, operation_id, created_at
		FROM events
		WHERE seq > ?`
	args := []interface{}{afterSeq}
	if projectID != "" {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY seq ASC`

	rows, err := j.db.Query(query, args...)
	if err != nil {
		panic(fmt.Sprintf("query replay events: %v", err))
	}
	defer rows.Close()

	events := make([]eventEnvelope, 0)
	for rows.Next() {
		event, err := scanEventEnvelope(rows)
		if err != nil {
			panic(fmt.Sprintf("scan replay event: %v", err))
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		panic(fmt.Sprintf("iterate replay events: %v", err))
	}

	var latest int64
	if err := j.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&latest); err != nil || latest < 0 {
		return events, 0
	}
	return events, uint64(latest)
}

func marshalEventParams(params interface{}) (string, error) {
	if params == nil {
		return "", nil
	}
	data, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("marshal event params: %w", err)
	}
	return string(data), nil
}

func scanEventEnvelope(scanner interface {
	Scan(dest ...interface{}) error
}) (eventEnvelope, error) {
	var (
		seq               int64
		agent             string
		method            string
		paramsJSON        sql.NullString
		projectID         string
		projectGeneration int64
		operationID       sql.NullString
		timestamp         int64
	)
	if err := scanner.Scan(&seq, &agent, &method, &paramsJSON, &projectID, &projectGeneration, &operationID, &timestamp); err != nil {
		return eventEnvelope{}, err
	}
	event := eventEnvelope{
		Seq:               uint64(seq),
		Agent:             agent,
		Method:            method,
		ProjectID:         projectID,
		ProjectGeneration: uint64(projectGeneration),
		OperationID:       operationID.String,
		Timestamp:         timestamp,
	}
	if paramsJSON.Valid && paramsJSON.String != "" {
		var params interface{}
		if err := json.Unmarshal([]byte(paramsJSON.String), &params); err != nil {
			return eventEnvelope{}, fmt.Errorf("unmarshal event params: %w", err)
		}
		event.Params = params
	}
	return event, nil
}
