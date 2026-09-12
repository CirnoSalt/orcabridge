#!/usr/bin/env python
# 真实上游 canary：验证原生工具调用与结果闭环。需要已登录 OrcaTerm。
import json
import sys
import urllib.error
import urllib.request
import uuid

PORT = sys.argv[1] if len(sys.argv) > 1 else "8083"
BASE = f"http://localhost:{PORT}"
TOOLS = [{"type": "function", "function": {
    "name": "fetch", "description": "抓取网页内容",
    "parameters": {"type": "object", "properties": {"url": {"type": "string"}},
                   "required": ["url"]}}}]
failures = []


class HeaderBag:
    """大小写无关的响应头视图。

    Go 用 textproto 规范化响应头（X-OrcaTerm-* 在线上是 X-Orcaterm-*），
    而 dict(response.headers) 的查找是大小写敏感的，直接用会误报。
    """

    def __init__(self, headers):
        self._items = {}
        for key, value in (headers.items() if headers else []):
            self._items[key.lower()] = value

    def get(self, key, default=None):
        return self._items.get(key.lower(), default)


def post(payload, headers=None):
    request = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=json.dumps(payload, ensure_ascii=False).encode(), method="POST")
    request.add_header("Content-Type", "application/json")
    for key, value in (headers or {}).items():
        request.add_header(key, value)
    try:
        response = urllib.request.urlopen(request, timeout=240)
        raw = response.read().decode("utf-8", "replace")
        return response.status, HeaderBag(response.headers), json.loads(raw)
    except urllib.error.HTTPError as error:
        raw = error.read().decode("utf-8", "replace")
        try:
            body = json.loads(raw)
        except json.JSONDecodeError:
            body = raw
        return error.code, HeaderBag(error.headers), body


def check(name, condition, detail=""):
    print(("  [OK]   " if condition else "  [FAIL] ") + name, detail)
    if not condition:
        failures.append(name)


def content_of(body):
    try:
        return body["choices"][0]["message"].get("content") or ""
    except (KeyError, IndexError, TypeError):
        return ""


session = "e2e-tools-" + uuid.uuid4().hex
headers = {"X-Session-Id": session}
question = "请调用 fetch 工具抓取 https://example.com 的内容"

print("=== 1. 请求原生工具 ===")
status, response_headers, body = post({
    "model": "orcaterm-assistant", "messages": [{"role": "user", "content": question}],
    "tools": TOOLS,
}, headers)
choice = body.get("choices", [{}])[0] if isinstance(body, dict) else {}
tool_calls = (choice.get("message") or {}).get("tool_calls") or []
check("状态 200", status == 200, f"status={status}")
check("finish_reason=tool_calls", choice.get("finish_reason") == "tool_calls")
check("返回一个 fetch 调用", len(tool_calls) == 1 and
      tool_calls[0].get("function", {}).get("name") == "fetch")
check("content 为 null", (choice.get("message") or {}).get("content") is None)
if not tool_calls:
    print("无法继续工具闭环。响应:", str(body)[:500])
    sys.exit(1)
tool_call = tool_calls[0]

print("\n=== 2. 不带私有 session，仅凭 tool_call_id 回传结果 ===")
messages = [
    {"role": "user", "content": question},
    {"role": "assistant", "content": None, "tool_calls": [tool_call]},
    {"role": "tool", "tool_call_id": tool_call["id"], "name": "fetch",
     "content": "MARKER-8899 页面标题是【青柠测试页】，正文只有一句话。"},
]
status, response_headers, body = post({
    "model": "orcaterm-assistant", "messages": messages, "tools": TOOLS,
})
answer = content_of(body)
check("状态 200", status == 200, f"status={status}")
check("标记为工具结果轮", response_headers.get("X-OrcaTerm-Tool-Result-Turn") == "1")
check("模型消费工具结果", "青柠" in answer or "MARKER-8899" in answer, answer[:200])

print("\n=== 3. 重复回传同一 tool_call_id 被拒绝 ===")
status, _, body = post({"model": "orcaterm-assistant", "messages": messages, "tools": TOOLS})
error_code = body.get("error", {}).get("code") if isinstance(body, dict) else ""
check("返回 400", status == 400, f"status={status}")
check("返回已消费错误", error_code == "tool_call_already_consumed", str(body)[:240])

print("\n=== 4. 普通问答不受影响 ===")
status, _, body = post({
    "model": "orcaterm-assistant",
    "messages": [{"role": "user", "content": "1+1等于几？只回答数字。"}],
}, {"X-Session-Id": "e2e-plain-" + uuid.uuid4().hex})
check("状态 200 且正文非空", status == 200 and bool(content_of(body).strip()))

print("\n" + ("全部通过 ✅" if not failures else f"失败 {len(failures)} 项: {failures}"))
sys.exit(1 if failures else 0)
