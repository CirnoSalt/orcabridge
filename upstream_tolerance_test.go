package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 上游把 AccessToken 过期也走 HTTP 200 + 业务码，而且 code 是**字符串**。
// 早期用 int 建模会先撞上 JSON 解析失败，把"登录态失效"报成 upstream_protocol_error。
func TestUpstreamStringBusinessCodeIsClassified(t *testing.T) {
	payload := map[string]interface{}{"model": "deepseek-v4-flash", "messages": []interface{}{userMessage("hi")}}

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "expired token as string code",
			body:       `{"code":"10050000","message":"Invalid or expired AccessToken","error":{"name":"ServerError","message":"Invalid or expired AccessToken"}}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_auth_error",
		},
		{
			name:       "expired token by message only",
			body:       `{"code":"9999","message":"Invalid or expired AccessToken"}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_auth_error",
		},
		{
			name:       "other string code",
			body:       `{"code":"40004","message":"quota exceeded"}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_error",
		},
		{
			name:       "numeric code still works",
			body:       `{"code":7,"message":"bad request"}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_error",
		},
		{
			name:       "empty string code treated as success",
			body:       `{"code":"","data":{"action":"completion","taskCompletion":"ok"}}`,
			wantStatus: http.StatusOK,
			wantCode:   "",
		},
		{
			name:       "non numeric string code is a protocol error",
			body:       `{"code":"oops","message":"?"}`,
			wantStatus: http.StatusBadGateway,
			wantCode:   "upstream_protocol_error",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := c.body
			fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
				return http.StatusOK, "application/json", body
			})
			ps := newTestProxy(t, fake.server.URL, nil)
			res := doChat(t, ps, payload, nil)
			if res.Status != c.wantStatus {
				t.Fatalf("状态 = %d，期望 %d: %s", res.Status, c.wantStatus, res.Body)
			}
			if c.wantCode == "" {
				if _, ok := res.JSON["error"]; ok {
					t.Fatalf("不应报错: %s", res.Body)
				}
				return
			}
			if code := errorCodeOf(t, res); code != c.wantCode {
				t.Fatalf("错误码 = %q，期望 %q (%s)", code, c.wantCode, res.Body)
			}
		})
	}
}

// 认证失败必须给出可操作的提示，而不是含糊的 "upstream failed"。
func TestAuthErrorTellsUserToRelogin(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"code":"10050000","message":"Invalid or expired AccessToken"}`
	})
	ps := newTestProxy(t, fake.server.URL, nil)
	res := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash", "messages": []interface{}{userMessage("hi")},
	}, nil)
	msg, _ := res.JSON["error"].(map[string]interface{})["message"].(string)
	if !strings.Contains(msg, "重新登录") {
		t.Errorf("认证错误信息应提示重新登录，实际: %s", msg)
	}
}

func TestUpstreamCodeUnmarshal(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{`0`, 0, false},
		{`7`, 7, false},
		{`"0"`, 0, false},
		{`"10050000"`, 10050000, false},
		{`""`, 0, false},
		{`null`, 0, false},
		{`"abc"`, 0, true},
		{`{}`, 0, true},
	}
	for _, c := range cases {
		var code upstreamCode
		err := json.Unmarshal([]byte(c.in), &code)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s 应报错，实际得到 %d", c.in, code)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s 不应报错: %v", c.in, err)
			continue
		}
		if int(code) != c.want {
			t.Errorf("%s -> %d，期望 %d", c.in, code, c.want)
		}
	}
}

// 真机发现：只声明 execute_terminal_command 时上游会先请求 list_terminals，
// 因此终端类任务需要整条工具链都在 allowlist 内。
func TestNativeToolAllowlistCoversTerminalChain(t *testing.T) {
	chain := []string{
		"fetch", "list_terminals", "get_terminal_detail", "get_terminal_output",
		"execute_terminal_command", "send_terminal_signal", "list_connect_configs",
		"get_command_history", "execute_command", "create_command", "query_commands_status",
		"run_commands", "run_sandbox_task", "submit_agent_tasks", "query_agent_tasks_status",
		"read_cloud_space_file", "read_cloud_space_file_list", "create_cloud_space_file",
		"remote_read", "remote_write", "remote_edit", "remote_multi_edit", "remote_glob",
		"remote_grep", "get_orcaterm_settings", "update_orcaterm_settings",
		"create_firewall_rules", "delete_firewall_rules",
	}
	for _, name := range chain {
		if !nativeToolNames[name] {
			t.Errorf("allowlist 缺少真机原生工具 %s", name)
		}
	}
	declared := make([]Tool, 0, len(chain))
	for _, name := range chain {
		declared = append(declared, Tool{Type: "function", Function: ToolFunction{Name: name}})
	}
	req := ChatCompletionRequest{Tools: declared}
	policy, code, err := normalizeToolPolicy(&req, "native")
	if err != nil {
		t.Fatalf("终端工具链应被接受: code=%q err=%v", code, err)
	}
	if !policy.Enabled || len(policy.Tools) != len(chain) {
		t.Fatalf("policy 未启用或工具数不符: %+v", policy)
	}
	// 表外名字仍然被拒
	bad := ChatCompletionRequest{Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "my_custom_fn"}}}}
	if _, code, err := normalizeToolPolicy(&bad, "native"); err == nil || code != "custom_tools_unsupported" {
		t.Fatalf("表外工具应被拒绝: code=%q err=%v", code, err)
	}
}

func makeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]interface{}{"exp": exp.Unix(), "userId": "42"})
	seg := base64.RawURLEncoding.EncodeToString(payload)
	return "eyJhbGciOiJIUzI1NiJ9." + seg + ".sig"
}

func TestJWTExpiry(t *testing.T) {
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	got, ok := jwtExpiry(makeJWT(t, future))
	if !ok || !got.Equal(future) {
		t.Errorf("jwtExpiry = %v ok=%v，期望 %v", got, ok, future)
	}
	for _, bad := range []string{"", "nodots", "a.b", "eyJhbGciOiJIUzI1NiJ9.bm90anNvbg.sig"} {
		if _, ok := jwtExpiry(bad); ok {
			t.Errorf("%q 不应解析出 exp", bad)
		}
	}
}

func TestHealthReportsTokenExpiry(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", nil)

	ps.cookies.creds = Credentials{SID: "sid", OT: makeJWT(t, time.Now().Add(time.Hour)), UserID: "42"}
	_, body := getJSON(t, ps, "/health")
	if body["token_expired"] != false {
		t.Errorf("未过期 token 应报 token_expired=false: %v", body)
	}
	if _, ok := body["token_expires_at"]; !ok {
		t.Errorf("/health 应包含 token_expires_at: %v", body)
	}

	ps.cookies.creds = Credentials{SID: "sid", OT: makeJWT(t, time.Now().Add(-time.Hour)), UserID: "42"}
	_, body = getJSON(t, ps, "/health")
	if body["token_expired"] != true {
		t.Errorf("过期 token 应报 token_expired=true: %v", body)
	}
}
