#!/usr/bin/env python
"""真机回归：模拟 agent 客户端（ZCode 形状）声明自己的工具集。

ZCode 实测会发送 model=hy4-preview 与自己的工具表（Agent/Bash/Read/Write/…）。
默认 pass 模式必须接受，并把"上游没有的工具"如实暴露在
X-OrcaTerm-Unsupported-Tools 里，而不是 400。

用法：
    .venv-cdp/Scripts/python.exe tools/reverse/proxy_agentclient_e2e.py [port]
"""
import json
import sys
import urllib.error
import urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8083"
BASE = f"http://localhost:{PORT}"
failures = []


class HeaderBag:
    def __init__(self, headers):
        self._items = {}
        for key, value in (headers.items() if headers else []):
            self._items[key.lower()] = value

    def get(self, key, default=None):
        return self._items.get(key.lower(), default)


def post(payload, timeout=240):
    request = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=json.dumps(payload, ensure_ascii=False).encode(), method="POST")
    request.add_header("Content-Type", "application/json")
    try:
        response = urllib.request.urlopen(request, timeout=timeout)
        raw = response.read().decode("utf-8", "replace")
        return response.status, HeaderBag(response.headers), json.loads(raw)
    except urllib.error.HTTPError as error:
        raw = error.read().decode("utf-8", "replace")
        try:
            body = json.loads(raw)
        except json.JSONDecodeError:
            body = {"_raw": raw}
        return error.code, HeaderBag(error.headers), body


def check(name, condition, detail=""):
    print(("  [OK]   " if condition else "  [FAIL] ") + name, str(detail)[:300])
    if not condition:
        failures.append(name)


def info(name, detail=""):
    print("  [INFO] " + name, str(detail)[:300])


def tool(name, desc=""):
    return {"type": "function", "function": {
        "name": name, "description": desc or (name + " tool"),
        "parameters": {"type": "object", "properties": {}}}}


def answer_of(body):
    try:
        return body["choices"][0]["message"].get("content") or ""
    except (KeyError, IndexError, TypeError):
        return ""


# ZCode 实测形状：自己的工具集 + hy4-preview
FOREIGN = ["Agent", "Bash", "Read", "Write", "Edit", "Glob", "Grep"]
MODEL = "hy4-preview"

print("=== 1. agent 客户端只声明自己的工具（ZCode 形状）===")
status, hdrs, body = post({
    "model": MODEL,
    "messages": [{"role": "user", "content": "用一句话说明你是什么。不要调用任何工具。"}],
    "tools": [tool(n) for n in FOREIGN],
})
check("状态 200（不再 400 custom_tools_unsupported）", status == 200,
      f"status={status} " + json.dumps(body, ensure_ascii=False)[:200])
check("X-OrcaTerm-Unsupported-Tools 如实列出",
      hdrs.get("X-OrcaTerm-Unsupported-Tools") == ",".join(FOREIGN),
      hdrs.get("X-OrcaTerm-Unsupported-Tools"))
check("返回可读正文", bool(answer_of(body).strip()),
      (answer_of(body) or json.dumps(body, ensure_ascii=False))[:200])
check("未返回调用方无法执行的 tool_calls",
      not (((body.get("choices") or [{}])[0].get("message") or {}).get("tool_calls")))
info("action", hdrs.get("X-OrcaTerm-Action"))
print("  正文:", answer_of(body)[:200])

print("\n=== 2. 混入一个原生工具：应启用原生服务且仍暴露外来名 ===")
status, hdrs, body = post({
    "model": MODEL,
    "messages": [{"role": "user", "content": "请调用 fetch 工具抓取 https://example.com"}],
    "tools": [tool("fetch", "抓取网页")] + [tool(n) for n in FOREIGN],
})
check("状态 200", status == 200, f"status={status}")
check("外来名仍然暴露",
      hdrs.get("X-OrcaTerm-Unsupported-Tools") == ",".join(FOREIGN),
      hdrs.get("X-OrcaTerm-Unsupported-Tools"))
calls = ((body.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") or []
if calls:
    info("返回原生工具调用", calls[0]["function"]["name"])
    check("调用的是原生工具", calls[0]["function"]["name"] in ("fetch", "list_terminals",
                                                          "execute_terminal_command"),
          calls[0]["function"]["name"])
else:
    info("本轮模型未发起工具调用（模型行为）", answer_of(body)[:150])

print("\n=== 3. 客户端回放自己的工具结果（role=tool，我方未知 id）===")
status, hdrs, body = post({
    "model": MODEL,
    "messages": [
        {"role": "user", "content": "看看当前目录"},
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "call_zcode_1", "type": "function",
             "function": {"name": "Bash", "arguments": "{\"command\":\"ls\"}"}}]},
        {"role": "tool", "tool_call_id": "call_zcode_1", "name": "Bash",
         "content": "MARKER-ZC-1 目录下有 README.md 和 main.go"},
    ],
    "tools": [tool(n) for n in FOREIGN],
})
check("状态 200（不再 400 unknown_tool_call_id）", status == 200,
      f"status={status} " + json.dumps(
          body.get("error") if isinstance(body, dict) and "error" in body else {}, ensure_ascii=False)[:200])
answer = answer_of(body)
check("客户端工具输出被模型消费", "MARKER-ZC-1" in answer or "README.md" in answer, answer[:200])
check("未返回 tool_calls", not (((body.get("choices") or [{}])[0].get("message") or {}).get("tool_calls")))
print("  正文:", answer[:200])

print("\n=== 4. 多个客户端工具结果也应被接受（这是我方限制，不适用于客户端自己的工具）===")
status, hdrs, body = post({
    "model": MODEL,
    "messages": [
        {"role": "user", "content": "看看这两样"},
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "call_zc_1", "type": "function",
             "function": {"name": "Bash", "arguments": "{}"}},
            {"id": "call_zc_2", "type": "function",
             "function": {"name": "Read", "arguments": "{}"}}]},
        {"role": "tool", "tool_call_id": "call_zc_1", "name": "Bash", "content": "MARKER-A"},
        {"role": "tool", "tool_call_id": "call_zc_2", "name": "Read", "content": "MARKER-B"},
    ],
    "tools": [tool(n) for n in FOREIGN],
})
check("状态 200", status == 200, f"status={status} " + json.dumps(
    body.get("error") if isinstance(body, dict) and "error" in body else {}, ensure_ascii=False)[:200])

print("\n=== 5. 严格模式仍是可选项 ===")
print("    设 ORCATERM_TOOLS_MODE=native 时，表外名字才会 400 custom_tools_unsupported")

print()
if failures:
    print(f"失败 {len(failures)} 项: {failures}")
    sys.exit(1)
print("全部通过 ✅")
