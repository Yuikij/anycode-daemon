package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
