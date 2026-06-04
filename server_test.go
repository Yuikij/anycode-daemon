package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRewriteTextResponseRewritesInlineCSSRootRefsInHTML(t *testing.T) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:3001", Path: "/"}
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body: io.NopCloser(strings.NewReader(
			`<html><head><style>@font-face{src:url(/_next/static/media/font.woff2)}</style></head></html>`,
		)),
	}

	if err := rewriteTextResponse(resp, target, "anycodeapp.com", "https"); err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	got := string(body)
	want := `url(/__anycode_proxy/http/127.0.0.1:3001/_next/static/media/font.woff2)`
	if !strings.Contains(got, want) {
		t.Fatalf("expected inline CSS URL to be proxied\nwant contains: %s\ngot: %s", want, got)
	}
}

func TestRelayProxyOpenDoesNotExposeDaemonTokenCookie(t *testing.T) {
	server := NewServer(0, ".", "secret-token")
	result, err := server.handleRelayProxyFetch(map[string]interface{}{
		"method": "GET",
		"path":   "/__anycode_proxy/open?url=http%3A%2F%2F127.0.0.1%3A3001%2F",
	})
	if err != nil {
		t.Fatal(err)
	}

	headers := result.(map[string]interface{})["headers"].(map[string][]string)
	for _, value := range headers["Set-Cookie"] {
		if strings.HasPrefix(value, proxyTokenCookie+"=") {
			t.Fatalf("relay response exposed daemon token cookie: %s", value)
		}
	}
}

func TestStripProxyCookiesRemovesAnyCodePlatformCookies(t *testing.T) {
	req, err := http.NewRequest("GET", "http://127.0.0.1:3001/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", strings.Join([]string{
		proxyTokenCookie + "=secret",
		proxyOriginCookie + "=origin",
		sessionCookieName + "=session",
		relayDeviceCookie + "=device",
		relayAuthCookie + "=auth",
		"target_cookie=kept",
	}, "; "))

	stripProxyCookies(req)

	cookie := req.Header.Get("Cookie")
	if strings.Contains(cookie, proxyTokenCookie+"=") ||
		strings.Contains(cookie, proxyOriginCookie+"=") ||
		strings.Contains(cookie, sessionCookieName+"=") ||
		strings.Contains(cookie, relayDeviceCookie+"=") ||
		strings.Contains(cookie, relayAuthCookie+"=") {
		t.Fatalf("reserved cookie leaked to target: %s", cookie)
	}
	if !strings.Contains(cookie, "target_cookie=kept") {
		t.Fatalf("target cookie was not preserved: %s", cookie)
	}
}

func TestBuildClaudeConfigPatchUsesCanonicalDefaults(t *testing.T) {
	patch := buildClaudeConfigPatch(map[string]interface{}{
		"model":          "",
		"effort":         "",
		"permissionMode": nil,
	})
	bridge := NewClaudeBridge()
	bridge.SetConfig(patch)
	config := bridge.ConfigSnapshot()
	if config["model"] != "default" {
		t.Fatalf("expected default model, got %#v", config["model"])
	}
	if config["effort"] != "medium" {
		t.Fatalf("expected medium effort, got %#v", config["effort"])
	}
	if config["permissionMode"] != "default" {
		t.Fatalf("expected default permission mode, got %#v", config["permissionMode"])
	}
}

func TestClaudeTaskStatusSortsPendingPermissions(t *testing.T) {
	bridge := NewClaudeBridge()
	bridge.pendingPerms["later"] = &pendingPermission{requestId: "later", toolName: "later", createdAt: time.Unix(20, 0)}
	bridge.pendingPerms["earlier"] = &pendingPermission{requestId: "earlier", toolName: "earlier", createdAt: time.Unix(10, 0)}
	status := bridge.TaskStatus()
	perms := status["pendingPerms"].([]map[string]interface{})
	if len(perms) != 2 {
		t.Fatalf("expected 2 pending perms, got %d", len(perms))
	}
	if perms[0]["requestId"] != "earlier" || perms[1]["requestId"] != "later" {
		t.Fatalf("unexpected order: %#v", perms)
	}
}
