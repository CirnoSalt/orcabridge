package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ===== 工具桥接：上游原生工具 ↔ 客户端声明工具 =====

// upstreamPromptText 取出第 idx 次上游请求里真正送给模型的文本（ACP 信封）。
func upstreamPromptText(t *testing.T, f *fakeUpstream, idx int) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if idx >= len(f.calls) {
		t.Fatalf("上游只被调用了 %d 次，取不到第 %d 次", len(f.calls), idx+1)
	}
	outer, _ := f.calls[idx].Body["input"].(map[string]interface{})
	raw, _ := outer["input"].(string)
	var parts []map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		return raw
	}
	var sb strings.Builder
	for _, p := range parts {
		if s, ok := p["text"].(string); ok {
			sb.WriteString(s)
		}
	}
	return sb.String()
}

type seenCall struct {
	ID        string
	Name      string
	Arguments string
}

func firstToolCall(t *testing.T, res chatResult) seenCall {
	t.Helper()
	choices, _ := res.JSON["choices"].([]interface{})
	if len(choices) == 0 {
		t.Fatalf("响应里没有 choices: %s", res.Body)
	}
	msg, _ := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	calls, _ := msg["tool_calls"].([]interface{})
	if len(calls) == 0 {
		t.Fatalf("响应里没有 tool_calls: %s", res.Body)
	}
	fn, _ := calls[0].(map[string]interface{})["function"].(map[string]interface{})
	return seenCall{
		ID:        calls[0].(map[string]interface{})["id"].(string),
		Name:      fn["name"].(string),
		Arguments: fn["arguments"].(string),
	}
}

func bashDecl() []interface{} {
	return []interface{}{map[string]interface{}{
		"type":     "function",
		"function": map[string]interface{}{"name": "Bash"},
	}}
}

// 上游下发 execute_command，调用方只声明了 Bash：应该改名叫 Bash 并重映射参数。
func TestBridgeRenamesUpstreamCallToClientTool(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		// 真机采样到的形状：带 OrcaTerm 专有的 terminal_id / connect_config_id
		return reviewPayload("execute_command", `{"command":"ls -la","terminal_id":"","connect_config_id":""}`)
	})
	ps := newTestProxy(t, fake.server.URL, func(c *Config) { c.Bridge = bridgeSafe })

	res := doChat(t, ps, map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{userMessage("看看当前目录")},
		"tools":    bashDecl(),
	}, nil)
	if res.Status != 200 {
		t.Fatalf("状态 %d，期望 200: %s", res.Status, res.Body)
	}

	call := firstToolCall(t, res)
	if call.Name != "Bash" {
		t.Errorf("对外工具名 = %q，期望 Bash（桥接改名）", call.Name)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		t.Fatalf("参数不是合法 JSON: %v", err)
	}
	if args["command"] != "ls -la" {
		t.Errorf("command = %v，期望 ls -la", args["command"])
	}
	// 上游专有的键必须丢弃：客户端的 schema 里没有它们，带过去只会报参数错误。
	for _, key := range []string{"terminal_id", "connect_config_id"} {
		if _, ok := args[key]; ok {
			t.Errorf("上游专有参数 %q 应被丢弃，实际仍在: %s", key, call.Arguments)
		}
	}
	if got := res.Header.Get("X-OrcaTerm-Bridge"); got != bridgeSafe {
		t.Errorf("X-OrcaTerm-Bridge = %q，期望 %q", got, bridgeSafe)
	}
}

// 桥接调用的结果必须能被认出来是"我方的"，而不是被当成客户端自己的工具结果。
// 这是本轮的关键修复：改名之后对外发的名字是 Bash，按名字判断会误判成外来工具。
func TestBridgeResultRoundTripUsesNativeName(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return reviewPayload("execute_command", `{"command":"ls"}`)
	})
	ps := newTestProxy(t, fake.server.URL, func(c *Config) { c.Bridge = bridgeSafe })

	first := doChat(t, ps, map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{userMessage("看看当前目录")},
		"tools":    bashDecl(),
	}, nil)
	if first.Status != 200 {
		t.Fatalf("第一轮状态 %d: %s", first.Status, first.Body)
	}
	call := firstToolCall(t, first)

	// 第二轮：客户端以 Bash 的名字回传结果（空结果走"拒绝"分支，拒绝文本里带工具名，
	// 正好可以验证回填用的是上游原生名 execute_command 而不是客户端名 Bash）。
	second := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash",
		"messages": []interface{}{
			userMessage("看看当前目录"),
			map[string]interface{}{
				"role": "assistant", "content": nil,
				"tool_calls": []interface{}{map[string]interface{}{
					"id": call.ID, "type": "function",
					"function": map[string]interface{}{"name": "Bash", "arguments": call.Arguments},
				}},
			},
			map[string]interface{}{
				"role": "tool", "tool_call_id": call.ID, "name": "Bash", "content": "",
			},
		},
		"tools": bashDecl(),
	}, nil)
	if second.Status != 200 {
		t.Fatalf("回传桥接结果应被接受，实际 %d: %s", second.Status, second.Body)
	}
	sent := upstreamPromptText(t, fake, 1)
	if !strings.Contains(sent, "execute_command") {
		t.Errorf("回填上游时应使用原生名 execute_command，实际送去的是: %q", sent)
	}
	if strings.Contains(sent, "tool: Bash") {
		t.Errorf("不应把客户端名 Bash 当成上游工具名回传: %q", sent)
	}
}

// 桥接关闭时，只声明 Bash 不应该启用上游工具服务，原生调用会被如实拒绝。
func TestBridgeOffRejectsUndeclaredTool(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return reviewPayload("execute_command", `{"command":"ls"}`)
	})
	ps := newTestProxy(t, fake.server.URL, func(c *Config) { c.Bridge = bridgeOff })

	res := doChat(t, ps, map[string]interface{}{
		"model":    "deepseek-v4-flash",
		"messages": []interface{}{userMessage("看看当前目录")},
		"tools":    bashDecl(),
	}, nil)
	if res.Status != 502 {
		t.Fatalf("状态 %d，期望 502: %s", res.Status, res.Body)
	}
	if code := errorCodeOf(t, res); code != "upstream_requested_undeclared_tool" {
		t.Errorf("错误码 = %q，期望 upstream_requested_undeclared_tool", code)
	}
	if fake.count() != 1 {
		t.Errorf("上游调用次数 = %d，期望 1", fake.count())
	}
}

func TestRemapBridgeArgs(t *testing.T) {
	entry, ok := BridgeEntryFor("Bash", bridgeSafe)
	if !ok {
		t.Fatal("Bash 在 safe 档应可桥接")
	}
	cases := []struct{ name, in, want string }{
		{"保留映射键", `{"command":"ls"}`, `{"command":"ls"}`},
		{"丢弃上游专有键", `{"command":"ls","terminal_id":"t1","connect_config_id":"c1"}`, `{"command":"ls"}`},
		{"缺键则留空", `{"other":1}`, `{}`},
		{"空参数", ``, `{}`},
		{"非法 JSON", `not-json`, `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RemapBridgeArgs(c.in, entry); got != c.want {
				t.Errorf("RemapBridgeArgs(%q) = %s，期望 %s", c.in, got, c.want)
			}
		})
	}
}

func TestPendingCallUpstreamName(t *testing.T) {
	if got := (PendingCall{Name: "fetch"}).UpstreamName(); got != "fetch" {
		t.Errorf("未桥接时应回退到 Name，实际 %q", got)
	}
	if got := (PendingCall{Name: "Bash", NativeName: "execute_command"}).UpstreamName(); got != "execute_command" {
		t.Errorf("桥接时应返回原生名，实际 %q", got)
	}
}

// Read 属于 riskRemote：safe 档不桥接（上游返回 remote_read 应被如实拒绝），
// all 档才桥接。
func TestBridgeRiskLevels(t *testing.T) {
	newProxy := func(t *testing.T, level string) (*fakeUpstream, *ProxyServer) {
		fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
			return reviewPayload("remote_read", `{"file_path":"/etc/hostname","terminal_id":"","connect_config_id":""}`)
		})
		return fake, newTestProxy(t, fake.server.URL, func(c *Config) { c.Bridge = level })
	}
	req := func() map[string]interface{} {
		return map[string]interface{}{
			"model":    "deepseek-v4-flash",
			"messages": []interface{}{userMessage("读一下 /etc/hostname")},
			"tools": []interface{}{map[string]interface{}{
				"type": "function", "function": map[string]interface{}{"name": "Read"},
			}},
		}
	}

	_, ps := newProxy(t, bridgeSafe)
	res := doChat(t, ps, req(), nil)
	if res.Status != 502 {
		t.Fatalf("safe 档不该桥接 Read，状态 %d: %s", res.Status, res.Body)
	}

	_, psAll := newProxy(t, bridgeAll)
	resAll := doChat(t, psAll, req(), nil)
	if resAll.Status != 200 {
		t.Fatalf("all 档应桥接 Read，状态 %d: %s", resAll.Status, resAll.Body)
	}
	if call := firstToolCall(t, resAll); call.Name != "Read" {
		t.Errorf("对外工具名 = %q，期望 Read", call.Name)
	}
}
