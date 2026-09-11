# OrcaBridge

把 **OrcaTerm（遨驰终端）自带的 AI 助手** 包装成一个标准的 **OpenAI 接口**，
这样任何支持 OpenAI API 的客户端、Agent 框架、脚本都能直接调用它。

> ⚠️ **非官方项目**。与腾讯云 / OrcaTerm 官方无任何关联，是个人逆向实现的本地桥接工具。
> 它复用你自己机器的登录态，不涉及破解或绕过鉴权；使用前请确认符合 OrcaTerm 的服务条款。

- 单文件 Go 程序，**零外部依赖**，编译出来就一个 exe
- 不用填 API Key：直接读本机 OrcaTerm 的登录状态
- **18 个模型可选**，切模型就是换个 `model` 字段
- 支持多轮对话、图片输入、流式输出、**工具调用（tools）**

---

## 快速开始

**1. 先在 OrcaTerm 里登录**

代理靠本机 OrcaTerm 的登录态工作，所以请先打开 OrcaTerm 并登录账号（微信或 OAuth 都行）。
之后不用做任何配置。

**2. 启动代理**

```powershell
.\orcabridge.exe
```

看到这几行就说明起来了：

```
[INFO] OrcaTerm AI Proxy v0.7.0 starting on http://localhost:8080
[INFO] 凭据: true (sid=true ot=true)
```

**3. 验证一下**

```powershell
curl http://localhost:8080/health
```

确认 `"authed": true` 即可。然后随便问一句：

```powershell
curl http://localhost:8080/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"1+1等于几？"}]}'
```

返回 `{"choices":[{"message":{"content":"2"}}]}` 就成了。

---

## 接到其他客户端上

把任何 OpenAI 兼容客户端指过来就行：

| 配置项 | 填什么 |
|---|---|
| Base URL | `http://localhost:8080/v1` |
| API Key | 随便填（例如 `sk-anything`），代理不校验 |
| Model | `deepseek-v4-flash`，或下面列表里的任意一个 |

> 想换端口：启动前设环境变量 `PROXY_PORT`，比如 `$env:PROXY_PORT=9000; .\orcabridge.exe`。

---

## 选择模型

OrcaTerm 内置了多个模型，代理把它们全部暴露出来。**拉一下列表就知道有哪些**：

```powershell
curl http://localhost:8080/v1/models
```

这些是和客户端模型选择器一一对应的 8 个（都实测可用）：

| `model` 填什么 | 说明 | 特点 |
|---|---|---|
| `deepseek-v4-flash` | DeepSeek 轻量版（**默认**） | 响应快，适合日常高频 |
| `deepseek-v4-pro` | DeepSeek 完整版 | 1M 上下文，擅长长程任务与复杂推理 |
| `hy3` | 腾讯混元 Hy3 | 中文理解与内容创作 |
| `hy4-preview` | 腾讯混元 Hy4（预览） | 复杂任务与长文本 |
| `kimi-k3` | Kimi K3 | 1M 上下文，长程自主任务、前端开发 |
| `glm-5.2` | 智谱 GLM 5.2 | 代码生成与工具调用 |
| `glm-5.3` | 智谱 GLM 5.3 | 同上，更新版 |
| `glm-5.3-flash` | GLM 轻量版 | 兼顾效果与速度 |

另外还有一批客户端界面里已经不展示、但实测仍然能用的模型，也能在列表里查到
（带 `legacy: true` 标记），比如 `kimi-k2.5-thinking`、`hunyuan-t1`、`qwen3` 等。

**怎么切**：在请求里把 `model` 换成上面任意一个值即可，一次请求一个模型：

```powershell
curl http://localhost:8080/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d '{"model":"glm-5.3","messages":[{"role":"user","content":"用一句话介绍杭州"}]}'
```

响应头会告诉你这一轮实际用了哪个上游模型：

```
X-Orcaterm-Model-Id: glm-5.3
X-Orcaterm-Model: TokenHub/glm-5.3
```

几个细节：

- **默认模型是 `deepseek-v4-flash`**。请求里 `model` 留空、或者沿用旧名字
  `orcaterm-assistant`，都会落到它上面（老调用方不用改）。
- **模型名写错会明确报 404**，而不是给你一个空回答。
  这点是刻意的——上游遇到不认识的模型会静默返回空内容，看起来像"成功"，很难排查。
  错误信息里会附上全部可用 id。
- 如果你用的模型不在列表里（比如服务端后来加了新模型），
  设 `ORCATERM_ALLOW_UNKNOWN_MODEL=1` 可以让任意模型名直通上游。
- 同一段多轮对话中途换模型是可以的，模型是按请求生效的。

---

## 用法

### 多轮对话

带上同一个 `X-Session-Id` 请求头（或者 OpenAI 标准的 `user` 字段）即可，
**对话历史由服务端保存**，你每次只需要发最新那一句话，不用把历史全传回来：

```powershell
# 第一轮
curl -H "X-Session-Id: chat-001" -H "Content-Type: application/json" `
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"我叫小明"}]}' `
  http://localhost:8080/v1/chat/completions

# 第二轮 —— 只发新问题
curl -H "X-Session-Id: chat-001" -H "Content-Type: application/json" `
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"我叫什么名字？"}]}' `
  http://localhost:8080/v1/chat/completions
```

不带 `X-Session-Id` 也能用，但每次都是全新对话（没有上下文）。

### 发图片

标准 OpenAI 多模态格式，base64 data URI 和远程 URL 都支持：

```json
{
  "model": "deepseek-v4-flash",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "这张图是什么颜色？"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,<base64>"}}
    ]
  }]
}
```

### 工具调用（tools）

**先说清楚它是怎么工作的**，不然容易踩坑：

这个代理**自己不执行任何工具**。它做的事是——模型想调工具时，把
「想调哪个工具、参数是什么」用标准 OpenAI `tool_calls` 交给你；
你去执行，然后把结果塞回来。所以是标准的两段式：

```powershell
# ① 第一轮：声明工具，模型会返回 tool_calls
curl -H "X-Session-Id: agent-001" -H "Content-Type: application/json" -d '{
  "model": "deepseek-v4-flash",
  "messages": [{"role":"user","content":"请调用 fetch 工具抓取 https://example.com"}],
  "tools": [{"type":"function","function":{
    "name":"fetch",
    "description":"抓取网页内容",
    "parameters":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}
  }}]
}' http://localhost:8080/v1/chat/completions
```

拿到的响应长这样（注意 `finish_reason` 是 `tool_calls`，`content` 是 `null`）：

```json
{"choices":[{"finish_reason":"tool_calls","message":{"content":null,
  "tool_calls":[{"id":"call_xxx","type":"function",
    "function":{"name":"fetch","arguments":"{\"url\":\"https://example.com\"}"}}]}}]}
```

```powershell
# ② 第二轮：把执行结果回传（注意：必须带同一个 X-Session-Id）
curl -H "X-Session-Id: agent-001" -H "Content-Type: application/json" -d '{
  "model": "deepseek-v4-flash",
  "messages": [
    {"role":"user","content":"请调用 fetch 工具抓取 https://example.com"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"call_xxx","type":"function",
      "function":{"name":"fetch","arguments":"{\"url\":\"https://example.com\"}"}}]},
    {"role":"tool","tool_call_id":"call_xxx","name":"fetch","content":"<你抓到的网页内容>"}
  ],
  "tools": [ ...和上面一样... ]
}' http://localhost:8080/v1/chat/completions
```

模型就会基于你给的工具结果继续作答。

**三条必须记住的规则：**

1. **工具名要用 OrcaTerm 自己的名字**，别自己起。可用的名字例如
   `fetch`、`execute_terminal_command`、`run_commands`、`submit_agent_tasks`、
   `read_cloud_space_file`。
   你声明一个模型不认识的工具（比如 `get_weather`），它会直接回答"当前环境没有该工具"。
   上游如果调了一个你没声明的工具，响应头会带 `X-OrcaTerm-Tool-Undeclared: 1`。
2. **回传结果时必须复用触发调用的那个 `X-Session-Id`**。工具结果只认原会话，
   用错会话代理会返回 400 `no_pending_tool`。
   （这是故意挡的：否则结果会被当成一句莫名其妙的话，产出垃圾回答。）
3. **想"拒绝执行"这个工具**，就把 `content` 传空字符串。代理会按 OrcaTerm 的格式
   告诉模型"用户拒绝了、环境没变化"，模型就会停下来不再重试。

流式（`stream: true`）同样支持，`finish_reason` 会是 `tool_calls`。

---

## 配置

全部通过环境变量，都有默认值，**不改也能跑**：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PROXY_PORT` | `8080` | 监听端口 |
| `ORCATERM_COOKIES` | `%LOCALAPPDATA%\com.orcaterm-desktop.app\.cookies` | OrcaTerm 登录文件位置 |
| `ORCATERM_UPSTREAM_MODEL` | `TokenHub/deepseek-v4-flash` | 默认模型的上游 id |
| `ORCATERM_MODEL` | `orcaterm-assistant` | 兼容别名，等价于默认模型（不建议再用于新代码） |
| `ORCATERM_ALLOW_UNKNOWN_MODEL` | `0` | 设为 `1` 时允许列表外的模型名直通上游 |
| `ORCATERM_SESSION_TTL` | `30m` | 会话多久不用就作废 |
| `ORCATERM_TOOLS_MODE` | `native` | `native` 原生工具透传（默认）/ `ignore` 忽略 tools / `error` 一律拒绝 |
| `ORCATERM_MODE` | `structured` | 一般不用改 |
| `ORCATERM_CONTEXT_LIMIT` | `128000` | 只用于估算"上下文还剩多少" |

响应头里会带一些有用的信息：

| 响应头 | 含义 |
|---|---|
| `X-OrcaTerm-Action` | 本轮模型干了什么：`completion` 回答 / `ask` 反问 / `review` 请求调工具 |
| `X-OrcaTerm-Model` / `-Model-Id` | 本轮实际使用的上游模型 id / 对外模型 id |
| `X-OrcaTerm-Tool` | 本轮请求的工具名 |
| `X-OrcaTerm-Tool-Result-Turn` | `1` 表示这是工具结果回传那一轮 |
| `X-OrcaTerm-Session` / `-Turn` | 命中的会话和轮次 |
| `X-OrcaTerm-Context-Remaining` | 上下文剩余量（估算值） |
| `X-OrcaTerm-Degraded` | `1` 表示本轮没取到有效回答 |

---

## 常见问题

**启动报 `bind: Only one usage of each socket address`**
端口被占了，八成是已经开着一个代理。换个端口：`$env:PROXY_PORT=8083`，
或者先关掉旧进程。

**`/health` 里 `authed` 是 `false`**
OrcaTerm 没登录或登录过期了。打开 OrcaTerm 重新登录一下，代理会自动重新读取登录文件，不用重启。

**模型一直说"当前环境没有 XX 工具"**
你声明了 OrcaTerm 不认识的工具名。换成它自带的工具名（`fetch` 等）。

**报 404 `model_not_found`**
模型名写错了。`GET /v1/models` 看可用列表，错误信息里也会列出全部合法 id。

**换个模型后回答变空了**
正常情况不会发生——代理对不在列表里的模型名会直接报 404。
如果你开了 `ORCATERM_ALLOW_UNKNOWN_MODEL=1`，写错名字就会拿到空回答
（上游对未知模型不报错，只返回空）。这时候用 `GET /v1/models` 核对一下名字。

**返回 400 `no_pending_tool`**
你回传工具结果时用的会话和发起调用的会话对不上。检查 `X-Session-Id` 是否一致。

**usage 里的 token 数是准的吗**
不准，是代理本地估算的（响应里 `estimated: true` 就是提醒这个）。
上游接口不返回任何 token 字段，所以只能估。

**想看上游到底返回了什么**
访问 `http://localhost:8080/debug/last`，能看到最近一次的原始响应和清洗结果。

---

## 目录结构

```
OrcaTermProxy/
├── README.md                    ← 你正在读的这个
├── main.go / content.go / session.go   ← 主程序
├── clean_test.go                ← 单元测试
├── orcabridge.exe           ← 编译产物
├── docs/
│   └── OrcaTerm AI 代理工具分析报告.md  ← 协议逆向细节，想深入了解看这个
└── tools/reverse/               ← 逆向与探测脚本（附 README）
```

自己编译（需要 Go 1.21+）：

```bash
go build -trimpath -o orcabridge.exe .
go test ./...
```

> `-trimpath` 请务必带上：不带的话，编译产物里会嵌入**你本机的源码绝对路径**
> （例如 `/home/dev/orcabridge/main.go`），发布二进制时会泄露目录结构，也会让构建不可复现。

---

## 已知限制

- **模型列表是"写死"的**。上游模型增删不会自动同步，需要更新代码里的目录
  （`content.go` 的 `modelCatalog`）。临时应急可以用 `ORCATERM_ALLOW_UNKNOWN_MODEL=1` 直通。
- **只能调 OrcaTerm 自带的工具**，没法注册你自己的函数。这是上游服务端的限制
  （服务端系统提示词对"工具是否注册"有最终裁决权，7 种注册路径都试过，全被拒）。
- **usage 和上下文剩余量都是本地估算**，跟真实计费口径无关。
- 依赖 OrcaTerm 的登录态，**登录过期后需要重新登录**。
- 会话默认 30 分钟过期，过期后同名会话会重新开始（上下文清空）。
  工具调用的两轮之间别隔太久。
- 不知道上游各模型确切的上下文长度，`ORCATERM_CONTEXT_LIMIT` 是统一按 128000 估的。

更详细的上游协议、逆向过程和实测记录都在
[`docs/OrcaTerm AI 代理工具分析报告.md`](docs/OrcaTerm%20AI%20代理工具分析报告.md)。
