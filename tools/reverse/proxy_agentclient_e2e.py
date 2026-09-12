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
# Bash 在默认桥接档（safe）下会被桥接到上游 execute_command，
# 因此它**不再**出现在 Unsupported 里 —— 这正是桥接生效的标志。
FOREIGN_UNSUPPORTED = [n for n in FOREIGN if n != "Bash"]
MODEL = "hy4-preview"

print("=== 1. agent 客户端只声明自己的工具（ZCode 形状）===")
status, hdrs, body = post({
    "model": MODEL,
    "messages": [{"role": "user", "content": "用一句话说明你是什么。不要调用任何工具。"}],
    "tools": [tool(n) for n in FOREIGN],
})
check("状态 200（不再 400 custom_tools_unsupported）", status == 200,
      f"status={status} " + json.dumps(body, ensure_ascii=False)[:200])
check("X-OrcaTerm-Unsupported-Tools 如实列出（Bash 已被桥接，不在其中）",
      hdrs.get("X-OrcaTerm-Unsupported-Tools") == ",".join(FOREIGN_UNSUPPORTED),
      hdrs.get("X-OrcaTerm-Unsupported-Tools"))
check("Bash 不再被当成 Unsupported（桥接生效）",
      "Bash" not in (hdrs.get("X-OrcaTerm-Unsupported-Tools") or ""),
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
check("外来名仍然暴露（Bash 已被桥接，不在其中）",
      hdrs.get("X-OrcaTerm-Unsupported-Tools") == ",".join(FOREIGN_UNSUPPORTED),
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

print("\n=== 6. 工具桥接：只声明 Bash，上游的 execute_command 应改名成 Bash ===")
print("    （默认 ORCATERM_TOOLS_BRIDGE=safe）")
status, hdrs, body = post({
    "model": MODEL,
    "messages": [{"role": "user", "content": "请执行命令 uname -a"}],
    "tools": [tool("Bash", "Run a shell command")],
})
check("状态 200", status == 200,
      f"status={status} " + json.dumps(body.get("error", {}), ensure_ascii=False)[:200])
calls6 = ((body.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") or []
bridge_call = None
if calls6:
    bridge_call = calls6[0]
    check("对外名字是客户端声明的 Bash", calls6[0]["function"]["name"] == "Bash",
          calls6[0]["function"]["name"])
    args6 = json.loads(calls6[0]["function"]["arguments"] or "{}")
    check("参数重映射后仍带 command", "command" in args6, json.dumps(args6, ensure_ascii=False))
    check("上游专有参数被丢弃",
          not ({"terminal_id", "connect_config_id"} & set(args6)),
          json.dumps(args6, ensure_ascii=False))
    check("响应头标出桥接档位", hdrs.get("X-OrcaTerm-Bridge") == "safe",
          hdrs.get("X-OrcaTerm-Bridge"))
else:
    info("本轮模型未发起工具调用（模型行为，不算失败）", answer_of(body)[:150])

print("\n=== 7. 桥接调用的结果能被回传（按 id 认领，不再 400）===")
if bridge_call is None:
    info("第 6 例没拿到工具调用，跳过回传验证")
else:
    status, hdrs, body = post({
        "model": MODEL,
        "messages": [
            {"role": "user", "content": "请执行命令 uname -a"},
            {"role": "assistant", "content": None, "tool_calls": [bridge_call]},
            {"role": "tool", "tool_call_id": bridge_call["id"], "name": "Bash",
             "content": "MARKER-BRIDGE-1 Linux orcabridge 6.6.0"},
        ],
        "tools": [tool("Bash", "Run a shell command")],
    })
    check("回传桥接结果 200（不再 400 unknown_tool_call_id）", status == 200,
          f"status={status} " + json.dumps(body.get("error", {}), ensure_ascii=False)[:200])
    check("模型消费了桥接结果",
          "MARKER-BRIDGE-1" in answer_of(body) or "Linux" in answer_of(body),
          answer_of(body)[:200])

print()
if failures:
    print(f"失败 {len(failures)} 项: {failures}")
    sys.exit(1)
print("全部通过 ✅")
