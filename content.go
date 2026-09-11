package main

import (
	"encoding/json"
	"strings"
)

// ===== OpenAI 消息内容解析（支持多模态 content 数组）=====

type ImageURL struct {
	URL string `json:"url"`
}

// ContentPart OpenAI content 数组中的元素（text / image_url）。
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text"`
	ImageURL *ImageURL `json:"image_url"`
}

// ParseContent 解析 message.content，兼容字符串与分部数组两种形态。
func ParseContent(raw json.RawMessage) (text string, images []ContentPart, err error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil, nil
	}
	// 形态一：纯字符串
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", nil, err
		}
		return s, nil, nil
	}
	// 形态二：分部数组
	if strings.HasPrefix(trimmed, "[") {
		var parts []ContentPart
		if err := json.Unmarshal(raw, &parts); err != nil {
			return "", nil, err
		}
		var sb strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "":
				sb.WriteString(p.Text)
			case "image_url":
				if p.ImageURL != nil && p.ImageURL.URL != "" {
					images = append(images, p)
				}
			}
		}
		return sb.String(), images, nil
	}
	return "", nil, nil
}

// splitDataURI 从 data:image/png;base64,xxx 中拆出 mime 与 base64 载荷。
func splitDataURI(u string) (mime, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(u, prefix) {
		return "", "", false
	}
	rest := u[len(prefix):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta, data := rest[:comma], rest[comma+1:]
	if i := strings.Index(meta, ";base64"); i >= 0 {
		mime = meta[:i]
		return mime, data, true
	}
	return "", "", false
}

// BuildACPPrompt 构造 ACP 信封。
// 实测上游支持两种图片形态：内联 base64 用 ACP 的 image，远程地址用 OpenAI 风格 image_url。
func BuildACPPrompt(text string, images []ContentPart) string {
	prompt := []map[string]interface{}{{"type": "text", "text": text}}
	for _, img := range images {
		u := img.ImageURL.URL
		if mime, data, ok := splitDataURI(u); ok {
			prompt = append(prompt, map[string]interface{}{
				"type": "image", "data": data, "mimeType": mime,
			})
			continue
		}
		prompt = append(prompt, map[string]interface{}{
			"type": "image_url", "image_url": map[string]string{"url": u},
		})
	}
	b, _ := json.Marshal(map[string]interface{}{
		"_format": "acp-prompt",
		"version": 1,
		"prompt":  prompt,
	})
	return string(b)
}

// ===== token 估算（上游不返回 usage，只能本地近似）=====

// EstimateTokens 粗略估算：CJK 字符按 1 token，其余按 4 字符 1 token。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if r > 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// ===== 上游模型目录（2026-09-12 实测逐个验证）=====
//
// 模型表取自客户端前端 AI chunk（module 95201）的常量数组，形如
//
//	{text:"GLM-5.3", value:"glm-5.3", model:"TokenHub/glm-5.3", type:"default",
//	 tags:[{text:"限时免费",type:"free"}], description:"…", capabilities:["推理","代码"]}
//
// 关键结论：聊天请求里 `user.setting.model` 要填的是**表中的 `model` 字段**
// （即 `TokenHub/glm-5.3` 这种全名），不是列表里的 `value`。
// 实测：填 value（如 `deepseek-v4-flash`）上游会静默返回空结果；
// 填一个不存在的 id 同样返回空（code 仍为 0，但 data.action 为 null）。
//
// 因此代理把上游 id 收敛成 OpenAI 风格的短 id，并做别名解析 + 未知名校验，
// 避免调用方拿到"看起来成功其实是空回答"的结果。

// ModelInfo 一个可用模型的元数据。
type ModelInfo struct {
	ID           string   // 对外 id（OpenAI 风格短名）
	Upstream     string   // 上游 user.setting.model 的真实取值
	Name         string   // 客户端里的显示名
	Description  string   // 官方描述
	Capabilities []string // 能力标签
	Free         bool     // 是否限时免费
	Legacy       bool     // true = 客户端选择器已不展示，但实测仍可用
	aliases      []string // 额外可接受的写法
}

// DefaultModelID 默认模型：与 v0.7.0 之前的行为保持一致。
const DefaultModelID = "deepseek-v4-flash"

// LegacyModelAlias 兼容旧版文档/调用方使用的模型名，等价于"默认模型"。
const LegacyModelAlias = "orcaterm-assistant"

// modelCatalog 可用模型表。前 8 个与客户端模型选择器一一对应。
var modelCatalog = []ModelInfo{
	{
		ID: "deepseek-v4-pro", Upstream: "TokenHub/deepseek-v4-pro", Name: "Deepseek-V4-Pro",
		Description: "1M 上下文，擅长长程任务与复杂推理", Capabilities: []string{"推理", "长上下文"}, Free: true,
	},
	{
		ID: "deepseek-v4-flash", Upstream: "TokenHub/deepseek-v4-flash", Name: "DeepSeek-V4-Flash",
		Description: "DeepSeek 轻量版，响应更快，适合日常高频任务", Capabilities: []string{"推理", "长上下文"}, Free: true,
	},
	{
		ID: "hy3", Upstream: "Hunyuan3/hy3", Name: "Hy3",
		Description: "腾讯混元，中文理解与内容创作表现优异", Capabilities: []string{"推理"}, Free: true,
		aliases: []string{"hunyuan"},
	},
	{
		ID: "hy4-preview", Upstream: "Hunyuan3/hy4-preview", Name: "Hy4-preview",
		Description:  "腾讯混元新一代预览模型，复杂任务与长文本表现突出",
		Capabilities: []string{"推理", "长上下文"}, Free: true,
		aliases: []string{"hunyuan4"},
	},
	{
		ID: "kimi-k3", Upstream: "TokenHub/kimi-k3", Name: "Kimi-K3",
		Description:  "1M 上下文，擅长处理复杂的长程自主任务，前端开发能力突出",
		Capabilities: []string{"长上下文"}, Free: true,
		aliases: []string{"kimi"},
	},
	{
		ID: "glm-5.2", Upstream: "TokenHub/glm-5.2", Name: "GLM-5.2",
		Description: "智谱 GLM，代码生成与工具调用能力强", Capabilities: []string{"推理", "代码"}, Free: true,
	},
	{
		ID: "glm-5.3", Upstream: "TokenHub/glm-5.3", Name: "GLM-5.3",
		Description: "智谱 GLM，代码生成与工具调用能力强", Capabilities: []string{"推理", "代码"}, Free: true,
	},
	{
		ID: "glm-5.3-flash", Upstream: "TokenHub/glm-5.3-flash", Name: "GLM-5.3-Flash",
		Description:  "GLM 轻量版，兼顾效果与速度，适合高频调用场景",
		Capabilities: []string{"推理", "代码"}, Free: true,
	},

	// ---- 以下为客户端选择器已不展示、但实测仍可用的模型（供需要时选用）----
	{
		ID: "deepseek-chat-latest", Upstream: "deepseek-chat-latest", Name: "DeepSeek Chat Latest",
		Description: "旧版 DeepSeek 对话模型", Legacy: true,
	},
	{
		ID: "glm-4.7", Upstream: "glm-4.7", Name: "GLM-4.7",
		Description: "旧版智谱 GLM", Capabilities: []string{"推理", "代码"}, Legacy: true,
	},
	{
		ID: "kimi-k2", Upstream: "kimi-k2", Name: "Kimi-k2",
		Description: "旧版 Kimi", Legacy: true,
	},
	{
		ID: "kimi-k2.5", Upstream: "kimi-k2.5", Name: "Kimi-k2.5",
		Description: "Kimi k2.5", Legacy: true,
	},
	{
		ID: "kimi-k2-thinking", Upstream: "kimi-k2-thinking", Name: "Kimi-k2-thinking",
		Description: "Kimi k2 深度思考版", Legacy: true,
	},
	{
		ID: "kimi-k2.5-thinking", Upstream: "kimi-k2.5-thinking", Name: "Kimi-k2.5-thinking",
		Description: "Kimi k2.5 深度思考版", Legacy: true,
	},
	{
		ID: "hunyuan-t1", Upstream: "hunyuan-t1", Name: "Hunyuan-T1",
		Description: "腾讯混元 T1", Legacy: true,
	},
	{
		ID: "hunyuan-turbos", Upstream: "hunyuan-turbos", Name: "Hunyuan-Turbos",
		Description: "腾讯混元 TurboS", Legacy: true,
	},
	{
		ID: "qwen3", Upstream: "qwen3", Name: "Qwen3",
		Description: "通义千问 3", Legacy: true,
	},
}

// Models 返回完整模型目录副本。
func Models() []ModelInfo {
	out := make([]ModelInfo, len(modelCatalog))
	copy(out, modelCatalog)
	return out
}

// FindModel 按 id / 上游全名 / 显示名 / 别名解析模型（忽略大小写与首尾空白）。
func FindModel(name string) (ModelInfo, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ModelInfo{}, false
	}
	for _, m := range modelCatalog {
		if strings.ToLower(m.ID) == n || strings.ToLower(m.Upstream) == n || strings.ToLower(m.Name) == n {
			return m, true
		}
		for _, a := range m.aliases {
			if strings.ToLower(a) == n {
				return m, true
			}
		}
	}
	return ModelInfo{}, false
}

// DefaultModel 返回默认模型；目录缺失时兜底成一个裸条目。
func DefaultModel() ModelInfo {
	if m, ok := FindModel(DefaultModelID); ok {
		return m
	}
	return ModelInfo{ID: DefaultModelID, Upstream: DefaultModelID, Name: DefaultModelID}
}

// ModelIDs 返回全部对外 id，用于错误提示。
func ModelIDs() []string {
	out := make([]string, 0, len(modelCatalog))
	for _, m := range modelCatalog {
		out = append(out, m.ID)
	}
	return out
}

// ===== 上游原生工具调用（真机协议，实测还原）=====
//
// 关键结论（2026-09-12 实测）：
//
//  1. 上游**确实会返回工具调用**，形如
//     {"code":0,"data":{"action":"review","thinking":"…","tool":{
//     "type":"mcp","mcpServer":"mcp-server-web-fetch","name":"fetch",
//     "description":"…","args":{"url":"https://example.com"}}}}
//     即 action=review + data.tool。此前代理把它当"退化输出"丢弃，这是 tools 不可用的根因。
//
//  2. 工具结果**用纯文本回传**（不需要特殊 ACP 类型），格式取自客户端真机实现：
//     kl = (label, out) => "<TOOL_META>" + label + "</TOOL_META>\n" + out
//     客户端调用 kl("返回结果" + 终端切换提示, 输出)，因此实际发送的正文是
//     "<TOOL_META>返回结果</TOOL_META>\n<工具输出>"
//
//  3. 拒绝回传同样走文本（客户端 Hn 函数），格式见 buildToolRejectPrompt。
//
//  4. 自定义工具**无法注册**：setting.tools 的三种 schema（内联定义 / OpenAI 格式 /
//     纯名字数组）、selectedUITools、以及提示词注入原生 review 格式，实测均被上游
//     服务端系统提示词压过（模型固定回答"环境中没有该工具"）。因此本代理走"原生工具
//     透传"路线——与真实客户端能力完全一致。

// UpstreamTool 上游 data.tool 的结构。
type UpstreamTool struct {
	Type        string          `json:"type"` // mcp / ui / function / system
	MCPServer   string          `json:"mcpServer"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Args        json.RawMessage `json:"args"`
}

const (
	toolMetaOpen  = "<TOOL_META>"
	toolMetaClose = "</TOOL_META>"
	// toolResultLabel 真机在 TOOL_META 内写入的固定标签。
	toolResultLabel = "返回结果"
)

// chatEnvelope 上游在流式模式下逐字输出的内部动作包络（```json {…} ```）。
type chatEnvelope struct {
	Action         string        `json:"action"`
	Thinking       string        `json:"thinking"`
	TaskCompletion string        `json:"taskCompletion"`
	Question       string        `json:"question"`
	Tool           *UpstreamTool `json:"tool"`
}

// BuildToolResultPrompt 构造工具结果回传文本，与客户端真机格式一致。
//
// toolName 仅在输出为空（视为拒绝）时用于生成拒绝文本；output 已是协议文本时原样透传。
func BuildToolResultPrompt(toolName, output string) string {
	out := strings.TrimSpace(output)
	if out == "" {
		return BuildToolRejectPrompt(toolName, "user_rejected")
	}
	// 调用方已按协议包装（或明确拒绝）时不再重复包装
	if strings.HasPrefix(out, toolMetaOpen) || strings.HasPrefix(out, "[TOOL_REJECTED]") {
		return out
	}
	return toolMetaOpen + toolResultLabel + toolMetaClose + "\n" + out
}

// BuildToolRejectPrompt 构造"工具未执行"的回传文本（对应客户端真机 Hn 函数）。
func BuildToolRejectPrompt(toolName, reason string) string {
	if reason != "user_rejected" && reason != "policy_denied" {
		reason = "user_rejected"
	}
	note := "权限规则或平台策略拒绝了该工具调用，不得尝试绕过策略。"
	if reason == "user_rejected" {
		note = "用户明确拒绝了该工具调用。"
	}
	if strings.TrimSpace(toolName) == "" {
		toolName = "unknown"
	}
	var sb strings.Builder
	sb.WriteString("[TOOL_REJECTED]\n")
	sb.WriteString("status: rejected\n")
	sb.WriteString("reason: " + reason + "\n")
	sb.WriteString("retryable: false\n")
	sb.WriteString("tool: " + toolName + "\n\n")
	sb.WriteString(note + "\n")
	sb.WriteString("该工具调用未执行，环境未发生变化。\n")
	sb.WriteString("本轮任务中不要再次调用相同工具、相同参数或等价操作。\n")
	sb.WriteString("如无法采用不需要该权限的方式继续，请停止操作并等待用户进一步指示。")
	return sb.String()
}

// ToolCallsFromUpstream 把上游 review 的 tool 映射为 OpenAI tool_calls。
func ToolCallsFromUpstream(t *UpstreamTool) []ToolCall {
	if t == nil || strings.TrimSpace(t.Name) == "" {
		return nil
	}
	args := "{}"
	if len(t.Args) > 0 {
		if s := strings.TrimSpace(string(t.Args)); s != "" && s != "null" {
			args = s
		}
	}
	tc := ToolCall{ID: "call_" + newUUID(), Type: "function"}
	tc.Function.Name = t.Name
	tc.Function.Arguments = args
	return []ToolCall{tc}
}

// ParseChatEnvelope 从 raw-stream 聚合文本中解析 ```json {…} ``` 包络。
func ParseChatEnvelope(text string) (*chatEnvelope, bool) {
	for _, cand := range fenceCandidates(text) {
		var env chatEnvelope
		if err := json.Unmarshal([]byte(cand), &env); err != nil {
			continue
		}
		if env.Action == "" {
			continue
		}
		return &env, true
	}
	return nil, false
}

// ParseReviewEnvelope 只在包络是工具调用（action=review + tool.name 非空）时返回。
func ParseReviewEnvelope(text string) (*UpstreamTool, bool) {
	env, ok := ParseChatEnvelope(text)
	if !ok || env.Action != "review" || env.Tool == nil || strings.TrimSpace(env.Tool.Name) == "" {
		return nil, false
	}
	return env.Tool, true
}

// fenceCandidates 截出所有 ```json … ``` 块，以及末尾平衡的 JSON 对象，供解析尝试。
// 上游流式输出可能被截断（围栏未闭合），因此额外用括号配平兜底。
func fenceCandidates(s string) []string {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, "```")
		if i < 0 {
			break
		}
		tail := rest[i+3:]
		if j := strings.Index(tail, "```"); j >= 0 {
			if b := trimToBalancedObject(tail[:j]); b != "" {
				out = append(out, b)
			}
			rest = tail[j+3:]
			continue
		}
		if b := trimToBalancedObject(tail); b != "" {
			out = append(out, b)
		}
		break
	}
	if b := trimToBalancedObject(s); b != "" {
		out = append(out, b)
	}
	return out
}

// trimToBalancedObject 从首个 '{' 起截到与之配平的 '}'（忽略字符串内的括号）。
func trimToBalancedObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// ===== 调用方消息 -> 上游输入（含工具结果轮次识别）=====

// ToolResultMessage 调用方回传的一条工具结果。
type ToolResultMessage struct {
	Name    string
	Content string
}

// InputPlan 本轮发往上游的输入方案。
type InputPlan struct {
	Text         string
	Images       []ContentPart
	IsToolResult bool // 本轮是"工具结果回传"，用于续跑上游同一会话
	Results      []ToolResultMessage
}

// extractInputPlan 在 extractInput 之上识别工具结果轮次。
//
// OpenAI 约定：工具结果以 role="tool" 的消息紧跟在触发它的 assistant tool_calls 之后。
// 命中时不再走普通问答路径，而是按真机格式回传结果并续跑同一上游会话。
func extractInputPlan(msgs []ChatMessage, multiTurn bool) (*InputPlan, error) {
	if results, ok := trailingToolResults(msgs); ok {
		parts := make([]string, 0, len(results))
		for _, r := range results {
			parts = append(parts, BuildToolResultPrompt(r.Name, r.Content))
		}
		return &InputPlan{
			Text:         strings.Join(parts, "\n\n"),
			IsToolResult: true,
			Results:      results,
		}, nil
	}
	text, images, err := extractInput(msgs, multiTurn)
	if err != nil {
		return nil, err
	}
	return &InputPlan{Text: text, Images: images}, nil
}

// trailingToolResults 取末尾连续的 role="tool" 消息（可能有多个）。
// 第二个返回值为 false 表示本轮不是工具结果回传。
func trailingToolResults(msgs []ChatMessage) ([]ToolResultMessage, bool) {
	var out []ToolResultMessage
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "tool" {
			break
		}
		text, _, err := ParseContent(msgs[i].Content)
		if err != nil {
			text = ""
		}
		out = append([]ToolResultMessage{{
			Name:    toolNameOf(msgs, i),
			Content: text,
		}}, out...)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// toolNameOf 找出触发第 idx 条工具结果的工具名（用于生成拒绝文本的 tool 字段）。
func toolNameOf(msgs []ChatMessage, idx int) string {
	if n := strings.TrimSpace(msgs[idx].Name); n != "" {
		return n
	}
	id := strings.TrimSpace(msgs[idx].ToolCallID)
	for i := idx - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		for _, tc := range msgs[i].ToolCalls {
			if id == "" || tc.ID == id {
				return tc.Function.Name
			}
		}
		if len(msgs[i].ToolCalls) > 0 {
			return msgs[i].ToolCalls[0].Function.Name
		}
		break
	}
	return ""
}

// declaresTool 判断调用方是否声明了某个工具名（用于提示"上游请求了未声明的工具"）。
func declaresTool(tools []Tool, name string) bool {
	for _, t := range tools {
		if strings.EqualFold(strings.TrimSpace(t.Function.Name), strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

// ===== tools 降级（上游不执行自定义 function）=====

// Tool OpenAI tools 定义
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall OpenAI 风格的调用结果
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolsInstruction 把 tools 定义转成提示词片段，要求模型以 JSON 返回调用意图。
func ToolsInstruction(tools []Tool) string {
	var sb strings.Builder
	sb.WriteString("\n\n[可用工具]\n")
	sb.WriteString("你可以从下列工具中选择调用。若需要调用工具，请只输出如下 JSON（不要有其他文字）：\n")
	sb.WriteString("{\"tool_calls\":[{\"name\":\"工具名\",\"arguments\":{...}}]}\n\n")
	for _, t := range tools {
		sb.WriteString("- 名称：" + t.Function.Name + "\n")
		if t.Function.Description != "" {
			sb.WriteString("  说明：" + t.Function.Description + "\n")
		}
		if len(t.Function.Parameters) > 0 {
			sb.WriteString("  参数：" + string(t.Function.Parameters) + "\n")
		}
	}
	sb.WriteString("\n若无需调用工具，直接正常回答即可。")
	return sb.String()
}

// ExtractToolCalls 从模型输出中尝试解析 tool_calls，返回调用列表与剥离后的正文。
func ExtractToolCalls(text string) ([]ToolCall, string) {
	cleaned := stripFencedJSON(text)
	candidates := []string{cleaned}
	// 也尝试从首个 '{' 起解析
	if i := strings.Index(cleaned, "{"); i >= 0 {
		candidates = append(candidates, cleaned[i:])
	}
	for _, c := range candidates {
		var payload struct {
			ToolCalls []struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(c)), &payload); err != nil {
			continue
		}
		if len(payload.ToolCalls) == 0 {
			continue
		}
		out := make([]ToolCall, 0, len(payload.ToolCalls))
		for _, c2 := range payload.ToolCalls {
			var tc ToolCall
			tc.ID = "call_" + newUUID()
			tc.Type = "function"
			tc.Function.Name = c2.Name
			if b, err := json.Marshal(c2.Arguments); err == nil {
				tc.Function.Arguments = string(b)
			} else {
				tc.Function.Arguments = "{}"
			}
			out = append(out, tc)
		}
		return out, strings.TrimSpace(cleaned)
	}
	return nil, cleaned
}
