package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

func (s *Server) handleProxyOpen(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token != s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rawURL := r.URL.Query().Get("url")
	target, err := parseProxyTarget(rawURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	setProxyCookie(w, proxyTokenCookie, token)
	setProxyCookie(w, proxyOriginCookie, encodeCookieValue(target.Scheme+"://"+target.Host))
	http.Redirect(w, r, proxyPathForTarget(target), http.StatusFound)
}

func (s *Server) handleProxyPrefix(w http.ResponseWriter, r *http.Request) {
	if !s.hasProxyAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	target, err := targetFromProxyPath(r.URL.Path, r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	setProxyCookie(w, proxyOriginCookie, encodeCookieValue(target.Scheme+"://"+target.Host))
	s.proxyToTarget(w, r, target, false)
}

// handleOriginProxy serves the subdomain-root relay mode. Each target origin
// gets its own proxy subdomain, so the worker can serve that origin at the
// subdomain ROOT and pass us the origin directly 闁?no `/__anycode_proxy/http/
// host/` path prefix, and therefore no need to rewrite every URL in the page.
// Root-relative ("/_next/...") and relative URLs resolve naturally; we only
// neutralize absolute same-origin URLs (see rewriteTextResponseRoot).
func (s *Server) handleOriginProxy(w http.ResponseWriter, r *http.Request, origin string) {
	base, err := parseProxyTarget(origin)
	if err != nil {
		log.Printf("[proxy] origin-mode invalid origin %q: %v", origin, err)
		http.Error(w, "invalid target origin", http.StatusBadRequest)
		return
	}
	// Defense-in-depth: drop any legacy /__anycode_proxy/<scheme>/<host>/ prefix
	// a cached page might still carry, so it maps onto the origin root.
	path, rawPath := stripLegacyProxyPrefix(r.URL.Path, r.URL.EscapedPath())
	target := *base
	target.Path = path
	target.RawPath = rawPath
	target.RawQuery = r.URL.RawQuery
	log.Printf("[proxy] origin-mode %s %s -> %s", r.Method, r.URL.RequestURI(), target.String())
	s.proxyToTarget(w, r, &target, true)
}

// stripLegacyProxyPrefix removes a leading /__anycode_proxy/<scheme>/<host>/
// segment from a (decoded path, escaped path) pair, returning the inner path.
func stripLegacyProxyPrefix(path, rawPath string) (string, string) {
	if !strings.HasPrefix(path, proxyPrefix) {
		return path, rawPath
	}
	inner, err := targetFromProxyPath(path, "")
	if err != nil {
		return path, rawPath
	}
	p := inner.Path
	if p == "" {
		p = "/"
	}
	return p, inner.EscapedPath()
}

func (s *Server) handleProxyFallback(w http.ResponseWriter, r *http.Request) {
	if !s.hasProxyAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	originCookie, err := r.Cookie(proxyOriginCookie)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	origin, err := decodeCookieValue(originCookie.Value)
	if err != nil {
		http.Error(w, "invalid proxy origin", http.StatusBadRequest)
		return
	}

	baseURL, err := parseProxyTarget(origin)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	target := *baseURL
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery
	s.proxyToTarget(w, r, &target, false)
}

func (s *Server) handleRelayProxyFetch(params map[string]interface{}) (interface{}, error) {
	method := getParamString(params, "method")
	if method == "" {
		method = http.MethodGet
	}
	proxyPath := getParamString(params, "path")
	if proxyPath == "" || !strings.HasPrefix(proxyPath, "/") {
		return nil, fmt.Errorf("invalid proxy path")
	}

	var body []byte
	if encoded := getParamString(params, "bodyBase64"); encoded != "" {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid body: %w", err)
		}
		body = decoded
	}

	req, err := http.NewRequest(method, "https://relay.internal"+proxyPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Host = "anycodeapp.com"

	if rawHeaders, ok := params["headers"].(map[string]interface{}); ok {
		for key, rawValue := range rawHeaders {
			switch values := rawValue.(type) {
			case []interface{}:
				for _, value := range values {
					if s, ok := value.(string); ok {
						req.Header.Add(key, s)
					}
				}
			case []string:
				for _, value := range values {
					req.Header.Add(key, value)
				}
			case string:
				req.Header.Add(key, values)
			}
		}
	}
	if forwardedHost := req.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		req.Host = forwardedHost
	}
	s.prepareRelayProxyRequest(req)

	rec := httptest.NewRecorder()
	if origin := req.Header.Get("X-Anycode-Target-Origin"); origin != "" {
		// Subdomain-root relay mode: the worker authorized the request and
		// tells us the exact upstream origin, so we proxy origin+path verbatim.
		s.handleOriginProxy(rec, req, origin)
	} else if req.URL.Path == proxyOpenPath {
		log.Printf("[proxy] legacy open %s", req.URL.RequestURI())
		s.handleProxyOpen(rec, req)
	} else if strings.HasPrefix(req.URL.Path, proxyPrefix) {
		log.Printf("[proxy] legacy prefix %s %s", req.Method, req.URL.RequestURI())
		s.handleProxyPrefix(rec, req)
	} else if s.hasProxyAuth(req) {
		log.Printf("[proxy] legacy fallback %s %s", req.Method, req.URL.RequestURI())
		s.handleProxyFallback(rec, req)
	} else {
		log.Printf("[proxy] no route (404) %s %s (no target-origin header; worker may be stale)", req.Method, req.URL.RequestURI())
		http.NotFound(rec, req)
	}

	resp := rec.Result()
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	headers := make(map[string][]string)
	for key, values := range resp.Header {
		headers[key] = append([]string(nil), values...)
	}
	filterRelayProxyResponseHeaders(headers)

	return map[string]interface{}{
		"status":     resp.StatusCode,
		"headers":    headers,
		"bodyBase64": base64.StdEncoding.EncodeToString(respBody),
	}, nil
}

func (s *Server) prepareRelayProxyRequest(req *http.Request) {
	if req.URL.Path == proxyOpenPath {
		q := req.URL.Query()
		if q.Get("token") == "" {
			q.Set("token", s.token)
			req.URL.RawQuery = q.Encode()
		}
		return
	}

	if _, err := req.Cookie(proxyTokenCookie); err == nil {
		return
	}
	req.AddCookie(&http.Cookie{Name: proxyTokenCookie, Value: s.token})
}

// proxyTransport mirrors http.DefaultTransport but routes loopback/LAN targets
// directly instead of through the daemon's configured upstream proxy.
//
// The built-in browser proxies through this daemon. When the user has set an
// HTTP/HTTPS proxy (a common setup, e.g. in mainland China), DefaultTransport
// would forward *every* request 闁?including `http://localhost:3000` for a local
// dev server 闁?to that upstream proxy, which can't reach the dev machine's
// loopback. The result: the built-in browser can't open the remote machine's
// localhost over the relay. Public targets still honor the configured proxy.
var proxyTransport = newProxyTransport()

func newProxyTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		if isDirectProxyHost(req.URL.Hostname()) {
			log.Printf("[proxy] direct (bypass upstream proxy) for %s", req.URL.Host)
			return nil, nil
		}
		p, err := http.ProxyFromEnvironment(req)
		if p != nil {
			log.Printf("[proxy] via upstream proxy %s for %s", p, req.URL.Host)
		}
		return p, err
	}
	return t
}

// isDirectProxyHost reports whether a target host should bypass the configured
// upstream proxy (loopback, private, or link-local addresses + "localhost").
func isDirectProxyHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func (s *Server) proxyToTarget(w http.ResponseWriter, r *http.Request, target *url.URL, rootMode bool) {
	origin := &url.URL{Scheme: target.Scheme, Host: target.Host}
	proxy := httputil.NewSingleHostReverseProxy(origin)
	proxy.Transport = proxyTransport
	proxy.FlushInterval = 100 * time.Millisecond

	targetPath := target.EscapedPath()
	if targetPath == "" {
		targetPath = "/"
	}
	targetQuery := target.RawQuery
	incomingHost := r.Host
	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		incomingHost = forwardedHost
	}
	incomingScheme := "http"
	if r.TLS != nil {
		incomingScheme = "https"
	}
	if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto == "http" || forwardedProto == "https" {
		incomingScheme = forwardedProto
	}

	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = targetPath
		req.URL.RawPath = target.RawPath
		req.URL.RawQuery = targetQuery
		req.Host = target.Host
		req.RequestURI = ""
		req.Header.Del("Accept-Encoding")
		stripProxyCookies(req)
		if rootMode {
			// Make the proxied request look same-origin to the upstream. A
			// transparent reverse proxy must not leak the public proxy host in
			// Origin/Referer: dev servers (e.g. Next.js 15) gate /_next/* dev
			// assets on origin and will 404/refuse cross-origin requests.
			rewriteUpstreamOriginHeaders(req, target, incomingHost)
		}
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		log.Printf("[proxy] upstream %s %s -> %d (%s)", resp.Request.Method, target.String(), resp.StatusCode, resp.Header.Get("Content-Type"))
		rewriteSetCookieHeaders(resp)
		if rootMode {
			rewriteLocationHeaderRoot(resp, target, incomingScheme, incomingHost)
			return rewriteTextResponseRoot(resp, target, incomingScheme, incomingHost)
		}
		rewriteLocationHeader(resp, target, incomingHost)
		return rewriteTextResponse(resp, target, incomingHost, incomingScheme)
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[proxy] %s -> %s error: %v", r.URL.String(), target.String(), err)
		http.Error(w, err.Error(), http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}

// rewriteUpstreamOriginHeaders points Origin/Referer at the upstream origin so
// the proxied request looks same-origin (preserving the Referer's path/query).
func rewriteUpstreamOriginHeaders(req *http.Request, target *url.URL, incomingHost string) {
	targetOrigin := target.Scheme + "://" + target.Host
	if req.Header.Get("Origin") != "" {
		req.Header.Set("Origin", targetOrigin)
	}
	if ref := req.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && (u.Host == incomingHost || u.Host == "") {
			u.Scheme = target.Scheme
			u.Host = target.Host
			req.Header.Set("Referer", u.String())
		}
	}
}

func parseProxyTarget(rawURL string) (*url.URL, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("target URL must include scheme and host")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("only http and https targets are supported")
	}
	if target.Path == "" {
		target.Path = "/"
	}
	return target, nil
}

func targetFromProxyPath(path, rawQuery string) (*url.URL, error) {
	rest := strings.TrimPrefix(path, proxyPrefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid proxy path")
	}

	scheme := parts[0]
	host, err := url.PathUnescape(parts[1])
	if err != nil {
		return nil, err
	}
	targetPath := "/"
	if len(parts) == 3 && parts[2] != "" {
		targetPath = "/" + parts[2]
	}

	return parseProxyTarget((&url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     targetPath,
		RawQuery: rawQuery,
	}).String())
}

func proxyPathForTarget(target *url.URL) string {
	targetPath := target.EscapedPath()
	if targetPath == "" {
		targetPath = "/"
	}

	proxyURL := &url.URL{
		Path:     proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host) + targetPath,
		RawQuery: target.RawQuery,
	}
	return proxyURL.String()
}

func (s *Server) hasProxyAuth(r *http.Request) bool {
	if r.URL.Query().Get("token") == s.token {
		return true
	}
	cookie, err := r.Cookie(proxyTokenCookie)
	return err == nil && cookie.Value == s.token
}

func setProxyCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int((24 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func encodeCookieValue(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCookieValue(value string) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func stripProxyCookies(req *http.Request) {
	cookies := req.Cookies()
	if len(cookies) == 0 {
		return
	}

	kept := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if isReservedProxyCookieName(cookie.Name) {
			continue
		}
		kept = append(kept, cookie.Name+"="+cookie.Value)
	}

	if len(kept) == 0 {
		req.Header.Del("Cookie")
		return
	}
	req.Header.Set("Cookie", strings.Join(kept, "; "))
}

func rewriteLocationHeader(resp *http.Response, target *url.URL, incomingHost string) {
	location := resp.Header.Get("Location")
	if location == "" {
		return
	}

	locURL, err := url.Parse(location)
	if err != nil {
		return
	}

	if locURL.IsAbs() && locURL.Host == target.Host && locURL.Scheme == target.Scheme {
		resp.Header.Set("Location", proxyPathForTarget(locURL))
		return
	}

	if strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
		relURL, err := url.Parse(location)
		if err != nil {
			return
		}
		locURL := *target
		locURL.Path = relURL.Path
		locURL.RawPath = relURL.RawPath
		locURL.RawQuery = relURL.RawQuery
		resp.Header.Set("Location", proxyPathForTarget(&locURL))
		return
	}

	if locURL.IsAbs() && incomingHost != "" {
		if locURL.Scheme == "ws" || locURL.Scheme == "wss" {
			proxyScheme := "http"
			if locURL.Scheme == "wss" {
				proxyScheme = "https"
			}
			locURL.Scheme = proxyScheme
			resp.Header.Set("Location", proxyPathForTarget(locURL))
		}
	}
}

// rewriteLocationHeaderRoot rewrites redirects for subdomain-root mode. An
// absolute redirect back to the target origin is kept on the proxy origin
// (root-mapped), preserving path+query. Root-relative / relative locations
// already resolve to the proxy subdomain, so they are left untouched.
func rewriteLocationHeaderRoot(resp *http.Response, target *url.URL, incomingScheme, incomingHost string) {
	if incomingHost == "" {
		return
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return
	}
	locURL, err := url.Parse(location)
	if err != nil || !locURL.IsAbs() || locURL.Host != target.Host {
		return
	}
	locURL.Scheme = incomingScheme
	locURL.Host = incomingHost
	resp.Header.Set("Location", locURL.String())
}

// rewriteTextResponseRoot is the subdomain-root rewriter and the "general"
// replacement for per-attribute rule matching. Because the target origin is
// served at the subdomain root, root-relative ("/_next/...") and relative URLs
// resolve correctly with NO rewriting. We only neutralize absolute URLs that
// point back at the target origin 闁?otherwise the browser would try to reach
// the dev machine's origin directly (unreachable) 闁?and upgrade ws:// links.
func rewriteTextResponseRoot(resp *http.Response, target *url.URL, incomingScheme, incomingHost string) error {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !isRewriteableContent(contentType) || resp.Body == nil || incomingHost == "" {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	proxyRoot := incomingScheme + "://" + incomingHost
	wsProxy := "ws://" + incomingHost
	if incomingScheme == "https" {
		wsProxy = "wss://" + incomingHost
	}

	text := string(body)
	text = strings.ReplaceAll(text, "ws://"+target.Host, wsProxy)
	text = strings.ReplaceAll(text, "wss://"+target.Host, wsProxy)
	text = strings.ReplaceAll(text, target.Scheme+"://"+target.Host, proxyRoot)
	text = strings.ReplaceAll(text, "//"+target.Host, "//"+incomingHost)

	rewritten := []byte(text)
	resp.Body = io.NopCloser(strings.NewReader(text))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	return nil
}

func rewriteSetCookieHeaders(resp *http.Response) {
	values := resp.Header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}

	resp.Header.Del("Set-Cookie")
	for _, value := range values {
		if isReservedProxyCookieName(setCookieName(value)) {
			continue
		}
		parts := strings.Split(value, ";")
		kept := parts[:0]
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToLower(trimmed), "domain=") {
				continue
			}
			kept = append(kept, part)
		}
		resp.Header.Add("Set-Cookie", strings.Join(kept, ";"))
	}
}

func filterRelayProxyResponseHeaders(headers map[string][]string) {
	for key, values := range headers {
		if !strings.EqualFold(key, "Set-Cookie") {
			continue
		}
		kept := values[:0]
		for _, value := range values {
			if strings.EqualFold(setCookieName(value), proxyTokenCookie) {
				continue
			}
			kept = append(kept, value)
		}
		if len(kept) == 0 {
			delete(headers, key)
		} else {
			headers[key] = kept
		}
	}
}

func setCookieName(value string) string {
	first := strings.SplitN(value, ";", 2)[0]
	name := strings.SplitN(first, "=", 2)[0]
	return strings.TrimSpace(name)
}

func isReservedProxyCookieName(name string) bool {
	switch strings.ToLower(name) {
	case strings.ToLower(proxyTokenCookie),
		strings.ToLower(proxyOriginCookie),
		strings.ToLower(sessionCookieName),
		strings.ToLower(relayDeviceCookie),
		strings.ToLower(relayAuthCookie):
		return true
	default:
		return false
	}
}

func rewriteTextResponse(resp *http.Response, target *url.URL, incomingHost, incomingScheme string) error {
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !isRewriteableContent(contentType) || resp.Body == nil {
		return nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	origin := target.Scheme + "://" + target.Host
	proxyOrigin := incomingScheme + "://" + incomingHost + proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)

	text := string(body)
	if incomingHost != "" {
		wsProxyOrigin := "ws://" + incomingHost + proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)
		if incomingScheme == "https" {
			wsProxyOrigin = "wss://" + incomingHost + proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)
		}
		text = strings.ReplaceAll(text, "ws://"+target.Host, wsProxyOrigin)
		text = strings.ReplaceAll(text, "wss://"+target.Host, wsProxyOrigin)
	}
	text = strings.ReplaceAll(text, origin, proxyOrigin)
	text = strings.ReplaceAll(text, "//"+target.Host, proxyOrigin)
	if strings.Contains(contentType, "text/html") {
		text = injectBaseTag(text, proxyOrigin+"/")
		text = rewriteHTMLRootRelativeRefs(text, target)
		text = rewriteCSSRootRelativeRefs(text, target)
	} else if strings.Contains(contentType, "text/css") {
		text = rewriteCSSRootRelativeRefs(text, target)
	} else if strings.Contains(contentType, "javascript") {
		text = rewriteJSPublicPath(text, proxyOrigin+"/")
	}

	rewritten := []byte(text)
	resp.Body = io.NopCloser(strings.NewReader(text))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	return nil
}

func isRewriteableContent(contentType string) bool {
	return strings.Contains(contentType, "text/html") ||
		strings.Contains(contentType, "text/css") ||
		strings.Contains(contentType, "javascript") ||
		strings.Contains(contentType, "application/json") ||
		strings.Contains(contentType, "text/plain")
}

func rewriteHTMLRootRelativeRefs(text string, target *url.URL) string {
	proxyBase := proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)
	replacements := [][2]string{
		{`href="/`, `href="` + proxyBase + `/`},
		{`src="/`, `src="` + proxyBase + `/`},
		{`action="/`, `action="` + proxyBase + `/`},
		{`poster="/`, `poster="` + proxyBase + `/`},
		{`href='/`, `href='` + proxyBase + `/`},
		{`src='/`, `src='` + proxyBase + `/`},
		{`action='/`, `action='` + proxyBase + `/`},
		{`poster='/`, `poster='` + proxyBase + `/`},
	}
	for _, pair := range replacements {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}

// injectBaseTag inserts <base href="baseHref"> as the first child of <head>.
// This helps browser-resolved relative paths that the string rewriter can't
// catch. Root-relative refs are rewritten separately.
func injectBaseTag(html, baseHref string) string {
	tag := `<base href="` + baseHref + `">`
	for _, marker := range []string{"<head>", "<head ", "<HEAD>", "<HEAD "} {
		if idx := strings.Index(html, marker); idx >= 0 {
			end := strings.Index(html[idx:], ">")
			if end < 0 {
				break
			}
			pos := idx + end + 1
			return html[:pos] + tag + html[pos:]
		}
	}
	// No <head> tag 闁?prepend to body as fallback.
	if idx := strings.Index(html, "<body"); idx >= 0 {
		return html[:idx] + "<head>" + tag + "</head>" + html[idx:]
	}
	return html
}

func rewriteJSPublicPath(text, proxyBase string) string {
	quoted := `"` + proxyBase + `"`
	for _, pat := range []string{
		`__webpack_public_path__="/"`,
		`__webpack_public_path__='/'`,
		`__webpack_public_path__ = "/"`,
		`__webpack_public_path__ = '/'`,
	} {
		text = strings.ReplaceAll(text, pat, `__webpack_public_path__=`+quoted)
	}
	return text
}

func rewriteCSSRootRelativeRefs(text string, target *url.URL) string {
	proxyBase := proxyPrefix + target.Scheme + "/" + url.PathEscape(target.Host)
	replacements := [][2]string{
		{`url("/`, `url("` + proxyBase + `/`},
		{`url('/`, `url('` + proxyBase + `/`},
		{`url(/`, `url(` + proxyBase + `/`},
	}
	for _, pair := range replacements {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}
