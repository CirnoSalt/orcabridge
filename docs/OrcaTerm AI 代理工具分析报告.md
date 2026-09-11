# OrcaBridge —— OrcaTerm AI 协议分析与实现报告

> 版本：v0.8.0
> 更新：2026-09-12
> 仓库：本仓库根目录（下文相对路径均相对仓库根）

---

## 一、项目概述

**目标**：把 OrcaTerm（遨驰终端）内置的 AI 助手封装为标准 **OpenAI API**，供任意 Agent / 工具直接调用。

**形态**：零配置、无外部依赖的 Go 程序，读取本机 OrcaTerm 的登录会话，把 OpenAI 请求转发到 ligai 对话接口。

**状态**：v0.8.0 已还原真实客户端的完整对话协议，**打通 tool 调用闭环**（调用 → 返回结果 → 续跑），
并**暴露完整的模型列表与切换能力**（18 个模型，聊天请求换 `model` 字段即切换）。

---

## 二、项目结构

```
<仓库根目录>\
├── README.md               # 使用说明（先看这个）
├── go.mod                  # Go 模块定义（零外部依赖）
├── main.go                 # 主程序（代理服务器 / 处理器 / 清洗管线）
├── session.go              # 多轮会话管理（TTL + 容量上限 + 待回传工具状态）
├── content.go              # 多模态解析 / ACP 信封 / 模型目录 / 上游原生工具协议 / token 估算
├── clean_test.go           # 单元测试（含工具协议用例）
├── orcabridge.exe      # 编译产物（Windows）
├── docs\                   # 技术文档
│   └── OrcaTerm AI 代理工具分析报告.md
└── tools\reverse\          # 逆向与探测脚本（可复用，见其 README）
    ├── cdp_scripts2.py     # 经 CDP 导出前端明文源码
    ├── ctx2.py             # 按关键字打印源码上下文
    ├── cc_scan.py          # V8 Code Cache 字节搜索
    ├── capture_real_request.py  # 被动抓包：记录客户端真实请求
    ├── tool_probe.py       # 探测上游工具调用协议
    ├── tool_e2e.py         # 上游层工具闭环验证
    ├── custom_tool_probe*.py    # 自定义工具注册路径负结果取证
    ├── proxy_tools_e2e.py  # 代理层端到端回归测试
    ├── verify_real.py      # 最小"正确请求"样例
    └── capability.py       # 多轮 / 多模态 / usage 能力验证
```

---

## 三、真实协议（抓包还原，v0.5.0 已实现）

### 3.1 端点与请求头

```
POST https://lightai.cloud.tencent.com/assistant/chat
     ?name=orcaterm&mode=0&csrfCode=&sseresume=true
```

| 头 | 值 |
|---|---|
| `Authorization` | `Bearer <ot_session JWT>` |
| `Cookie` | `ot_session=…; userAccountLoginMethod=oauth; userAccountType=wechat; userId=<userId>` |
| `X-Product` | `orcaterm` |
| `X-Seq-Id` | **UUID**（不是毫秒时间戳——早期版本的错误之一） |
| `X-Csrfcode` | 空 |
| `X-Referer` | `http://tauri.localhost/` |
| `Origin` | `http://tauri.localhost` |
| `User-Agent` | `Mozilla/5.0 (Macintosh; …) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36` |
| `Accept` | `text/event-stream` |

### 3.2 请求体（关键）

```jsonc
{
  "conversationId": "cid-<uuid>-orcaterm",   // ① 必须是 cid- 前缀 + 产品后缀
  "user": {
    "id": "<userId>",                        // ② 来自 ot_session JWT 的 userId
    "setting": {
      "mcpServers": ["mcp-server-orcaterm-oauth"],
      "uiServers":  ["ui-tools-orcaterm-explorer"],
      "tools": [], "approvedTools": [], "approvedMCPTools": [],
      "enableAutoSubtaskExecution": false,
      "env": [],
      "model": "TokenHub/deepseek-v4-flash",
      "modelDesc": {}
      // 真实客户端还会带 extra（超长提示词）与 selectedMCPTools/selectedUITools，
      // 实测省略后不影响正常问答
    }
  },
  "input": {                                 // ③ 是对象不是字符串
    "type": "start",
    "input": "{\"_format\":\"acp-prompt\",\"version\":1,\"prompt\":[{\"type\":\"text\",\"text\":\"用户的话\"}]}",
    "id": "<消息 UUID>",
    "emphasisPrompt": "",
    "files": []
  },
  "stream": false
}
```

**三个致命细节**（缺一即导致上游退化成输出开场白/活动卡片）：

1. `conversationId` 必须是 `cid-<uuid>-<product>` 格式，裸 UUID 不行
2. `input` 是**嵌套对象**，内层 `input.input` 才是 ACP 信封；直接传字符串无效
3. `user.id` 必须带（从 `ot_session` JWT 解出）

### 3.3 ACP 信封

```json
{"_format":"acp-prompt","version":1,"prompt":[{"type":"text","text":"用户的话"}]}
```

上游只把 `prompt[].text` 当作用户指令。也支持 `resource_link` 类型的上下文引用。

### 3.4 响应

`stream=false` 返回干净结构化 JSON：

```json
{"code":0,"data":{"action":"completion","thinking":"…","taskCompletion":"…","question":"…"}}
```

- `action`：`completion` 已作答 / `ask` 反问 / **`review` 请求调用工具**（关键，见 §3.7）
- `taskCompletion`：实质答复（代理取此字段）

### 3.5 其他已探明端点

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/assistant/conversation` | 创建会话，需 `{tags,title,customTitle,aiTitle,ts}`，返回会话 id |
| PUT | `/assistant/conversation/{id}` | 更新会话（title/aiTitle/ts） |
| GET | `/assistant/conversation/list?offset=0&limit=100&orderBy=ts&order=desc&filter={"tags":["orcaterm"]}` | 会话列表（**必须带 filter**，否则恒返回空） |
| GET | `/assistant/messages/cid-<uuid>-orcaterm?name=orcaterm&mode=0&csrfCode=` | 读取会话消息 |

### 3.6 `user.setting` 的完整字段（V8 Code Cache 提取）

```
conversationId / mcpServers / uiServers / tools / approvedTools / approvedMCPTools /
selectedMCPTools / selectedUITools / enableAutoSubtaskExecution / modelDesc /
isNormalToolMessageIgnored / shouldHideDirectOutputToolReview / hideToolMessage /
baseUrl / a2a / multimodal / env / extra / model / language
```

真实客户端默认带 `mcpServers:["mcp-server-orcaterm-oauth"]`、`uiServers:["ui-tools-orcaterm-explorer"]`，
**这两个字段就是"上游自带工具"的开关**——代理照抄即可获得与客户端等价的工具能力。

### 3.7 工具调用协议（v0.7.0 打通，本节是 tools 支持的全部秘密）

#### ① 上游返回工具调用

`action=review` + `data.tool`（实测抓取 example.com）：

```json
{
  "code": 0,
  "data": {
    "action": "review",
    "thinking": "用户要求抓取 https://example.com 的内容，直接执行，请用户审核。",
    "tool": {
      "type": "mcp",
      "mcpServer": "mcp-server-web-fetch",
      "name": "fetch",
      "description": "抓取 https://example.com 的内容并提取为 Markdown 格式",
      "args": { "url": "https://example.com", "requestMode": "user_specified" }
    }
  }
}
```

> 此前 v0.6.0 把 `action=review` 当作"退化输出"直接丢弃 —— 这就是 tools 一直不可用的根因。

#### ② 流式模式下工具调用是一段 fenced JSON

`stream=true` 时上游把同一个内部动作**逐字**吐成 `type:"ai"` 帧，拼起来是：

````
```json
{"action":"review","thinking":"…","tool":{"type":"mcp","name":"fetch","args":{…}}}
```
````

所以 raw-stream 模式只需按围栏块 + 括号配平解析即可（见 `ParseChatEnvelope`）。

#### ③ 工具结果用**纯文本**回传（不需要特殊 ACP 类型）

反编译前端 AI chunk 得到客户端的真机实现：

```js
const _l = /<TOOL_META>[\s\S]*?<\/TOOL_META>\s*/, wl = /^\[返回结果]：/;
const kl = (label, out) => "<TOOL_META>" + label + "</TOOL_META>\n" + out;
// 调用点：V = kl("返回结果" + 终端切换提示, 输出)
```

即在**同一个 conversationId** 上再发一条普通用户消息，正文为：

```
<TOOL_META>返回结果</TOOL_META>
<工具输出>
```

代理的 `BuildToolResultPrompt` 与之逐字节一致。

#### ④ 拒绝/未执行的回传格式（客户端 `Hn` 函数）

```
[TOOL_REJECTED]
status: rejected
reason: user_rejected        # 或 policy_denied
retryable: false
tool: <工具名>

<说明；该工具调用未执行，环境未发生变化。本轮任务中不要再次调用相同工具…>
```

#### ⑤ 已验证的真实闭环

```
轮1 POST /assistant/chat  "请调用 fetch 抓取 https://example.com"
    -> action=review, tool={name:"fetch", args:{url:...}}
轮2 POST /assistant/chat  同 cid，正文 "<TOOL_META>返回结果</TOOL_META>\n<输出>"
    -> action=completion, 模型正确引用工具输出内容
```

#### ⑥ 自定义工具为什么仍然不可用（实测排除）

| 尝试 | 结果 |
|---|---|
| `setting.tools` = 内联定义 `{name,description,parameters}` | 模型："当前环境未注册天气工具" |
| `setting.tools` = OpenAI 格式 `{type:function,function:{…}}` | 同上 |
| `setting.tools` = 纯名字数组 `["get_weather"]` | 同上 |
| `selectedUITools:["get_weather"]` | 模型改去调 `cloud_ai_assistant_search` 找工具 |
| 提示词注入原生 `action=review` 格式 | 模型："该工具未在当前环境中注册" |
| 提示词改用"输出协议" `@@REQ{...}`（不称其为工具） | 模型不遵守该协议 |
| 把协议写进 `setting.extra`（客户端系统提示通道） | 仍不遵守 |

**结论**：上游服务端系统提示词对"工具是否已注册"有最终裁决权，客户端无法注册自定义 function。
因此代理走**原生工具透传**：上游自带的工具（fetch / 终端 / 云空间 / 云脚本 …）以标准
OpenAI `tool_calls` 暴露给调用方，调用方执行后回传结果即可。这与真实客户端的能力完全一致。

---

## 三.8、能力矩阵（实测）

| OpenAI 能力 | 支持 | 实现方式 |
|---|---|---|
| 多轮上下文 | ✅ 完全支持 | 复用上游 `conversationId`（服务端持久化历史）。用 `X-Session-Id` 头或 `user` 字段标识会话 |
| 多模态（图片） | ✅ 支持 | `content` 数组中的 `image_url`；data URI 转 ACP `image`（base64+mime），远程 URL 用 `image_url` |
| 模型列表 | ✅ | `GET /v1/models`，含能力声明与上游模型名 |
| 用量统计 usage | ⚠️ 本地估算 | 上游不返回任何 token 字段；按 CJK 1 token / 其他 4 字符 1 token 近似，`estimated: true` |
| 上下文剩余量 | ⚠️ 本地估算 | 响应头 `X-OrcaTerm-Context-Used/Limit/Remaining`（上限由 `ORCATERM_CONTEXT_LIMIT` 指定） |
| 流式 stream | ✅ | 内部非流式取答案后本地切分为 SSE，末尾附 usage |
| **tools 调用** | ✅ **原生透传** | `action=review`+`data.tool` → OpenAI `tool_calls`；`role:"tool"` 结果 → `<TOOL_META>返回结果</TOOL_META>` 续跑同会话 |
| **模型切换** | ✅ 18 个模型 | 请求里的 `model` 映射到上游 `user.setting.model`；`GET /v1/models` 列表 |
| 自定义 function | ❌ 上游无法注册 | 见 §3.7 ⑥；调用方应以 OrcaTerm 原生工具名声明 tools |
| 会话管理 | ✅ 代理自建 | `GET/DELETE /v1/sessions`（含 `pending_tool` 字段） |

---

## 三.9、模型列表与切换（v0.8.0）

### 模型表来源

客户端前端 AI chunk（module `95201`）里有一个常量数组，形如：

```js
const s = [
  {text:"Deepseek-V4-Pro", value:"deepseek-v4-pro", type:"default",
   model:"TokenHub/deepseek-v4-pro", priceRate:0,
   tags:[{text:"限时免费",type:"free"}],
   description:"1M 上下文，擅长长程任务与复杂推理",
   capabilities:["推理","长上下文"]},
  {text:"DeepSeek-V4-Flash", value:"deepseek-v4-flash", model:"TokenHub/deepseek-v4-flash", …},
  {text:"Hy3",         value:"hunyuan",  model:"Hunyuan3/hy3",          …},
  {text:"Hy4-preview", value:"hunyuan4", model:"Hunyuan3/hy4-preview",  …},
  {text:"Kimi-K3",     value:"kimi",     model:"TokenHub/kimi-k3",      …},
  {text:"GLM-5.2",     value:"glm-5.2",  model:"TokenHub/glm-5.2",      …},
  {text:"GLM-5.3",     value:"glm-5.3",  model:"TokenHub/glm-5.3",      …},
  {text:"GLM-5.3-Flash", value:"glm-5.3-flash", model:"TokenHub/glm-5.3-flash", …},
]
// 默认项： value === "hunyuan"（即 Hy3），找不到则取第一项
```

### 关键结论（实测）

1. **请求里 `user.setting.model` 要填 `model` 字段（全名）**，不是列表里的 `value`。
2. **填 `value` 会静默失败**：实测填 `deepseek-v4-flash`（而不带 `TokenHub/` 前缀）
   上游返回 `data.action = null`、无正文，但 **`code` 仍然是 0**。
3. **未知模型同样静默返回空**（`code=0` + 空 `data`），不报错。
   → 这就是代理要做**本地模型名校验**的原因：否则调用方拿到的是"看起来成功、其实是空回答"。

### 与 `model` 相关的其他请求字段

| 字段 | 内置模型 | 自定义/第三方模型 |
|---|---|---|
| `setting.model` | 模型全名（如 `TokenHub/glm-5.3`） | 同左 |
| `setting.modelDesc` | `{}` | `{source:"orcaterm", secretId, config:{name, provider, providerDetails:{name, baseUrl, adapter}, providerModel}}` |

客户端源码里内置模型只发 `{model, extra}`，第三方模型才带 `secretId` / `baseUrl` / `adapter`。
代理只实现内置模型；第三方模型需要用户自己的凭证，不在范围内。

### 代理实现

- 目录：`content.go` 的 `modelCatalog`（对外短 id ↔ 上游全名 ↔ 显示名 ↔ 别名）
- 8 个客户端模型 + 10 个实测仍可用的旧版模型（标 `legacy: true`）
- 兼容别名 `orcaterm-assistant` → 默认模型（`ORCATERM_UPSTREAM_MODEL`，默认 `deepseek-v4-flash`）
- 未知名默认返回 404 `model_not_found` 并列出全部合法 id；
  设 `ORCATERM_ALLOW_UNKNOWN_MODEL=1` 可直通上游
- 响应头 `X-OrcaTerm-Model-Id` / `X-OrcaTerm-Model` 回显本轮实际使用的模型
- 端点：`GET /v1/models`（列表）、`GET /v1/models/{id}`（单模型）

### 实测过程

对每个候选 id 发同一问题，比对回答是否不同（相同指纹 = 可能没真正切换）：

```
Deepseek-V4-Pro   TokenHub/deepseek-v4-pro   → "杭州是一座融西湖山水与数字创新于一体的江南名城。"
DeepSeek-V4-Flash TokenHub/deepseek-v4-flash → "杭州：人间天堂，数字经济之城。"
Hy3               Hunyuan3/hy3               → "杭州是西湖畔的历史名城，人间天堂。"
Hy4-preview       Hunyuan3/hy4-preview       → "杭州是浙江省会，以西湖闻名的人间天堂。"
Kimi-K3           TokenHub/kimi-k3           → "杭州是诗意江南与创新活力之城。"
GLM-5.2           TokenHub/glm-5.2           → "杭州，西湖畔的历史名城，人间天堂。"
GLM-5.3           TokenHub/glm-5.3           → "杭州：西湖美景与千年古韵交融的江南名城。"
GLM-5.3-Flash     TokenHub/glm-5.3-flash     → "杭州：人间天堂，西湖山水甲天下。"
[对照] TokenHub/definitely-not-a-model       → 空（action=null）
[对照] deepseek-v4-flash（value 形式）        → 空（action=null）
```

8 个模型回答各不相同 → 切换确实生效；两个对照组都返回空 → 校验逻辑的必要性得到验证。
脚本：`tools/reverse/model_probe.py`。

---

## 四、逆向方法（可复用）

1. **V8 Code Cache 字符串提取**：exe 内无明文 JS、WebView HTTP 缓存也不存 JS，但
   `EBWebView\Default\Code Cache\js\*` 保留前端字符串常量表，可字节级搜索还原字段名。
   由此定位 AI 组件 `agent-workbench`，并还原出 `user.setting` 全字段、
   客户端状态机（`queued/preparing/model_requesting/thinking/waiting_approval/
   tool_executing/tool_result_reporting/answering/reconnecting`）与工具名集合。
   脚本：`tools/reverse/cc_scan.py`。
2. **CDP 取前端明文源码**（v0.7.0 新增，最有效）：客户端带 `--remote-debugging-port=9333`
   启动，用 `Debugger.enable` → 收集重放的 `Debugger.scriptParsed`（注意：事件在 enable
   的响应之前就到达，必须全程收集，否则拿到 0 个脚本）→ `Debugger.getScriptSource` 取源码。
   AI/tools 逻辑位于 `node_modules_pnpm_tencent_i18n_*_react-dom_*.js`。
   脚本：`tools/reverse/cdp_scripts2.py`（导出源码）、`tools/reverse/ctx2.py`（按关键字打印上下文）。
   > 注意：页面内 `fetch('http://tauri.localhost/xxx.js')` 会返回 `{"message":"Not Found"}`，
   > 必须走 `Debugger.getScriptSource`。
3. **IndexedDB 消息结构**：`EBWebView\Default\IndexedDB\http_tauri.localhost_0.indexeddb.leveldb\000003.log`
   存有真实对话记录（含 ACP 信封），注意中文以 UTF-16LE 存储。
4. **错误驱动探测**：对接口发空 body，从 `tags 不能为空` → `title 必须是字符串` → …
   逐字段逼出契约。
5. **主动探测（v0.7.0 新增）**：直接对 `/assistant/chat` 发构造好的请求，观察返回。
   工具调用就是这样发现的——发一句"请调用 fetch 抓取 https://example.com"即可拿到
   `action=review` + `tool`。脚本：`tools/reverse/tool_probe.py`、`tools/reverse/tool_e2e.py`。
   驱动工具调用是安全的：`approvedTools` 为空时上游只回"待审批"，不会真的执行。
6. **模型可用性探测（v0.8.0 新增）**：对每个候选模型 id 发同一问题，
   比对回答指纹——**相同指纹说明模型可能没真正切换**。
   未知模型与错误的 id 形式都返回空（`code=0` 但 `action=null`），
   所以必须用"回答是否不同"来判断，不能只看 `code`。脚本：`tools/reverse/model_probe.py`。
7. **被动抓包**（早期决定性手段）：CDP 注入**只读**记录器包装 `window.fetch` 与
   `__TAURI_INTERNALS__.invoke`，由用户手动发一条消息拿到完整请求。
   脚本：`tools/reverse/capture_real_request.py`（注入后读 `window.__acpFull`）。
   **注意**：记录时切勿截断 body——早期因截断 8000 字符导致 JSON 不完整、无法解析。

> 脚本运行环境：`tools/reverse/*.py` 依赖 `websocket-client`，本仓库已带
> `.venv-cdp`（见 `tools/reverse/README.md`）。

---

## 五、配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PROXY_PORT` | `8080` | 监听端口 |
| `ORCATERM_COOKIES` | `%LOCALAPPDATA%\com.orcaterm-desktop.app\.cookies` | cookies 路径 |
| `ORCATERM_API_URL` | `https://lightai.cloud.tencent.com` | 上游地址 |
| `ORCATERM_MODEL` | `orcaterm-assistant` | 兼容别名，等价于默认模型（v0.8.0 起不再是对外模型名） |
| `ORCATERM_UPSTREAM_MODEL` | `TokenHub/deepseek-v4-flash` | 默认模型的上游 id |
| `ORCATERM_PRODUCT` | `orcaterm` | X-Product / conversationId 后缀 |
| `ORCATERM_USER_AGENT` | 客户端真实 UA | 可覆盖 |
| `ORCATERM_MODE` | `structured` | `structured`（默认）/ `raw-stream` |
| `ORCATERM_ON_DEGRADED` | `empty` | 退化时：`empty` / `raw` / `error` |
| `ORCATERM_PROMPT_OVERRIDE` | 空 | 附加指令（实测易干扰，默认关闭） |
| `ORCATERM_STRIP_PERSONA` | `1` | 清洗管线兜底开关 |
| `ORCATERM_SESSION_TTL` | `30m` | 会话空闲保活时长（超时后下一轮自动新建） |
| `ORCATERM_SESSION_MAX` | `1000` | 会话表容量上限（超出淘汰最久未用） |
| `ORCATERM_TOOLS_MODE` | `native` | `native`（默认，上游原生工具透传）/ `ignore` 忽略 / `inject` 提示词模拟（legacy，不可靠）/ `error` 一律拒绝 |
| `ORCATERM_ALLOW_UNKNOWN_MODEL` | `0` | `1`=允许列表外的模型名直通上游（默认拒绝并报 404） |
| `ORCATERM_CONTEXT_LIMIT` | `128000` | 上下文上限，用于计算剩余量 |

响应头：

| 头 | 含义 |
|---|---|
| `X-OrcaTerm-Action` | 上游 action（completion/ask/review） |
| `X-OrcaTerm-Degraded` | `1` 表示本轮未取得有效答案 |
| `X-OrcaTerm-Session` / `-Turn` | 命中的会话键与轮次 |
| `X-OrcaTerm-Model-Id` / `-Model` | 本轮实际使用的对外模型 id / 上游模型全名 |
| `X-OrcaTerm-Tool` | 本轮请求的工具名（review 轮） |
| `X-OrcaTerm-Tool-Result-Turn` | `1` 表示本轮是工具结果回传 |
| `X-OrcaTerm-Tool-Undeclared` | `1` 表示上游请求的工具不在调用方 `tools` 声明里 |
| `X-OrcaTerm-Context-Used` / `-Limit` / `-Remaining` | 上下文用量（估算） |
| `X-OrcaTerm-Usage-Estimated` | 固定 `1`，提示 usage 为估算值 |

端点：`GET /health`、`GET /v1/models`、`GET /v1/models/{id}`、`POST /v1/chat/completions`、
`GET|DELETE /v1/sessions`、`GET /debug/last`。

---

## 六、使用

```powershell
.\orcabridge.exe
```

单轮：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"orcaterm-assistant","messages":[{"role":"user","content":"1+1等于几？"}]}'
# → {"choices":[{"message":{"role":"assistant","content":"2"}}]}
```

**多轮上下文**：带上同一个 `X-Session-Id`（或用 OpenAI 的 `user` 字段）即可，
上游会保留会话历史，无需每次回传全部消息：

```bash
curl -H 'X-Session-Id: my-chat-001' -H 'Content-Type: application/json' \
  -d '{"model":"orcaterm-assistant","messages":[{"role":"user","content":"我叫小明"}]}' \
  http://localhost:8080/v1/chat/completions

curl -H 'X-Session-Id: my-chat-001' -H 'Content-Type: application/json' \
  -d '{"model":"orcaterm-assistant","messages":[{"role":"user","content":"我叫什么名字？"}]}' \
  http://localhost:8080/v1/chat/completions
# → 「小明」；响应头带 X-OrcaTerm-Turn: 2
```

**tools 调用（v0.7.0）**：声明 tools 并保持同一会话键，即可完成标准的两段式闭环：

```bash
# ① 第一轮：模型返回 tool_calls
curl -H 'X-Session-Id: agent-001' -H 'Content-Type: application/json' -d '{
  "model":"orcaterm-assistant",
  "messages":[{"role":"user","content":"请调用 fetch 工具抓取 https://example.com"}],
  "tools":[{"type":"function","function":{"name":"fetch",
    "description":"抓取网页内容",
    "parameters":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}}}]
}' http://localhost:8080/v1/chat/completions
# → {"choices":[{"finish_reason":"tool_calls","message":{"content":null,
#      "tool_calls":[{"id":"call_…","function":{"name":"fetch",
#      "arguments":"{\"url\":\"https://example.com\"…}"}}]}}]}

# ② 第二轮：把执行结果以 role="tool" 回传（必须带同一个 X-Session-Id）
curl -H 'X-Session-Id: agent-001' -H 'Content-Type: application/json' -d '{
  "model":"orcaterm-assistant",
  "messages":[
    {"role":"user","content":"请调用 fetch 工具抓取 https://example.com"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"call_…","type":"function",
      "function":{"name":"fetch","arguments":"{\"url\":\"https://example.com\"}"}}]},
    {"role":"tool","tool_call_id":"call_…","name":"fetch","content":"<你抓到的网页内容>"}
  ],
  "tools":[ …同上… ]
}' http://localhost:8080/v1/chat/completions
# → 模型基于工具结果作答；响应头带 X-OrcaTerm-Tool-Result-Turn: 1
```

约定与注意事项：

- **必须使用 OrcaTerm 原生工具名声明 tools**（如 `fetch`、`execute_terminal_command`、
  `run_commands`、`submit_agent_tasks`、`read_cloud_space_file` 等）。上游不认识自定义名字，
  会转而回答"环境中没有该工具"。上游请求了未声明的工具时，响应头会带
  `X-OrcaTerm-Tool-Undeclared: 1` 并在日志中告警。
- **工具结果轮必须复用触发调用的那个会话键**，否则返回 400 `no_pending_tool`——
  这是刻意设计：孤儿工具结果会被上游当作孤立文本，静默产出垃圾回答。
- 工具结果的 `content` 传空字符串即视为**拒绝执行**，代理会按真机格式回传
  `[TOOL_REJECTED]`，模型会理解"环境未发生变化"而停止重试。
- 代理对工具结果做了截断保护（与客户端一致，超长输出建议先自行裁剪）。
- 流式模式同样支持：`finish_reason` 为 `tool_calls`，`delta.tool_calls` 一次性给出完整调用。

图片（多模态）：

```bash
curl -H 'Content-Type: application/json' -d '{
  "model":"orcaterm-assistant",
  "messages":[{"role":"user","content":[
    {"type":"text","text":"这张图是什么颜色？"},
    {"type":"image_url","image_url":{"url":"data:image/png;base64,<base64>"}}
  ]}]}' http://localhost:8080/v1/chat/completions
```

---

## 七、遗留说明

- 清洗管线（人设/营销/菜单剔除 + 退化检测）**保留为兜底**：协议正确时基本不触发，
  但可防止上游偶发的活动技能劫持污染输出。工具调用轮次**不经过清洗管线**，
  避免 `action=review` 被判为退化或 fenced JSON 被误删。
- **tools 已支持（原生透传）**：`action=review` + `data.tool` → OpenAI `tool_calls`；
  工具结果按真机文本协议 `<TOOL_META>返回结果</TOOL_META>\n<输出>` 续跑同一会话。
  已验证：`fetch` 调用闭环、同会话拒绝、流式 tool_calls、普通问答不受影响。
- **自定义 function 仍不可用**：上游服务端系统提示词对"工具是否注册"有最终裁决权，
  7 种注册/注入路径实测全部被拒（详见 §3.7 ⑥）。调用方应以 OrcaTerm 原生工具名声明 tools。
- **usage / 上下文剩余量是本代理本地估算**，与上游真实计费口径无关；上游响应中
  不含任何 token 字段（已确认 `data` 仅 `action`/`thinking`/`taskCompletion`/`question`/`options`/`tool`）。
- **模型目录是代码里写死的**（`content.go` 的 `modelCatalog`）：上游增删模型不会自动同步。
  临时应急可用 `ORCATERM_ALLOW_UNKNOWN_MODEL=1` 直通；长期应更新目录。
- 会话默认 30 分钟过期，过期后同名的 `X-Session-Id` 会开启新会话（上下文重置）。
  **工具调用两轮之间不要跨越 TTL**，否则工具结果轮会因找不到待处理工具而 400。
- 曾注入的只读记录器已清理；**建议重启 OrcaTerm 一次**以彻底还原被包装的 `fetch`。

---

*本文档按 2026-09-12 实测结果重写。*
*v0.7.0 推翻了两条旧结论：①「上游不返回工具调用」——实际返回 `action=review`+`data.tool`；
②「服务端拒绝非运维问题」——早期误判。*
*v0.8.0 新增模型列表与切换：模型表取自前端常量数组，`setting.model` 必须填全名；
未知模型上游静默返回空，故代理改为本地校验 + 404。*
