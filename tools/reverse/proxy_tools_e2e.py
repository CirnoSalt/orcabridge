#!/usr/bin/env python
# 通过代理验证 tools 全链路：tool_calls 返回 + role=tool 回传续跑
import json, urllib.request, urllib.error, sys

PORT = sys.argv[1] if len(sys.argv) > 1 else "8083"
BASE = f"http://localhost:{PORT}"


def post(payload, hdrs=None):
    req = urllib.request.Request(BASE + "/v1/chat/completions",
                                 data=json.dumps(payload, ensure_ascii=False).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    for k, v in (hdrs or {}).items():
        req.add_header(k, v)
    try:
        r = urllib.request.urlopen(req, timeout=240)
        return r.status, dict(r.headers), json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode("utf-8", "replace")


TOOLS = [{"type": "function",
          "function": {"name": "fetch", "description": "抓取网页内容",
                       "parameters": {"type": "object",
                                      "properties": {"url": {"type": "string"}},
                                      "required": ["url"]}}}]
SID = {"X-Session-Id": "e2e-tools-1"}

print("=== 轮1：带 tools 提问（期望 tool_calls）===")
st, h, d = post({"model": "orcaterm-assistant",
                 "messages": [{"role": "user", "content": "请调用 fetch 工具抓取 https://example.com 的内容"}],
                 "tools": TOOLS}, SID)
print("  status:", st, "| action:", h.get("X-OrcaTerm-Action"), "| tool:", h.get("X-OrcaTerm-Tool"))
if not isinstance(d, dict) or "choices" not in d:
    print("  响应:", str(d)[:400]); sys.exit(1)
ch = d["choices"][0]
print("  finish_reason:", ch["finish_reason"])
msg = ch["message"]
if not msg.get("tool_calls"):
    print("  !! 未返回 tool_calls，正文:", (msg.get("content") or "")[:200]); sys.exit(1)
tc = msg["tool_calls"][0]
print("  tool_call:", tc["function"]["name"], tc["function"]["arguments"])
print("  content 为 null:", msg.get("content") is None)

print("\n=== 轮2：回传工具结果（期望模型消费）===")
msgs = [
    {"role": "user", "content": "请调用 fetch 工具抓取 https://example.com 的内容"},
    {"role": "assistant", "content": None, "tool_calls": [tc]},
    {"role": "tool", "tool_call_id": tc["id"], "name": tc["function"]["name"],
     "content": "MARKER-8899 页面标题是【青柠测试页】，正文只有一句话。"},
]
st2, h2, d2 = post({"model": "orcaterm-assistant", "messages": msgs, "tools": TOOLS}, SID)
print("  status:", st2, "| action:", h2.get("X-OrcaTerm-Action"),
      "| toolResultTurn:", h2.get("X-OrcaTerm-Tool-Result-Turn"))
body = d2["choices"][0]["message"]["content"] if isinstance(d2, dict) and "choices" in d2 else str(d2)
print("  回答:", (body or "")[:300].replace("\n", " "))
print("\n  模型是否消费工具结果:", "青柠" in (body or "") or "MARKER-8899" in (body or ""))

print("\n=== 轮3：拒绝工具（空结果，期望走 [TOOL_REJECTED]）===")
msgs3 = msgs[:2] + [{"role": "tool", "tool_call_id": tc["id"], "name": tc["function"]["name"], "content": ""}]
st3, h3, d3 = post({"model": "orcaterm-assistant", "messages": msgs3, "tools": TOOLS},
                   {"X-Session-Id": "e2e-tools-reject"})
b3 = d3["choices"][0]["message"]["content"] if isinstance(d3, dict) and "choices" in d3 else str(d3)
print("  status:", st3, "| action:", h3.get("X-OrcaTerm-Action"))
print("  回答:", (b3 or "")[:220].replace("\n", " "))

print("\n=== 轮4：不带 tools 的普通问答不受影响 ===")
st4, h4, d4 = post({"model": "orcaterm-assistant",
                    "messages": [{"role": "user", "content": "1+1等于几？只回答数字。"}]},
                   {"X-Session-Id": "e2e-plain"})
b4 = d4["choices"][0]["message"]["content"] if isinstance(d4, dict) and "choices" in d4 else str(d4)
print("  status:", st4, "| 回答:", (b4 or "")[:120].replace("\n", " "))
