package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	proxyVersion       = "0.8.0"
	maxUpstreamBody    = 8 << 20
	maxSSEEventData    = 1 << 20
	maxUpstreamRawText = 8 << 20
)

// ===== 配置 =====

// Config 代理配置。默认零配置：自动读取运行中的 OrcaTerm 的 .cookies 作为认证。
type Config struct {
	Port           string        // 监听端口（默认 localhost:8080）
	Cookies        string        // OrcaTerm .cookies 文件路径
	Upstream       string        // ligai 后端地址
	AliasModel     string        // 兼容别名：该名字等价于"默认模型"（默认 orcaterm-assistant）
	Product        string        // X-Product 头
	Origin         string        // Origin 头
	UserAgent      string        // User-Agent 头（默认模拟客户端真实 UA）
	UpstreamModel  string        // 默认模型的上游 id（默认 TokenHub/deepseek-v4-flash）
	AllowUnknown   bool          // 是否允许未在目录中的模型 id 直通上游（默认否）
	Timeouts       int           // 上游请求超时秒数
	Mode           string        // structured：用非流式结构化响应（默认，干净）；raw-stream：直连 SSE
	OnDegraded     string        // 退化输出处理：empty（默认，返回空）/ raw（返回清洗后原文）/ error
	Prompt         string        // 追加在 input 前的用户级覆盖指令（可空禁用）
	Strip          bool          // 是否启用清洗管线
	HideTools      bool          // 是否要求上游隐藏工具消息（hideToolMessage 等三个开关）
	SessionTTL     time.Duration // 会话空闲保活时长（默认 30 分钟）
	SessionMax     int           // 会话表容量上限（默认 1000）
	ToolsMode      string        // tools 处理：native（默认，上游原生工具透传）/ error / inject / ignore
	ContextLimit   int           // 上下文上限，用于估算剩余量（默认 128000）
	CORSOrigins    []string      // 允许跨域访问的精确 Origin；空表示禁用 CORS；"*" 表示任意 Origin
	ConfigWarnings []string      // 环境变量解析警告；validateConfig 会拒绝启动
}

// defaultPromptOverride 默认覆盖指令。
// 实测结论：在上游的意图识别环节，附加的指令文本反而会干扰用户消息被正确识别
// （曾出现模型把指令里的"自我介绍"一词误当作用户输入的情形），因此默认不附加任何
// 指令，让原始问题直达上游。如需附加可通过 ORCATERM_PROMPT_OVERRIDE 设置。
const defaultPromptOverride = ""

// defaultUserAgent 客户端真实 UA（抓包取得，含 OrcaTerm 标识）。
const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36"

// defaultUpstreamModel 上游实际使用的模型（抓包取得）。
const defaultUpstreamModel = "TokenHub/deepseek-v4-flash"

func defaultCookiesPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "com.orcaterm-desktop.app", ".cookies")
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "com.orcaterm-desktop.app", ".cookies")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "com.orcaterm-desktop.app", ".cookies")
}

func loadConfig() Config {
	c := Config{
		Port:          "8080",
		Cookies:       defaultCookiesPath(),
		Upstream:      "https://lightai.cloud.tencent.com",
		AliasModel:    LegacyModelAlias,
		Product:       "orcaterm",
		Origin:        "http://tauri.localhost",
		UserAgent:     defaultUserAgent,
		UpstreamModel: defaultUpstreamModel,
		Timeouts:      300,
		Mode:          "structured",
		OnDegraded:    "empty",
		Prompt:        defaultPromptOverride,
		Strip:         true,
		HideTools:     true,
		SessionTTL:    30 * time.Minute,
		SessionMax:    1000,
		ToolsMode:     "native",
		ContextLimit:  128000,
	}
	if v := os.Getenv("PROXY_PORT"); v != "" {
		c.Port = v
	}
	if v := os.Getenv("ORCATERM_COOKIES"); v != "" {
		c.Cookies = v
	}
	if v := os.Getenv("ORCATERM_API_URL"); v != "" {
		c.Upstream = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("ORCATERM_MODEL"); v != "" {
		c.AliasModel = v
	}
	if v := os.Getenv("ORCATERM_PRODUCT"); v != "" {
		c.Product = v
	}
	if v := os.Getenv("ORCATERM_USER_AGENT"); v != "" {
		c.UserAgent = v
	}
	if v := os.Getenv("ORCATERM_UPSTREAM_MODEL"); v != "" {
		c.UpstreamModel = v
	}
	if v, ok := os.LookupEnv("ORCATERM_ALLOW_UNKNOWN_MODEL"); ok {
		v = strings.ToLower(strings.TrimSpace(v))
		c.AllowUnknown = v == "1" || v == "on" || v == "true" || v == "yes"
	}
	if v := os.Getenv("ORCATERM_MODE"); v != "" {
		c.Mode = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("ORCATERM_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			c.ConfigWarnings = append(c.ConfigWarnings, "ORCATERM_TIMEOUT must be an integer")
		} else {
			c.Timeouts = n
		}
	}
	if v := os.Getenv("ORCATERM_ON_DEGRADED"); v != "" {
		c.OnDegraded = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := os.LookupEnv("ORCATERM_PROMPT_OVERRIDE"); ok {
		c.Prompt = v
	}
	if v, ok := os.LookupEnv("ORCATERM_STRIP_PERSONA"); ok {
		v = strings.ToLower(strings.TrimSpace(v))
		c.Strip = !(v == "0" || v == "off" || v == "false" || v == "no")
	}
	if v, ok := os.LookupEnv("ORCATERM_HIDE_TOOLS"); ok {
		v = strings.ToLower(strings.TrimSpace(v))
		c.HideTools = !(v == "0" || v == "off" || v == "false" || v == "no")
	}
	if v := os.Getenv("ORCATERM_SESSION_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err != nil {
			c.ConfigWarnings = append(c.ConfigWarnings, "ORCATERM_SESSION_TTL must be a duration")
		} else {
			c.SessionTTL = d
		}
	}
	if v := os.Getenv("ORCATERM_SESSION_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			c.ConfigWarnings = append(c.ConfigWarnings, "ORCATERM_SESSION_MAX must be an integer")
		} else {
			c.SessionMax = n
		}
	}
	if v := os.Getenv("ORCATERM_TOOLS_MODE"); v != "" {
		c.ToolsMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("ORCATERM_CONTEXT_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err != nil {
			c.ConfigWarnings = append(c.ConfigWarnings, "ORCATERM_CONTEXT_LIMIT must be an integer")
		} else {
			c.ContextLimit = n
		}
	}
	if v, ok := os.LookupEnv("ORCATERM_CORS_ORIGINS"); ok {
		for _, raw := range strings.Split(v, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			origin, err := normalizeOrigin(raw)
			if err != nil {
				c.ConfigWarnings = append(c.ConfigWarnings, err.Error())
				continue
			}
			c.CORSOrigins = append(c.CORSOrigins, origin)
		}
	}
	return c
}

// normalizeOrigin 规范化 CORS Origin：只允许 http/https 的 scheme://host[:port]，或显式 "*"。
// 尾部斜杠、路径、查询串与片段都会被拒绝，避免出现"看起来允许实际不匹配"的配置。
func normalizeOrigin(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if s == "*" {
		return "*", nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("ORCATERM_CORS_ORIGINS entry %q is not a valid origin", s)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("ORCATERM_CORS_ORIGINS entry %q must use http or https", s)
	}
	if u.Host == "" {
		return "", fmt.Errorf("ORCATERM_CORS_ORIGINS entry %q is missing a host", s)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("ORCATERM_CORS_ORIGINS entry %q must not include a path, query, or fragment", s)
	}
	return u.Scheme + "://" + u.Host, nil
}

func validateConfig(c Config) error {
	if len(c.ConfigWarnings) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(c.ConfigWarnings, "; "))
	}
	if c.Mode != "structured" && c.Mode != "raw-stream" {
		return fmt.Errorf("ORCATERM_MODE must be structured or raw-stream, got %q", c.Mode)
	}
	if c.ToolsMode != "native" && c.ToolsMode != "error" && c.ToolsMode != "inject" && c.ToolsMode != "ignore" {
		return fmt.Errorf("ORCATERM_TOOLS_MODE must be native, error, inject, or ignore, got %q", c.ToolsMode)
	}
	if c.OnDegraded != "empty" && c.OnDegraded != "raw" && c.OnDegraded != "error" {
		return fmt.Errorf("ORCATERM_ON_DEGRADED must be empty, raw, or error, got %q", c.OnDegraded)
	}
	if c.Timeouts <= 0 {
		return fmt.Errorf("ORCATERM_TIMEOUT must be a positive number of seconds, got %d", c.Timeouts)
	}
	if c.SessionTTL <= 0 {
		return fmt.Errorf("ORCATERM_SESSION_TTL must be a positive duration, got %s", c.SessionTTL)
	}
	if c.SessionMax <= 0 {
		return fmt.Errorf("ORCATERM_SESSION_MAX must be positive, got %d", c.SessionMax)
	}
	if c.ContextLimit <= 0 {
		return fmt.Errorf("ORCATERM_CONTEXT_LIMIT must be positive, got %d", c.ContextLimit)
	}
	for _, origin := range c.CORSOrigins {
		if origin == "" {
			continue
		}
		if _, err := normalizeOrigin(origin); err != nil {
			return err
		}
	}
	return nil
}

// ===== 凭据：从 .cookies 读取（零配置、自动刷新）=====

type Credentials struct {
	SID    string // ligai 域 sid cookie
	OT     string // ot_session JWT（Bearer）
	UserID string // 从 ot_session JWT 解出的 userId（真实请求需要放在 user.id）
}

func (c Credentials) Ready() bool { return c.SID != "" && c.OT != "" }

// parseJWTUserID 从 ot_session（JWT）的 payload 中取出 userId，无需验签。
func parseJWTUserID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	p := parts[1]
	if pad := len(p) % 4; pad > 0 {
		p += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(p)
		if err != nil {
			return ""
		}
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	switch v := m["userId"].(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		return v
	}
	return ""
}

type CookieProvider struct {
	path    string
	mu      sync.Mutex
	creds   Credentials
	lastMod time.Time
}

func NewCookieProvider(path string) *CookieProvider { return &CookieProvider{path: path} }

// Get 返回当前凭据；.cookies 变更时自动重载。
func (cp *CookieProvider) Get() (Credentials, bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if fi, err := os.Stat(cp.path); err == nil {
		if !fi.ModTime().Equal(cp.lastMod) {
			if err := cp.loadLocked(); err != nil {
				log.Printf("[WARN] 读取 .cookies 失败: %v（继续用旧凭据）", err)
			}
			cp.lastMod = fi.ModTime()
		}
	}
	if cp.creds.SID == "" || cp.creds.OT == "" {
		if err := cp.loadLocked(); err != nil && (cp.creds.SID == "" || cp.creds.OT == "") {
			return cp.creds, false
		}
	}
	return cp.creds, cp.creds.SID != "" && cp.creds.OT != ""
}

func (cp *CookieProvider) loadLocked() error {
	data, err := os.ReadFile(cp.path)
	if err != nil {
		return fmt.Errorf("read .cookies: %w", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(data, &arr); err != nil {
		return fmt.Errorf("parse .cookies: %w", err)
	}
	var sid, ot string
	for _, item := range arr {
		domain := domainOf(item)
		raw, _ := item["raw_cookie"].(string)
		first := firstKVPair(raw)
		k, v := first.k, first.v
		if strings.Contains(domain, "lightai.cloud.tencent.com") && k == "sid" {
			sid = v
		}
		if strings.Contains(domain, "orcaterm.cloud.tencent.com") && k == "ot_session" {
			ot = v
		}
	}
	if sid == "" || ot == "" {
		return fmt.Errorf("cookies 中缺少 ligai sid 或 ot_session（请先登录 OrcaTerm）")
	}
	cp.creds = Credentials{SID: sid, OT: ot, UserID: parseJWTUserID(ot)}
	return nil
}

type kv struct{ k, v string }

func domainOf(item map[string]interface{}) string {
	if d, ok := item["domain"].(map[string]interface{}); ok {
		if v, ok := d["HostOnly"].(string); ok && v != "" {
			return v
		}
		if v, ok := d["Suffix"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstKVPair(raw string) kv {
	if i := strings.Index(raw, ";"); i >= 0 {
		raw = raw[:i]
	}
	raw = strings.TrimSpace(raw)
	if eq := strings.Index(raw, "="); eq >= 0 {
		return kv{k: strings.TrimSpace(raw[:eq]), v: strings.TrimSpace(raw[eq+1:])}
	}
	return kv{}
}

// ===== 模型解析 =====

// defaultModel 返回"默认模型"：优先 ORCATERM_UPSTREAM_MODEL 指定的上游 id，
// 否则用目录中的默认项。
func (ps *ProxyServer) defaultModel() ModelInfo {
	raw := strings.TrimSpace(ps.config.UpstreamModel)
	if raw == "" {
		return DefaultModel()
	}
	if m, ok := FindModel(raw); ok {
		return m
	}
	// 上游 id 不（再）在目录里：构造一个直通条目，保持行为可控
	base := DefaultModel()
	base.Upstream = raw
	return base
}

// resolveModel 把调用方传的 model 解析成上游 id。
//
// 接受：目录 id / 上游全名 / 显示名 / 别名 / 兼容别名（orcaterm-assistant）/ 空值。
// 未知模型默认报错——实测上游对未知 id 会静默返回空回答，宁可显式失败。
// 需要直通时设 ORCATERM_ALLOW_UNKNOWN_MODEL=1。
func (ps *ProxyServer) resolveModel(name string) (ModelInfo, error) {
	n := strings.TrimSpace(name)
	if n == "" || strings.EqualFold(n, ps.config.AliasModel) {
		return ps.defaultModel(), nil
	}
	if m, ok := FindModel(n); ok {
		return m, nil
	}
	if ps.config.AllowUnknown {
		return ModelInfo{ID: n, Upstream: n, Name: n}, nil
	}
	return ModelInfo{}, fmt.Errorf("%q", n)
}

// ===== 上游响应结构（stream=false 时为干净的结构化 JSON）=====

type upstreamData struct {
	Action         string        `json:"action"`
	Thinking       string        `json:"thinking"`
	TaskCompletion string        `json:"taskCompletion"`
	Question       string        `json:"question"`
	Options        []string      `json:"options"`
	Tool           *UpstreamTool `json:"tool"`
}

type upstreamResp struct {
	Code    upstreamCode  `json:"code"`
	Data    *upstreamData `json:"data"`
	Message string        `json:"message"`
}

// upstreamCode 兼容上游把 code 写成数字或字符串两种形态。
//
// 实测（2026-09-12 真机）：成功响应是 `"code":0`（数字），
// 而鉴权/业务失败是 `"code":"10050000"`（字符串）配 HTTP 200。
// 早期用 int 建模会把"登录过期"变成 JSON 解析失败，对外报成协议错误。
type upstreamCode int

// upstreamCodeAuthExpired 上游 AccessToken 失效的业务码。
const upstreamCodeAuthExpired = 10050000

func (c *upstreamCode) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*c = 0
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s = strings.TrimSpace(s); s == "" {
			*c = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("upstream code %q is not numeric", s)
		}
		*c = upstreamCode(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*c = upstreamCode(n)
	return nil
}

func (c upstreamCode) MarshalJSON() ([]byte, error) { return json.Marshal(int(c)) }

// UpstreamOutcome 是 structured/raw 两种传输统一产出的结果，供 handler 后续消费。
type UpstreamOutcome struct {
	HTTPStatus int
	Code       int
	Action     string
	Text       string
	Thinking   string
	Tool       *UpstreamTool
	Raw        string
}

func (o *UpstreamOutcome) ToolCalls() []ToolCall {
	if o == nil || o.Action != "review" {
		return nil
	}
	return ToolCallsFromUpstream(o.Tool)
}

func outcomeFromStructured(resp *upstreamResp, raw string) (*UpstreamOutcome, error) {
	if resp == nil {
		return nil, upstreamProtocolError("upstream response is nil")
	}
	if resp.Code != 0 {
		return nil, upstreamBusinessError(int(resp.Code), resp.Message)
	}
	if resp.Data == nil {
		return nil, upstreamProtocolError("upstream data is missing")
	}
	text, err := validateUpstreamData(resp.Data)
	if err != nil {
		return nil, err
	}
	return &UpstreamOutcome{
		HTTPStatus: http.StatusOK,
		Code:       int(resp.Code),
		Action:     resp.Data.Action,
		Text:       text,
		Thinking:   resp.Data.Thinking,
		Tool:       resp.Data.Tool,
		Raw:        raw,
	}, nil
}

func validateUpstreamData(data *upstreamData) (string, error) {
	if data == nil {
		return "", upstreamProtocolError("upstream data is missing")
	}
	switch data.Action {
	case "completion":
		if data.Tool != nil || strings.TrimSpace(data.TaskCompletion) == "" {
			return "", upstreamProtocolError("invalid completion payload")
		}
		return data.TaskCompletion, nil
	case "ask":
		if data.Tool != nil || strings.TrimSpace(data.Question) == "" {
			return "", upstreamProtocolError("invalid ask payload")
		}
		return data.Question, nil
	case "review":
		if err := validateUpstreamTool(data.Tool); err != nil {
			return "", err
		}
		return "", nil
	default:
		return "", upstreamProtocolError("unsupported upstream action %q", data.Action)
	}
}

func validateUpstreamTool(tool *UpstreamTool) error {
	if tool == nil || strings.TrimSpace(tool.Name) == "" {
		return upstreamProtocolError("review payload is missing tool name")
	}
	if !validToolArguments(tool.Args) {
		return upstreamProtocolError("tool %q has invalid arguments", tool.Name)
	}
	return nil
}

// ===== 请求体结构 =====

type ChatCompletionRequest struct {
	Model             string          `json:"model"`
	Messages          []ChatMessage   `json:"messages"`
	Stream            bool            `json:"stream"`
	StreamOptions     StreamOptions   `json:"stream_options"`
	User              string          `json:"user"`
	SessionID         string          `json:"session_id"`
	Tools             []Tool          `json:"tools"`
	Functions         []ToolFunction  `json:"functions"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	FunctionCall      json.RawMessage `json:"function_call"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type LegacyFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatMessage struct {
	Role         string              `json:"role"`
	Content      json.RawMessage     `json:"content"`
	ToolCalls    []ToolCall          `json:"tool_calls"`
	ToolCallID   string              `json:"tool_call_id"`
	Name         string              `json:"name"`
	FunctionCall *LegacyFunctionCall `json:"function_call"`
}

// ===== 代理服务器 =====

type ProxyServer struct {
	config   Config
	cookies  *CookieProvider
	client   *http.Client
	sessions *SessionStore // 调用方会话 -> 上游 conversationId（多轮上下文）

	debugMu   sync.Mutex
	lastRaw   string
	lastClean string
	lastMeta  map[string]interface{}
}

func NewProxyServer(c Config) *ProxyServer {
	return &ProxyServer{
		config:   c,
		cookies:  NewCookieProvider(c.Cookies),
		client:   &http.Client{Timeout: time.Duration(c.Timeouts) * time.Second},
		sessions: NewSessionStore(c.SessionTTL, c.SessionMax),
	}
}

// authHeaders 构造发往 ligai 的认证头（与真实客户端一致）。
// 注意：X-Seq-Id 是 UUID 而非时间戳；Cookie 里需带 userId。
func (ps *ProxyServer) authHeaders(creds Credentials) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+creds.OT)
	h.Set("X-Product", ps.config.Product)
	h.Set("X-Seq-Id", newUUID())
	h.Set("X-Csrfcode", "")
	h.Set("X-Referer", ps.config.Origin+"/")
	h.Set("Origin", ps.config.Origin)
	if ps.config.UserAgent != "" {
		h.Set("User-Agent", ps.config.UserAgent)
	}
	h.Set("Accept", "text/event-stream, application/json")
	return h
}

// cookieHeader 构造 Cookie（ot_session + 账号元信息，与真实客户端一致）。
func (ps *ProxyServer) cookieHeader(creds Credentials) string {
	parts := []string{"ot_session=" + creds.OT, "userAccountLoginMethod=oauth", "userAccountType=wechat"}
	if creds.UserID != "" {
		parts = append(parts, "userId="+creds.UserID)
	}
	return strings.Join(parts, "; ")
}

func (ps *ProxyServer) effectiveInput(input string) string {
	if ps.config.Prompt == "" {
		return input
	}
	return ps.config.Prompt + "\n\n" + input
}

// newConversationID 生成客户端同款会话 ID：cid-<uuid>-<product>
func (ps *ProxyServer) newConversationID() string {
	return "cid-" + newUUID() + "-" + ps.config.Product
}

// chatBody 构造上游请求体（结构来自真实客户端抓包还原）。
//
// 两个关键点，缺一不可：
//  1. conversationId 必须是 "cid-<uuid>-<product>" 格式；
//  2. input 不是字符串而是嵌套对象 —— 内层 input.input 才是 ACP 信封，
//     另有 type="start"、消息 id、files。
//
// 缺少这些时用户消息无法送达模型，上游会退化成输出自我介绍或活动卡片。
//
// tools 策略只控制是否启用上游原生工具服务：setting.tools 始终为空，
// 因为实测自定义 function 无法在上游注册（服务端系统提示词有最终裁决权）。
func (ps *ProxyServer) chatBody(conversationID, input, userID, upstreamModel string, images []ContentPart, stream bool, policy toolPolicy) map[string]interface{} {
	empty := []interface{}{}
	mcpServers, uiServers := []string{}, []string{}
	if policy.Enabled {
		mcpServers = []string{"mcp-server-orcaterm-oauth"}
		uiServers = []string{"ui-tools-orcaterm-explorer"}
	}
	setting := map[string]interface{}{
		"mcpServers":                       mcpServers,
		"uiServers":                        uiServers,
		"tools":                            empty,
		"approvedTools":                    empty,
		"approvedMCPTools":                 empty,
		"enableAutoSubtaskExecution":       false,
		"env":                              empty,
		"model":                            upstreamModel,
		"modelDesc":                        map[string]interface{}{},
		"hideToolMessage":                  ps.config.HideTools,
		"isNormalToolMessageIgnored":       ps.config.HideTools,
		"shouldHideDirectOutputToolReview": ps.config.HideTools,
	}
	return map[string]interface{}{
		"conversationId": conversationID,
		"user": map[string]interface{}{
			"id":      userID,
			"setting": setting,
		},
		"input": map[string]interface{}{
			"type":           "start",
			"input":          BuildACPPrompt(ps.effectiveInput(input), images),
			"id":             newUUID(),
			"emphasisPrompt": "",
			"files":          empty,
		},
		"stream": stream,
	}
}

// callStructured 以 stream=false 调用上游，并归一化为统一结果。
func (ps *ProxyServer) callStructured(ctx context.Context, creds Credentials, conversationID, input, upstreamModel string, images []ContentPart, policy toolPolicy) (*UpstreamOutcome, error) {
	body, _ := json.Marshal(ps.chatBody(conversationID, input, creds.UserID, upstreamModel, images, false, policy))
	resp, err := ps.doChat(ctx, creds, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readLimited(resp.Body, maxUpstreamBody)
	if err != nil {
		return nil, &UpstreamError{Status: resp.StatusCode, Op: "read upstream response", Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, upstreamHTTPError(resp.StatusCode, raw)
	}
	var parsed upstreamResp
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, upstreamProtocolError("parse upstream json: %v", err)
	}
	outcome, err := outcomeFromStructured(&parsed, raw)
	if outcome != nil {
		outcome.HTTPStatus = resp.StatusCode
	}
	return outcome, err
}

// callRawStream 以 stream=true 调用上游并聚合 SSE，不直接写 ResponseWriter。
func (ps *ProxyServer) callRawStream(ctx context.Context, creds Credentials, conversationID, input, upstreamModel string, images []ContentPart, policy toolPolicy) (*UpstreamOutcome, error) {
	body, _ := json.Marshal(ps.chatBody(conversationID, input, creds.UserID, upstreamModel, images, true, policy))
	req, err := ps.newChatRequest(ctx, creds, body)
	if err != nil {
		return nil, err
	}
	resp, err := ps.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := readLimited(resp.Body, maxUpstreamBody)
		if readErr != nil {
			return nil, &UpstreamError{Status: resp.StatusCode, Op: "read upstream error response", Err: readErr}
		}
		return nil, upstreamHTTPError(resp.StatusCode, raw)
	}
	outcome, err := ParseRawUpstream(resp.Body, maxSSEEventData, maxUpstreamRawText)
	if outcome != nil {
		outcome.HTTPStatus = resp.StatusCode
	}
	return outcome, err
}

// doChat 只负责发出请求；响应状态与有界读取由调用方统一检查。
func (ps *ProxyServer) doChat(ctx context.Context, creds Credentials, body []byte) (*http.Response, error) {
	req, err := ps.newChatRequest(ctx, creds, body)
	if err != nil {
		return nil, err
	}
	return ps.client.Do(req)
}

type UpstreamError struct {
	Status int
	Op     string
	Detail string
	Err    error
}

func (e *UpstreamError) Error() string {
	parts := []string{e.Op}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.Detail != "" {
		parts = append(parts, truncate(e.Detail, 300))
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *UpstreamError) Unwrap() error { return e.Err }

// errUpstreamProtocol 标记"上游协议损坏"（HTTP 200 但 JSON/包络不可解析或字段不合法）。
var errUpstreamProtocol = errors.New("upstream protocol error")

// errUpstreamAuth 标记上游认证失效。上游把 AccessToken 过期也走 HTTP 200 + 业务码，
// 必须与"协议损坏"区分开，否则调用方只会看到一个含糊的错误。
var errUpstreamAuth = errors.New("upstream authentication failed: AccessToken invalid or expired")

// upstreamBusinessError 归类 HTTP 200 但 code != 0 的响应。
func upstreamBusinessError(code int, message string) error {
	msg := truncate(strings.TrimSpace(message), maxUpstreamErrorDetail)
	if code == upstreamCodeAuthExpired || strings.Contains(strings.ToLower(msg), "invalid or expired accesstoken") {
		return fmt.Errorf("%w (%s)", errUpstreamAuth, msg)
	}
	return fmt.Errorf("upstream code %d: %s", code, msg)
}

// upstreamProtocolError 包装协议类错误，便于 upstreamErrorResponse 归类。
func upstreamProtocolError(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", errUpstreamProtocol, fmt.Sprintf(format, args...))
}

// maxUpstreamErrorDetail 上游错误正文在对外错误信息中保留的最大字符数。
const maxUpstreamErrorDetail = 300

func upstreamHTTPError(status int, body string) error {
	return &UpstreamError{
		Status: status,
		Op:     "upstream request failed",
		Detail: truncate(strings.TrimSpace(body), maxUpstreamErrorDetail),
	}
}

// upstreamErrorResponse 把上游错误映射成对外 HTTP 状态与错误码。
// 401/403 一律转成 502 upstream_auth_error，避免把上游认证问题误报为调用方未授权。
func upstreamErrorResponse(err error) (int, string) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		switch {
		case ue.Status == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "upstream_rate_limited"
		case ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden:
			return http.StatusBadGateway, "upstream_auth_error"
		}
	}
	if errors.Is(err, errUpstreamAuth) {
		return http.StatusBadGateway, "upstream_auth_error"
	}
	if errors.Is(err, errUpstreamProtocol) {
		return http.StatusBadGateway, "upstream_protocol_error"
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return http.StatusGatewayTimeout, "upstream_timeout"
	}
	return http.StatusBadGateway, "upstream_error"
}

func readLimited(r io.Reader, limit int) (string, error) {
	if limit <= 0 {
		return "", fmt.Errorf("invalid size limit %d", limit)
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(b) > limit {
		return "", fmt.Errorf("response exceeds %d bytes", limit)
	}
	return string(b), nil
}

func (ps *ProxyServer) newChatRequest(ctx context.Context, creds Credentials, body []byte) (*http.Request, error) {
	// sseresume=true 为真实客户端固定携带的参数
	u := ps.config.Upstream + "/assistant/chat?name=" + ps.config.Product + "&mode=0&csrfCode=&sseresume=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", ps.cookieHeader(creds))
	for k, vs := range ps.authHeaders(creds) {
		req.Header[k] = vs
	}
	return req, nil
}

// ===== HTTP 处理器 =====

func (ps *ProxyServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", ps.handleModels)
	mux.HandleFunc("/v1/models/", ps.handleModelByID)
	mux.HandleFunc("/v1/chat/completions", ps.handleChatCompletions)
	mux.HandleFunc("/v1/sessions", ps.handleSessions)
	mux.HandleFunc("/health", ps.handleHealth)
	mux.HandleFunc("/debug/last", ps.handleDebugLast)
	return corsMiddleware(mux, ps.config.CORSOrigins)
}

// jwtExpiry 从 ot_session JWT 里读出 exp（不校验签名——只用于提前提示登录态是否过期）。
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func (ps *ProxyServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	creds, ok := ps.cookies.Get()
	out := map[string]interface{}{
		"status":      "ok",
		"version":     proxyVersion,
		"has_sid":     creds.SID != "",
		"has_ot":      creds.OT != "",
		"authed":      ok,
		"model":       ps.config.AliasModel,
		"backend":     ps.config.Upstream,
		"mode":        ps.config.Mode,
		"on_degraded": ps.config.OnDegraded,
		"strip":       ps.config.Strip,
	}
	// 上游把 AccessToken 过期也走 HTTP 200 + 业务码，代价是事发前完全看不出来。
	// 这里本地解一次 JWT exp，让调用方能在请求失败前发现登录态需要刷新。
	if exp, hasExp := jwtExpiry(creds.OT); hasExp {
		out["token_expires_at"] = exp.UTC().Format(time.RFC3339)
		out["token_expired"] = time.Now().After(exp)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleDebugLast 返回最近一次上游原始响应与清洗结果，便于调参。
func (ps *ProxyServer) handleDebugLast(w http.ResponseWriter, r *http.Request) {
	ps.debugMu.Lock()
	defer ps.debugMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"raw":   ps.lastRaw,
		"clean": ps.lastClean,
		"meta":  ps.lastMeta,
	})
}

// handleSessions 会话管理：GET 列出全部会话，DELETE 清除（?key=xx 单个，否则全部）。
func (ps *ProxyServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		list := ps.sessions.List()
		items := make([]map[string]interface{}, 0, len(list))
		for _, s := range list {
			items = append(items, map[string]interface{}{
				"key":             s.Key,
				"conversation_id": s.CID,
				"turns":           s.Turns,
				"tokens":          s.Tokens,
				"pending_tool":    s.PendingTool,
				"last":            s.Last.Format(time.RFC3339),
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": items})
	case http.MethodDelete:
		if key := r.URL.Query().Get("key"); key != "" {
			json.NewEncoder(w).Encode(map[string]interface{}{"deleted": ps.sessions.Delete(key)})
			return
		}
		count := 0
		for _, s := range ps.sessions.List() {
			if ps.sessions.Delete(s.Key) {
				count++
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"deleted": count})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "", "use GET or DELETE")
	}
}

func (ps *ProxyServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "", "use GET")
		return
	}
	creds, _ := ps.cookies.Get()
	def := ps.defaultModel()
	created := time.Now().Unix()
	items := make([]map[string]interface{}, 0, len(modelCatalog)+1)
	for _, m := range Models() {
		items = append(items, modelEntry(m, created, m.ID == def.ID))
	}
	// 兼容别名：等价于默认模型，方便旧调用方继续使用
	if entry, ok := ps.aliasEntry(created); ok {
		items = append(items, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   items,
		"note": map[string]interface{}{
			"authed":              creds.Ready(),
			"message":             "model represents OrcaTerm AI (ligai /assistant/chat)",
			"default_model":       def.ID,
			"alias":               ps.config.AliasModel,
			"upstream_model":      def.Upstream,
			"context_limit":       ps.config.ContextLimit,
			"allow_unknown_model": ps.config.AllowUnknown,
			"capabilities": map[string]interface{}{
				"multi_turn":       true,                // 复用 X-Session-Id / user 字段即可多轮
				"multimodal_image": true,                // 支持 image_url（data URI 或远程 URL）
				"tools":            ps.config.ToolsMode, // native=上游原生工具透传
				"tools_note": "native：上游自带的工具（fetch / 终端 / 云空间等）会以标准 OpenAI " +
					"tool_calls 返回；调用方执行后把结果回传即可。现代格式只凭 tool_call_id 就能闭环，" +
					"legacy role=\"function\" 结果必须复用原会话键。自定义 function 无法在上游注册，" +
					"因此只能使用 OrcaTerm 原生工具名。",
				"model_switch":      true,        // 传不同 model 即可切换上游模型
				"usage":             "estimated", // 上游不返回 token，代理本地估算
				"context_remaining": true,        // 通过响应头 X-OrcaTerm-Context-Remaining
			},
		},
	})
}

// modelEntry 把目录条目转成 OpenAI /v1/models 列表项。
func modelEntry(m ModelInfo, created int64, isDefault bool) map[string]interface{} {
	e := map[string]interface{}{
		"id": m.ID, "object": "model", "created": created, "owned_by": "orcaterm",
		"upstream": m.Upstream, "name": m.Name,
	}
	if m.Description != "" {
		e["description"] = m.Description
	}
	if len(m.Capabilities) > 0 {
		e["capabilities"] = m.Capabilities
	}
	if m.Free {
		e["free"] = true
	}
	if m.Legacy {
		e["legacy"] = true
	}
	if isDefault {
		e["default"] = true
	}
	return e
}

// aliasEntry 构造 deprecated 兼容别名的模型条目。
// 当别名与默认模型 id 相同时返回 false——此时列表里不会出现独立条目，
// 检索该 id 也应落到普通目录条目，保证 list/retrieve 一致。
func (ps *ProxyServer) aliasEntry(created int64) (map[string]interface{}, bool) {
	alias := strings.TrimSpace(ps.config.AliasModel)
	def := ps.defaultModel()
	if alias == "" || alias == def.ID {
		return nil, false
	}
	return map[string]interface{}{
		"id": alias, "object": "model", "created": created, "owned_by": "orcaterm",
		"alias_of":   def.ID,
		"upstream":   def.Upstream,
		"name":       def.Name + " (alias)",
		"deprecated": true,
	}, true
}

// handleModelByID 支持 GET /v1/models/{id}（OpenAI 标准端点）。
func (ps *ProxyServer) handleModelByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "", "use GET")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	created := time.Now().Unix()
	if alias := strings.TrimSpace(ps.config.AliasModel); alias != "" && strings.EqualFold(strings.TrimSpace(id), alias) {
		if entry, ok := ps.aliasEntry(created); ok {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(entry)
			return
		}
	}
	m, ok := FindModel(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"model %q not found; available: %s", id, strings.Join(ModelIDs(), ", "))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(modelEntry(m, created, m.ID == ps.defaultModel().ID))
}

// toolPolicy 是调用方 tools/functions 声明经过校验后的规范化结果。
type toolPolicy struct {
	Tools   []Tool
	Dialect string // modern（tools）/ legacy（functions）
	Choice  string // auto / none
	Enabled bool   // 是否向上游启用原生工具服务
	Mode    string // 生效的 ORCATERM_TOOLS_MODE
}

// nativeToolNames 是 OrcaTerm 客户端实际注册的原生工具名。
//
// 取自客户端前端 bundle 的 V8 code cache 字符串表（EBWebView/Default/Code Cache/js），
// 并用真机请求逐项验证：只声明 execute_terminal_command 时，上游会先请求
// list_terminals，导致 "upstream requested undeclared tool" —— 说明终端类任务是
// 一条工具链，客户端必须能一次性声明整条链路。
//
// 这些名字只用于**校验调用方的 tools 声明**：自定义 function 无法在上游注册，
// 所以声明表外的名字一律拒绝。上游真正调用什么由服务端系统提示词决定。
var nativeToolNames = map[string]bool{
	// 网页抓取
	"fetch": true,
	// 终端与终端会话
	"list_terminals":           true,
	"get_terminal_detail":      true,
	"get_terminal_output":      true,
	"execute_terminal_command": true,
	"send_terminal_signal":     true,
	"list_connect_configs":     true,
	"get_command_history":      true,
	// 命令与后台任务
	"execute_command":          true,
	"create_command":           true,
	"query_commands_status":    true,
	"run_commands":             true,
	"run_sandbox_task":         true,
	"submit_agent_tasks":       true,
	"query_agent_tasks_status": true,
	// 云空间与远程文件
	"read_cloud_space_file":      true,
	"read_cloud_space_file_list": true,
	"create_cloud_space_file":    true,
	"remote_read":                true,
	"remote_write":               true,
	"remote_edit":                true,
	"remote_multi_edit":          true,
	"remote_glob":                true,
	"remote_grep":                true,
	// 设置与防火墙
	"get_orcaterm_settings":    true,
	"update_orcaterm_settings": true,
	"create_firewall_rules":    true,
	"delete_firewall_rules":    true,
}

// parseToolChoice 解析 tool_choice / function_call。
// 只支持 "auto" 与 "none"；"required"、具名强制选择与任意对象都会得到明确错误。
func parseToolChoice(raw json.RawMessage) (string, string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "auto", "", nil
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "", "forced_tool_choice_unsupported",
			fmt.Errorf("named or forced tool selection is not supported")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", "tool_choice_unsupported", fmt.Errorf("tool_choice must be a string")
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto", "":
		return "auto", "", nil
	case "none":
		return "none", "", nil
	case "required", "any":
		return "", "tool_choice_required_unsupported",
			fmt.Errorf("tool_choice %q is not supported because the upstream model decides tool use", value)
	default:
		return "", "forced_tool_choice_unsupported",
			fmt.Errorf("forced tool choice %q is not supported", value)
	}
}

// normalizeToolPolicy 校验 tools / functions 声明并归一化成一个策略。
func normalizeToolPolicy(req *ChatCompletionRequest, mode string) (toolPolicy, string, error) {
	if len(req.Tools) > 0 && len(req.Functions) > 0 {
		return toolPolicy{}, "conflicting_tool_schemas",
			fmt.Errorf("tools and functions cannot be used in the same request")
	}
	if req.ParallelToolCalls != nil && *req.ParallelToolCalls {
		return toolPolicy{}, "parallel_tool_calls_unsupported",
			fmt.Errorf("parallel_tool_calls is not supported; the upstream handles one pending call at a time")
	}
	p := toolPolicy{Tools: req.Tools, Dialect: "modern", Mode: mode}
	choiceRaw := req.ToolChoice
	if len(req.Functions) > 0 {
		p.Dialect = "legacy"
		p.Tools = make([]Tool, 0, len(req.Functions))
		for _, fn := range req.Functions {
			p.Tools = append(p.Tools, Tool{Type: "function", Function: fn})
		}
		choiceRaw = req.FunctionCall
	}
	choice, code, err := parseToolChoice(choiceRaw)
	if err != nil {
		return toolPolicy{}, code, err
	}
	p.Choice = choice
	if mode == "error" && len(p.Tools) > 0 {
		return toolPolicy{}, "tools_unsupported",
			fmt.Errorf("tool requests are rejected because ORCATERM_TOOLS_MODE=error")
	}
	if mode == "native" {
		seen := map[string]bool{}
		for _, tool := range p.Tools {
			name := strings.TrimSpace(tool.Function.Name)
			if tool.Type != "" && tool.Type != "function" {
				return toolPolicy{}, "custom_tools_unsupported",
					fmt.Errorf("tool type %q is not supported", tool.Type)
			}
			if !nativeToolNames[name] {
				return toolPolicy{}, "custom_tools_unsupported",
					fmt.Errorf("custom tool %q cannot be registered upstream; supported names: %s",
						name, strings.Join(nativeToolNameList(), ", "))
			}
			if seen[name] {
				return toolPolicy{}, "duplicate_tool", fmt.Errorf("tool %q is declared more than once", name)
			}
			seen[name] = true
		}
	}
	p.Enabled = len(p.Tools) > 0 && choice != "none" && mode == "native"
	return p, "", nil
}

// nativeToolNameList 返回稳定的原生工具名列表（用于错误信息与文档一致性）。
func nativeToolNameList() []string {
	names := make([]string, 0, len(nativeToolNames))
	for name := range nativeToolNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// sessionAcquireErrorCode 映射"获取会话租约"阶段的错误。
// 它与工具调用错误不同：这里是调用方会话键与 conversation 不一致或请求被取消。
func sessionAcquireErrorCode(err error) string {
	switch {
	case IsStoreError(err, StoreErrorMismatch):
		return "session_mismatch"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "request_cancelled"
	default:
		return storeErrorCode(err)
	}
}

func storeErrorCode(err error) string {
	switch {
	case IsStoreError(err, StoreErrorUnknownCall):
		return "unknown_tool_call_id"
	case IsStoreError(err, StoreErrorConsumed):
		return "tool_call_already_consumed"
	case IsStoreError(err, StoreErrorExpired):
		return "expired_tool_call_id"
	case IsStoreError(err, StoreErrorMismatch):
		return "tool_call_mismatch"
	case IsStoreError(err, StoreErrorInFlight):
		return "tool_call_in_flight"
	default:
		return "invalid_tool_call"
	}
}

// inputPlanErrorCode 把内容解析错误映射成稳定的错误码。
func inputPlanErrorCode(err error) string {
	switch {
	case errors.Is(err, errToolResultMissingID):
		return "tool_call_id_required"
	case errors.Is(err, errMultipleToolResults):
		return "multiple_tool_results_unsupported"
	default:
		return "invalid_content"
	}
}

func (ps *ProxyServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "", "use POST")
		return
	}
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Invalid request: %v", err)
		return
	}
	policy, code, err := normalizeToolPolicy(&req, ps.config.ToolsMode)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", code, "%v", err)
		return
	}
	creds, ok := ps.cookies.Get()
	if !ok {
		writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "not_logged_in", "OrcaTerm not logged in. Please sign into OrcaTerm first.")
		return
	}
	model, err := ps.resolveModel(req.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model %s not found; available: %s", err.Error(), strings.Join(ModelIDs(), ", "))
		return
	}
	key := sessionKeyOf(r.Header.Get("X-Session-Id"), req.User)
	if key == "" {
		key = req.SessionID
	}
	// 先探测是否是工具结果轮次：工具结果必须复用已有 conversation，不能新建会话。
	probe, err := extractInputPlan(req.Messages, false)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", inputPlanErrorCode(err), "%v", err)
		return
	}
	var lease *Lease
	var plan *InputPlan
	committed := false
	defer func() {
		if lease != nil && !committed {
			_ = lease.Rollback()
		}
	}()
	if probe.IsToolResult {
		result := probe.Results[0]
		var pending PendingCall
		if result.Legacy {
			if key == "" {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "legacy_function_result_requires_session",
					"legacy role=\"function\" results carry no tool_call_id; reuse the X-Session-Id or user of the triggering request")
				return
			}
			pending, err = ps.sessions.UniquePendingCall(key)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", storeErrorCode(err), "%v", err)
				return
			}
			result.ToolCallID = pending.ID
		}
		lease, pending, err = ps.sessions.AcquireCall(r.Context(), result.ToolCallID, key)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", storeErrorCode(err), "%v", err)
			return
		}
		if name := strings.TrimSpace(result.Name); name != "" && name != pending.Name {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "tool_result_name_mismatch",
				"tool result name %q does not match pending tool %q", name, pending.Name)
			return
		}
		plan = &InputPlan{
			Text:         BuildToolResultPrompt(pending.Name, result.Content),
			IsToolResult: true,
			Results: []ToolResultMessage{{
				ToolCallID: pending.ID, Name: pending.Name, Content: result.Content, Legacy: result.Legacy,
			}},
		}
	} else {
		cid := ps.newConversationID()
		if key != "" {
			snapshot, _ := ps.sessions.GetOrCreate(key, cid)
			cid = snapshot.CID
		}
		lease, err = ps.sessions.Acquire(r.Context(), key, cid)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", sessionAcquireErrorCode(err), "%v", err)
			return
		}
		// lease.Session 是拿到同一 CID 租约之后取得的快照，因此并发请求看到的也是
		// 串行化之后的轮次：新建会话或上游尚无历史轮次时都按首轮处理，
		// 必须发送完整 system + 初始历史。
		multiTurn := key != "" && lease.Session.Turns > 0
		plan, err = extractInputPlan(req.Messages, multiTurn)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", inputPlanErrorCode(err), "%v", err)
			return
		}
	}
	input, images := plan.Text, plan.Images
	if strings.TrimSpace(input) == "" && len(images) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing_messages", "no user message found")
		return
	}
	if ps.config.ToolsMode == "inject" && len(policy.Tools) > 0 {
		input += ToolsInstruction(policy.Tools)
	}
	promptTokens := lease.Session.Tokens + EstimateTokens(input)
	respModel := strings.TrimSpace(req.Model)
	if respModel == "" {
		respModel = model.ID
	}
	var outcome *UpstreamOutcome
	if ps.config.Mode == "raw-stream" {
		outcome, err = ps.callRawStream(r.Context(), creds, lease.CID, input, model.Upstream, images, policy)
	} else {
		outcome, err = ps.callStructured(r.Context(), creds, lease.CID, input, model.Upstream, images, policy)
	}
	if err != nil {
		status, errorCode := upstreamErrorResponse(err)
		if errorCode == "upstream_auth_error" {
			writeOpenAIError(w, status, "api_error", errorCode,
				"OrcaTerm 登录态已失效，请在 OrcaTerm 客户端重新登录后重试（%v）", err)
			return
		}
		writeOpenAIError(w, status, "api_error", errorCode, "upstream failed: %v", err)
		return
	}
	text, action := outcome.Text, outcome.Action
	toolCalls := outcome.ToolCalls()
	if len(toolCalls) > 0 {
		// 上游调用了调用方没有声明的工具：明确报错，并且不创建 pending call。
		if !declaresTool(policy.Tools, toolCalls[0].Function.Name) {
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream_requested_undeclared_tool",
				"upstream requested undeclared tool %q; declare it in tools/functions or remove the tool schema", toolCalls[0].Function.Name)
			return
		}
		text = ""
	}
	cleaned := text
	if ps.config.Strip && len(toolCalls) == 0 {
		cleaned = cleanAnswer(text)
	}
	if len(toolCalls) == 0 && ps.config.ToolsMode == "inject" && len(policy.Tools) > 0 {
		toolCalls, cleaned = ExtractToolCalls(cleaned)
	}
	degraded := len(toolCalls) == 0 && isDegraded(text, cleaned, action)
	out := cleaned
	if degraded {
		switch ps.config.OnDegraded {
		case "error":
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream_degraded", "upstream returned no valid answer")
			return
		case "empty":
			out = ""
		}
	}
	completionTokens := EstimateTokens(out)
	for _, tc := range toolCalls {
		completionTokens += EstimateTokens(tc.Function.Name) + EstimateTokens(tc.Function.Arguments)
	}
	total := promptTokens + completionTokens
	usage := map[string]interface{}{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": total, "estimated": true}
	var nextPending *PendingCall
	if len(toolCalls) > 0 {
		nextPending = &PendingCall{ID: toolCalls[0].ID, Name: toolCalls[0].Function.Name, Arguments: toolCalls[0].Function.Arguments}
	}
	if err := lease.Commit(1, total, nextPending); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "session_commit_failed", "%v", err)
		return
	}
	committed = true
	ps.debugMu.Lock()
	ps.lastRaw, ps.lastClean = outcome.Raw, cleaned
	ps.lastMeta = map[string]interface{}{"action": action, "degraded": degraded, "session": key, "tool_calls": len(toolCalls), "model_id": model.ID}
	ps.debugMu.Unlock()
	w.Header().Set("X-OrcaTerm-Action", action)
	w.Header().Set("X-OrcaTerm-Degraded", boolFlag(degraded))
	w.Header().Set("X-OrcaTerm-Usage-Estimated", "1")
	w.Header().Set("X-OrcaTerm-Model-Id", model.ID)
	w.Header().Set("X-OrcaTerm-Model", model.Upstream)
	if key != "" {
		w.Header().Set("X-OrcaTerm-Session", key)
		w.Header().Set("X-OrcaTerm-Turn", strconv.Itoa(lease.Session.Turns+1))
		w.Header().Set("X-OrcaTerm-Context-Used", strconv.Itoa(total))
		w.Header().Set("X-OrcaTerm-Context-Limit", strconv.Itoa(ps.config.ContextLimit))
		w.Header().Set("X-OrcaTerm-Context-Remaining", strconv.Itoa(maxInt(0, ps.config.ContextLimit-total)))
	}
	if plan.IsToolResult {
		w.Header().Set("X-OrcaTerm-Tool-Result-Turn", "1")
	}
	if req.Stream {
		ps.writeSyntheticStream(w, respModel, out, toolCalls, usage, policy.Dialect, req.StreamOptions.IncludeUsage)
		return
	}
	writeChatCompletionJSON(w, respModel, out, toolCalls, usage, policy.Dialect)
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ===== 输出构造 =====

func writeChatCompletionJSON(w http.ResponseWriter, model, content string, toolCalls []ToolCall, usage map[string]interface{}, dialect string) {
	msg := map[string]interface{}{"role": "assistant", "content": content}
	finish := "stop"
	if len(toolCalls) > 0 {
		if dialect == "legacy" {
			msg["function_call"] = map[string]string{"name": toolCalls[0].Function.Name, "arguments": toolCalls[0].Function.Arguments}
			finish = "function_call"
		} else {
			msg["tool_calls"] = toolCalls
			finish = "tool_calls"
		}
		if strings.TrimSpace(content) == "" {
			msg["content"] = nil
		}
	}
	respBody := map[string]interface{}{
		"id":      "chatcmpl-" + newUUID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
		"usage": usage,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(respBody); err != nil {
		log.Printf("[ERROR] encode response: %v", err)
	}
}

// writeSyntheticStream 把已获得的答案本地切分为 SSE 流，兼容 OpenAI 流式客户端。
// 末尾附带 usage（对应 OpenAI 的 stream_options.include_usage 语义）。
func (ps *ProxyServer) writeSyntheticStream(w http.ResponseWriter, model, text string, toolCalls []ToolCall, usage map[string]interface{}, dialect string, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "no_flusher", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id := "chatcmpl-" + newUUID()
	created := time.Now().Unix()
	fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n", id, created, model)
	flusher.Flush()
	emit := func(s string) {
		if s == "" {
			return
		}
		chunk := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`,
			id, created, model, jsonString(s))
		fmt.Fprint(w, chunk+"\n\n")
		flusher.Flush()
	}
	for _, c := range splitRunes(text, 3) {
		emit(c)
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		if dialect == "legacy" {
			fc := map[string]string{"name": toolCalls[0].Function.Name, "arguments": toolCalls[0].Function.Arguments}
			if b, err := json.Marshal(fc); err == nil {
				fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"function_call\":%s},\"finish_reason\":null}]}\n\n", id, created, model, b)
			}
			finish = "function_call"
		} else {
			indexed := make([]map[string]interface{}, 0, len(toolCalls))
			for i, tc := range toolCalls {
				indexed = append(indexed, map[string]interface{}{"index": i, "id": tc.ID, "type": tc.Type, "function": tc.Function})
			}
			if b, err := json.Marshal(indexed); err == nil {
				fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":%s},\"finish_reason\":null}]}\n\n", id, created, model, b)
			}
			finish = "tool_calls"
		}
		flusher.Flush()
	}
	end := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`,
		id, created, model, finish)
	fmt.Fprint(w, end+"\n\n")
	if includeUsage {
		if b, err := json.Marshal(usage); err == nil {
			fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[],\"usage\":%s}\n\n", id, created, model, string(b))
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// SSEEventReader 按 SSE 规范读取事件：空行分隔，多条 data 以换行拼接，兼容 CRLF 与 EOF 尾事件。
type SSEEventReader struct {
	r        *bufio.Reader
	maxData  int
	finished bool
}

func NewSSEEventReader(r io.Reader, maxData int) *SSEEventReader {
	return &SSEEventReader{r: bufio.NewReader(r), maxData: maxData}
}

// Next 返回下一条 data；done 表示收到 [DONE] 或输入正常结束。
func (r *SSEEventReader) Next() (data string, done bool, err error) {
	if r.finished {
		return "", true, nil
	}
	if r.maxData <= 0 {
		return "", false, fmt.Errorf("invalid SSE data limit %d", r.maxData)
	}
	var lines []string
	size := 0
	for {
		line, readErr := r.r.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if len(lines) > 0 {
					return r.finishEvent(lines)
				}
			} else if !strings.HasPrefix(line, ":") {
				field, value := line, ""
				if i := strings.IndexByte(line, ':'); i >= 0 {
					field, value = line[:i], line[i+1:]
					if strings.HasPrefix(value, " ") {
						value = value[1:]
					}
				}
				if field == "data" {
					if len(lines) > 0 {
						size++
					}
					size += len(value)
					if size > r.maxData {
						return "", false, fmt.Errorf("SSE event exceeds %d bytes", r.maxData)
					}
					lines = append(lines, value)
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return "", false, readErr
			}
			r.finished = true
			if len(lines) > 0 {
				return r.finishEvent(lines)
			}
			return "", true, nil
		}
	}
}

func (r *SSEEventReader) finishEvent(lines []string) (string, bool, error) {
	data := strings.Join(lines, "\n")
	if strings.TrimSpace(data) == "[DONE]" {
		r.finished = true
		return "", true, nil
	}
	return data, false, nil
}

// ParseRawUpstream 聚合上游 SSE 的 ai 帧，并解析成与 structured 模式一致的结果。
func ParseRawUpstream(r io.Reader, maxEventData, maxText int) (*UpstreamOutcome, error) {
	if maxText <= 0 {
		return nil, fmt.Errorf("invalid raw text limit %d", maxText)
	}
	reader := NewSSEEventReader(r, maxEventData)
	var text strings.Builder
	var raw strings.Builder
	for {
		data, done, err := reader.Next()
		if err != nil {
			return nil, fmt.Errorf("read upstream SSE: %w", err)
		}
		if done {
			break
		}
		if data == "" {
			continue
		}
		if raw.Len()+len(data)+1 > maxText {
			return nil, fmt.Errorf("raw upstream payload exceeds %d bytes", maxText)
		}
		raw.WriteString(data)
		raw.WriteByte('\n')
		var frame struct {
			Code    *upstreamCode   `json:"code"`
			Message string          `json:"message"`
			Content string          `json:"content"`
			Type    string          `json:"type"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return nil, upstreamProtocolError("parse upstream SSE data: %v", err)
		}
		if frame.Code != nil && *frame.Code != 0 {
			return nil, upstreamBusinessError(int(*frame.Code), frame.Message)
		}
		if frame.Type == "ai" {
			if text.Len()+len(frame.Content) > maxText {
				return nil, upstreamProtocolError("raw upstream text exceeds %d bytes", maxText)
			}
			text.WriteString(frame.Content)
		}
	}
	joined := text.String()
	env, ok := ParseChatEnvelope(joined)
	if !ok {
		return nil, upstreamProtocolError("raw upstream response has no valid OrcaTerm envelope")
	}
	data := &upstreamData{
		Action:         env.Action,
		Thinking:       env.Thinking,
		TaskCompletion: env.TaskCompletion,
		Question:       env.Question,
		Options:        env.Options,
		Tool:           env.Tool,
	}
	answer, err := validateUpstreamData(data)
	if err != nil {
		return nil, err
	}
	return &UpstreamOutcome{
		Code:     0,
		Action:   data.Action,
		Text:     answer,
		Thinking: data.Thinking,
		Tool:     data.Tool,
		Raw:      raw.String(),
	}, nil
}

// ===== raw-stream 模式（先完整解析上游，再由 handler 输出 OpenAI 响应）=====

// ===== 清洗管线 =====

// stripThoughts 只剥离 fence 外闭合的 <think>…</think>；未闭合内容原样保留。
func stripThoughts(s string) string {
	var out strings.Builder
	for pos := 0; pos < len(s); {
		fence := strings.Index(s[pos:], "```")
		think := strings.Index(s[pos:], "<think>")
		switch {
		case think < 0:
			out.WriteString(s[pos:])
			return out.String()
		case fence >= 0 && fence < think:
			fenceStart := pos + fence
			out.WriteString(s[pos:fenceStart])
			closeRel := strings.Index(s[fenceStart+3:], "```")
			if closeRel < 0 {
				out.WriteString(s[fenceStart:])
				return out.String()
			}
			fenceEnd := fenceStart + 3 + closeRel + 3
			out.WriteString(s[fenceStart:fenceEnd])
			pos = fenceEnd
		default:
			start := pos + think
			endRel := strings.Index(s[start+len("<think>"):], "</think>")
			if endRel < 0 {
				out.WriteString(s[pos:])
				return out.String()
			}
			out.WriteString(s[pos:start])
			pos = start + len("<think>") + endRel + len("</think>")
		}
	}
	return out.String()
}

// stripFencedJSON 只剥离严格可信的 OrcaTerm completion/ask/review JSON 围栏。
func stripFencedJSON(s string) string {
	var out strings.Builder
	for pos := 0; pos < len(s); {
		i, prefixLen := nextJSONFence(s, pos)
		if i < 0 {
			out.WriteString(s[pos:])
			break
		}
		out.WriteString(s[pos:i])
		bodyStart := i + prefixLen
		closeRel := strings.Index(s[bodyStart:], "```")
		if closeRel < 0 {
			out.WriteString(s[i:])
			break
		}
		close := bodyStart + closeRel
		blockEnd := close + 3
		block := s[i:blockEnd]
		if _, ok := ParseChatEnvelope(block); !ok {
			out.WriteString(block)
		}
		pos = blockEnd
	}
	return out.String()
}

// cleanAnswer 仅做协议级清洗：保留普通 Markdown、列表、运维/周年文本，
// 只删除 fence 外闭合 think 与严格可信的 OrcaTerm 动作包络。
func cleanAnswer(s string) string {
	s = stripThoughts(s)
	s = stripFencedJSON(s)
	return strings.TrimSpace(s)
}

// isDegraded 只依据协议有效性判定，避免把正常业务关键词误判为退化输出。
func isDegraded(raw, cleaned, action string) bool {
	_ = raw
	if strings.TrimSpace(cleaned) != "" {
		return false
	}
	return action != "review"
}

// ===== 工具函数 =====

// extractInput 提取本次要发送的输入文本与图片。
// multiTurn=true 时上游已持久化历史，只需发最后一条用户消息，避免重复堆叠；
// 否则把所有消息渲染成对话脚本（一次性请求，无服务端上下文）。
func extractInput(msgs []ChatMessage, multiTurn bool) (string, []ContentPart, error) {
	if multiTurn {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "user" {
				return ParseContent(msgs[i].Content)
			}
		}
		return "", nil, nil
	}
	var sb strings.Builder
	var images []ContentPart
	for _, m := range msgs {
		text, imgs, err := ParseContent(m.Content)
		if err != nil {
			return "", nil, err
		}
		if len(imgs) > 0 {
			images = imgs
		}
		switch m.Role {
		case "system":
			sb.WriteString("[系统]\n" + text + "\n\n")
		case "assistant":
			sb.WriteString("[助手]\n" + text + "\n\n")
		default:
			sb.WriteString("[用户]\n" + text + "\n\n")
		}
	}
	return strings.TrimSpace(sb.String()), images, nil
}

func splitRunes(s string, size int) []string {
	r := []rune(s)
	var out []string
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, code, format string, args ...interface{}) {
	resp := map[string]interface{}{
		"error": map[string]interface{}{
			"message": fmt.Sprintf(format, args...),
			"type":    errType, "param": nil, "code": code,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// corsAllowedHeaders 允许的请求头。X-Session-Id 是代理自定义的会话键，必须显式许可。
const corsAllowedHeaders = "Content-Type, Authorization, X-Session-Id, OpenAI-Organization, OpenAI-Project, OpenAI-Beta"

// corsExposedHeaders 浏览器可读的自定义响应头。
const corsExposedHeaders = "X-OrcaTerm-Action, X-OrcaTerm-Degraded, X-OrcaTerm-Session, X-OrcaTerm-Turn, " +
	"X-OrcaTerm-Context-Used, X-OrcaTerm-Context-Limit, X-OrcaTerm-Context-Remaining, " +
	"X-OrcaTerm-Usage-Estimated, X-OrcaTerm-Tool-Result-Turn, X-OrcaTerm-Model, X-OrcaTerm-Model-Id"

// corsMiddleware 按 allowlist 处理跨域。
//
// 默认（origins 为空）完全不返回 CORS 许可头；只有显式配置的 origin 才会被允许，
// 且必须显式配置 "*" 才允许任意 origin。未允许 origin 的预检返回 403。
func corsMiddleware(next http.Handler, origins []string) http.Handler {
	allowAll := false
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		switch o = strings.TrimSpace(o); o {
		case "":
			continue
		case "*":
			allowAll = true
		default:
			allowed[o] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		allowOrigin := ""
		if origin != "" {
			switch {
			case allowAll:
				allowOrigin = "*"
			case allowed[origin]:
				allowOrigin = origin
			}
		}
		if allowOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
			w.Header().Set("Access-Control-Expose-Headers", corsExposedHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			if allowOrigin == "" && (preflight || (origin != "" && r.Header.Get("Access-Control-Request-Headers") != "")) {
				writeOpenAIError(w, http.StatusForbidden, "invalid_request_error", "cors_origin_denied",
					"origin %q is not allowed by ORCATERM_CORS_ORIGINS", origin)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ===== 入口 =====

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	config := loadConfig()
	if err := validateConfig(config); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
	server := NewProxyServer(config)

	addr := "localhost:" + config.Port
	srv := &http.Server{Addr: addr, Handler: server.Handler()}

	go func() {
		log.Printf("[INFO] OrcaBridge v%s starting on http://%s", proxyVersion, addr)
		log.Printf("[INFO] Backend(cookies): %s", config.Cookies)
		log.Printf("[INFO] Model: default=%s(alias %s) -> %s (%d models)",
			server.defaultModel().ID, config.AliasModel, config.Upstream, len(modelCatalog))
		log.Printf("[INFO] Mode: %s, on_degraded: %s", config.Mode, config.OnDegraded)
		creds, ok := server.cookies.Get()
		log.Printf("[INFO] 凭据: %v (sid=%v ot=%v)", ok, creds.SID != "", creds.OT != "")
		if exp, hasExp := jwtExpiry(creds.OT); hasExp && time.Now().After(exp) {
			log.Printf("[WARN] OrcaTerm 登录态已过期（%s）。上游会用 HTTP 200 + code=10050000 拒绝请求，"+
				"请在 OrcaTerm 客户端重新登录后再使用。", exp.Local().Format(time.RFC3339))
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[FATAL] %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("[INFO] shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[WARN] shutdown: %v", err)
	}
	log.Printf("[INFO] bye")
}
