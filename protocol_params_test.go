package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testValidator(t *testing.T) *protocolValidator {
	t.Helper()
	// Validate against the source-of-truth file directly (not the embedded copy)
	// so these assertions reflect protocol/methods.json even if the embedded
	// copy is momentarily stale; TestEmbeddedCatalogMatchesSource guards drift.
	data, err := os.ReadFile(filepath.Join("..", "protocol", "methods.json"))
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	v, err := newProtocolValidatorFromBytes(data)
	if err != nil {
		t.Fatalf("load validator: %v", err)
	}
	return v
}

// TestEmbeddedCatalogMatchesSource ensures the catalog compiled into the daemon
// (daemon-go/methods.json, embedded for runtime param validation) is a verbatim
// copy of the single source of truth, protocol/methods.json. build.sh re-syncs
// it; this test fails loudly if someone edits the source without rebuilding.
func TestEmbeddedCatalogMatchesSource(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "protocol", "methods.json"))
	if err != nil {
		t.Fatalf("read source catalog: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(source), bytes.TrimSpace(embeddedMethodsCatalog)) {
		t.Fatalf("daemon-go/methods.json is stale — run ./build.sh (or cp ../protocol/methods.json ./methods.json) to re-sync it with protocol/methods.json")
	}
}

func rawParams(t *testing.T, payload string) *json.RawMessage {
	t.Helper()
	raw := json.RawMessage([]byte(payload))
	return &raw
}

// TestProtocolParamValidatorRejectsInvalid covers the daemon boundary
// validation described in 协议对齐与稳定性收口计划.md §阶段一·2: required params
// must be present and declared params must match their declared type.
func TestProtocolParamValidatorRejectsInvalid(t *testing.T) {
	v := testValidator(t)
	cases := []struct {
		name    string
		method  string
		payload string
	}{
		{"missing required string", "share.create", `{}`},
		{"empty required string", "fs.readAbsolute", `{"path":""}`},
		{"wrong type for required string", "share.create", `{"html":123}`},
		{"missing one of two required", "gemini.setMode", `{"sessionId":"s"}`},
		{"wrong type for number", "git.log", `{"count":"20"}`},
		{"wrong type for string[]", "claude.prompt", `{"images":[1,2,3]}`},
		{"missing required any", "codex.respond", `{}`},
		{"missing required cron field", "cron.create", `{"name":"a","agent":"claude","prompt":"hi"}`},
		{"params not an object", "fs.readAbsolute", `"oops"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := v.validate(tc.method, rawParams(t, tc.payload)); err == nil {
				t.Fatalf("expected validation error for %s with %s", tc.method, tc.payload)
			}
		})
	}
}

func TestProtocolParamValidatorAcceptsValid(t *testing.T) {
	v := testValidator(t)
	cases := []struct {
		name    string
		method  string
		payload string
	}{
		{"required present", "fs.readAbsolute", `{"path":"/tmp/x"}`},
		{"optional only", "claude.prompt", `{"prompt":"hi","images":["a","b"]}`},
		{"no params declared", "daemon.version", `{}`},
		{"nil params for no-required", "claude.cancel", ``},
		{"passthrough ignores params", "codex.threadStart", `{"anything":true,"n":5}`},
		{"number ok", "git.log", `{"count":20}`},
		{"capabilities array", "client.hello", `{"capabilities":["client.hello"],"lastSeq":3}`},
		{"gemini prompt without text is allowed at boundary", "gemini.prompt", `{"sessionId":"s"}`},
		{"unknown method not validated", "totally.unknown", `{"x":1}`},
		{"cron create full", "cron.create", `{"name":"n","agent":"claude","prompt":"p","expression":"* * * * *","enabled":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw *json.RawMessage
			if tc.payload != "" {
				raw = rawParams(t, tc.payload)
			}
			if err := v.validate(tc.method, raw); err != nil {
				t.Fatalf("unexpected validation error for %s: %v", tc.method, err)
			}
		})
	}
}

// TestTypedRequestStructsMatchCatalog guards against drift between the daemon's
// typed request structs (request_params.go) and the cross-language single source
// of truth (protocol/methods.json). Every json field a typed struct decodes must
// be a param the catalog declares for that method — otherwise the daemon would
// silently read a field no other end knows about.
func TestTypedRequestStructsMatchCatalog(t *testing.T) {
	catalog := loadMethodCatalog(t)
	declared := make(map[string]map[string]bool, len(catalog.Methods))
	for _, m := range catalog.Methods {
		set := make(map[string]bool, len(m.Params))
		for _, p := range m.Params {
			set[p.Name] = true
		}
		declared[m.Name] = set
	}

	bindings := []struct {
		method string
		typ    reflect.Type
	}{
		{"fs.browse", reflect.TypeOf(fsBrowseParams{})},
		{"fs.readAbsolute", reflect.TypeOf(fsReadAbsoluteParams{})},
		{"fs.writeAbsolute", reflect.TypeOf(fsWriteAbsoluteParams{})},
		{"fs.tree", reflect.TypeOf(fsTreeParams{})},
		{"project.open", reflect.TypeOf(projectOpenParams{})},
		{"daemon.configWrite", reflect.TypeOf(daemonConfigWriteParams{})},
		{"share.create", reflect.TypeOf(shareCreateParams{})},
		{"cron.create", reflect.TypeOf(cronCreateParams{})},
		{"cron.update", reflect.TypeOf(cronUpdateParams{})},
		{"cron.delete", reflect.TypeOf(cronDeleteParams{})},
		{"git.status", reflect.TypeOf(gitStatusParams{})},
		{"git.diff", reflect.TypeOf(gitDiffParams{})},
		{"git.log", reflect.TypeOf(gitLogParams{})},
		{"codex.start", reflect.TypeOf(codexStartParams{})},
		{"codex.respond", reflect.TypeOf(codexRespondParams{})},
		{"claude.start", reflect.TypeOf(claudeStartParams{})},
		{"claude.sessionList", reflect.TypeOf(claudeSessionListParams{})},
		{"claude.newSession", reflect.TypeOf(claudeNewSessionParams{})},
		{"claude.loadSession", reflect.TypeOf(claudeLoadSessionParams{})},
		{"claude.sessionDelete", reflect.TypeOf(claudeSessionDeleteParams{})},
		{"claude.sessionRename", reflect.TypeOf(claudeSessionRenameParams{})},
		{"claude.prompt", reflect.TypeOf(claudePromptParams{})},
		{"claude.permission/respond", reflect.TypeOf(claudePermissionRespondParams{})},
		{"gemini.start", reflect.TypeOf(geminiStartParams{})},
		{"gemini.newSession", reflect.TypeOf(geminiNewSessionParams{})},
		{"gemini.sessionList", reflect.TypeOf(geminiSessionListParams{})},
		{"gemini.loadSession", reflect.TypeOf(geminiLoadSessionParams{})},
		{"gemini.prompt", reflect.TypeOf(geminiPromptParams{})},
		{"gemini.cancel", reflect.TypeOf(geminiCancelParams{})},
		{"gemini.setMode", reflect.TypeOf(geminiSetModeParams{})},
		{"gemini.setModel", reflect.TypeOf(geminiSetModelParams{})},
	}

	for _, b := range bindings {
		set, ok := declared[b.method]
		if !ok {
			t.Errorf("typed struct bound to %q which is not in the catalog", b.method)
			continue
		}
		for _, name := range jsonFieldNames(b.typ) {
			if !set[name] {
				t.Errorf("typed struct %s decodes field %q that protocol/methods.json does not declare for %s", b.typ.Name(), name, b.method)
			}
		}
	}
}

// jsonFieldNames returns the JSON field names a struct type decodes, flattening
// anonymous embedded structs (e.g. projectScope) so promoted fields are checked.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			names = append(names, jsonFieldNames(field.Type)...)
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// TestEveryRoutedMethodHasParamSpec asserts every statically-routed (non
// passthrough) method declares a `params` array in the catalog, so boundary
// validation has something to check and the contract stays complete.
func TestEveryRoutedMethodHasParamSpec(t *testing.T) {
	catalog := loadMethodCatalog(t)
	server := newTestServer(t, ".")
	for _, m := range catalog.Methods {
		if m.Dynamic || m.Passthrough {
			continue
		}
		if _, isRoute := server.routes[m.Name]; !isRoute {
			continue
		}
		if m.Params == nil {
			t.Errorf("routed method %q is missing a params spec in protocol/methods.json", m.Name)
		}
	}
}
