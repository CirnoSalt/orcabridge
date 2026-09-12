#!/usr/bin/env python
"""真实上游 canary：Markdown 保真、代码块保真、agent 客户端任务闭环。

针对性回归前几轮报告里的清洗器缺陷：
  - 普通 Markdown（标题/列表/表格/粗体）不能被删
  - 普通 ```json 围栏不能被当内部包络删除
  - 代码围栏内的 <think> 必须保留
  - 周年/抽奖/日志分析/文件操作等业务关键词不能被当噪音删除

用法：
    .venv-cdp/Scripts/python.exe tools/reverse/proxy_content_e2e.py [port]
"""
import json
import re
import sys
import urllib.error
import urllib.request
import uuid

PORT = sys.argv[1] if len(sys.argv) > 1 else "8083"
BASE = f"http://localhost:{PORT}"
failures = []
infos = []


class HeaderBag:
    def __init__(self, headers):
        self._items = {}
        for key, value in (headers.items() if headers else []):
            self._items[key.lower()] = value

    def get(self, key, default=None):
        return self._items.get(key.lower(), default)


def post(path, payload, headers=None, timeout=240):
    request = urllib.request.Request(
        BASE + path,
        data=json.dumps(payload, ensure_ascii=False).encode() if payload is not None else None,
        method="POST" if payload is not None else "GET")
    if payload is not None:
        request.add_header("Content-Type", "application/json")
    for key, value in (headers or {}).items():
        request.add_header(key, value)
    try:
        response = urllib.request.urlopen(request, timeout=timeout)
        raw = response.read().decode("utf-8", "replace")
        return response.status, HeaderBag(response.headers), raw
    except urllib.error.HTTPError as error:
        return error.code, HeaderBag(error.headers), error.read().decode("utf-8", "replace")


def chat(prompt, model="deepseek-v4-flash", headers=None, extra=None, timeout=240):
    payload = {"model": model, "messages": [{"role": "user", "content": prompt}]}
    if extra:
        payload.update(extra)
    status, hdrs, raw = post("/v1/chat/completions", payload, headers, timeout)
    try:
        body = json.loads(raw)
    except json.JSONDecodeError:
        body = {"_raw": raw}
    return status, hdrs, body


def answer_of(body):
    try:
        return body["choices"][0]["message"].get("content") or ""
    except (KeyError, IndexError, TypeError):
        return ""


def show(label, text, limit=700):
    print(f"  --- {label} ---")
    for line in (text or "").splitlines()[:28]:
        print("    " + line[:160])
    if len(text or "") > limit:
        print(f"    …（共 {len(text)} 字符）")


def check(name, condition, detail=""):
    print(("  [OK]   " if condition else "  [FAIL] ") + name, str(detail)[:240])
    if not condition:
        failures.append(name)


def info(name, detail=""):
    print("  [INFO] " + name, str(detail)[:240])
    infos.append(name)


def status_detail(status, body):
    """状态断言的自解释详情：带上错误码与消息，否则 502 只能靠猜。"""
    if isinstance(body, dict) and isinstance(body.get("error"), dict):
        err = body["error"]
        return f"status={status} code={err.get('code')} msg={err.get('message')}"
    return f"status={status}"


def action_of(hdrs):
    """代理通过 X-OrcaTerm-Action 暴露上游动作。"""
    return (hdrs.get("X-OrcaTerm-Action") or "").strip()


def is_ask(hdrs):
    return action_of(hdrs) == "ask"


def fences(text):
    """返回 [(lang, body), ...]"""
    return [(m.group(1).strip(), m.group(2)) for m in
            re.finditer(r"```([^\n`]*)\n(.*?)```", text or "", re.S)]


# ===== A. Markdown 保真 =====

print("=== A1. Markdown 结构（标题/列表/表格/粗体）保真 ===")
status, hdrs, body = chat(
    "写一份简短的《服务器上线检查清单》Markdown 文档，要求必须包含："
    "一个二级标题、一个有序列表、一个无序列表、一个两列的 Markdown 表格、以及至少一处 **粗体**。"
    "只输出 Markdown 本身，不要任何前言。")
text = answer_of(body)
show("回答", text)
check("状态 200", status == 200, status_detail(status, body))
check("包含二级标题", re.search(r"^##\s", text, re.M) is not None)
check("包含列表项", re.search(r"^\s*[-*]\s", text, re.M) is not None)
check("包含表格分隔行", re.search(r"^\s*\|[\s:|-]+\|\s*$", text, re.M) is not None)
check("包含粗体", "**" in text)
check("未泄漏内部包络", '"taskCompletion"' not in text and '"action"' not in text)
check("X-OrcaTerm-Action 合法", action_of(hdrs) in ("completion", "ask", "review"), action_of(hdrs))

print("\n=== A2. 业务关键词不得被清洗（周年/抽奖/日志分析/文件操作）===")
keywords = ["周年", "抽奖", "日志分析", "文件操作"]
prompt = ("请用中文写一段 120 字左右的运维说明，必须原样包含下面四个词各至少一次："
          "周年、抽奖、日志分析、文件操作。不要解释，直接给正文。")
# 上游经常先用 action=ask 反问确认（真机观察到），那属模型行为：
# 代理必须如实把它标成 ask，而不是让调用方以为拿到了正文。
text, final_headers = "", None
for attempt in range(2):
    status, final_headers, body = chat(prompt)
    text = answer_of(body)
    if status != 200 or is_ask(final_headers) or all(k in text for k in keywords):
        break
show("回答", text)
check("状态 200", status == 200, status_detail(status, body))
check("X-OrcaTerm-Action 合法",
      action_of(final_headers) in ("completion", "ask", "review"), action_of(final_headers))
if is_ask(final_headers):
    info("A2: 上游返回 action=ask 反问而非正文（模型行为，正文未被清洗）", text[:160])
    check("A2: 反问文本本身未被清洗为空", bool(text.strip()))
else:
    missing = [k for k in keywords if k not in text]
    check("四个业务关键词全部保留", not missing, f"缺失={missing}")
    check("正文长度合理", len(text) >= 60, f"len={len(text)}")

# ===== B. 代码块保真 =====

def fence_run(label, prompt, validate, tries=2):
    """代码围栏保真用例。

    真机观察到上游模型有时会拒绝输出代码围栏，并把内部 JSON 输出约束泄漏成正文
    （"我的输出必须遵循系统规定的JSON格式…"）。那属模型行为，不是清洗缺陷。
    所以：模型没给围栏只记 INFO；只要给了围栏，内容就必须原样通过清洗管线。
    """
    text = ""
    for _ in range(tries):
        status, _, body = chat(prompt)
        if status != 200:
            check(label + " 状态 200", False, status_detail(status, body))
            return
        text = answer_of(body)
        if fences(text):
            break
    if not fences(text):
        if re.search(r"JSON\s*格式|无法直接|必须遵循|不可以?输出", text):
            info(label + ": 上游拒绝并泄漏内部 JSON 输出约束（模型行为，非清洗缺陷）", text[:150])
        else:
            info(label + ": 本轮未返回代码围栏（模型行为）", text[:150])
        check(label + ": 无围栏时仍应有可读正文", bool(text.strip()))
        return
    validate(text)


def b1_validate(text):
    show("回答", text)
    blocks = fences(text)
    go_blocks = [b for lang, b in blocks if lang.lower() == "go"]
    check("B1: 存在 go 围栏", bool(go_blocks), "围栏=" + str([l for l, _ in blocks]))
    check("B1: 围栏内容含 package main", any("package main" in b for b in go_blocks))
    check("B1: 围栏内容含 fmt.Println", any("fmt.Println" in b for b in go_blocks))
    check("B1: 围栏闭合（无残余反引号）", text.count("```") % 2 == 0,
          "反引号数=" + str(text.count("```")))


def b2_validate(text):
    show("回答", text)
    blocks = fences(text)
    json_blocks = [b for lang, b in blocks if lang.lower() in ("json", "")]
    check("B2: json 围栏未被删除", bool(json_blocks), "围栏=" + str([l for l, _ in blocks]))
    check("B2: 保留了 name/port 字段", "name" in text and "port" in text)
    check("B2: 未出现 action/taskCompletion 内部字段", '"action"' not in text)


def b3_validate(text):
    show("回答", text)
    check("B3: 围栏内 <think> 未被剥离", "<think>" in text and "</think>" in text, text[:200])
    check("B3: 第二三行保留", "第二行" in text and "第三行" in text)


def b4_validate(text):
    show("回答", text)
    langs = [l.lower() for l, _ in fences(text)]
    check("B4: python 围栏保留", any(l.startswith("python") for l in langs), "langs=" + str(langs))
    check("B4: sql 围栏保留", "sql" in langs, "langs=" + str(langs))


B1_PROMPT = (
    "给出一个 Go 语言的 hello world 完整程序，必须放在 ```go 围栏里，"
    "并在围栏前用一句话说明。只输出这一句说明和代码块。"
)
B2_PROMPT = (
    "给我一个示例 JSON 配置，放在 ```json 围栏里，字段包含 name 和 port 两个键，"
    "值分别是 demo 和 8080。只输出这个代码块。"
)
B3_PROMPT = (
    "请输出一个 ```text 围栏，围栏内容必须原样是这三行：\n"
    "<think>示例</think>\n第二行\n第三行\n"
    "除这个代码块外不要输出别的内容。"
)
B4_PROMPT = (
    "依次输出两个代码块：第一个是 ```python，内容是 print('hello')；"
    "第二个是 ```sql，内容是 SELECT 1;。不要其他文字。"
)

print("\n=== B1. Go 代码围栏逐字保留 ===")
fence_run("B1", B1_PROMPT, b1_validate)

print("\n=== B2. 普通 ```json 围栏不得被当内部包络删除 ===")
fence_run("B2", B2_PROMPT, b2_validate)

print("\n=== B3. 代码围栏内的 <think> 必须保留 ===")
fence_run("B3", B3_PROMPT, b3_validate)

print("\n=== B4. Python + SQL 围栏 ===")
fence_run("B4", B4_PROMPT, b4_validate)

# ===== C. agent 客户端任务模拟 =====

print("\n=== C1. 多轮会话：同一 X-Session-Id 上下文连贯 ===")
sess = "live-" + str(uuid.uuid4())[:8]
headers = {"X-Session-Id": sess}
status1, hdrs1, body1 = chat("我叫青柠，请记住这个名字。只回复“记住了”。", headers=headers)
status2, hdrs2, body2 = chat("我叫什么名字？只回复名字。", headers=headers)
show("第一轮", answer_of(body1), 200)
show("第二轮", answer_of(body2), 200)
check("两轮都 200", status1 == 200 and status2 == 200, f"{status1}/{status2}")
check("会话头回显", hdrs2.get("X-OrcaTerm-Session") == sess, hdrs2.get("X-OrcaTerm-Session"))
check("上下文连贯（记得名字）", "青柠" in answer_of(body2), answer_of(body2)[:120])
try:
    turn = int(hdrs2.get("X-OrcaTerm-Turn") or 0)
    check("第二轮标记为第 2 轮", turn >= 2, f"turn={turn}")
except ValueError:
    check("第二轮标记为第 2 轮", False, "X-OrcaTerm-Turn 非数字")

print("\n=== C2. agent 任务：终端工具链闭环 ===")
TOOLS = [
    {"type": "function", "function": {
        "name": "list_terminals", "description": "列出可用的终端会话",
        "parameters": {"type": "object", "properties": {}}}},
    {"type": "function", "function": {
        "name": "select_connect_config", "description": "选择一个已保存的连接配置",
        "parameters": {"type": "object", "properties": {}}}},
    {"type": "function", "function": {
        "name": "list_connect_configs", "description": "列出已保存的连接配置",
        "parameters": {"type": "object", "properties": {}}}},
    {"type": "function", "function": {
        "name": "get_terminal_detail", "description": "查看终端详情",
        "parameters": {"type": "object", "properties": {"terminalId": {"type": "string"}}}}},
    {"type": "function", "function": {
        "name": "execute_terminal_command",
        "description": "在服务器上执行终端命令",
        "parameters": {"type": "object",
                       "properties": {"command": {"type": "string"}},
                       "required": ["command"]}}},
    {"type": "function", "function": {
        "name": "get_terminal_output", "description": "读取终端输出",
        "parameters": {"type": "object", "properties": {"terminalId": {"type": "string"}}}}},
    {"type": "function", "function": {
        "name": "fetch", "description": "抓取网页内容",
        "parameters": {"type": "object",
                       "properties": {"url": {"type": "string"}},
                       "required": ["url"]}}},
]
task = ("请用 execute_terminal_command 工具查看当前目录下的文件列表，"
        "然后告诉我有哪些文件。")
status, hdrs, body = chat(task, extra={"tools": TOOLS})
# 真机回归点：早期只声明 execute_terminal_command 时，上游会先请求 list_terminals，
# 代理返回 502 upstream_requested_undeclared_tool，整条终端任务直接失败。
check("未因未声明工具被拒", not (isinstance(body, dict) and body.get("error", {}).get("code")
                              == "upstream_requested_undeclared_tool"),
      json.dumps(body, ensure_ascii=False)[:200])
check("状态 200", status == 200, status_detail(status, body))
tool_calls = ((body.get("choices") or [{}])[0].get("message", {}) or {}).get("tool_calls") or []
if tool_calls:
    call = tool_calls[0]
    print("  工具调用:", call["function"]["name"],
          call["function"]["arguments"][:160])
    check("finish_reason=tool_calls",
          (body.get("choices") or [{}])[0].get("finish_reason") == "tool_calls")
    check("content 为 null",
          (body.get("choices") or [{}])[0].get("message", {}).get("content") is None)
    check("工具名在原生 allowlist 内", call["function"]["name"] in (
        "fetch", "list_terminals", "select_connect_config", "list_connect_configs",
        "get_terminal_detail", "get_terminal_output", "get_active_terminal",
        "execute_terminal_command", "send_terminal_signal",
        "get_command_history"), call["function"]["name"])
    messages = [
        {"role": "user", "content": task},
        {"role": "assistant", "content": None, "tool_calls": [call]},
        {"role": "tool", "tool_call_id": call["id"], "name": call["function"]["name"],
         "content": "total 8\n-rw-r--r-- 1 root root  120 Sep 12 12:00 README.md\n"
                    "-rw-r--r-- 1 root root 2048 Sep 12 12:00 main.go\n"
                    "drwxr-xr-x 2 root root  4096 Sep 12 12:00 cmd"},
    ]
    status2, hdrs2, raw2 = post("/v1/chat/completions",
                                {"model": "deepseek-v4-flash", "messages": messages, "tools": TOOLS})
    try:
        body2 = json.loads(raw2)
    except json.JSONDecodeError:
        body2 = {"_raw": raw2}
    ans = answer_of(body2)
    show("工具结果后的回答", ans)
    check("工具结果轮 200", status2 == 200, status_detail(status2, body2))
    check("模型消费了工具输出",
          any(k in ans for k in ("README.md", "main.go", "cmd")), ans[:200])
    check("工具结果轮标记", hdrs2.get("X-OrcaTerm-Tool-Result-Turn") == "1")
else:
    info("本轮模型未发起工具调用（选择了反问/澄清）",
         f"action={action_of(hdrs)} " + answer_of(body)[:140])
    check("未发起工具调用时也应有可读正文", bool(answer_of(body).strip()))

print("\n=== C3. agent 任务：多步日志分析（不依赖工具）===")
status, hdrs, body = chat(
    "以下是一段 nginx 日志片段，请分析并给出结论，用 Markdown 列表输出至少 3 条：\n"
    "10.0.0.1 - - [12/Sep/2026:10:00:01] \"GET /api/a HTTP/1.1\" 200 512\n"
    "10.0.0.2 - - [12/Sep/2026:10:00:02] \"GET /api/b HTTP/1.1\" 500 128\n"
    "10.0.0.2 - - [12/Sep/2026:10:00:03] \"GET /api/b HTTP/1.1\" 500 128\n"
    "10.0.0.3 - - [12/Sep/2026:10:00:04] \"POST /api/c HTTP/1.1\" 404 64")
text = answer_of(body)
show("回答", text)
check("状态 200", status == 200, status_detail(status, body))
check("有 Markdown 列表", re.search(r"^\s*[-*\d]", text, re.M) is not None)
check("识别到 500 错误", "500" in text)
check("未泄漏内部包络", '"taskCompletion"' not in text)

print("\n=== C4. 流式协议（含工具调用） ===")
status, hdrs, body = chat(
    "1+1等于几？只回答数字。", extra={"stream": True,
                                     "stream_options": {"include_usage": True}},
    timeout=240)
raw = body.get("_raw", "")
chunks = []
for block in raw.split("\n\n"):
    line = block.strip()
    if not line.startswith("data:"):
        continue
    payload = line[5:].strip()
    if payload == "[DONE]":
        chunks.append({"__done": True})
        continue
    try:
        chunks.append(json.loads(payload))
    except json.JSONDecodeError:
        check("SSE 分片可解析", False, payload[:120])
deltas = [c for c in chunks if "choices" in c and c["choices"]]
first_delta = deltas[0]["choices"][0].get("delta", {}) if deltas else {}
check("状态 200", status == 200, status_detail(status, body))
check("Content-Type 是 SSE", (hdrs.get("Content-Type") or "").startswith("text/event-stream"),
      hdrs.get("Content-Type"))
check("首块含 delta.role=assistant", first_delta.get("role") == "assistant", first_delta)
check("以 [DONE] 结束", bool(chunks) and chunks[-1].get("__done") is True)
usage_chunks = [c for c in chunks if "usage" in c]
check("include_usage 时发送 usage chunk", bool(usage_chunks))
if usage_chunks:
    check("usage chunk 的 choices 为空数组", usage_chunks[0].get("choices") == [], usage_chunks[0].get("choices"))
    check("usage chunk 带 created", "created" in usage_chunks[0])
streamed = "".join((c["choices"][0].get("delta", {}) or {}).get("content", "")
                   for c in deltas if c["choices"])
check("流式正文非空", bool(streamed.strip()), repr(streamed[:80]))

print("\n=== C5. 流式 + 工具调用 delta 形状 ===")
# 注意：按名字取工具，不要用索引 —— C2 的工具表顺序会变。
FETCH_TOOL = next(t for t in TOOLS if t["function"]["name"] == "fetch")
status, hdrs, body = chat("请用 fetch 工具抓取 https://example.com", extra={
    "stream": True, "tools": [FETCH_TOOL]}, timeout=240)
raw = body.get("_raw", "")
tool_delta = None
for block in raw.split("\n\n"):
    line = block.strip()
    if not line.startswith("data:") or line[5:].strip() == "[DONE]":
        continue
    try:
        chunk = json.loads(line[5:].strip())
    except json.JSONDecodeError:
        continue
    for choice in chunk.get("choices") or []:
        calls = (choice.get("delta") or {}).get("tool_calls")
        if calls:
            tool_delta = calls
check("状态 200", status == 200, status_detail(status, body))
check("流中出现 tool_calls delta", tool_delta is not None)
if tool_delta:
    call = tool_delta[0]
    for key in ("index", "id", "type", "function"):
        check(f"tool_calls delta 含 {key}", key in call, json.dumps(call, ensure_ascii=False)[:160])
else:
    info("本轮模型未发起工具调用（属模型行为，非协议缺陷）")

# ===== D. 调试端点对照 raw / cleaned =====

print("\n=== D1. /debug/last 对照上游原始响应与清洗结果 ===")
status, hdrs, raw = post("/debug/last", None)
try:
    debug = json.loads(raw)
except json.JSONDecodeError:
    debug = {}
check("debug 端点可用", status == 200 and bool(debug), f"status={status}")
if isinstance(debug, dict) and debug:
    keys = list(debug)
    info("debug 字段", keys)
    meta = debug.get("meta") or {}
    info("meta", json.dumps(meta, ensure_ascii=False)[:200])
    check("meta.action 存在", "action" in meta, meta)

print()
if failures:
    print(f"失败 {len(failures)} 项: {failures}")
    sys.exit(1)
print("全部通过 ✅")
