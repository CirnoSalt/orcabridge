package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func corsRequest(t *testing.T, ps *ProxyServer, method, path, origin string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ps.Handler().ServeHTTP(rec, req)
	return rec
}

func preflightHeaders(method string) map[string]string {
	return map[string]string{
		"Access-Control-Request-Method":  method,
		"Access-Control-Request-Headers": "content-type, authorization, x-session-id",
	}
}

// CORS 默认必须完全关闭：不返回任何许可头，未允许 origin 的预检直接拒绝。
func TestCORSDefaultDisabled(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", nil)

	rec := corsRequest(t, ps, http.MethodGet, "/health", "http://evil.example", nil)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("默认不应返回 Access-Control-Allow-Origin，实际 %q", got)
	}
	for _, h := range []string{"Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Expose-Headers"} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("默认不应返回 %s，实际 %q", h, got)
		}
	}

	pre := corsRequest(t, ps, http.MethodOptions, "/v1/chat/completions", "http://evil.example", preflightHeaders("POST"))
	if pre.Code != http.StatusForbidden {
		t.Errorf("默认配置下跨域预检应 403，实际 %d", pre.Code)
	}
	if !strings.Contains(pre.Body.String(), "cors_origin_denied") {
		t.Errorf("预检拒绝缺少错误码: %s", pre.Body.String())
	}

	// 本地/非浏览器预检（无 Origin）不应被拒绝
	plain := corsRequest(t, ps, http.MethodOptions, "/v1/chat/completions", "", nil)
	if plain.Code >= 300 {
		t.Errorf("无 Origin 的 OPTIONS 不应失败，实际 %d", plain.Code)
	}
}

func TestCORSAllowlist(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", func(c *Config) {
		c.CORSOrigins = []string{"http://localhost:3000", "https://app.example.com"}
	})

	rec := corsRequest(t, ps, http.MethodOptions, "/v1/chat/completions", "http://localhost:3000", preflightHeaders("DELETE"))
	if rec.Code >= 300 {
		t.Fatalf("允许的 origin 预检失败: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Allow-Origin = %q", got)
	}
	methods := rec.Header().Get("Access-Control-Allow-Methods")
	for _, m := range []string{"GET", "POST", "DELETE", "OPTIONS"} {
		if !strings.Contains(methods, m) {
			t.Errorf("Allow-Methods 缺少 %s: %q", m, methods)
		}
	}
	headers := rec.Header().Get("Access-Control-Allow-Headers")
	for _, h := range []string{"Content-Type", "Authorization", "X-Session-Id", "OpenAI-Organization", "OpenAI-Project", "OpenAI-Beta"} {
		if !strings.Contains(headers, h) {
			t.Errorf("Allow-Headers 缺少 %s: %q", h, headers)
		}
	}
	if expose := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(expose, "X-OrcaTerm-Session") {
		t.Errorf("Expose-Headers 缺少 X-OrcaTerm-Session: %q", expose)
	}

	if got := corsRequest(t, ps, http.MethodOptions, "/v1/chat/completions", "http://localhost:4000", preflightHeaders("POST")); got.Code != http.StatusForbidden {
		t.Errorf("未允许 origin 的预检应 403，实际 %d", got.Code)
	}
	if got := corsRequest(t, ps, http.MethodGet, "/health", "http://localhost:3000", nil).Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("普通请求也应带许可头，实际 %q", got)
	}
}

func TestCORSWildcardRequiresExplicitStar(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", func(c *Config) { c.CORSOrigins = []string{"*"} })
	rec := corsRequest(t, ps, http.MethodOptions, "/v1/chat/completions", "https://anything.example", preflightHeaders("POST"))
	if rec.Code >= 300 {
		t.Fatalf("显式 * 应允许任意 origin: %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q，期望 *", got)
	}
}

func TestCORSIgnoresBlankEntries(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", func(c *Config) { c.CORSOrigins = []string{"", "  "} })
	if got := corsRequest(t, ps, http.MethodGet, "/health", "http://localhost:3000", nil).Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("空白配置应等效关闭 CORS，实际 %q", got)
	}
}
