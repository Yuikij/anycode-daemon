package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type protocolMethodCatalogEntry struct {
	Name        string                `json:"name"`
	Group       string                `json:"group"`
	Clients     []string              `json:"clients"`
	Dynamic     bool                  `json:"dynamic"`
	Passthrough bool                  `json:"passthrough"`
	Deprecated  bool                  `json:"deprecated"`
	Summary     string                `json:"summary"`
	Params      []protocolMethodParam `json:"params"`
}

type protocolMethodParam struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

type protocolMethodCatalog struct {
	Version int                          `json:"version"`
	Methods []protocolMethodCatalogEntry `json:"methods"`
}

func loadMethodCatalog(t *testing.T) protocolMethodCatalog {
	t.Helper()
	path := filepath.Join("..", "protocol", "methods.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read method catalog: %v", err)
	}
	var catalog protocolMethodCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("decode method catalog: %v", err)
	}
	return catalog
}

// TestProtocolMethodCatalogMatchesDaemonRoutes is the cross-end drift safety
// net described in 协议对齐与稳定性收口计划.md §1.2/阶段一. The daemon's actual
// served method set (static routes + named dynamic codex passthroughs) must
// exactly equal protocol/methods.json. Adding, removing, or renaming a method
// on the daemon without updating the catalog turns this test red — and the web
// and iOS contract tests validate their own usage against the same file.
func TestProtocolMethodCatalogMatchesDaemonRoutes(t *testing.T) {
	catalog := loadMethodCatalog(t)
	if catalog.Version < 1 {
		t.Fatalf("expected catalog version >= 1, got %d", catalog.Version)
	}

	catalogSet := make(map[string]bool, len(catalog.Methods))
	for _, m := range catalog.Methods {
		if m.Name == "" {
			t.Fatalf("catalog entry missing name: %#v", m)
		}
		if m.Group == "" {
			t.Fatalf("catalog entry %q missing group", m.Name)
		}
		if catalogSet[m.Name] {
			t.Fatalf("duplicate catalog entry: %q", m.Name)
		}
		catalogSet[m.Name] = true
	}

	server := newTestServer(t, ".")
	daemonSet := make(map[string]bool)
	for _, name := range server.registeredMethodNames() {
		daemonSet[name] = true
	}

	var undocumented []string
	for name := range daemonSet {
		if !catalogSet[name] {
			undocumented = append(undocumented, name)
		}
	}
	var missing []string
	for name := range catalogSet {
		if !daemonSet[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(missing)

	if len(undocumented) > 0 {
		t.Errorf("daemon exposes methods absent from protocol/methods.json (add them to the catalog): %v", undocumented)
	}
	if len(missing) > 0 {
		t.Errorf("protocol/methods.json lists methods the daemon does not serve (update the catalog or the routes): %v", missing)
	}
}

// TestClientHelloAdvertisesMethodCatalog verifies the capability discovery path
// (协议对齐与稳定性收口计划.md §1.4/阶段二·4): when a client negotiates the
// `method.catalog` capability, client.hello advertises the daemon's full RPC
// method set so the client can gate features instead of blindly calling
// unsupported methods. Clients that do not request it get no `methods` field.
func TestClientHelloAdvertisesMethodCatalog(t *testing.T) {
	server := newTestServer(t, ".")

	withCatalog := json.RawMessage([]byte(`{"clientId":"web-test","capabilities":["client.hello","method.catalog"]}`))
	result, err := server.handleClientHello(RpcRequest{Method: "client.hello", Params: &withCatalog}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := result.(map[string]interface{})
	methods, ok := payload["methods"].([]string)
	if !ok {
		t.Fatalf("expected methods catalog in hello, got %#v", payload["methods"])
	}
	advertised := make(map[string]bool, len(methods))
	for _, m := range methods {
		advertised[m] = true
	}
	for _, expected := range []string{"client.hello", "claude.prompt", "codex.threadStart", "gemini.start"} {
		if !advertised[expected] {
			t.Fatalf("expected advertised method %q, got %#v", expected, methods)
		}
	}
	if len(methods) != len(server.registeredMethodNames()) {
		t.Fatalf("expected advertised catalog to match registered methods, got %d vs %d", len(methods), len(server.registeredMethodNames()))
	}

	without := json.RawMessage([]byte(`{"clientId":"web-test","capabilities":["client.hello"]}`))
	result, err = server.handleClientHello(RpcRequest{Method: "client.hello", Params: &without}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.(map[string]interface{})["methods"]; ok {
		t.Fatalf("did not expect methods catalog when capability not negotiated")
	}
}

// TestProtocolMethodCatalogDynamicFlags verifies the `dynamic` flag in the
// catalog stays aligned with how the daemon dispatches codex methods: dynamic
// entries must be served through handleCodexDynamic (codexDynamicMethods),
// while non-dynamic entries must be statically registered routes.
func TestProtocolMethodCatalogDynamicFlags(t *testing.T) {
	catalog := loadMethodCatalog(t)
	server := newTestServer(t, ".")

	for _, m := range catalog.Methods {
		_, isDynamic := codexDynamicMethods[m.Name]
		_, isRoute := server.routes[m.Name]
		if m.Dynamic {
			if !isDynamic {
				t.Errorf("catalog marks %q dynamic but it is not a codex dynamic method", m.Name)
			}
		} else {
			if !isRoute {
				t.Errorf("catalog marks %q static but it is not a registered route", m.Name)
			}
		}
	}
}
