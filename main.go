package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const proxyVersion = "0.8.0"

// ===== 配置 =====

// Config 代理配置。默认零配置：自动读取运行中的 OrcaTerm 的 .cookies 作为认证。
type Config struct {
	Port          string        // 监听端口（默认 localhost:8080）
	Cookies       string        // OrcaTerm .cookies 文件路径
	Upstream      string        // ligai 后端地址
	AliasModel    string        // 兼容别名：该名字等价于"默认模型"（默认 orcaterm-assistant）
	Product       string        // X-Product 头
	Origin        string        // Origin 头
	UserAgent     string        // User-Agent 头（默认模拟客户端真实 UA）
	UpstreamModel string        // 默认模型的上游 id（默认 TokenHub/deepseek-v4-flash）
	AllowUnknown  bool          // 是否允许未在目录中的模型 id 直通上游（默认否）
	Timeouts      int           // 上游请求超时秒数
	Mode          string        // structured：用非流式结构化响应（默认，干净）；raw-stream：直连 SSE
	OnDegraded    string        // 退化输出处理：empty（默认，返回空）/ raw（返回清洗后原文）/ error
	Prompt        string        // 追加在 input 前的用户级覆盖指令（可空禁用）
	Strip         bool          // 是否启用清洗管线
	HideTools     bool          // 是否要求上游隐藏工具消息（hideToolMessage 等三个开关）
	SessionTTL    time.Duration // 会话空闲保活时长（默认 30 分钟）
	SessionMax    int           // 会话表容量上限（默认 1000）
	ToolsMode     string        // tools 处理：native（默认，上游原生工具透传）/ error / inject / ignore
	ContextLimit  int           // 上下文上限，用于估算剩余量（默认 128000）
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
		if d, err := time.ParseDuration(v); err == nil {
			c.SessionTTL = d
		}
	}
	if v := os.Getenv("ORCATERM_SESSION_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.SessionMax = n
		}
	}
	if v := os.Getenv("ORCATERM_TOOLS_MODE"); v != "" {
		c.ToolsMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v := os.Getenv("ORCATERM_CONTEXT_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.ContextLimit = n
		}
	}
	return c
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
	Code    int           `json:"code"`
	Data    *upstreamData `json:"data"`
	Message string        `json:"message"`
}

// reviewTool 返回上游本轮想调用的工具（action=review 且 tool.name 非空时）。
func (r *upstreamResp) reviewTool() *UpstreamTool {
	if r == nil || r.Data == nil || r.Data.Tool == nil {
		return nil
	}
	if strings.TrimSpace(r.Data.Tool.Name) == "" {
		return nil
	}
	return r.Data.Tool
}

// answer 从结构化响应中挑出应返回给调用方的文本与 action。
// action: completion=已作答 / ask=模型在反问 / review=模型想调用工具（本代理无工具运行时）。
func (r *upstreamResp) answer() (string, string) {
	if r.Data == nil {
		return "", ""
	}
	action := r.Data.Action
	if t := strings.TrimSpace(r.Data.TaskCompletion); t != "" {
		return t, action
	}
	if t := strings.TrimSpace(r.Data.Question); t != "" {
		return t, action
	}
	return "", action
}

func (r *upstreamResp) safeThinking() string {
	if r.Data == nil {
		return ""
	}
	return r.Data.Thinking
}

// ===== 请求体结构 =====

type ChatCompletionRequest struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	User      string        `json:"user"`       // OpenAI 标准字段，用作会话键
	SessionID string        `json:"session_id"` // 可选的显式会话标识
	Tools     []Tool        `json:"tools"`
}

type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`      // 字符串或多模态分部数组
	ToolCalls  []ToolCall      `json:"tool_calls"`   // assistant 发起工具调用时携带
	ToolCallID string          `json:"tool_call_id"` // role="tool" 时对应哪次调用
	Name       string          `json:"name"`         // role="tool" 时为工具名
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
func (ps *ProxyServer) chatBody(conversationID, input, userID, upstreamModel string, images []ContentPart, stream bool) map[string]interface{} {
	empty := []interface{}{}
	return map[string]interface{}{
		"conversationId": conversationID,
		"user": map[string]interface{}{
			"id": userID,
			"setting": map[string]interface{}{
				"mcpServers":                 []string{"mcp-server-orcaterm-oauth"},
				"uiServers":                  []string{"ui-tools-orcaterm-explorer"},
				"tools":                      empty,
				"approvedTools":              empty,
				"approvedMCPTools":           empty,
				"enableAutoSubtaskExecution": false,
				"env":                        empty,
				"model":                      upstreamModel,
				"modelDesc":                  map[string]interface{}{},
			},
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

// callStructured 以 stream=false 调用上游，获取结构化 JSON（默认路径）。
func (ps *ProxyServer) callStructured(ctx context.Context, creds Credentials, conversationID, input, upstreamModel string, images []ContentPart) (*upstreamResp, string, error) {
	body, _ := json.Marshal(ps.chatBody(conversationID, input, creds.UserID, upstreamModel, images, false))
	raw, err := ps.doChat(ctx, creds, body)
	if err != nil {
		return nil, "", err
	}
	var parsed upstreamResp
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, raw, fmt.Errorf("parse upstream json: %w", err)
	}
	return &parsed, raw, nil
}

// callRawStream 以 stream=true 调用上游（低延迟路径，内容较脏）。
func (ps *ProxyServer) callRawStream(ctx context.Context, creds Credentials, conversationID, input, upstreamModel string, images []ContentPart) (*http.Response, error) {
	body, _ := json.Marshal(ps.chatBody(conversationID, input, creds.UserID, upstreamModel, images, true))
	req, err := ps.newChatRequest(ctx, creds, body)
	if err != nil {
		return nil, err
	}
	return ps.client.Do(req)
}

func (ps *ProxyServer) doChat(ctx context.Context, creds Credentials, body []byte) (string, error) {
	req, err := ps.newChatRequest(ctx, creds, body)
	if err != nil {
		return "", err
	}
	resp, err := ps.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
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
	return corsMiddleware(mux)
}

func (ps *ProxyServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	creds, ok := ps.cookies.Get()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
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
	})
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
	if a := strings.TrimSpace(ps.config.AliasModel); a != "" && a != def.ID {
		items = append(items, map[string]interface{}{
			"id": a, "object": "model", "created": created, "owned_by": "orcaterm",
			"alias_of":   def.ID,
			"upstream":   def.Upstream,
			"name":       def.Name + " (alias)",
			"deprecated": true,
		})
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
					"tool_calls 返回；调用方执行后把结果以 role=\"tool\" 回传即可，必须带同一个会话键。" +
					"自定义 function 无法在上游注册，因此以调用方自己实现的原生工具名为准。",
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

// handleModelByID 支持 GET /v1/models/{id}（OpenAI 标准端点）。
func (ps *ProxyServer) handleModelByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "", "use GET")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	m, ok := FindModel(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"model %q not found; available: %s", id, strings.Join(ModelIDs(), ", "))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(modelEntry(m, time.Now().Unix(), m.ID == ps.defaultModel().ID))
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
	creds, ok := ps.cookies.Get()
	if !ok {
		writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", "not_logged_in",
			"%s", "OrcaTerm not logged in. Please sign into OrcaTerm first.")
		return
	}

	// ---- 模型解析：把对外 id 映射到上游 setting.model ----
	model, err := ps.resolveModel(req.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"model %s not found; available: %s (set ORCATERM_ALLOW_UNKNOWN_MODEL=1 to pass through)",
			err.Error(), strings.Join(ModelIDs(), ", "))
		return
	}

	// ---- 会话选择：命中即复用上游 conversationId，从而获得多轮上下文 ----
	key := sessionKeyOf(r.Header.Get("X-Session-Id"), req.User)
	if key == "" {
		key = req.SessionID
	}
	var sess *Session
	var cid string
	if key != "" {
		if sess = ps.sessions.Get(key); sess == nil {
			sess = ps.sessions.Create(key, ps.newConversationID())
		}
		cid = sess.CID
		sess.Last = time.Now()
		sess.Turns++
	} else {
		cid = ps.newConversationID() // 无会话标识：一次性请求，无上下文
	}

	// ---- 输入提取：识别"工具结果回传"轮次，否则按多轮/一次性规则取用户消息 ----
	plan, err := extractInputPlan(req.Messages, sess != nil)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_content", "parse content: %v", err)
		return
	}
	input, images := plan.Text, plan.Images
	if strings.TrimSpace(input) == "" && len(images) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing_messages", "no user message found")
		return
	}
	// 工具结果回传必须落在同一个会话上：否则上游拿不到原会话上下文，
	// 孤儿结果会被当作孤立文本，静默产出无意义回答。
	if plan.IsToolResult && ps.config.ToolsMode == "native" {
		if key == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "tool_result_without_session",
				"a tool result must be sent with the same session (X-Session-Id / user) as the tool call")
			return
		}
		if sess == nil || sess.PendingTool == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "no_pending_tool",
				"session %q has no pending tool call; reuse the session key from the tool_calls response", key)
			return
		}
	}

	// ---- tools：默认走"上游原生工具透传"，见 content.go 顶部说明 ----
	if len(req.Tools) > 0 {
		switch ps.config.ToolsMode {
		case "ignore":
			// 忽略，正常问答
		case "inject":
			input += ToolsInstruction(req.Tools)
		case "error":
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "tools_unsupported",
				"ORCATERM_TOOLS_MODE=error rejects all tool requests; use the default 'native' mode")
			return
		default: // native
			// 上游按 setting（uiServers/mcpServers）自带工具，无需额外声明
		}
	}

	promptTokens := EstimateTokens(input)
	turn := 0
	if sess != nil {
		promptTokens += sess.Tokens
		turn = sess.Turns
	}
	// 回显调用方请求的模型名（OpenAI 行为）；未指定时回显解析结果
	respModel := strings.TrimSpace(req.Model)
	if respModel == "" {
		respModel = model.ID
	}
	log.Printf("[INFO] chat session=%q turn=%d model=%s(%s) images=%d stream=%v toolResult=%v input=%s",
		key, turn, model.ID, model.Upstream, len(images), req.Stream, plan.IsToolResult, truncate(input, 80))

	if ps.config.Mode == "raw-stream" {
		nativeTools := len(req.Tools) > 0 && ps.config.ToolsMode == "native"
		upstream, err := ps.callRawStream(r.Context(), creds, cid, input, model.Upstream, images)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "backend_unreachable", "backend request failed: %v", err)
			return
		}
		defer upstream.Body.Close()
		if req.Stream {
			ps.forwardRawStream(w, upstream, respModel, nativeTools)
		} else {
			ps.forwardRawNonStream(w, upstream, respModel, nativeTools)
		}
		return
	}

	// structured 模式（默认）
	parsed, raw, err := ps.callStructured(r.Context(), creds, cid, input, model.Upstream, images)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream_error", "upstream failed: %v", err)
		return
	}
	text, action := parsed.answer()

	// ---- 原生工具调用：action=review + data.tool 映射为 OpenAI tool_calls ----
	var toolCalls []ToolCall
	undeclared := false
	nativeTools := len(req.Tools) > 0 && ps.config.ToolsMode == "native"
	if nativeTools {
		if up := parsed.reviewTool(); up != nil {
			toolCalls = ToolCallsFromUpstream(up)
			text = "" // review 轮没有正文，只有工具意图
		}
	}

	cleaned := text
	degraded := false
	if len(toolCalls) > 0 {
		cleaned = ""
	} else {
		if ps.config.Strip {
			cleaned = cleanAnswer(text)
		}
		degraded = isDegraded(text, cleaned, action)
	}

	// tools 模拟模式（legacy）：从正文中解析调用意图
	if len(toolCalls) == 0 && len(req.Tools) > 0 && ps.config.ToolsMode == "inject" {
		if tcs, rest := ExtractToolCalls(cleaned); len(tcs) > 0 {
			toolCalls, cleaned = tcs, rest
		}
	}

	completionTokens := EstimateTokens(cleaned)
	if len(toolCalls) > 0 {
		completionTokens = 0
		for _, tc := range toolCalls {
			completionTokens += EstimateTokens(tc.Function.Name) + EstimateTokens(tc.Function.Arguments)
		}
	}
	usage := map[string]interface{}{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
		"estimated":         true,
	}
	if sess != nil {
		sess.Tokens = promptTokens + completionTokens
	}

	toolName := ""
	if len(toolCalls) > 0 {
		toolName = toolCalls[0].Function.Name
	}
	// 维护"待回传工具"状态：review 轮置位，工具结果轮清空
	if sess != nil {
		switch {
		case toolName != "":
			sess.PendingTool = toolName
		case plan.IsToolResult:
			sess.PendingTool = ""
		}
	}
	ps.debugMu.Lock()
	ps.lastRaw = raw
	ps.lastClean = cleaned
	ps.lastMeta = map[string]interface{}{
		"action": action, "degraded": degraded, "code": parsed.Code,
		"raw_len": len(text), "clean_len": len(cleaned),
		"session": key, "turn": turn, "images": len(images), "tool_calls": len(toolCalls),
		"model_id": model.ID, "model_upstream": model.Upstream,
		"tool_name": toolName, "tool_result_turn": plan.IsToolResult,
		"thinking": truncate(parsed.safeThinking(), 300),
	}
	ps.debugMu.Unlock()

	if degraded {
		log.Printf("[WARN] 上游输出退化 action=%s raw=%d clean=%d", action, len(text), len(cleaned))
	}
	if len(toolCalls) > 0 {
		log.Printf("[INFO] 上游请求工具 %s args=%s", toolName, truncate(toolCalls[0].Function.Arguments, 120))
		if len(req.Tools) > 0 && !declaresTool(req.Tools, toolName) {
			undeclared = true
			log.Printf("[WARN] 上游请求的工具 %q 不在调用方声明的 tools 中；调用方需自行实现或映射该名称", toolName)
		}
	}

	out := cleaned
	switch ps.config.OnDegraded {
	case "raw":
		// 保留清洗后原文
	case "error":
		if degraded {
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "upstream_degraded",
				"upstream returned a persona/promotional message instead of an answer (action=%s)", action)
			return
		}
	default: // empty
		if degraded {
			out = ""
		}
	}

	w.Header().Set("X-OrcaTerm-Action", action)
	w.Header().Set("X-OrcaTerm-Degraded", boolFlag(degraded))
	w.Header().Set("X-OrcaTerm-Usage-Estimated", "1")
	w.Header().Set("X-OrcaTerm-Model-Id", model.ID)
	w.Header().Set("X-OrcaTerm-Model", model.Upstream)
	if toolName != "" {
		w.Header().Set("X-OrcaTerm-Tool", toolName)
	}
	if plan.IsToolResult {
		w.Header().Set("X-OrcaTerm-Tool-Result-Turn", "1")
	}
	if undeclared {
		w.Header().Set("X-OrcaTerm-Tool-Undeclared", "1")
	}
	total := promptTokens + completionTokens
	if key != "" {
		w.Header().Set("X-OrcaTerm-Session", key)
		w.Header().Set("X-OrcaTerm-Turn", strconv.Itoa(turn))
		w.Header().Set("X-OrcaTerm-Context-Used", strconv.Itoa(total))
		w.Header().Set("X-OrcaTerm-Context-Limit", strconv.Itoa(ps.config.ContextLimit))
		w.Header().Set("X-OrcaTerm-Context-Remaining", strconv.Itoa(maxInt(0, ps.config.ContextLimit-total)))
	}

	if req.Stream {
		ps.writeSyntheticStream(w, respModel, out, toolCalls, usage)
		return
	}
	writeChatCompletionJSON(w, respModel, out, toolCalls, usage)
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

func writeChatCompletionJSON(w http.ResponseWriter, model, content string, toolCalls []ToolCall, usage map[string]interface{}) {
	msg := map[string]interface{}{"role": "assistant", "content": content}
	finish := "stop"
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if strings.TrimSpace(content) == "" {
			msg["content"] = nil
		}
		finish = "tool_calls"
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
func (ps *ProxyServer) writeSyntheticStream(w http.ResponseWriter, model, text string, toolCalls []ToolCall, usage map[string]interface{}) {
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
		time.Sleep(12 * time.Millisecond)
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		if b, err := json.Marshal(toolCalls); err == nil {
			chunk := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{"tool_calls":%s},"finish_reason":null}]}`,
				id, created, model, string(b))
			fmt.Fprint(w, chunk+"\n\n")
			flusher.Flush()
		}
		finish = "tool_calls"
	}
	end := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`,
		id, created, model, finish)
	fmt.Fprint(w, end+"\n\n")
	if b, err := json.Marshal(usage); err == nil {
		fmt.Fprintf(w, "data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[],\"usage\":%s}\n\n", id, model, string(b))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ===== raw-stream 模式（保留，低延迟但内容较脏）=====

func (ps *ProxyServer) forwardRawStream(w http.ResponseWriter, resp *http.Response, model string, nativeTools bool) {
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		writeOpenAIError(w, resp.StatusCode, "api_error", "upstream_error", "%s", truncate(string(body), 500))
		return
	}
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
	emit := func(s string) {
		if s == "" {
			return
		}
		chunk := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`,
			id, created, model, jsonString(s))
		fmt.Fprint(w, chunk+"\n\n")
		flusher.Flush()
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var sp personaStripper
	var env strings.Builder
	var toolCalls []ToolCall
	inEnvelope := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var frame struct {
			Content string `json:"content"`
			Type    string `json:"type"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil || frame.Type != "ai" || frame.Content == "" {
			continue
		}
		if inEnvelope {
			env.WriteString(frame.Content)
			continue
		}
		if looksLikeEnvelope(frame.Content) {
			inEnvelope = true
			env.WriteString(frame.Content)
			emit(sp.flush())
			continue
		}
		if !ps.config.Strip {
			emit(frame.Content)
			continue
		}
		if out := sp.feed(frame.Content); out != "" {
			emit(out)
		}
	}
	if inEnvelope {
		// 上游在流式模式把内部动作以 ```json 包络逐字吐出：提取工具调用或有正文
		if nativeTools {
			if up, ok := ParseReviewEnvelope(env.String()); ok {
				toolCalls = ToolCallsFromUpstream(up)
			}
		}
		if len(toolCalls) == 0 {
			if parsed, ok := ParseChatEnvelope(env.String()); ok {
				if t := strings.TrimSpace(parsed.TaskCompletion); t != "" {
					emit(t)
				} else if q := strings.TrimSpace(parsed.Question); q != "" {
					emit(q)
				}
			}
		}
	}
	emit(sp.flush())
	finish := "stop"
	if len(toolCalls) > 0 {
		if b, err := json.Marshal(toolCalls); err == nil {
			chunk := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{"tool_calls":%s},"finish_reason":null}]}`,
				id, created, model, string(b))
			fmt.Fprint(w, chunk+"\n\n")
			flusher.Flush()
		}
		finish = "tool_calls"
	}
	end := fmt.Sprintf(`data: {"id":%q,"object":"chat.completion.chunk","created":%d,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`,
		id, created, model, finish)
	fmt.Fprint(w, end+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (ps *ProxyServer) forwardRawNonStream(w http.ResponseWriter, resp *http.Response, model string, nativeTools bool) {
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		writeOpenAIError(w, resp.StatusCode, "api_error", "upstream_error", "%s", truncate(string(body), 500))
		return
	}
	var sb strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var frame struct {
			Content string `json:"content"`
			Type    string `json:"type"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil || frame.Type != "ai" {
			continue
		}
		sb.WriteString(frame.Content)
	}
	text := sb.String()
	if nativeTools {
		if up, ok := ParseReviewEnvelope(text); ok {
			writeChatCompletionJSON(w, model, "", ToolCallsFromUpstream(up), nil)
			return
		}
	}
	if ps.config.Strip {
		text = stripThoughts(text)
		text = stripFencedJSON(text)
		text = stripEnvelope(text)
	}
	writeChatCompletionJSON(w, model, strings.TrimSpace(text), nil, nil)
}

// ===== 清洗管线 =====

var (
	personaMarkers = []string{
		"OrcaTerm AI", "OrcaTerm（遨驰终端）", "遨驰终端", "我是 OrcaTerm", "我是OrcaTerm",
		"云端服务器运维专家", "云端服务器运维助手", "云端运维助手", "云端运维专家", "运维专家",
	}
	capabilityWords = []string{
		"远程服务器", "远程连接", "远程登录", "命令执行", "执行命令", "文件操作", "文件读写",
		"编辑文件", "搜索文件", "创建文件", "故障排查", "排查故障", "软件安装", "安装软件",
		"软件部署", "服务部署", "部署服务", "配置修改", "配置服务", "服务启停", "启停服务",
		"管理服务", "云产品", "云资源", "云资源管理", "批量操作", "TAT Agent", "性能诊断",
		"系统监控", "状态监控", "系统诊断", "安全审计", "应用部署", "文档查询", "文档搜索",
		"日志分析", "分析日志", "查看日志", "日志查看", "问题定位", "定位问题", "修复故障",
		"错误定位", "自动化运维", "终端管理", "终端连接", "脚本运行", "运行脚本", "最佳实践",
	}
	activityWords = []string{
		"周年", "抽奖", "玩法", "祝福", "Lighthouse", "活动规则", "邀请码", "开奖",
		"送祝福", "选择你的故事", "云上故事", "你的故事", "活动页", "口令",
		"轻量应用服务器六周年", "祝福方式", "分享邀请好友",
	}
	guidancePhrases = []string{
		"请告诉我您需要", "请告诉我您的", "请告诉我您想要", "请直接告诉我", "请告诉我",
		"请问您需要", "您需要什么帮助", "需要什么帮助", "请提供", "请描述您",
		"有什么可以帮", "我会立即", "我会尽力", "随时为您", "等待您提出",
		// 能力清单的引导句（"您可以通过我完成以下工作："）
		"以下工作", "服务范围", "核心能力", "以下方面", "您可以完成", "能为您完成",
	}
)

func containsAny(hay string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}

// stripThoughts 去掉 <think>…</think> 思考段。
func stripThoughts(s string) string {
	for {
		start := strings.Index(s, "<think>")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "</think>")
		if end < 0 {
			return strings.TrimSpace(s[:start])
		}
		s = s[:start] + s[start+end+len("</think>"):]
	}
}

// stripFencedJSON 去掉所有 ```…``` 围栏块（上游的内部动作/技能包络都长这样）。
func stripFencedJSON(s string) string {
	var out strings.Builder
	inFence := false
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			out.WriteString(ln)
			out.WriteString("\n")
		}
	}
	return out.String()
}

// isListLine 判断是否为列表项（- / * / • / 数字. 开头）。
func isListLine(t string) bool {
	if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "• ") {
		return true
	}
	if len(t) > 2 && t[0] >= '0' && t[0] <= '9' && (t[1] == '.' || t[1] == ')') {
		return true
	}
	return false
}

// isNoiseLine 判断一行是否属于"人设/营销/菜单"噪音。
func isNoiseLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	if containsAny(t, activityWords) {
		return true
	}
	if strings.Contains(t, "我是") && containsAny(t, personaMarkers) {
		return true
	}
	if strings.HasPrefix(t, "我可以") || strings.HasPrefix(t, "我能") ||
		strings.HasPrefix(t, "我能够") || strings.HasPrefix(t, "我支持") ||
		strings.HasPrefix(t, "我协助") {
		if containsAny(t, []string{"帮", "协助", "支持", "提供", "完成", "处理"}) && len([]rune(t)) < 120 {
			return true
		}
	}
	if isListLine(t) && containsAny(t, capabilityWords) {
		return true
	}
	if containsAny(t, guidancePhrases) && len([]rune(t)) < 100 {
		return true
	}
	if strings.HasPrefix(t, "#") && (containsAny(t, activityWords) || containsAny(t, personaMarkers)) {
		return true
	}
	return false
}

// cleanAnswer 行级清洗：剔除人设、能力罗列、活动营销、选择菜单与收尾引导语。
// 若检测到活动/营销上下文，则进入激进模式：连编号选项行一并丢弃，避免菜单残留。
func cleanAnswer(s string) string {
	s = stripThoughts(s)
	s = stripFencedJSON(s)
	aggressive := containsAny(s, activityWords)
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, ln := range lines {
		if isNoiseLine(ln) {
			continue
		}
		if aggressive && isListLine(strings.TrimSpace(ln)) {
			continue
		}
		kept = append(kept, ln)
	}
	out := strings.TrimSpace(strings.Join(kept, "\n"))
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}

// isDegraded 判断本轮上游是否"退化"（未给出有效答案）。
func isDegraded(raw, cleaned, action string) bool {
	if strings.TrimSpace(cleaned) == "" {
		return true
	}
	if containsAny(raw, activityWords) {
		return true
	}
	head := raw
	if len([]rune(head)) > 120 {
		head = string([]rune(head)[:120])
	}
	if containsAny(head, personaMarkers) {
		return true
	}
	if action == "review" && strings.TrimSpace(cleaned) == "" {
		return true
	}
	return false
}

// looksLikeEnvelope 判断流式片段是否进入上游内部动作 JSON 包络。
func looksLikeEnvelope(s string) bool {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		return true
	}
	return strings.HasPrefix(t, "{") &&
		(strings.Contains(t, "\"action\"") || strings.Contains(t, "taskCompletion") || strings.Contains(t, "\"tool\""))
}

// stripEnvelope 剥离聚合正文尾部的平衡 JSON 对象（已修复 s[:0] 切空的缺陷）。
func stripEnvelope(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "```json"); i > 0 {
		if cut := strings.TrimSpace(s[:i]); cut != "" {
			return cut
		}
	}
	if !strings.HasSuffix(s, "}") {
		return s
	}
	for i := len(s) - 1; i > 0; i-- {
		if s[i] == '{' {
			var probe interface{}
			if json.Unmarshal([]byte(s[i:]), &probe) == nil {
				if cut := strings.TrimSpace(s[:i]); cut != "" {
					return cut
				}
			}
		}
	}
	return strings.TrimSpace(s)
}

// personaStripper 流式人设前导剥离（raw-stream 模式使用）。
type personaStripper struct {
	head   strings.Builder
	active bool
	done   bool
}

func (p *personaStripper) feed(s string) string {
	if p.done {
		return s
	}
	p.head.WriteString(s)
	h := p.head.String()
	if !p.active {
		if containsAny(h, personaMarkers) {
			p.active = true
			return ""
		}
		if p.head.Len() > 40 || strings.ContainsAny(h, "\n") {
			out := h
			p.head.Reset()
			p.done = true
			return out
		}
		return ""
	}
	if strings.Contains(h, "需要什么帮助") || strings.Contains(h, "请告诉我") {
		p.head.Reset()
		p.done = true
		return ""
	}
	if p.head.Len() > 800 {
		out := h
		p.head.Reset()
		p.done = true
		return out
	}
	return ""
}

func (p *personaStripper) flush() string {
	if p.done {
		return ""
	}
	out := strings.TrimSpace(p.head.String())
	p.head.Reset()
	p.done = true
	return out
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

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Expose-Headers",
			"X-OrcaTerm-Action, X-OrcaTerm-Degraded, X-OrcaTerm-Session, X-OrcaTerm-Turn, "+
				"X-OrcaTerm-Context-Used, X-OrcaTerm-Context-Limit, X-OrcaTerm-Context-Remaining, "+
				"X-OrcaTerm-Usage-Estimated, X-OrcaTerm-Tool, X-OrcaTerm-Tool-Result-Turn, X-OrcaTerm-Tool-Undeclared, "+
				"X-OrcaTerm-Model, X-OrcaTerm-Model-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(200)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ===== 入口 =====

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	config := loadConfig()
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
