package main

import (
	"encoding/json"
	"fmt"
)

// decodeParams unmarshals an RPC request's params into a typed struct T.
//
// This is the typed alternative to digging fields out of
// map[string]interface{} via getParamString/getParamInt/... It gives handlers
// compile-time field names and types, and surfaces JSON type mismatches (e.g.
// a number where a string was expected) as a clear boundary error instead of
// silently coercing to a zero value. Undeclared fields in the payload are
// ignored, matching the lenient param-validation policy in protocol_params.go.
//
// New handlers — and any handler being touched — should prefer decodeParams
// over raw map access. See 协议对齐与稳定性收口计划.md §阶段一·2 (弱类型收口).
func decodeParams[T any](req RpcRequest) (T, error) {
	var out T
	if req.Params == nil || len(*req.Params) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(*req.Params, &out); err != nil {
		return out, fmt.Errorf("invalid params: %w", err)
	}
	return out, nil
}

// ---- Typed request param structs ----
//
// These mirror the per-method `params` declared in protocol/methods.json. The
// catalog stays the cross-language single source of truth; these structs are
// the daemon-side typed view. A drift test (protocol_params_test.go) asserts
// every json field here is declared in the catalog so the two can't diverge.

type fsBrowseParams struct {
	Path       string `json:"path"`
	ShowHidden bool   `json:"showHidden"`
}

type fsReadAbsoluteParams struct {
	Path string `json:"path"`
}

type fsWriteAbsoluteParams struct {
	Path                      string `json:"path"`
	Content                   string `json:"content"`
	ProjectID                 string `json:"projectId"`
	ExpectedProjectGeneration int    `json:"expectedProjectGeneration"`
}

type fsTreeParams struct {
	Path  string `json:"path"`
	Depth *int   `json:"depth"`
}

type projectOpenParams struct {
	Path string `json:"path"`
}

type daemonConfigWriteParams struct {
	Proxy string `json:"proxy"`
}

type shareCreateParams struct {
	HTML string `json:"html"`
}

type cronCreateParams struct {
	Name       string `json:"name"`
	Agent      string `json:"agent"`
	SessionID  string `json:"sessionId"`
	Prompt     string `json:"prompt"`
	Expression string `json:"expression"`
	Enabled    bool   `json:"enabled"`
}

type cronUpdateParams struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Agent      string `json:"agent"`
	SessionID  string `json:"sessionId"`
	Prompt     string `json:"prompt"`
	Expression string `json:"expression"`
	Enabled    bool   `json:"enabled"`
}

type cronDeleteParams struct {
	ID string `json:"id"`
}

// ---- Project-scoped agent + git request structs ----
//
// These embed projectScope (projectId/expectedProjectGeneration/cwd) and are
// resolved via s.resolveScope, replacing the map-based normalizeProjectScopedParams
// / resolveProjectRequestContext digging in the agent and git handlers.

type gitStatusParams struct {
	projectScope
}

type gitDiffParams struct {
	projectScope
	Path string `json:"path"`
}

type gitLogParams struct {
	projectScope
	Count *int `json:"count"`
}

type codexStartParams struct {
	projectScope
}

type codexRespondParams struct {
	RequestID interface{} `json:"requestId"`
	Result    interface{} `json:"result"`
}

type claudeStartParams struct {
	projectScope
}

type claudeSessionListParams struct {
	projectScope
}

type claudeNewSessionParams struct {
	projectScope
}

type claudeLoadSessionParams struct {
	projectScope
	SessionID string `json:"sessionId"`
}

type claudeSessionDeleteParams struct {
	projectScope
	SessionID string `json:"sessionId"`
}

type claudeSessionRenameParams struct {
	projectScope
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
}

type claudePromptParams struct {
	projectScope
	Prompt string   `json:"prompt"`
	Text   string   `json:"text"`
	Images []string `json:"images"`
}

type claudePermissionRespondParams struct {
	RequestID string `json:"requestId"`
	OptionID  string `json:"optionId"`
	Cancelled bool   `json:"cancelled"`
}

type geminiStartParams struct {
	projectScope
}

type geminiNewSessionParams struct {
	projectScope
}

type geminiSessionListParams struct {
	projectScope
}

type geminiLoadSessionParams struct {
	projectScope
	SessionID string `json:"sessionId"`
}

type geminiPromptParams struct {
	projectScope
	SessionID string   `json:"sessionId"`
	Prompt    string   `json:"prompt"`
	Text      string   `json:"text"`
	Images    []string `json:"images"`
}

type geminiCancelParams struct {
	SessionID string `json:"sessionId"`
}

type geminiSetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type geminiSetModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}
