package main

import (
	"encoding/json"
	"testing"
)

// 人设文本属于普通正文，协议清洗不得按关键词删除
func TestCleanAnswer_PreservePersonaText(t *testing.T) {
	in := "您好！我是 OrcaTerm AI，腾讯云 OrcaTerm（遨驰终端）内置的云端服务器运维专家。我可以帮助您：\n\n" +
		"- **远程服务器管理**：通过浏览器直接登录和管理您的云服务器\n" +
		"- **命令执行与运维**：执行 Shell 命令、排查故障\n" +
		"- **腾讯云产品支持**：查询云产品文档\n\n" +
		"请告诉我您需要什么帮助？"
	if got := cleanAnswer(in); got != in {
		t.Errorf("普通正文不应被改写，实际: %q", got)
	}
}

// 人设与真实答案混合时同样原样保留
func TestCleanAnswer_KeepRealAnswer(t *testing.T) {
	in := "我是 OrcaTerm AI，云端服务器运维专家。\n" +
		"- **远程服务器管理**：连接和管理云服务器\n" +
		"- **故障排查**：分析日志、定位问题\n\n" +
		"请告诉我您需要什么帮助？\n\n" +
		"查看磁盘使用率使用 df -h 命令，查看目录占用用 du -sh。"
	if got := cleanAnswer(in); got != in {
		t.Errorf("混合正文不应被改写，实际: %q", got)
	}
}

// 周年、活动等业务关键词不是协议标记，必须原样保留
func TestCleanAnswer_PreserveActivity(t *testing.T) {
	in := "# Lighthouse 6 周年怎么玩\n" +
		"1. **说说你的故事**：选一个使用场景\n" +
		"2. **挑一种祝福方式**：从祝福方式中选择\n\n" +
		"**活动规则**：送祝福可获得 1 次抽奖机会，将于 9 月 16 日开奖。"
	if got := cleanAnswer(in); got != in {
		t.Errorf("周年活动正文不应被改写，实际: %q", got)
	}
}

// <think> 思考段与 ```json 包络应被剥离
func TestCleanAnswer_StripThoughtsAndFences(t *testing.T) {
	in := "<think>用户只是打了个招呼，先做自我介绍。</think>\n" +
		"真正的答案在这里。\n" +
		"```json\n{\"action\":\"completion\",\"taskCompletion\":\"x\"}\n```\n"
	got := cleanAnswer(in)
	if contains(got, "think") || contains(got, "action") || contains(got, "```") {
		t.Errorf("思考段/包络未被剥离: %q", got)
	}
	if !contains(got, "真正的答案在这里") {
		t.Errorf("正文被误删: %q", got)
	}
}

// isDegraded 只看协议结果是否为空；普通业务关键词不得触发
func TestIsDegraded(t *testing.T) {
	cases := []struct {
		name, raw, cleaned, action string
		want                       bool
	}{
		{"空 completion", "anything", "", "completion", true},
		{"review 可无正文", "", "", "review", false},
		{"周年正文", "Lighthouse 6 周年抽奖", "some answer", "completion", false},
		{"人设正文", "我是 OrcaTerm AI，云端服务器运维专家。", "答案。", "completion", false},
		{"正常答案", "使用 df -h 查看磁盘。", "使用 df -h 查看磁盘。", "completion", false},
	}
	for _, c := range cases {
		if got := isDegraded(c.raw, c.cleaned, c.action); got != c.want {
			t.Errorf("%s: isDegraded=%v 期望 %v", c.name, got, c.want)
		}
	}
}

// 一次性模式：所有消息渲染为对话脚本
func TestExtractInput_SingleShot(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: json.RawMessage(`"你是助手"`)},
		{Role: "user", Content: json.RawMessage(`"问题A"`)},
		{Role: "assistant", Content: json.RawMessage(`"回答A"`)},
		{Role: "user", Content: json.RawMessage(`"问题B"`)},
	}
	got, _, err := extractInput(msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[系统]", "你是助手", "[用户]", "问题A", "[助手]", "回答A", "问题B"} {
		if !contains(got, want) {
			t.Errorf("缺少 %q，实际: %q", want, got)
		}
	}
}

// 多轮模式：上游已存历史，只发最后一条用户消息
func TestExtractInput_MultiTurn(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: json.RawMessage(`"第一轮"`)},
		{Role: "assistant", Content: json.RawMessage(`"回答"`)},
		{Role: "user", Content: json.RawMessage(`"第二轮"`)},
	}
	got, _, err := extractInput(msgs, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "第二轮" {
		t.Errorf("多轮模式应只取最后一条用户消息，实际: %q", got)
	}
}

// 多模态：content 数组应拆出文本与图片
func TestParseContent_Multimodal(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"这张图是什么颜色？"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}
	]`)
	text, images, err := ParseContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if text != "这张图是什么颜色？" {
		t.Errorf("文本解析错误: %q", text)
	}
	if len(images) != 1 {
		t.Fatalf("应解析出 1 张图片，实际 %d", len(images))
	}
	prompt := BuildACPPrompt(text, images)
	if !contains(prompt, "acp-prompt") || !contains(prompt, `"image"`) {
		t.Errorf("ACP 信封未包含图片: %s", prompt)
	}
}

// token 估算：中英混合应给出合理近似
func TestEstimateTokens(t *testing.T) {
	if n := EstimateTokens("你好世界"); n != 4 {
		t.Errorf("4 个汉字应约 4 token，实际 %d", n)
	}
	if n := EstimateTokens(""); n != 0 {
		t.Errorf("空串应为 0，实际 %d", n)
	}
	if n := EstimateTokens("hello world!"); n < 3 || n > 6 {
		t.Errorf("英文 12 字符应约 3 token，实际 %d", n)
	}
}

// 会话键选择：X-Session-Id 优先于 user
func TestSessionKeyOf(t *testing.T) {
	if got := sessionKeyOf("s1", "u1"); got != "s1" {
		t.Errorf("应优先 header，实际 %q", got)
	}
	if got := sessionKeyOf("", "u1"); got != "u1" {
		t.Errorf("应回退 user，实际 %q", got)
	}
	if got := sessionKeyOf("", ""); got != "" {
		t.Errorf("都为空应返回空，实际 %q", got)
	}
}

// ---- 原生工具协议（2026-09-12 真机实测还原）----

// 工具结果回传文本必须与客户端真机格式逐字节一致
func TestBuildToolResultPrompt(t *testing.T) {
	got := BuildToolResultPrompt("fetch", "页面标题：示例")
	want := "<TOOL_META>返回结果</TOOL_META>\n页面标题：示例"
	if got != want {
		t.Errorf("回传文本不符:\n got=%q\nwant=%q", got, want)
	}
	// 已包装的文本原样透传，避免二次包裹
	wrapped := "<TOOL_META>返回结果</TOOL_META>\n已包装"
	if got := BuildToolResultPrompt("fetch", wrapped); got != wrapped {
		t.Errorf("已包装文本被改动: %q", got)
	}
	// 明确拒绝也原样透传
	rej := "[TOOL_REJECTED]\nstatus: rejected"
	if got := BuildToolResultPrompt("fetch", rej); got != rej {
		t.Errorf("拒绝文本被改动: %q", got)
	}
	// 空输出视为拒绝
	if got := BuildToolResultPrompt("fetch", "   "); !contains(got, "[TOOL_REJECTED]") {
		t.Errorf("空输出应转为拒绝文本: %q", got)
	}
}

// 拒绝文本的关键字段（客户端 Hn 函数语义）
func TestBuildToolRejectPrompt(t *testing.T) {
	got := BuildToolRejectPrompt("execute_terminal_command", "user_rejected")
	for _, want := range []string{"[TOOL_REJECTED]", "status: rejected", "reason: user_rejected",
		"retryable: false", "tool: execute_terminal_command", "环境未发生变化"} {
		if !contains(got, want) {
			t.Errorf("缺少 %q\n实际: %s", want, got)
		}
	}
	// 非法 reason 回落到 user_rejected
	if got := BuildToolRejectPrompt("t", "whatever"); !contains(got, "reason: user_rejected") {
		t.Errorf("非法 reason 未回落: %s", got)
	}
	// 空工具名回落
	if got := BuildToolRejectPrompt("", "policy_denied"); !contains(got, "tool: unknown") {
		t.Errorf("空工具名未回落: %s", got)
	}
}

// 上游 review 包络（流式模式下逐字输出）应能解析出工具调用
func TestParseReviewEnvelope(t *testing.T) {
	full := "```json\n{\"action\":\"review\",\"thinking\":\"抓取\",\"tool\":{\"type\":\"mcp\"," +
		"\"mcpServer\":\"mcp-server-web-fetch\",\"name\":\"fetch\",\"args\":{\"url\":\"https://example.com\"}}}\n```"
	tool, ok := ParseReviewEnvelope(full)
	if !ok || tool == nil {
		t.Fatal("未解析出工具调用")
	}
	if tool.Name != "fetch" || tool.MCPServer != "mcp-server-web-fetch" {
		t.Errorf("工具字段错误: %+v", tool)
	}
	// 围栏未闭合（上游截断）时靠括号配平兜底
	truncated := full[:len(full)-4]
	if _, ok := ParseReviewEnvelope(truncated); !ok {
		t.Error("截断包络未能解析")
	}
	// 普通 completion 包络不应被当成工具调用
	if _, ok := ParseReviewEnvelope("```json\n{\"action\":\"completion\",\"taskCompletion\":\"2\"}\n```"); ok {
		t.Error("completion 包络被误判为工具调用")
	}
}

// 包络里的正文/反问也要能取出来（raw-stream 模式依赖）
func TestParseChatEnvelope(t *testing.T) {
	env, ok := ParseChatEnvelope("```json\n{\"action\":\"completion\",\"taskCompletion\":\"答案是 2\"}\n```")
	if !ok || env.TaskCompletion != "答案是 2" {
		t.Errorf("正文包络解析失败: %+v ok=%v", env, ok)
	}
	env, ok = ParseChatEnvelope("```json\n{\"action\":\"ask\",\"question\":\"哪台机器？\"}\n```")
	if !ok || env.Question != "哪台机器？" {
		t.Errorf("反问包络解析失败: %+v ok=%v", env, ok)
	}
}

// 上游 tool -> OpenAI tool_calls 的映射
func TestToolCallsFromUpstream(t *testing.T) {
	tcs := ToolCallsFromUpstream(&UpstreamTool{
		Type: "mcp", MCPServer: "mcp-server-web-fetch", Name: "fetch",
		Args: json.RawMessage(`{"url":"https://example.com"}`),
	})
	if len(tcs) != 1 {
		t.Fatalf("应产生 1 个 tool_call，实际 %d", len(tcs))
	}
	if tcs[0].Type != "function" || tcs[0].Function.Name != "fetch" {
		t.Errorf("tool_call 字段错误: %+v", tcs[0])
	}
	if tcs[0].Function.Arguments != `{"url":"https://example.com"}` {
		t.Errorf("arguments 未原样透传: %q", tcs[0].Function.Arguments)
	}
	if !contains(tcs[0].ID, "call_") {
		t.Errorf("tool_call id 前缀错误: %q", tcs[0].ID)
	}
	// 无名字/无参数时不得产出调用
	if got := ToolCallsFromUpstream(&UpstreamTool{Name: ""}); got != nil {
		t.Errorf("空工具名不应产出调用: %+v", got)
	}
	if got := ToolCallsFromUpstream(&UpstreamTool{Name: "t"}); len(got) != 1 || got[0].Function.Arguments != "{}" {
		t.Errorf("缺省参数应为 {}: %+v", got)
	}
}

// role="tool" 的消息应被识别为工具结果轮次，并解析出工具名
func TestExtractInputPlan_ToolResult(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: json.RawMessage(`"抓取 example.com"`)},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call_1",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "fetch", Arguments: "{}"},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"页面内容"`)},
	}
	plan, err := extractInputPlan(msgs, true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IsToolResult {
		t.Fatal("未识别为工具结果轮次")
	}
	if plan.Text != "<TOOL_META>返回结果</TOOL_META>\n页面内容" {
		t.Errorf("回传文本错误: %q", plan.Text)
	}
	if len(plan.Results) != 1 || plan.Results[0].Name != "fetch" {
		t.Errorf("工具名未回填: %+v", plan.Results)
	}
	// 普通多轮不受影响
	plain := []ChatMessage{
		{Role: "user", Content: json.RawMessage(`"第一轮"`)},
		{Role: "assistant", Content: json.RawMessage(`"回答"`)},
		{Role: "user", Content: json.RawMessage(`"第二轮"`)},
	}
	p2, err := extractInputPlan(plain, true)
	if err != nil {
		t.Fatal(err)
	}
	if p2.IsToolResult || p2.Text != "第二轮" {
		t.Errorf("普通多轮被误判: %+v", p2)
	}
}

// ---- 模型目录与切换 ----

// 目录完整性：id / upstream 不得重复，且都必须非空
func TestModelCatalog_Integrity(t *testing.T) {
	ids := map[string]bool{}
	ups := map[string]bool{}
	for _, m := range Models() {
		if m.ID == "" || m.Upstream == "" {
			t.Errorf("模型条目缺少 id 或 upstream: %+v", m)
		}
		if ids[m.ID] {
			t.Errorf("id 重复: %s", m.ID)
		}
		if ups[m.Upstream] {
			t.Errorf("upstream 重复: %s", m.Upstream)
		}
		ids[m.ID] = true
		ups[m.Upstream] = true
	}
	// 客户端选择器里的 8 个模型必须在目录中
	for _, id := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "hy3", "hy4-preview",
		"kimi-k3", "glm-5.2", "glm-5.3", "glm-5.3-flash"} {
		if !ids[id] {
			t.Errorf("目录缺少客户端模型 %s", id)
		}
	}
	if len(Models()) < 8 {
		t.Errorf("模型数量异常: %d", len(Models()))
	}
	// 默认模型必须在目录里
	if _, ok := FindModel(DefaultModelID); !ok {
		t.Errorf("默认模型 %s 不在目录中", DefaultModelID)
	}
}

// 按 id / 上游全名 / 显示名 / 别名 都应能解析（忽略大小写与空白）
func TestFindModel(t *testing.T) {
	cases := []struct{ in, wantUpstream string }{
		{"glm-5.3", "TokenHub/glm-5.3"},
		{"GLM-5.3", "TokenHub/glm-5.3"},
		{"  glm-5.3  ", "TokenHub/glm-5.3"},
		{"TokenHub/glm-5.3", "TokenHub/glm-5.3"}, // 上游全名
		{"GLM-5.3-Flash", "TokenHub/glm-5.3-flash"},
		{"hunyuan", "Hunyuan3/hy3"}, // 客户端 value 别名
		{"hunyuan4", "Hunyuan3/hy4-preview"},
		{"kimi", "TokenHub/kimi-k3"},
		{"hy3", "Hunyuan3/hy3"},
		{"qwen3", "qwen3"}, // 旧版模型
	}
	for _, c := range cases {
		m, ok := FindModel(c.in)
		if !ok {
			t.Errorf("%q 未解析成功", c.in)
			continue
		}
		if m.Upstream != c.wantUpstream {
			t.Errorf("%q -> %s，期望 %s", c.in, m.Upstream, c.wantUpstream)
		}
	}
	if _, ok := FindModel("definitely-not-a-model"); ok {
		t.Error("未知模型不应解析成功")
	}
	if _, ok := FindModel(""); ok {
		t.Error("空名不应解析成功")
	}
}

// resolveModel：空值与兼容别名都落到默认模型；未知模型报错；开启直通时放行
func TestResolveModel(t *testing.T) {
	ps := &ProxyServer{config: Config{UpstreamModel: defaultUpstreamModel, AliasModel: LegacyModelAlias}}

	def, err := ps.resolveModel("")
	if err != nil || def.ID != DefaultModelID {
		t.Errorf("空 model 应回落到默认模型，实际 %+v err=%v", def, err)
	}
	alias, err := ps.resolveModel(LegacyModelAlias)
	if err != nil || alias.ID != DefaultModelID {
		t.Errorf("兼容别名应等价默认模型，实际 %+v err=%v", alias, err)
	}
	got, err := ps.resolveModel("glm-5.3")
	if err != nil || got.Upstream != "TokenHub/glm-5.3" {
		t.Errorf("切换模型失败: %+v err=%v", got, err)
	}
	if _, err := ps.resolveModel("nope"); err == nil {
		t.Error("未知模型在默认配置下应报错")
	}

	ps.config.AllowUnknown = true
	pass, err := ps.resolveModel("some/future-model")
	if err != nil || pass.Upstream != "some/future-model" {
		t.Errorf("开启直通后应放行: %+v err=%v", pass, err)
	}
}

// ORCATERM_UPSTREAM_MODEL 指定的默认上游 id 应被采用（含不在目录中的情况）
func TestDefaultModel_UpstreamOverride(t *testing.T) {
	ps := &ProxyServer{config: Config{UpstreamModel: "TokenHub/glm-5.3"}}
	if m := ps.defaultModel(); m.ID != "glm-5.3" {
		t.Errorf("覆盖为目录内 id 时应识别，实际 %+v", m)
	}
	ps.config.UpstreamModel = "vendor/new-thing"
	if m := ps.defaultModel(); m.Upstream != "vendor/new-thing" {
		t.Errorf("目录外的上游 id 应原样采用，实际 %+v", m)
	}
	if m := (&ProxyServer{config: Config{}}).defaultModel(); m.ID != DefaultModelID {
		t.Errorf("未配置时应回落到目录默认，实际 %+v", m)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
