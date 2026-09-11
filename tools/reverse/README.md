# tools/reverse —— 逆向与探测脚本

这些脚本是开发和维护 `orcabridge` 时用来摸清 OrcaTerm 协议的工具。
**平时不需要跑它们**，只在两种情况下用：协议变了要重新抓，或者想验证某个行为。

## 环境准备

脚本依赖 `websocket-client`（只有 CDP 相关的几个需要）。仓库里已带好虚拟环境：

```powershell
# 运行任何脚本都用这个解释器
.\.venv-cdp\Scripts\python.exe tools\reverse\<脚本名>.py
```

如果需要重建环境：

```powershell
uv venv .venv-cdp
uv pip install --python .venv-cdp\Scripts\python.exe websocket-client
```

多数脚本要求 **OrcaTerm 正在运行且开着 CDP 调试端口 9333**
（浏览器内核系客户端通常在 `http://127.0.0.1:9333/json/version` 暴露调试端点，
可以先 curl 一下确认）：

```powershell
curl http://127.0.0.1:9333/json/version
```

本机实测 9333 一直是开着的，脚本直接可用。如果连不上，说明客户端没有开调试端口——
Chromium / WebView2 系一般是启动时加 `--remote-debugging-port=9333`。
本仓库没有固化这一启动方式（当时的环境里它就是开着的），
换机器时请按实际客户端版本确认。

脚本会自动从 `%LOCALAPPDATA%\com.orcaterm-desktop.app\.cookies` 读取登录态
（实际路径见各脚本开头的 `COOKIES` 常量）。

---

## 脚本清单

### 摸协议

| 脚本 | 作用 |
|---|---|
| `verify_real.py` | **最简"正确请求"样例**。想知道 `/assistant/chat` 的正确报文长什么样，看这个就够 |
| `tool_probe.py` | 直接打上游，观察工具调用返回。`--stream` 可看流式帧格式 |
| `model_probe.py` | **探测哪些模型 id 可用**。对每个候选发同一问题、比对回答指纹（指纹相同 = 可能没真正切换） |
| `capability.py` | 验证多轮上下文、消息读回、usage 字段是否存在 |
| `custom_tool_probe.py` | 取证：自定义工具为什么注册不了（`setting.tools` 三种 schema） |
| `custom_tool_probe2.py` | 取证续：输出协议框架 / `setting.extra` 通道也走不通 |

### 前端源码 / Code Cache

| 脚本 | 作用 |
|---|---|
| `cdp_scripts2.py` | **经 CDP 导出前端明文源码**到 `js_src/`。找实现细节的首选手段 |
| `ctx2.py` | 在某个源码文件里按关键字打印上下文（配合上一条用） |
| `cc_scan.py` | 在 V8 Code Cache 里按字节搜关键字。挖 serde 字段名、枚举集合特别有效 |
| `capture_real_request.py` | 被动抓包：注入只读记录器，由你在客户端手动发消息，然后读 `window.__acpFull` |

### 验证代理

| 脚本 | 作用 |
|---|---|
| `proxy_tools_e2e.py` | **tools 回归测试**。改完代码跑这个，一条命令确认 tools 还正常 |
| `proxy_models_e2e.py` | **模型功能回归测试**。覆盖列表 / 单模型 / 切换生效 / 别名 / 未知模型 404 / tools 回归 |

```powershell
# 先起代理，再跑回归
$env:PROXY_PORT=8083; .\orcabridge.exe
.\.venv-cdp\Scripts\python.exe tools\reverse\proxy_tools_e2e.py 8083
.\.venv-cdp\Scripts\python.exe tools\reverse\proxy_models_e2e.py 8083
```

`proxy_tools_e2e.py` 依次验证：触发 `tool_calls` → 回传结果被模型消费 → 孤儿结果被正确拒绝
→ 普通问答不受影响。

`proxy_models_e2e.py` 会断言"不同模型给出不同回答"，用来确认模型切换真的生效
（只看 HTTP 200 是不够的，上游对未知模型也会返回 200 + 空内容）。

---

## 几个容易踩的坑

- **`cdp_scripts2.py` 里必须全程收集 CDP 消息**。`Debugger.enable` 会重放
  `scriptParsed` 事件，而且这些事件在 enable 的响应**之前**就到达了。
  如果用"发一条等一条 id"的写法，会拿到 0 个脚本。
- **页面内 `fetch` 取不到前端 JS**：`http://tauri.localhost/xxx.js` 会返回
  `{"message":"Not Found"}`。必须走 `Debugger.getScriptSource`。
- **`tool_e2e.py` 会往上游发真实请求**，但只发到"模型请求工具"为止就停，
  不会执行工具（`approvedTools` 为空时上游只回"待审批"）。
- 抓包时**不要截断 body**。早期因为截断到 8000 字符，JSON 不完整直接解析失败，
  白排查很久。
- 前端 AI/tools 逻辑集中在 `node_modules_pnpm_tencent_i18n_*_react-dom_*.js`
  这个 chunk（约 2.1MB），搜关键字时优先看它。几个常用坐标：
  - **模型表**：该文件里搜 `TokenHub/`，附近就是完整的模型常量数组
    （`text` 显示名 / `value` 短名 / `model` 上游全名 / `tags` / `description`）
  - **工具结果协议**：搜 `<TOOL_META>`、`TOOL_REJECTED`
  - `orcaterm_start_core.zh.*.js` 里有 `assistant_setting` 的完整字段列表与模型枚举

## 关于 `js_src/`

`cdp_scripts2.py` 导出的源码会放在 `js_src/`（约 48MB，都是压缩过的第三方 bundle）。
这个目录**不纳入版本管理**，需要时重新跑一次脚本即可重建，用完可以删。
