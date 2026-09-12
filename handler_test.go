package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ===== hermetic 上游替身 =====

type upstreamCall struct {
	Body   map[string]interface{}
	Raw    string
	Header http.Header
}

type fakeUpstream struct {
	server  *httptest.Server
	mu      sync.Mutex
	calls   []upstreamCall
	handler func(n int, body map[string]interface{}) (int, string, string)
}

func newFakeUpstream(t *testing.T, handler func(n int, body map[string]interface{}) (int, string, string)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{handler: handler}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.calls = append(f.calls, upstreamCall{Body: body, Raw: string(raw), Header: r.Header.Clone()})
		n := len(f.calls)
		f.mu.Unlock()
		status, ctype, payload := f.handler(n, body)
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		io.WriteString(w, payload)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeUpstream) call(t *testing.T, i int) upstreamCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.calls) {
		t.Fatalf("upstream call %d out of range (%d recorded)", i, len(f.calls))
	}
	return f.calls[i]
}

// ===== 上游响应构造 =====

func completionPayload(text string) (int, string, string) {
	b, _ := json.Marshal(map[string]interface{}{
		"code": 0,
		"data": map[string]interface{}{"action": "completion", "taskCompletion": text},
	})
	return http.StatusOK, "application/json", string(b)
}

func reviewPayload(name, args string) (int, string, string) {
	b, _ := json.Marshal(map[string]interface{}{
		"code": 0,
		"data": map[string]interface{}{
			"action": "review", "thinking": "需要工具",
			"tool": map[string]interface{}{
				"type": "mcp", "mcpServer": "mcp-server-web-fetch",
				"name": name, "args": json.RawMessage(args),
			},
		},
	})
	return http.StatusOK, "application/json", string(b)
}

func rawStreamPayload(envelope string) (int, string, string) {
	runes := []rune(envelope)
	var sb strings.Builder
	for i := 0; i < len(runes); i += 9 {
		end := i + 9
		if end > len(runes) {
			end = len(runes)
		}
		frame, _ := json.Marshal(map[string]interface{}{"type": "ai", "content": string(runes[i:end])})
		sb.WriteString("data: " + string(frame) + "\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return http.StatusOK, "text/event-stream", sb.String()
}

func envelopeJSON(action, field, value string) string {
	b, _ := json.Marshal(map[string]interface{}{"action": action, field: value})
	return "```json\n" + string(b) + "\n```"
}

// ===== 代理构造与请求辅助 =====

func newTestProxy(t *testing.T, upstream string, mutate func(*Config)) *ProxyServer {
	t.Helper()
	cfg := Config{
		Port:          "0",
		Cookies:       filepath.Join(t.TempDir(), "absent.cookies"),
		Upstream:      upstream,
		AliasModel:    LegacyModelAlias,
		Product:       "orcaterm",
		Origin:        "http://tauri.localhost",
		UserAgent:     "test-agent",
		UpstreamModel: defaultUpstreamModel,
		Timeouts:      30,
		Mode:          "structured",
		OnDegraded:    "empty",
		Strip:         true,
		HideTools:     true,
		SessionTTL:    time.Hour,
		SessionMax:    100,
		ToolsMode:     "native",
		ContextLimit:  128000,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ps := NewProxyServer(cfg)
	// 直接注入假凭据，避免读取真实 .cookies，也不发真实网络请求。
	ps.cookies.creds = Credentials{SID: "sid", OT: "ot", UserID: "42"}
	t.Cleanup(ps.sessions.Close)
	return ps
}

type chatResult struct {
	Status int
	Header http.Header
	Body   []byte
	JSON   map[string]interface{}
}

func doChat(t *testing.T, ps *ProxyServer, payload map[string]interface{}, headers map[string]string) chatResult {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ps.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	out := chatResult{Status: res.StatusCode, Header: res.Header, Body: body}
	_ = json.Unmarshal(body, &out.JSON)
	return out
}

func errorCodeOf(t *testing.T, res chatResult) string {
	t.Helper()
	errObj, ok := res.JSON["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("响应缺少 error 对象: %s", res.Body)
	}
	code, _ := errObj["code"].(string)
	return code
}

func settingOf(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	user, _ := body["user"].(map[string]interface{})
	setting, _ := user["setting"].(map[string]interface{})
	if setting == nil {
		t.Fatalf("上游请求体缺少 user.setting: %v", body)
	}
	return setting
}

func inputTextOf(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	input, _ := body["input"].(map[string]interface{})
	raw, _ := input["input"].(string)
	if raw == "" {
		t.Fatalf("上游请求体缺少 input.input: %v", body)
	}
	var env struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("解析 ACP 信封失败: %v (%s)", err, raw)
	}
	var sb strings.Builder
	for _, p := range env.Prompt {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func conversationIDOf(t *testing.T, body map[string]interface{}) string {
	t.Helper()
	id, _ := body["conversationId"].(string)
	if id == "" {
		t.Fatalf("上游请求体缺少 conversationId: %v", body)
	}
	return id
}

func firstChoice(t *testing.T, res chatResult) map[string]interface{} {
	t.Helper()
	choices, ok := res.JSON["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("响应缺少 choices: %s", res.Body)
	}
	choice, _ := choices[0].(map[string]interface{})
	return choice
}

func messageOf(t *testing.T, res chatResult) map[string]interface{} {
	t.Helper()
	msg, _ := firstChoice(t, res)["message"].(map[string]interface{})
	if msg == nil {
		t.Fatalf("响应缺少 message: %s", res.Body)
	}
	return msg
}

func finishReasonOf(t *testing.T, res chatResult) string {
	t.Helper()
	reason, _ := firstChoice(t, res)["finish_reason"].(string)
	return reason
}

func userMessage(text string) map[string]interface{} {
	return map[string]interface{}{"role": "user", "content": text}
}

// ===== 1&2. 首轮 system 语义 =====

func TestChatFirstTurnSendsSystemPrompt(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("好的")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	res := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "你是运维助手"},
			userMessage("磁盘满了怎么办"),
		},
	}, map[string]string{"X-Session-Id": "sess-first"})

	if res.Status != http.StatusOK {
		t.Fatalf("状态码 %d: %s", res.Status, res.Body)
	}
	if fake.count() != 1 {
		t.Fatalf("上游调用次数 = %d", fake.count())
	}
	got := inputTextOf(t, fake.call(t, 0).Body)
	for _, want := range []string{"[系统]", "你是运维助手", "[用户]", "磁盘满了怎么办"} {
		if !strings.Contains(got, want) {
			t.Errorf("首轮输入缺少 %q\n实际: %s", want, got)
		}
	}
	if cid := conversationIDOf(t, fake.call(t, 0).Body); !strings.HasPrefix(cid, "cid-") || !strings.HasSuffix(cid, "-orcaterm") {
		t.Errorf("conversationId 格式错误: %q", cid)
	}
}

func TestChatSubsequentTurnSendsOnlyLatestUser(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("已处理")
	})
	ps := newTestProxy(t, fake.server.URL, nil)
	headers := map[string]string{"X-Session-Id": "sess-multi"}

	first := doChat(t, ps, map[string]interface{}{
		"model": "glm-5.3",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "系统指令"},
			userMessage("第一轮问题"),
		},
	}, headers)
	if first.Status != http.StatusOK {
		t.Fatalf("首轮状态码 %d: %s", first.Status, first.Body)
	}

	second := doChat(t, ps, map[string]interface{}{
		"model": "glm-5.3",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "系统指令"},
			userMessage("第一轮问题"),
			map[string]interface{}{"role": "assistant", "content": "第一轮回答"},
			userMessage("第二个问题"),
		},
	}, headers)
	if second.Status != http.StatusOK {
		t.Fatalf("次轮状态码 %d: %s", second.Status, second.Body)
	}

	firstCall := fake.call(t, 0).Body
	secondCall := fake.call(t, 1).Body
	if conversationIDOf(t, firstCall) != conversationIDOf(t, secondCall) {
		t.Errorf("同一 session 未复用 conversationId: %q vs %q",
			conversationIDOf(t, firstCall), conversationIDOf(t, secondCall))
	}
	got := inputTextOf(t, secondCall)
	if got != "第二个问题" {
		t.Errorf("后续轮应只发送最新 user，实际: %q", got)
	}
	if strings.Contains(got, "[系统]") || strings.Contains(got, "第一轮问题") {
		t.Errorf("后续轮重复发送了历史/system: %q", got)
	}
	// 首轮的 system 必须真的发出过
	if firstText := inputTextOf(t, firstCall); !strings.Contains(firstText, "[系统]") {
		t.Errorf("首轮未发送 system: %q", firstText)
	}
}

// ===== 3. sessionless modern tool 闭环 =====

func TestChatSessionlessModernToolResultRecoversConversation(t *testing.T) {
	fake := newFakeUpstream(t, func(n int, body map[string]interface{}) (int, string, string) {
		if strings.Contains(inputTextOf(t, body), "<TOOL_META>") {
			return completionPayload("工具结果已收到")
		}
		return reviewPayload("fetch", `{"url":"https://example.com"}`)
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	toolDecl := []interface{}{
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}},
	}
	first := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash",
		"messages": []interface{}{
			userMessage("抓取 example.com"),
		},
		"tools": toolDecl,
	}, nil)
	if first.Status != http.StatusOK {
		t.Fatalf("工具调用轮状态码 %d: %s", first.Status, first.Body)
	}
	msg := messageOf(t, first)
	calls, _ := msg["tool_calls"].([]interface{})
	if len(calls) != 1 {
		t.Fatalf("未返回 tool_calls: %s", first.Body)
	}
	call := calls[0].(map[string]interface{})
	callID, _ := call["id"].(string)
	if callID == "" {
		t.Fatalf("tool_call 缺少 id: %s", first.Body)
	}
	if finish := firstChoice(t, first)["finish_reason"]; finish != "tool_calls" {
		t.Errorf("finish_reason = %v，期望 tool_calls", finish)
	}

	// 关键：不带任何会话键，仅凭 tool_call_id 闭环
	second := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash",
		"messages": []interface{}{
			userMessage("抓取 example.com"),
			map[string]interface{}{
				"role": "assistant",
				"tool_calls": []interface{}{map[string]interface{}{
					"id": callID, "type": "function",
					"function": map[string]interface{}{"name": "fetch", "arguments": `{"url":"https://example.com"}`},
				}},
			},
			map[string]interface{}{"role": "tool", "tool_call_id": callID, "content": "页面标题：示例"},
		},
		"tools": toolDecl,
	}, nil)
	if second.Status != http.StatusOK {
		t.Fatalf("工具结果轮状态码 %d: %s", second.Status, second.Body)
	}
	if fake.count() != 2 {
		t.Fatalf("上游调用次数 = %d，期望 2", fake.count())
	}
	if a, b := conversationIDOf(t, fake.call(t, 0).Body), conversationIDOf(t, fake.call(t, 1).Body); a != b {
		t.Errorf("工具结果未回到原 conversation: %q vs %q", a, b)
	}
	gotInput := inputTextOf(t, fake.call(t, 1).Body)
	if gotInput != "<TOOL_META>返回结果</TOOL_META>\n页面标题：示例" {
		t.Errorf("工具结果回传文本错误: %q", gotInput)
	}
	if header := second.Header.Get("X-OrcaTerm-Tool-Result-Turn"); header != "1" {
		t.Errorf("缺少工具结果轮标记: %q", header)
	}
}

func TestChatModernToolResultRequiresToolCallID(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("不应被调用")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	res := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash",
		"messages": []interface{}{
			userMessage("hi"),
			map[string]interface{}{"role": "assistant", "content": "ok"},
			map[string]interface{}{"role": "tool", "content": "结果"},
		},
	}, nil)
	if res.Status != http.StatusBadRequest {
		t.Fatalf("状态码 %d，期望 400: %s", res.Status, res.Body)
	}
	if code := errorCodeOf(t, res); code != "tool_call_id_required" {
		t.Errorf("错误码 = %q，期望 tool_call_id_required", code)
	}
	if fake.count() != 0 {
		t.Errorf("非法请求不应触达上游，实际 %d 次", fake.count())
	}
}

// ===== 4. tool_call 状态机错误映射 =====

func TestChatToolCallErrorMapping(t *testing.T) {
	toolResult := func(id string) map[string]interface{} {
		return map[string]interface{}{
			"model": "deepseek-v4-flash",
			"messages": []interface{}{
				userMessage("go"),
				map[string]interface{}{"role": "tool", "tool_call_id": id, "content": "结果"},
			},
		}
	}

	t.Run("unknown", func(t *testing.T) {
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			return completionPayload("x")
		})
		ps := newTestProxy(t, fake.server.URL, nil)
		res := doChat(t, ps, toolResult("call_nope"), nil)
		if res.Status != http.StatusBadRequest || errorCodeOf(t, res) != "unknown_tool_call_id" {
			t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
		}
	})

	t.Run("consumed", func(t *testing.T) {
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			return completionPayload("x")
		})
		ps := newTestProxy(t, fake.server.URL, nil)
		if _, err := ps.sessions.PutPendingCall(PendingCall{ID: "call_used", Name: "fetch", Arguments: "{}", CID: "cid-x"}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.sessions.ConsumePendingCall("call_used", "", ""); err != nil {
			t.Fatal(err)
		}
		res := doChat(t, ps, toolResult("call_used"), nil)
		if res.Status != http.StatusBadRequest || errorCodeOf(t, res) != "tool_call_already_consumed" {
			t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
		}
	})

	t.Run("expired", func(t *testing.T) {
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			return completionPayload("x")
		})
		ps := newTestProxy(t, fake.server.URL, func(c *Config) { c.SessionTTL = 30 * time.Millisecond })
		if _, err := ps.sessions.PutPendingCall(PendingCall{
			ID: "call_old", Name: "fetch", Arguments: "{}", CID: "cid-old",
			CreatedAt: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		res := doChat(t, ps, toolResult("call_old"), nil)
		if res.Status != http.StatusBadRequest || errorCodeOf(t, res) != "expired_tool_call_id" {
			t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			return completionPayload("x")
		})
		ps := newTestProxy(t, fake.server.URL, nil)
		ps.sessions.GetOrCreate("owner", "cid-owner")
		if _, err := ps.sessions.PutPendingCall(PendingCall{
			ID: "call_mine", Name: "fetch", Arguments: "{}", CID: "cid-owner", SessionKey: "owner",
		}); err != nil {
			t.Fatal(err)
		}
		res := doChat(t, ps, toolResult("call_mine"), map[string]string{"X-Session-Id": "intruder"})
		if res.Status != http.StatusBadRequest || errorCodeOf(t, res) != "tool_call_mismatch" {
			t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
		}
	})

	t.Run("in_flight", func(t *testing.T) {
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			return completionPayload("x")
		})
		ps := newTestProxy(t, fake.server.URL, nil)
		if _, err := ps.sessions.PutPendingCall(PendingCall{ID: "call_busy", Name: "fetch", Arguments: "{}", CID: "cid-busy"}); err != nil {
			t.Fatal(err)
		}
		held, _, err := ps.sessions.AcquireCall(context.Background(), "call_busy", "")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Rollback()
		res := doChat(t, ps, toolResult("call_busy"), nil)
		if res.Status != http.StatusBadRequest || errorCodeOf(t, res) != "tool_call_in_flight" {
			t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
		}
	})

	t.Run("name_mismatch", func(t *testing.T) {
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			return completionPayload("x")
		})
		ps := newTestProxy(t, fake.server.URL, nil)
		if _, err := ps.sessions.PutPendingCall(PendingCall{ID: "call_named", Name: "fetch", Arguments: "{}", CID: "cid-named"}); err != nil {
			t.Fatal(err)
		}
		payload := toolResult("call_named")
		payload["messages"] = []interface{}{
			userMessage("go"),
			map[string]interface{}{"role": "tool", "tool_call_id": "call_named", "name": "run_commands", "content": "结果"},
		}
		res := doChat(t, ps, payload, nil)
		if res.Status != http.StatusBadRequest || errorCodeOf(t, res) != "tool_result_name_mismatch" {
			t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
		}
		// 回滚后该 pending call 仍可使用
		if _, err := ps.sessions.ResolvePendingCall("call_named", "", ""); err != nil {
			t.Fatalf("失败请求污染了 pending call: %v", err)
		}
	})
}

// ===== 5. 多 tool result 拒绝 =====

func TestChatMultipleToolResultsRejected(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("x")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	res := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash",
		"messages": []interface{}{
			userMessage("go"),
			map[string]interface{}{"role": "tool", "tool_call_id": "call_a", "content": "A"},
			map[string]interface{}{"role": "tool", "tool_call_id": "call_b", "content": "B"},
		},
	}, nil)
	if res.Status != http.StatusBadRequest {
		t.Fatalf("状态码 %d，期望 400: %s", res.Status, res.Body)
	}
	if code := errorCodeOf(t, res); code != "multiple_tool_results_unsupported" {
		t.Errorf("错误码 = %q，期望 multiple_tool_results_unsupported", code)
	}
	if fake.count() != 0 {
		t.Errorf("多工具结果不应触达上游")
	}
}

// ===== 6-9. tools / functions 规范化 =====

func TestNormalizeToolPolicy(t *testing.T) {
	nativeDecl := []Tool{{Type: "function", Function: ToolFunction{Name: "fetch"}}}
	legacyDecl := []ToolFunction{{Name: "run_commands"}}
	boolPtr := func(b bool) *bool { return &b }

	cases := []struct {
		name       string
		req        ChatCompletionRequest
		mode       string
		wantCode   string
		wantDial   string
		wantChoice string
		wantEnable bool
	}{
		{name: "no tools", req: ChatCompletionRequest{}, mode: "native", wantChoice: "auto"},
		{name: "modern auto", req: ChatCompletionRequest{Tools: nativeDecl}, mode: "native",
			wantDial: "modern", wantChoice: "auto", wantEnable: true},
		{name: "modern none", req: ChatCompletionRequest{Tools: nativeDecl, ToolChoice: json.RawMessage(`"none"`)},
			mode: "native", wantDial: "modern", wantChoice: "none"},
		{name: "legacy functions", req: ChatCompletionRequest{Functions: legacyDecl}, mode: "native",
			wantDial: "legacy", wantChoice: "auto", wantEnable: true},
		{name: "legacy function_call none",
			req:  ChatCompletionRequest{Functions: legacyDecl, FunctionCall: json.RawMessage(`"none"`)},
			mode: "native", wantDial: "legacy", wantChoice: "none"},
		{name: "parallel false", req: ChatCompletionRequest{Tools: nativeDecl, ParallelToolCalls: boolPtr(false)},
			mode: "native", wantDial: "modern", wantChoice: "auto", wantEnable: true},
		{name: "conflict", req: ChatCompletionRequest{Tools: nativeDecl, Functions: legacyDecl},
			mode: "native", wantCode: "conflicting_tool_schemas"},
		{name: "parallel true", req: ChatCompletionRequest{Tools: nativeDecl, ParallelToolCalls: boolPtr(true)},
			mode: "native", wantCode: "parallel_tool_calls_unsupported"},
		{name: "required", req: ChatCompletionRequest{Tools: nativeDecl, ToolChoice: json.RawMessage(`"required"`)},
			mode: "native", wantCode: "tool_choice_required_unsupported"},
		{name: "named", req: ChatCompletionRequest{Tools: nativeDecl, ToolChoice: json.RawMessage(`"fetch"`)},
			mode: "native", wantCode: "forced_tool_choice_unsupported"},
		{name: "named object",
			req:  ChatCompletionRequest{Tools: nativeDecl, ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"fetch"}}`)},
			mode: "native", wantCode: "forced_tool_choice_unsupported"},
		{name: "custom tool", req: ChatCompletionRequest{Tools: []Tool{{Type: "function", Function: ToolFunction{Name: "my_custom"}}}},
			mode: "native", wantCode: "custom_tools_unsupported"},
		{name: "wrong type", req: ChatCompletionRequest{Tools: []Tool{{Type: "retrieval", Function: ToolFunction{Name: "fetch"}}}},
			mode: "native", wantCode: "custom_tools_unsupported"},
		{name: "duplicate", req: ChatCompletionRequest{Tools: []Tool{
			{Type: "function", Function: ToolFunction{Name: "fetch"}},
			{Type: "function", Function: ToolFunction{Name: "fetch"}},
		}}, mode: "native", wantCode: "duplicate_tool"},
		{name: "mode error", req: ChatCompletionRequest{Tools: nativeDecl}, mode: "error", wantCode: "tools_unsupported"},
		{name: "mode ignore", req: ChatCompletionRequest{Tools: nativeDecl}, mode: "ignore",
			wantDial: "modern", wantChoice: "auto"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := c.req
			policy, code, err := normalizeToolPolicy(&req, c.mode)
			if c.wantCode != "" {
				if err == nil || code != c.wantCode {
					t.Fatalf("期望错误码 %q，实际 code=%q err=%v", c.wantCode, code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: code=%q err=%v", code, err)
			}
			if c.wantDial != "" && policy.Dialect != c.wantDial {
				t.Errorf("dialect = %q，期望 %q", policy.Dialect, c.wantDial)
			}
			if policy.Choice != c.wantChoice {
				t.Errorf("choice = %q，期望 %q", policy.Choice, c.wantChoice)
			}
			if policy.Enabled != c.wantEnable {
				t.Errorf("enabled = %v，期望 %v", policy.Enabled, c.wantEnable)
			}
			if policy.Mode != c.mode {
				t.Errorf("mode = %q，期望 %q", policy.Mode, c.mode)
			}
		})
	}
}

func TestChatRejectsInvalidToolSchemas(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("x")
	})
	ps := newTestProxy(t, fake.server.URL, nil)
	base := map[string]interface{}{"model": "deepseek-v4-flash", "messages": []interface{}{userMessage("hi")}}

	cases := []struct {
		name string
		body map[string]interface{}
		code string
	}{
		{"conflict", map[string]interface{}{
			"tools":     []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}}},
			"functions": []interface{}{map[string]interface{}{"name": "fetch"}},
		}, "conflicting_tool_schemas"},
		{"parallel", map[string]interface{}{
			"tools":               []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}}},
			"parallel_tool_calls": true,
		}, "parallel_tool_calls_unsupported"},
		{"required", map[string]interface{}{
			"tools":       []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}}},
			"tool_choice": "required",
		}, "tool_choice_required_unsupported"},
		{"forced", map[string]interface{}{
			"tools":       []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}}},
			"tool_choice": "run_commands",
		}, "forced_tool_choice_unsupported"},
		{"custom", map[string]interface{}{
			"tools": []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "not_a_native_tool"}}},
		}, "custom_tools_unsupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]interface{}{}
			for k, v := range base {
				body[k] = v
			}
			for k, v := range c.body {
				body[k] = v
			}
			res := doChat(t, ps, body, nil)
			if res.Status != http.StatusBadRequest {
				t.Fatalf("状态 %d，期望 400: %s", res.Status, res.Body)
			}
			if code := errorCodeOf(t, res); code != c.code {
				t.Errorf("错误码 = %q，期望 %q", code, c.code)
			}
		})
	}
}

func TestChatUndeclaredUpstreamToolRejected(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return reviewPayload("execute_terminal_command", `{"cmd":"ls"}`)
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	res := doChat(t, ps, map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{userMessage("列目录")},
		"tools": []interface{}{
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}},
		},
	}, nil)
	if res.Status != http.StatusBadGateway {
		t.Fatalf("状态 %d，期望 502: %s", res.Status, res.Body)
	}
	if code := errorCodeOf(t, res); code != "upstream_requested_undeclared_tool" {
		t.Errorf("错误码 = %q", code)
	}
	for _, s := range ps.sessions.List() {
		if s.PendingCallID != "" {
			t.Errorf("未声明工具不应创建 pending call: %+v", s)
		}
	}
}

// ===== 10. chatBody 工具策略与 HideTools =====

func TestChatBodyToolPolicyAndHideTools(t *testing.T) {
	ps := newTestProxy(t, "http://127.0.0.1:1", nil)

	t.Run("tools off", func(t *testing.T) {
		body := ps.chatBody("cid-1", "hi", "42", defaultUpstreamModel, nil, false, toolPolicy{Enabled: false})
		setting := settingOf(t, body)
		if got := setting["mcpServers"]; !reflect.DeepEqual(got, []string{}) {
			t.Errorf("mcpServers = %#v，期望空数组", got)
		}
		if got := setting["uiServers"]; !reflect.DeepEqual(got, []string{}) {
			t.Errorf("uiServers = %#v，期望空数组", got)
		}
	})

	t.Run("tools on", func(t *testing.T) {
		body := ps.chatBody("cid-1", "hi", "42", defaultUpstreamModel, nil, false, toolPolicy{Enabled: true})
		setting := settingOf(t, body)
		if got := setting["mcpServers"]; !reflect.DeepEqual(got, []string{"mcp-server-orcaterm-oauth"}) {
			t.Errorf("mcpServers = %#v", got)
		}
		if got := setting["uiServers"]; !reflect.DeepEqual(got, []string{"ui-tools-orcaterm-explorer"}) {
			t.Errorf("uiServers = %#v", got)
		}
	})

	t.Run("setting fields", func(t *testing.T) {
		body := ps.chatBody("cid-1", "hi", "42", defaultUpstreamModel, nil, true, toolPolicy{})
		setting := settingOf(t, body)
		if got, ok := setting["tools"].([]interface{}); !ok || len(got) != 0 {
			t.Errorf("setting.tools 应为空数组（自定义工具无法注册）: %#v", setting["tools"])
		}
		if setting["model"] != defaultUpstreamModel {
			t.Errorf("setting.model = %v", setting["model"])
		}
		if body["stream"] != true {
			t.Errorf("stream 未透传: %v", body["stream"])
		}
		for _, key := range []string{"hideToolMessage", "isNormalToolMessageIgnored", "shouldHideDirectOutputToolReview"} {
			if setting[key] != true {
				t.Errorf("%s = %v，期望 true（HideTools 默认开启）", key, setting[key])
			}
		}
	})

	t.Run("hide tools off", func(t *testing.T) {
		ps2 := newTestProxy(t, "http://127.0.0.1:1", func(c *Config) { c.HideTools = false })
		setting := settingOf(t, ps2.chatBody("cid-1", "hi", "42", defaultUpstreamModel, nil, false, toolPolicy{}))
		for _, key := range []string{"hideToolMessage", "isNormalToolMessageIgnored", "shouldHideDirectOutputToolReview"} {
			if setting[key] != false {
				t.Errorf("%s = %v，期望 false", key, setting[key])
			}
		}
	})
}

// ===== 11. modern / legacy 非流式响应形状 =====

func TestChatModernToolCallResponseShape(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return reviewPayload("fetch", `{"url":"https://a.example"}`)
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	res := doChat(t, ps, map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{userMessage("抓取")},
		"tools": []interface{}{
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}},
		},
	}, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("状态 %d: %s", res.Status, res.Body)
	}
	msg := messageOf(t, res)
	if msg["content"] != nil {
		t.Errorf("工具调用轮的 content 应为 null，实际 %#v", msg["content"])
	}
	calls, _ := msg["tool_calls"].([]interface{})
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", msg["tool_calls"])
	}
	call := calls[0].(map[string]interface{})
	if call["type"] != "function" {
		t.Errorf("tool_call type = %v", call["type"])
	}
	fn, _ := call["function"].(map[string]interface{})
	if fn["name"] != "fetch" || fn["arguments"] != `{"url":"https://a.example"}` {
		t.Errorf("tool_call function = %#v", fn)
	}
	if finish := firstChoice(t, res)["finish_reason"]; finish != "tool_calls" {
		t.Errorf("finish_reason = %v，期望 tool_calls", finish)
	}
}

func TestChatLegacyFunctionResponseShape(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return reviewPayload("run_commands", `{"commands":["ls"]}`)
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	res := doChat(t, ps, map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{userMessage("列目录")},
		"functions": []interface{}{
			map[string]interface{}{"name": "run_commands"},
		},
	}, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("状态 %d: %s", res.Status, res.Body)
	}
	msg := messageOf(t, res)
	if _, ok := msg["tool_calls"]; ok {
		t.Errorf("legacy 方言不应返回 tool_calls: %#v", msg)
	}
	fc, ok := msg["function_call"].(map[string]interface{})
	if !ok {
		t.Fatalf("legacy 方言应返回 function_call: %s", res.Body)
	}
	if fc["name"] != "run_commands" {
		t.Errorf("function_call.name = %v", fc["name"])
	}
	if finish := finishReasonOf(t, res); finish != "function_call" {
		t.Errorf("finish_reason = %v，期望 function_call", finish)
	}
	// legacy 声明也应启用上游原生工具服务（请求体经 JSON 往返，元素是 interface{}）
	if got := settingOf(t, fake.call(t, 0).Body)["mcpServers"]; !reflect.DeepEqual(got, []interface{}{"mcp-server-orcaterm-oauth"}) {
		t.Errorf("legacy functions 未启用原生工具: %#v", got)
	}
}

func TestChatLegacyFunctionResultRequiresSession(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("收到")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	msgs := []interface{}{
		userMessage("跑一下"),
		map[string]interface{}{"role": "assistant", "function_call": map[string]interface{}{"name": "fetch", "arguments": "{}"}},
		map[string]interface{}{"role": "function", "name": "fetch", "content": "结果"},
	}
	res := doChat(t, ps, map[string]interface{}{"model": "deepseek-v4-flash", "messages": msgs}, nil)
	if res.Status != http.StatusBadRequest || errorCodeOf(t, res) != "legacy_function_result_requires_session" {
		t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
	}

	// 复用原会话键即可继续
	ps.sessions.GetOrCreate("legacy-sess", "cid-legacy")
	if _, err := ps.sessions.PutPendingCall(PendingCall{
		ID: "call-legacy", Name: "fetch", Arguments: "{}", CID: "cid-legacy", SessionKey: "legacy-sess",
	}); err != nil {
		t.Fatal(err)
	}
	ok := doChat(t, ps, map[string]interface{}{"model": "deepseek-v4-flash", "messages": msgs},
		map[string]string{"X-Session-Id": "legacy-sess"})
	if ok.Status != http.StatusOK {
		t.Fatalf("复用会话键后仍失败: %d %s", ok.Status, ok.Body)
	}
	if got := inputTextOf(t, fake.call(t, 0).Body); got != "<TOOL_META>返回结果</TOOL_META>\n结果" {
		t.Errorf("legacy 工具结果回传文本错误: %q", got)
	}
	// 同一 pending call 只能消费一次
	again := doChat(t, ps, map[string]interface{}{"model": "deepseek-v4-flash", "messages": msgs},
		map[string]string{"X-Session-Id": "legacy-sess"})
	if again.Status != http.StatusBadRequest || errorCodeOf(t, again) != "tool_call_already_consumed" {
		t.Fatalf("重复消费未被拒绝: %d %s", again.Status, again.Body)
	}
}

// ===== 12. 流式协议 =====

func sseChunks(t *testing.T, body []byte) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, block := range strings.Split(string(body), "\n\n") {
		line := strings.TrimSpace(block)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			out = append(out, map[string]interface{}{"__done": true})
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("解析 SSE 分片失败: %v (%s)", err, payload)
		}
		out = append(out, chunk)
	}
	return out
}

func TestChatStreamTextShape(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("你好世界")
	})
	ps := newTestProxy(t, fake.server.URL, func(c *Config) { c.HideTools = true })

	res := doChat(t, ps, map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{userMessage("你好")},
		"stream":   true,
	}, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("状态 %d: %s", res.Status, res.Body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	chunks := sseChunks(t, res.Body)
	if len(chunks) < 3 {
		t.Fatalf("分片过少: %v", chunks)
	}
	first := firstDelta(t, chunks[0])
	if first["role"] != "assistant" {
		t.Errorf("首块缺少 delta.role=assistant: %#v", first)
	}
	if chunks[len(chunks)-1]["__done"] != true {
		t.Errorf("流未以 [DONE] 结束: %v", chunks[len(chunks)-1])
	}
	var text strings.Builder
	sawUsage := false
	for _, c := range chunks {
		if _, ok := c["usage"]; ok {
			sawUsage = true
		}
		delta := deltaOf(c)
		if s, ok := delta["content"].(string); ok {
			text.WriteString(s)
		}
	}
	if text.String() != "你好世界" {
		t.Errorf("流式正文 = %q", text.String())
	}
	if sawUsage {
		t.Error("未请求 include_usage 时不应发送 usage chunk")
	}
	if finish := streamFinishReason(t, chunks); finish != "stop" {
		t.Errorf("finish_reason = %v，期望 stop", finish)
	}
}

func TestChatStreamToolCallAndUsage(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return reviewPayload("fetch", `{"url":"https://b.example"}`)
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	res := doChat(t, ps, map[string]interface{}{
		"model":          "deepseek-v4-flash",
		"messages":       []interface{}{userMessage("抓取")},
		"stream":         true,
		"stream_options": map[string]interface{}{"include_usage": true},
		"tools": []interface{}{
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}},
		},
	}, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("状态 %d: %s", res.Status, res.Body)
	}
	chunks := sseChunks(t, res.Body)

	var toolDelta []interface{}
	usageChunk := map[string]interface{}(nil)
	for _, c := range chunks {
		if _, ok := c["usage"]; ok {
			usageChunk = c
		}
		if calls, ok := deltaOf(c)["tool_calls"].([]interface{}); ok {
			toolDelta = calls
		}
	}
	if toolDelta == nil {
		t.Fatalf("流中未出现 tool_calls delta: %s", res.Body)
	}
	call := toolDelta[0].(map[string]interface{})
	for _, key := range []string{"index", "id", "type", "function"} {
		if _, ok := call[key]; !ok {
			t.Errorf("tool_calls delta 缺少 %q: %#v", key, call)
		}
	}
	if call["index"] != float64(0) {
		t.Errorf("tool_calls index = %v", call["index"])
	}
	if usageChunk == nil {
		t.Fatal("include_usage=true 时未发送 usage chunk")
	}
	if _, ok := usageChunk["created"]; !ok {
		t.Error("usage chunk 缺少 created")
	}
	if choices, ok := usageChunk["choices"].([]interface{}); !ok || len(choices) != 0 {
		t.Errorf("usage chunk 的 choices 应为空数组: %#v", usageChunk["choices"])
	}
	if finish := streamFinishReason(t, chunks); finish != "tool_calls" {
		t.Errorf("finish_reason = %v，期望 tool_calls", finish)
	}
}

func firstDelta(t *testing.T, chunk map[string]interface{}) map[string]interface{} {
	t.Helper()
	return deltaOf(chunk)
}

// streamFinishReason 从 SSE 分片中找出带 finish_reason 的那一片（与 usage chunk 的顺序无关）。
func streamFinishReason(t *testing.T, chunks []map[string]interface{}) string {
	t.Helper()
	for _, c := range chunks {
		choices, _ := c["choices"].([]interface{})
		for _, raw := range choices {
			choice, _ := raw.(map[string]interface{})
			if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
				return reason
			}
		}
	}
	t.Fatalf("SSE 流中没有 finish_reason: %v", chunks)
	return ""
}

func deltaOf(chunk map[string]interface{}) map[string]interface{} {
	choices, _ := chunk["choices"].([]interface{})
	if len(choices) == 0 {
		return map[string]interface{}{}
	}
	choice, _ := choices[0].(map[string]interface{})
	delta, _ := choice["delta"].(map[string]interface{})
	if delta == nil {
		return map[string]interface{}{}
	}
	return delta
}

// ===== 15. structured / raw-stream parity =====

func TestStructuredRawOutcomeParity(t *testing.T) {
	envelope := envelopeJSON("completion", "taskCompletion", "同一答案")
	structured := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("同一答案")
	})
	raw := newFakeUpstream(t, func(n int, body map[string]interface{}) (int, string, string) {
		return rawStreamPayload(envelope)
	})

	payload := map[string]interface{}{"model": "deepseek-v4-flash", "messages": []interface{}{userMessage("问题")}}

	psA := newTestProxy(t, structured.server.URL, func(c *Config) { c.Mode = "structured" })
	psB := newTestProxy(t, raw.server.URL, func(c *Config) { c.Mode = "raw-stream" })

	resA := doChat(t, psA, payload, nil)
	resB := doChat(t, psB, payload, nil)
	if resA.Status != http.StatusOK || resB.Status != http.StatusOK {
		t.Fatalf("状态 structured=%d raw=%d\n%s\n%s", resA.Status, resB.Status, resA.Body, resB.Body)
	}
	textA := messageOf(t, resA)["content"]
	textB := messageOf(t, resB)["content"]
	if textA != "同一答案" || textA != textB {
		t.Errorf("structured/raw 结果不一致: %v vs %v", textA, textB)
	}
	if actionA, actionB := resA.Header.Get("X-OrcaTerm-Action"), resB.Header.Get("X-OrcaTerm-Action"); actionA != actionB {
		t.Errorf("action 不一致: %q vs %q", actionA, actionB)
	}
	// 上游 stream 标志正确区分
	if streamA, _ := structured.call(t, 0).Body["stream"].(bool); streamA {
		t.Error("structured 模式应以 stream=false 调用上游")
	}
	if streamB, _ := raw.call(t, 0).Body["stream"].(bool); !streamB {
		t.Error("raw-stream 模式应以 stream=true 调用上游")
	}
}

func TestStructuredRawToolCallParity(t *testing.T) {
	envelope := "```json\n" +
		`{"action":"review","tool":{"type":"mcp","mcpServer":"mcp-server-web-fetch","name":"fetch","args":{"url":"https://c.example"}}}` +
		"\n```"
	structured := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return reviewPayload("fetch", `{"url":"https://c.example"}`)
	})
	raw := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return rawStreamPayload(envelope)
	})
	payload := map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{userMessage("抓取")},
		"tools": []interface{}{
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}},
		},
	}
	psA := newTestProxy(t, structured.server.URL, nil)
	psB := newTestProxy(t, raw.server.URL, func(c *Config) { c.Mode = "raw-stream" })

	for name, ps := range map[string]*ProxyServer{"structured": psA, "raw-stream": psB} {
		res := doChat(t, ps, payload, nil)
		if res.Status != http.StatusOK {
			t.Fatalf("%s 状态 %d: %s", name, res.Status, res.Body)
		}
		msg := messageOf(t, res)
		calls, _ := msg["tool_calls"].([]interface{})
		if len(calls) != 1 {
			t.Fatalf("%s 未返回 tool_calls: %s", name, res.Body)
		}
		fn := calls[0].(map[string]interface{})["function"].(map[string]interface{})
		if fn["name"] != "fetch" || fn["arguments"] != `{"url":"https://c.example"}` {
			t.Errorf("%s function = %#v", name, fn)
		}
	}
}

// ===== 16. 上游错误映射 =====

func TestUpstreamErrorMapping(t *testing.T) {
	payload := map[string]interface{}{"model": "deepseek-v4-flash", "messages": []interface{}{userMessage("hi")}}

	cases := []struct {
		name       string
		handler    func(int, map[string]interface{}) (int, string, string)
		wantStatus int
		wantCode   string
	}{
		{"unauthorized", func(int, map[string]interface{}) (int, string, string) {
			return http.StatusUnauthorized, "application/json", `{"message":"expired"}`
		}, http.StatusBadGateway, "upstream_auth_error"},
		{"forbidden", func(int, map[string]interface{}) (int, string, string) {
			return http.StatusForbidden, "application/json", `{"message":"denied"}`
		}, http.StatusBadGateway, "upstream_auth_error"},
		{"rate limited", func(int, map[string]interface{}) (int, string, string) {
			return http.StatusTooManyRequests, "application/json", `{"message":"slow down"}`
		}, http.StatusTooManyRequests, "upstream_rate_limited"},
		{"server error", func(int, map[string]interface{}) (int, string, string) {
			return http.StatusInternalServerError, "application/json", `{"message":"boom"}`
		}, http.StatusBadGateway, "upstream_error"},
		{"non zero code", func(int, map[string]interface{}) (int, string, string) {
			return http.StatusOK, "application/json", `{"code":7,"message":"bad request"}`
		}, http.StatusBadGateway, "upstream_error"},
		{"broken json", func(int, map[string]interface{}) (int, string, string) {
			return http.StatusOK, "application/json", `{"code":0,"data":`
		}, http.StatusBadGateway, "upstream_protocol_error"},
		{"unknown action", func(int, map[string]interface{}) (int, string, string) {
			return http.StatusOK, "application/json", `{"code":0,"data":{"action":"sing"}}`
		}, http.StatusBadGateway, "upstream_protocol_error"},
		{"review without tool", func(int, map[string]interface{}) (int, string, string) {
			return http.StatusOK, "application/json", `{"code":0,"data":{"action":"review"}}`
		}, http.StatusBadGateway, "upstream_protocol_error"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeUpstream(t, c.handler)
			ps := newTestProxy(t, fake.server.URL, nil)
			res := doChat(t, ps, payload, nil)
			if res.Status != c.wantStatus {
				t.Fatalf("状态 = %d，期望 %d: %s", res.Status, c.wantStatus, res.Body)
			}
			if code := errorCodeOf(t, res); code != c.wantCode {
				t.Errorf("错误码 = %q，期望 %q", code, c.wantCode)
			}
		})
	}
}

func TestUpstreamNetworkFailureAndTimeout(t *testing.T) {
	payload := map[string]interface{}{"model": "deepseek-v4-flash", "messages": []interface{}{userMessage("hi")}}

	t.Run("unreachable", func(t *testing.T) {
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			return completionPayload("x")
		})
		ps := newTestProxy(t, fake.server.URL, nil)
		fake.server.Close() // 关闭后连接被拒绝
		res := doChat(t, ps, payload, nil)
		if res.Status != http.StatusBadGateway || errorCodeOf(t, res) != "upstream_error" {
			t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			<-release
			return completionPayload("x")
		})
		t.Cleanup(func() { close(release) })
		ps := newTestProxy(t, fake.server.URL, nil)
		ps.client = &http.Client{Timeout: 60 * time.Millisecond}
		res := doChat(t, ps, payload, nil)
		if res.Status != http.StatusGatewayTimeout || errorCodeOf(t, res) != "upstream_timeout" {
			t.Fatalf("状态=%d code=%q body=%s", res.Status, errorCodeOf(t, res), res.Body)
		}
	})
}

func TestUpstreamErrorDetailIsTruncated(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return http.StatusInternalServerError, "application/json", huge
	})
	ps := newTestProxy(t, fake.server.URL, nil)
	res := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash", "messages": []interface{}{userMessage("hi")},
	}, nil)
	if res.Status != http.StatusBadGateway {
		t.Fatalf("状态 %d: %s", res.Status, res.Body)
	}
	if len(res.Body) > 2000 {
		t.Errorf("错误响应未限长: %d 字节", len(res.Body))
	}
	if bytes.Count(res.Body, []byte("xxxxx")) > 100 {
		t.Errorf("错误正文泄漏了完整上游响应")
	}
}

// ===== 会话与并发边界 =====

func TestChatSessionlessRequestDoesNotCreateSession(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("ok")
	})
	ps := newTestProxy(t, fake.server.URL, nil)
	for i := 0; i < 3; i++ {
		res := doChat(t, ps, map[string]interface{}{
			"model": "deepseek-v4-flash", "messages": []interface{}{userMessage("hi")},
		}, nil)
		if res.Status != http.StatusOK {
			t.Fatalf("状态 %d: %s", res.Status, res.Body)
		}
	}
	if list := ps.sessions.List(); len(list) != 0 {
		t.Errorf("无会话键请求不应留下会话: %+v", list)
	}
	if fake.count() != 3 {
		t.Fatalf("上游调用次数 = %d", fake.count())
	}
	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		ids[conversationIDOf(t, fake.call(t, i).Body)] = true
	}
	if len(ids) != 3 {
		t.Errorf("无会话键请求应各自使用新 conversationId: %v", ids)
	}
}

func TestChatSameSessionIsSerialized(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	fake := newFakeUpstream(t, func(n int, body map[string]interface{}) (int, string, string) {
		started <- struct{}{}
		<-release
		return completionPayload("ok")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := doChat(t, ps, map[string]interface{}{
				"model": "deepseek-v4-flash",
				"messages": []interface{}{
					userMessage("hi"),
				},
			}, map[string]string{"X-Session-Id": "serial"})
			statuses[i] = res.Status
		}(i)
	}
	// 只有第一个请求能到达上游，第二个必须等待同一 CID 的租约
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("首个请求未到达上游")
	}
	select {
	case <-started:
		t.Fatal("同一会话的两个请求并发到达了上游")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	for i, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("第 %d 个请求状态 = %d", i, s)
		}
	}
	if fake.count() != 2 {
		t.Fatalf("上游调用次数 = %d", fake.count())
	}
	first := conversationIDOf(t, fake.call(t, 0).Body)
	second := conversationIDOf(t, fake.call(t, 1).Body)
	if first != second {
		t.Errorf("同一会话串行执行后 conversationId 应一致: %q vs %q", first, second)
	}
	if got := inputTextOf(t, fake.call(t, 1).Body); got != "hi" {
		t.Errorf("第二次请求应作为后续轮只发最新 user，实际 %q", got)
	}
}
