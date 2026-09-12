package main

import (
	"net/http"
	"strings"
	"testing"
)

// agent 客户端（ZCode / Cline / Roo 等）自带一套工具，执行完会把自己的结果按
// role="tool" 回放。它们不是我方的待处理调用：必须当成本轮输入交给上游，
// 而不是 400 unknown_tool_call_id，否则这类客户端的工具循环一用就断。

func foreignToolCallHistory() []interface{} {
	return []interface{}{
		userMessage("看看当前目录"),
		map[string]interface{}{
			"role": "assistant", "content": nil,
			"tool_calls": []interface{}{map[string]interface{}{
				"id": "call_zcode_1", "type": "function",
				"function": map[string]interface{}{"name": "Bash", "arguments": `{"command":"ls"}`},
			}},
		},
	}
}

func foreignTools() []interface{} {
	return []interface{}{
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "Agent"}},
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "Bash"}},
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "Read"}},
	}
}

func TestForeignToolResultIsSentAsInputNotRejected(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("目录里有 README.md 和 main.go。")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	msgs := append(foreignToolCallHistory(),
		map[string]interface{}{
			"role": "tool", "tool_call_id": "call_zcode_1", "name": "Bash",
			"content": "README.md main.go",
		})
	res := doChat(t, ps, map[string]interface{}{
		"model": "hy4-preview", "messages": msgs, "tools": foreignTools(),
	}, nil)

	if res.Status != http.StatusOK {
		t.Fatalf("客户端自己的工具结果应被接受，实际 %d: %s", res.Status, res.Body)
	}
	if fake.count() != 1 {
		t.Fatalf("上游调用次数 = %d", fake.count())
	}
	// 工具输出必须真的送进上游，不能丢
	if got := inputTextOf(t, fake.call(t, 0).Body); !strings.Contains(got, "README.md main.go") {
		t.Errorf("工具输出未进入上游输入: %q", got)
	}
	if got := inputTextOf(t, fake.call(t, 0).Body); !strings.Contains(got, "[工具结果 Bash]") {
		t.Errorf("工具结果缺少可读标注: %q", got)
	}
	// 没有原生工具声明 → 不启用原生工具服务
	if setting := settingOf(t, fake.call(t, 0).Body); len(setting["mcpServers"].([]interface{})) != 0 {
		t.Errorf("不应启用原生工具服务: %#v", setting["mcpServers"])
	}
}

func TestForeignToolResultWithoutNameStillClassified(t *testing.T) {
	// OpenAI 的 role="tool" 允许省略 name，此时靠 assistant.tool_calls 反查名字
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("ok")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	msgs := append(foreignToolCallHistory(),
		map[string]interface{}{"role": "tool", "tool_call_id": "call_zcode_1", "content": "README.md"})
	res := doChat(t, ps, map[string]interface{}{
		"model": "hy4-preview", "messages": msgs, "tools": foreignTools(),
	}, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("省略 name 时也应能识别为客户端工具，实际 %d: %s", res.Status, res.Body)
	}
	if got := inputTextOf(t, fake.call(t, 0).Body); !strings.Contains(got, "README.md") {
		t.Errorf("工具输出未进入上游输入: %q", got)
	}
}

func TestMultipleForeignToolResultsAccepted(t *testing.T) {
	// 客户端自己的工具可以并行，多个结果只是上下文；这条限制只针对我方待处理调用
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("ok")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	msgs := append(foreignToolCallHistory(),
		map[string]interface{}{"role": "tool", "tool_call_id": "call_zcode_1", "name": "Bash", "content": "a.txt"},
		map[string]interface{}{"role": "tool", "tool_call_id": "call_zcode_2", "name": "Read", "content": "b.txt"},
	)
	res := doChat(t, ps, map[string]interface{}{
		"model": "hy4-preview", "messages": msgs, "tools": foreignTools(),
	}, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("多个客户端工具结果应被接受，实际 %d: %s", res.Status, res.Body)
	}
	got := inputTextOf(t, fake.call(t, 0).Body)
	for _, want := range []string{"a.txt", "b.txt", "[工具结果 Bash]", "[工具结果 Read]"} {
		if !strings.Contains(got, want) {
			t.Errorf("上游输入缺少 %q: %q", want, got)
		}
	}
}

// 我方 OrcaTerm 工具结果仍然保持严格语义：id 未知就是 400。
func TestNativeToolResultStaysStrict(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("ok")
	})
	ps := newTestProxy(t, fake.server.URL, nil)

	msgs := []interface{}{
		userMessage("抓取 example.com"),
		map[string]interface{}{
			"role": "assistant", "content": nil,
			"tool_calls": []interface{}{map[string]interface{}{
				"id": "call_ghost", "type": "function",
				"function": map[string]interface{}{"name": "fetch", "arguments": `{"url":"https://x"}`},
			}},
		},
		map[string]interface{}{
			"role": "tool", "tool_call_id": "call_ghost", "name": "fetch", "content": "页面内容",
		},
	}
	res := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash", "messages": msgs,
		"tools": []interface{}{
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}},
		},
	}, nil)
	if res.Status != http.StatusBadRequest || errorCodeOf(t, res) != "unknown_tool_call_id" {
		t.Fatalf("我方工具结果 id 未知时应 400，实际 %d: %s", res.Status, res.Body)
	}
	if fake.count() != 0 {
		t.Errorf("非法请求不应触达上游")
	}
}

// 混入外来结果时整体按"客户端输入"处理：不去消费我方的 pending call，
// 但也不把结果丢掉。
func TestMixedToolResultsPreferClientInput(t *testing.T) {
	fake := newFakeUpstream(t, func(int, map[string]interface{}) (int, string, string) {
		return completionPayload("ok")
	})
	ps := newTestProxy(t, fake.server.URL, nil)
	if _, err := ps.sessions.PutPendingCall(PendingCall{
		ID: "call_ours", Name: "fetch", Arguments: `{"url":"https://x"}`, CID: "cid-mixed",
	}); err != nil {
		t.Fatal(err)
	}
	msgs := []interface{}{
		userMessage("一起处理"),
		map[string]interface{}{
			"role": "assistant", "content": nil,
			"tool_calls": []interface{}{
				map[string]interface{}{"id": "call_ours", "type": "function",
					"function": map[string]interface{}{"name": "fetch", "arguments": `{"url":"https://x"}`}},
				map[string]interface{}{"id": "call_zcode_9", "type": "function",
					"function": map[string]interface{}{"name": "Bash", "arguments": `{}`}},
			},
		},
		map[string]interface{}{"role": "tool", "tool_call_id": "call_ours", "name": "fetch", "content": "页面内容"},
		map[string]interface{}{"role": "tool", "tool_call_id": "call_zcode_9", "name": "Bash", "content": "终端输出"},
	}
	res := doChat(t, ps, map[string]interface{}{
		"model": "deepseek-v4-flash", "messages": msgs,
		"tools": []interface{}{
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "fetch"}},
			map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "Bash"}},
		},
	}, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("混合结果应被接受，实际 %d: %s", res.Status, res.Body)
	}
	got := inputTextOf(t, fake.call(t, 0).Body)
	for _, want := range []string{"页面内容", "终端输出"} {
		if !strings.Contains(got, want) {
			t.Errorf("上游输入缺少 %q: %q", want, got)
		}
	}
	// 我方的 pending call 仍然可用（没有被误消费）
	if _, err := ps.sessions.ResolvePendingCall("call_ours", "", ""); err != nil {
		t.Errorf("我方 pending call 被误消费: %v", err)
	}
}

func TestHasForeignToolResultUnit(t *testing.T) {
	cases := []struct {
		name    string
		results []ToolResultMessage
		want    bool
	}{
		{"native only", []ToolResultMessage{{Name: "fetch"}, {Name: "run_commands"}}, false},
		{"foreign", []ToolResultMessage{{Name: "Bash"}}, true},
		{"mixed", []ToolResultMessage{{Name: "fetch"}, {Name: "Agent"}}, true},
		{"unknown name 保守按我方", []ToolResultMessage{{Name: ""}}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := HasForeignToolResult(c.results); got != c.want {
			t.Errorf("%s: HasForeignToolResult = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestRenderClientToolResults(t *testing.T) {
	got := RenderClientToolResults([]ToolResultMessage{
		{Name: "Bash", Content: "README.md"},
		{Name: "", Content: "匿名输出"},
	})
	for _, want := range []string{"[工具结果 Bash]", "README.md", "[工具结果 工具]", "匿名输出"} {
		if !strings.Contains(got, want) {
			t.Errorf("渲染结果缺少 %q: %q", want, got)
		}
	}
	if RenderClientToolResults(nil) != "" {
		t.Error("空输入应返回空串")
	}
}
