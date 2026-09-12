# OrcaBridge：OrcaTerm AI 协议与兼容性分析

> 版本：v0.8.0
> 更新：2026-09-12
> 范围：本修复版本的已实现行为

## 1. 定位与结论

OrcaBridge 读取本机 OrcaTerm 登录态，将 `lightai.cloud.tencent.com/assistant/chat` 桥接为 **OpenAI Chat Completions 兼容子集**，而不是完整 OpenAI API 实现。

关键结论：

1. 模型目录有 17 个唯一上游模型：8 个 current、9 个 legacy；默认还暴露 deprecated alias `orcaterm-assistant`，所以 `/v1/models` 默认有 18 个列表项。
2. `structured` 和 `raw-stream` 都先完整读取、解析上游响应，再生成 OpenAI JSON 或合成 SSE；两种模式都不是首 token 低延迟直通。
3. tools 是 OrcaTerm 原生工具桥接：不能注册任意自定义 function，不支持强制 tool choice，
   一次只产出一个工具调用。第三方 agent 客户端自己的工具声明会被接受但不会被调用，
   并通过响应头 `X-OrcaTerm-Unsupported-Tools` 如实暴露（见 §4.4）。
4. 现代 `tool_call_id` 可在无私有 session header 时关联闭环；legacy `role:"function"` 结果没有调用 id，必须复用会话键。
5. system 是首轮会话语义；CORS 默认关闭，管理端点应视为敏感接口。

## 2. 上游协议

端点：

```text
POST https://lightai.cloud.tencent.com/assistant/chat
     ?name=orcaterm&mode=0&csrfCode=&sseresume=true
```

关键请求头包括 `Authorization: Bearer <ot_session>`、OrcaTerm cookies、`X-Product`、UUID 格式 `X-Seq-Id`、`Origin: http://tauri.localhost` 与 `Accept: text/event-stream`。

关键请求体结构：

```json
{
  "conversationId": "cid-<uuid>-orcaterm",
  "user": {"id":"<userId>","setting":{
    "mcpServers":["mcp-server-orcaterm-oauth"],
    "uiServers":["ui-tools-orcaterm-explorer"],
    "model":"TokenHub/deepseek-v4-flash"
  }},
  "input": {"type":"start","input":"<ACP JSON>","id":"<uuid>","files":[]},
  "stream": false
}
```

`conversationId` 格式、`user.id` 和嵌套的 `input.input` 都不可省略。ACP 文本信封为：

```json
{"_format":"acp-prompt","version":1,"prompt":[{"type":"text","text":"用户输入"}]}
```

结构化响应的核心字段是 `data.action`、`data.taskCompletion`、`data.question` 与 `data.tool`。`action=completion` 表示回答，`ask` 表示反问，`review` 表示等待执行工具。

## 3. Chat Completions 兼容范围

| 能力 | 本修复版本行为 |
|---|---|
| `/v1/chat/completions` | 支持单 choice 的常用聊天格式 |
| `messages` | 支持 `system`、`user`、`assistant`、`tool`，并兼容 legacy `function` 结果 |
| 图片 | 支持 `image_url` 的 data URI 与远程 URL |
| 多轮 | `X-Session-Id` 或 `user` 可复用上游 conversationId |
| `stream` | 完整解析后生成合成 SSE |
| `stream_options.include_usage` | 为 true 时在结束前输出 `choices: []` 的 usage chunk |
| usage | 本地估算，非上游计费数据 |
| tools | 仅桥接 OrcaTerm 原生工具 |
| 自定义 function | 不支持，上游不能由客户端注册 |
| 强制 tool choice | 不支持；由上游模型自动决定 |
| 并行工具 | 上游一次只产出一个调用；调用方可传 `parallel_tool_calls`，代理只返回一个 |

未明确列出的 OpenAI 字段或端点不属于兼容承诺。调用方不应假定 `temperature`、`top_p`、`response_format`、多 choice 或批处理等能力生效。

### 3.1 流式处理

两种上游模式的输入格式不同，但对外时序一致：

1. `structured` 获取完整结构化 JSON；
2. `raw-stream` 完整收集上游 SSE 帧，并解析 fenced JSON 动作包络；
3. 代理完成清洗、工具动作识别和 usage 估算；
4. `stream:false` 返回 Chat Completion JSON，`stream:true` 才把最终结果切分为合成 SSE。

因此 `raw-stream` 不表示字节直通，也不应宣传为低首 token 延迟。`stream_options.include_usage=true` 只控制流末尾 usage chunk；usage 仍是估算值。

### 3.2 system 与会话

新会话首个请求会把 system 与首轮消息一起构造成上游输入。命中已有会话后，上游已经持久化历史，代理只发送当前轮需要的用户输入；后续 system 不是可逐轮覆盖上游系统提示的配置。

需要改变 system 语义时应使用新的 `X-Session-Id`、新的 `user`，或不复用会话。无会话键的请求每次均创建一次性新 conversationId。

### 3.3 上游的 `action=ask` 与"格式约束泄漏"

真机实测（2026-09-12）上游有两条会影响 agent 客户端的行为，二者都**不是**代理能修正的：

1. **先反问再执行**：给出"请用某工具做某事"这类指令时，模型经常不直接调用工具，而是回一句确认
   （"是否按这个思路开始执行？"、"请问您希望在哪台服务器上查看？"）。此时响应是
   `action=ask`、HTTP 200、`finish_reason="stop"`，正文就是那句反问。
2. **泄漏内部输出约束**：少数提示词会让模型把服务端系统提示词里的输出格式要求当成回答，
   回复"我的输出必须遵循系统规定的 JSON 格式，无法直接输出…"并拒绝任务。

对 agent 客户端的影响与应对：

- OpenAI 标准字段里**没有**表达"模型在提问"的位置，`finish_reason` 仍是 `stop`。
  代理通过响应头 `X-OrcaTerm-Action: completion|ask|review` 暴露真实动作，
  客户端应读取该头来区分"最终答案"与"反问"。
- 第 2 类回复非空，因此不会触发退化检测；代理只能如实透传，客户端需要把它当成
  一次不成功的回答（可重试或改写提示词），不要当成模型的最终结论。
- 附带 `X-OrcaTerm-Degraded` 供区分"清洗后为空"的情况。

## 4. 原生工具桥接

上游工具调用格式为 `action=review` 加 `data.tool`：

```json
{"action":"review","tool":{"type":"mcp","name":"fetch","args":{"url":"https://example.com"}}}
```

代理将其转换为 OpenAI `assistant.tool_calls`。可声明的名称必须对应 OrcaTerm 已注册工具。

| 分组 | 原生工具名 |
|---|---|
| 网页抓取 | `fetch` |
| 终端会话 | `list_terminals`、`get_terminal_detail`、`get_terminal_output`、`get_active_terminal`、`execute_terminal_command`、`send_terminal_signal`、`get_command_history` |
| 连接与隧道 | `list_connect_configs`、`select_connect_config`、`connect_by_history_session`、`connect_share`、`connect_share_visit`、`get_confirmed_session`、`close_tunnel` |
| 命令与任务 | `execute_command`、`create_command`、`query_commands_status`、`run_commands`、`run_sandbox_task`、`submit_agent_tasks`、`query_agent_tasks_status` |
| 云空间与远程文件 | `read_cloud_space_file`、`read_cloud_space_file_list`、`create_cloud_space_file`、`create_file`、`create_directory`、`delete_local`、`delete_remote`、`delete_history_connection`、`remote_read`、`remote_write`、`remote_edit`、`remote_multi_edit`、`remote_glob`、`remote_grep` |
| 面板与标签页 | `open_file_manager`、`close_file_manager`、`open_tab_page`、`close_tab_page`、`open_next_tab_page`、`open_last_tab_page`、`open_proj`、`open_session_recorder`、`open_light`、`search_light`、`close_panel`、`close_approval_panel`、`send_offer` |
| 设置与防火墙 | `get_orcaterm_settings`、`update_orcaterm_settings`、`create_firewall_rules`、`delete_firewall_rules` |

**这是一份快照，不是权威全量**：上游真正的工具表由服务端下发，客户端 bundle 只留名字字符串。
刷新方式：`tools/reverse/native_tools_scan.py`（加 `--go` 直接产出可粘贴的 Go 条目）。

**终端类任务是一条工具链**：真机实测只声明 `execute_terminal_command` 时，上游会先请求 `list_terminals`；
只声明终端四件套时，上游又会先请求 `select_connect_config` 去选连接配置。任何一环缺失都会在任务中途
撞上 502 `upstream_requested_undeclared_tool`。调用方应把可能用到的整条链路一次性声明。

`tools` 声明无法向上游注册新的任意 function，表外的名字**不会**被注册到上游（见 §4.4）。

工具结果按真机协议回传到对应 conversationId：

```text
<TOOL_META>返回结果</TOOL_META>
<工具输出>
```

空结果表示拒绝执行，代理会构造 OrcaTerm 可识别的拒绝文本。

### 4.1 现代与 legacy 闭环

现代格式通过 `assistant.tool_calls[].id` 和 `role:"tool"` 的 `tool_call_id` 明确关联。本修复版本可从完整消息链识别对应调用并完成闭环，因此不强制私有 `X-Session-Id`；显式会话键仍可用于稳定的多轮上下文。

legacy `role:"function"` 结果不含 `tool_call_id`，仅靠消息无法可靠定位待处理调用，必须复用触发工具调用时的 `X-Session-Id` 或 `user`。代理不支持同时挂起或回传多个并行**原生**工具调用，也不支持强制选择指定工具；调用方自己工具的结果不受此限制（见 §4.4）。

### 4.2 明确拒绝的用法

上游无法支持的能力一律显式报错，不会静默忽略或降级。所有错误都是 OpenAI 形状的 `{"error":{"code":...}}`，HTTP 400 表示请求侧问题，502 表示上游侧问题。

| 用法 | HTTP | `error.code` |
|---|---|---|
| 同时传非空 `tools` 与 `functions` | 400 | `conflicting_tool_schemas` |
| `tool_choice:"required"` | 400 | `tool_choice_required_unsupported` |
| 具名强制选择（`tool_choice:"fetch"` 或对象形式） | 400 | `forced_tool_choice_unsupported` |
| 声明表外工具（自定义 function / 非 `function` 类型） | 400 | `custom_tools_unsupported`（仅 `native` 模式；默认 `pass` 会接受并暴露） |
| 重复声明同名工具 | 400 | `duplicate_tool` |
| `role:"tool"` 缺少 `tool_call_id` | 400 | `tool_call_id_required` |
| 一次回传多个**原生**工具结果 | 400 | `multiple_tool_results_unsupported` |
| legacy `role:"function"` 结果但未提供会话键 | 400 | `legacy_function_result_requires_session` |
| 工具结果名与待处理调用名不一致 | 400 | `tool_result_name_mismatch` |
| 未知 / 已消费 / 过期 / 会话不符 / 正在处理中的 `tool_call_id` | 400 | `unknown_tool_call_id` / `tool_call_already_consumed` / `expired_tool_call_id` / `tool_call_mismatch` / `tool_call_in_flight` |
| 上游请求了调用方未声明的工具 | 502 | `upstream_requested_undeclared_tool` |
| 上游 401/403 | 502 | `upstream_auth_error` |
| HTTP 200 但 `code=10050000`（AccessToken 失效） | 502 | `upstream_auth_error` |
| 上游协议损坏（200 但 JSON/包络非法） | 502 | `upstream_protocol_error` |
| 上游 429 | 429 | `upstream_rate_limited` |
| 上游超时 / 网络不可达 | 504 / 502 | `upstream_timeout` / `upstream_error` |

上游错误正文在对外错误信息中最多保留 300 字符，不会把完整上游响应透传给调用方。`tool_choice:"none"` 与 `function_call:"none"` 是合法值，表示不启用上游原生工具。

`parallel_tool_calls` 接受 `true` / `false`，不再拒绝：它表达的是"**允许**并行"，而上游一次只产出一个调用，
"只返回一个调用"本身就是该字段的合法响应。代理不会伪造第二个调用。

### 4.4 与第三方 agent 客户端的兼容（tools 模式）

真实的 agent 客户端（ZCode / Cline / Roo 等）都会声明**自己的一套工具**（`Agent`、`Bash`、`Read`…），
而这些名字上游一个都没有。如果因此拒绝请求，客户端每次调用都会 400，代理等于不可用。

因此 `ORCATERM_TOOLS_MODE` 默认是 `pass`：

| 值 | 行为 |
|---|---|
| `pass`（默认） | 接受任意工具声明。只有声明里含原生工具时才启用上游原生工具服务；外来名字记入响应头 `X-OrcaTerm-Unsupported-Tools` 并打 `[WARN]` 日志 |
| `native` | 严格模式：表外名字 400 `custom_tools_unsupported`，适合想靠代理校验工具名拼写的场景 |
| `ignore` | 忽略 tools 声明：不校验，也不启用原生工具服务 |
| `error` | 只要声明了 tools 就 400 `tools_unsupported` |
| `inject` | legacy 提示词注入模拟，不可靠 |

`pass` 模式的设计要点是**接受但不静默**：被忽略的工具名一定会在响应头与日志里出现，
调用方不会误以为自己的工具已经生效。

客户端回放**自己**工具的结果（`role:"tool"`，id 不是代理发过的）不会被当成待回填调用，
而是作为本轮输入送进上游，`content` 不会丢；代理只在工具名为原生工具时才去匹配待处理调用。
多个客户端工具结果也允许（"一次只能一个"的限制只针对原生工具）。

### 4.5 上游请求了未声明的工具

上游偶尔会请求调用方没声明的原生工具（见 §4 的工具链说明）。代理返回
502 `upstream_requested_undeclared_tool`，**不创建 pending call**，并在错误消息里点名是哪个工具。
这是刻意的硬失败：静默丢弃会让调用方拿到一个看起来正常、实际半途而废的回答。

### 4.6 上游 `code` 的两种形态

实测（2026-09-12 真机）上游的 `code` 字段有两种写法：

```jsonc
// 成功：数字
{"code":0,"data":{"action":"completion","taskCompletion":"…"},"message":"OK"}

// 鉴权失败：字符串，且 HTTP 状态是 200
{"code":"10050000","message":"Invalid or expired AccessToken","error":{"name":"ServerError",…}}
```

因此代理对 `code` 做宽容解析（数字或数字字符串），并把 `10050000` 这类业务码归到
`upstream_auth_error`，而不是让它退化成 `upstream_protocol_error`。登录态失效时调用方会收到：

```text
OrcaTerm 登录态已失效，请在 OrcaTerm 客户端重新登录后重试
```

`GET /health` 另外附带 `token_expires_at` 与 `token_expired` 两个字段（本地解 JWT `exp`，
不联网、不校验签名），启动时若 token 已过期也会打印 `[WARN]`。**上游没有刷新接口，
代理无法自行续期**，只能提前告知。

## 5. 模型目录

8 个 current：`deepseek-v4-pro`、`deepseek-v4-flash`、`hy3`、`hy4-preview`、`kimi-k3`、`glm-5.2`、`glm-5.3`、`glm-5.3-flash`。

9 个 legacy：`deepseek-chat-latest`、`glm-4.7`、`kimi-k2`、`kimi-k2.5`、`kimi-k2-thinking`、`kimi-k2.5-thinking`、`hunyuan-t1`、`hunyuan-turbos`、`qwen3`。

`orcaterm-assistant` 是指向默认模型的 deprecated alias，不是第 18 个上游模型。上游要求 `setting.model` 使用真实全名；错误或未知 id 可能返回 `code=0` 但正文为空，所以代理默认本地校验并返回 404。`ORCATERM_ALLOW_UNKNOWN_MODEL=1` 只适合临时探测。

## 6. 配置合法值

| 变量 | 默认值 | 合法值或约束 |
|---|---|---|
| `PROXY_PORT` | `8080` | 端口字符串 |
| `ORCATERM_MODE` | `structured` | `structured` / `raw-stream` |
| `ORCATERM_ON_DEGRADED` | `empty` | `empty` / `raw` / `error` |
| `ORCATERM_TOOLS_MODE` | `pass` | `pass` / `native` / `ignore` / `error`；`inject` 为 legacy、不可靠。见 §4.4 |
| `ORCATERM_HIDE_TOOLS` | `1` | `1` 开启 HideTools，`0` 关闭 |
| `ORCATERM_STRIP_PERSONA` | `1` | `0` / `1` |
| `ORCATERM_ALLOW_UNKNOWN_MODEL` | `0` | `0` / `1` |
| `ORCATERM_SESSION_TTL` | `30m` | Go duration |
| `ORCATERM_SESSION_MAX` | `1000` | 正整数 |
| `ORCATERM_CONTEXT_LIMIT` | `128000` | 正整数，仅供估算 |
| `ORCATERM_CORS_ORIGINS` | 空 | 关闭；逗号分隔 origin allowlist，或显式 `*` |

HideTools 控制发往上游 setting 的工具消息隐藏相关开关，用于抑制内部工具过程直接混入普通回答；它不增加工具能力，也不改变"不支持自定义 function、强制 tool choice、一次只产出一个调用"的边界。

## 7. CORS、安全与端点

CORS 默认关闭。只有设置 `ORCATERM_CORS_ORIGINS` 后才对匹配 origin 返回跨域许可；`*` 必须显式配置。allowlist 应填写完整 origin（scheme、host、port），多个值用逗号分隔。配置项会被规范化（去掉尾部 `/`），带路径、查询串、片段或非 http/https scheme 的值会被拒绝。允许的方法为 `GET`、`POST`、`DELETE`、`OPTIONS`，允许的请求头包含 `Content-Type`、`Authorization`、`X-Session-Id`、`OpenAI-Organization`、`OpenAI-Project` 与 `OpenAI-Beta`。未在 allowlist 中的 origin 发起的预检返回 403 `cors_origin_denied`。

配置项在启动前由 `validateConfig` 校验：`ORCATERM_MODE`、`ORCATERM_TOOLS_MODE`、`ORCATERM_ON_DEGRADED` 只接受枚举内的值，`ORCATERM_TIMEOUT`、`ORCATERM_SESSION_TTL`、`ORCATERM_SESSION_MAX`、`ORCATERM_CONTEXT_LIMIT` 必须为正，非法值会让进程启动失败，不会静默回退到默认值。

端点包括 `/health`、`/v1/models`、`/v1/models/{id}`、`/v1/chat/completions`、`/v1/sessions` 与 `/debug/last`。其中会话管理和调试端点可能暴露 conversationId、输入、工具结果及上游原始数据，属于敏感管理面；`/health` 也会透露认证与后端状态。

代理没有入站 API Key 鉴权。本修复版本应保持 localhost 监听；若必须通过反向代理提供服务，应在外层增加认证，并限制 `/v1/sessions`、`/debug/last` 与不必要的管理访问。

## 8. 已知限制

- 模型目录是静态快照，不会自动跟随上游更新。
- usage 与上下文剩余量均为本地估算。
- 依赖 OrcaTerm 登录态；登录过期后需重新登录。
- 会话受 TTL 与容量限制；legacy 工具结果必须在原会话仍有效时回传。
- 上游服务端决定原生工具集合；客户端 tools schema 不能扩展该集合。
