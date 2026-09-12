# OrcaBridge

把 OrcaTerm（遨驰终端）内置 AI 助手桥接为 **OpenAI Chat Completions 兼容子集**。它读取本机 OrcaTerm 登录态，适合接入支持自定义 OpenAI Base URL 的客户端。

> 非官方项目，与腾讯云或 OrcaTerm 官方无关。代理不校验客户端 API Key，也不应直接暴露到公网；使用前请确认符合 OrcaTerm 服务条款。

## 能力边界

- 支持 `POST /v1/chat/completions`、`GET /v1/models`、多轮会话、图片输入、合成 SSE 和估算 usage。
- 支持的主要请求字段：`model`、`messages`、`stream`、`stream_options.include_usage`、`user`、`tools`。
- 不是 OpenAI API 的完整实现；未列出的采样、响应格式及批处理能力不要假定有效。
- tools 是 **OrcaTerm 原生工具桥接**，不是任意 OpenAI function 执行器：不能注册任意自定义 function，不支持强制指定 tool choice，一次也只产出一个工具调用。
- 第三方 agent 客户端（ZCode / Cline / Roo 等）声明自己的工具集是允许的：默认 `pass` 模式会接受，并通过响应头 `X-OrcaTerm-Unsupported-Tools` 如实列出上游没有的那些名字。
- `structured` 与 `raw-stream` 都会先完整读取并解析上游响应，再输出普通 JSON 或合成 SSE；`stream: true` 主要用于客户端协议兼容，不提供上游首 token 低延迟。

## 快速开始

先打开 OrcaTerm 并登录，然后启动：

```powershell
.\orcabridge.exe
```

正常日志示例：

```text
[INFO] OrcaTerm AI Proxy v0.9.0 starting on http://localhost:8080
[INFO] 凭据: true (sid=true ot=true)
```

验证登录态并请求聊天：

```powershell
curl http://localhost:8080/health
curl http://localhost:8080/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"1+1等于几？"}]}'
```

客户端配置：Base URL 填 `http://localhost:8080/v1`；API Key 可填任意非空占位值；默认模型为 `deepseek-v4-flash`。

## 模型

`GET /v1/models` 默认返回 18 个列表项，但其中只有 **17 个唯一上游模型**：8 个 current、9 个 legacy，另有 1 个 deprecated 兼容别名 `orcaterm-assistant`，指向默认模型。

| current id | 上游模型 |
|---|---|
| `deepseek-v4-pro` | `TokenHub/deepseek-v4-pro` |
| `deepseek-v4-flash`（默认） | `TokenHub/deepseek-v4-flash` |
| `hy3` | `Hunyuan3/hy3` |
| `hy4-preview` | `Hunyuan3/hy4-preview` |
| `kimi-k3` | `TokenHub/kimi-k3` |
| `glm-5.2` | `TokenHub/glm-5.2` |
| `glm-5.3` | `TokenHub/glm-5.3` |
| `glm-5.3-flash` | `TokenHub/glm-5.3-flash` |

9 个 legacy id：`deepseek-chat-latest`、`glm-4.7`、`kimi-k2`、`kimi-k2.5`、`kimi-k2-thinking`、`kimi-k2.5-thinking`、`hunyuan-t1`、`hunyuan-turbos`、`qwen3`。它们仍可用，但已不在 OrcaTerm 当前模型选择器中。

未知模型默认返回 404；仅在明确设置 `ORCATERM_ALLOW_UNKNOWN_MODEL=1` 时直通上游。上游对错误模型可能静默返回空内容，因此不建议开启。

`GET /v1/models/{id}` 接受目录 id、上游全名与显示名，别名 `orcaterm-assistant` 的检索结果与列表项一致（带 `alias_of` 与 `deprecated`）。

## 消息与会话

带同一个 `X-Session-Id` 请求头或 OpenAI `user` 字段可复用上游会话；不带会话键时，每次请求都是新会话。

`system` 消息采用**首轮语义**：它在新会话的首个请求中参与输入；复用已有会话时，不应把后续 `system` 当作可覆盖上游系统提示的逐轮配置。需要更换 system 语义时，请创建新会话。

图片使用 Chat Completions 的 `image_url` 内容块，支持 data URI 与远程 URL。

## 流式与 usage

`stream: true` 返回代理在完整解析上游结果后合成的 SSE。需要流式 usage 时设置：

```json
{"stream":true,"stream_options":{"include_usage":true}}
```

开启后，结束前会增加一个 `choices: []` 的 usage chunk。usage 与上下文余量均为本地估算，不代表上游计费值。

## tools

声明的工具名必须是 OrcaTerm 原生工具。`GET /v1/models` 之外，完整可用名字与分组见 [`docs/OrcaTerm AI 代理工具分析报告.md`](docs/OrcaTerm%20AI%20代理工具分析报告.md) §4；常用的是 `fetch`、`list_terminals`、`execute_terminal_command`、`run_commands`、`submit_agent_tasks`、`read_cloud_space_file`。代理把上游 `action=review` 转成 OpenAI `tool_calls`，由调用方执行后回传结果。

终端类任务是一条工具链：只声明 `execute_terminal_command` 时，上游会先请求 `list_terminals`，任务会以 `upstream_requested_undeclared_tool` 失败。请把可能用到的整条链路一次性声明。

上游经常先用一句反问确认（"是否按这个思路开始执行？"），此时响应是正常 200、`finish_reason` 为 `stop`，但真实动作记录在响应头 `X-OrcaTerm-Action: ask` 里；agent 客户端应读取该头来区分"最终答案"与"反问"。详见分析报告 §3.3。

## 登录态

代理读取本机 OrcaTerm 的 `.cookies`，登录态过期后上游会返回 HTTP 200 加 `code=10050000`，代理会把它归类为 502 `upstream_auth_error` 并提示重新登录。`GET /health` 会附带 `token_expires_at` 与 `token_expired`，可在请求失败前发现需要刷新登录；上游没有刷新接口，代理无法自行续期。

现代格式使用 `assistant.tool_calls[].id` 与 `role:"tool"` 的 `tool_call_id` 关联。在本修复版本中，只要完整回传对应消息，现代 tool result 可在没有私有 `X-Session-Id` 请求头时闭环；仍可使用 `X-Session-Id` 或 `user` 显式维持会话。

旧式 `role:"function"` 结果没有 `tool_call_id`，无法仅靠消息可靠关联待处理调用，因此必须复用原来的 `X-Session-Id` 或 `user`。原生工具一次只处理一个待处理调用；调用方传 `parallel_tool_calls` 是允许的，代理只返回一个调用。工具 `content` 为空字符串表示拒绝执行。

客户端回放**自己**工具的结果（`role:"tool"` 且 id 不是代理发出的）不会被当作待回填调用，而是作为本轮输入送进上游，`content` 不会丢。代理只在工具名属于 OrcaTerm 原生工具时才去匹配待处理调用。

上游不支持的能力会显式报错而不是静默忽略：同时传 `tools` 与 `functions`、`tool_choice:"required"`、具名强制选择、`parallel_tool_calls:true`、自定义 function、一次回传多个工具结果，以及缺少 `tool_call_id` 的 `role:"tool"` 都会返回 400，错误码见分析报告 §4.2。

## CORS 与管理端点

CORS 默认关闭，不返回跨域许可头。浏览器确需跨域时，显式设置 `ORCATERM_CORS_ORIGINS`：多个 origin 用逗号分隔；值为 `*` 才允许任意 origin。未在 allowlist 中的 origin 发起的预检返回 403。

```powershell
$env:ORCATERM_CORS_ORIGINS="http://localhost:3000,https://app.example.com"
```

`GET|DELETE /v1/sessions` 可读取或清除会话，`GET /debug/last` 可查看最近一次上游原始响应；它们可能暴露提示词、工具结果、会话标识或调试数据，属于敏感管理端点。代理无入站鉴权，务必保持 localhost 监听，并避免经反向代理公开这些端点。

## 配置

| 变量 | 默认值 | 合法值或说明 |
|---|---|---|
| `PROXY_PORT` | `8080` | 本地监听端口 |
| `ORCATERM_COOKIES` | OrcaTerm 本机 cookies 路径 | 登录文件路径 |
| `ORCATERM_UPSTREAM_MODEL` | `TokenHub/deepseek-v4-flash` | 默认上游模型 id |
| `ORCATERM_MODEL` | `orcaterm-assistant` | deprecated 兼容别名 |
| `ORCATERM_ALLOW_UNKNOWN_MODEL` | `0` | `0` / `1` |
| `ORCATERM_MODE` | `structured` | `structured` / `raw-stream`；两者均先完整解析 |
| `ORCATERM_ON_DEGRADED` | `empty` | `empty` / `raw` / `error` |
| `ORCATERM_TOOLS_MODE` | `pass` | `pass` / `native` / `ignore` / `error`；`inject` 仅为 legacy 模拟且不可靠。见分析报告 §4.4 |
| `ORCATERM_HIDE_TOOLS` | `1` | `1` 要求上游隐藏内部工具消息；`0` 关闭 HideTools |
| `ORCATERM_STRIP_PERSONA` | `1` | `0` / `1`，清洗兜底 |
| `ORCATERM_SESSION_TTL` | `30m` | Go duration，如 `10m`、`1h` |
| `ORCATERM_SESSION_MAX` | `1000` | 正整数 |
| `ORCATERM_CONTEXT_LIMIT` | `128000` | 正整数，仅用于估算 |
| `ORCATERM_CORS_ORIGINS` | 空 | 关闭；逗号分隔 allowlist，或显式 `*` |

另有 `ORCATERM_API_URL`、`ORCATERM_PRODUCT`、`ORCATERM_USER_AGENT`、`ORCATERM_PROMPT_OVERRIDE` 等上游兼容配置；通常无需修改。

枚举或数值非法时进程会在启动前直接失败并打印原因，不会静默回退到默认值。CORS origin 会被规范化（去掉尾部 `/`），带路径、查询串、片段或非 http/https scheme 的值视为非法。

## 构建与限制

```bash
go build -trimpath -o orcabridge.exe .
go test ./...
```

模型目录为静态快照；上游变更不会自动同步。服务依赖 OrcaTerm 登录态；会话默认空闲 30 分钟失效。更详细的协议与实现边界见 [`docs/OrcaTerm AI 代理工具分析报告.md`](docs/OrcaTerm%20AI%20代理工具分析报告.md)。
