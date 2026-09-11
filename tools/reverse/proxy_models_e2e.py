#!/usr/bin/env python
# 代理层模型功能回归测试：列表 / 单模型 / 切换生效 / 别名 / 未知模型 / 工具回归
# 用法: python proxy_models_e2e.py [port]
import json, sys, urllib.request, urllib.error

PORT = sys.argv[1] if len(sys.argv) > 1 else "8083"
BASE = f"http://localhost:{PORT}"
QUESTION = "用一句话介绍杭州，不要超过 20 字。"


def get(path):
    try:
        r = urllib.request.urlopen(BASE + path, timeout=30)
        return r.status, {k.lower(): v for k, v in r.headers.items()}, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            return e.code, {}, json.loads(raw)
        except Exception:
            return e.code, {}, raw


def post(payload, hdrs=None):
    req = urllib.request.Request(BASE + "/v1/chat/completions",
                                 data=json.dumps(payload, ensure_ascii=False).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    for k, v in (hdrs or {}).items():
        req.add_header(k, v)
    try:
        r = urllib.request.urlopen(req, timeout=240)
        return r.status, {k.lower(): v for k, v in r.headers.items()}, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, {}, e.read().decode()


def ask(model, question=QUESTION, sid=None):
    msg = {"model": model, "messages": [{"role": "user", "content": question}]}
    return post(msg, {"X-Session-Id": sid} if sid else None)


fail = []


def check(name, cond, extra=""):
    print(("  [OK]   " if cond else "  [FAIL] ") + name + (("  " + extra) if extra else ""))
    if not cond:
        fail.append(name)


print("=== 1. GET /v1/models ===")
st, h, d = get("/v1/models")
ids = [m["id"] for m in d["data"]]
check("状态 200", st == 200)
check("返回 8 个客户端模型", sum(1 for m in d["data"] if not m.get("legacy") and not m.get("alias_of")) >= 8)
check("含 glm-5.3", "glm-5.3" in ids)
check("含默认模型", d["note"]["default_model"] in ids)
check("声明支持模型切换", d["note"]["capabilities"]["model_switch"] is True)
check("默认模型唯一", sum(1 for m in d["data"] if m.get("default")) == 1)
check("每个模型都有 upstream", all(m.get("upstream") for m in d["data"]))

print("\n=== 2. GET /v1/models/{id} ===")
st, h, m = get("/v1/models/glm-5.3")
check("状态 200", st == 200)
check("upstream 正确", m.get("upstream") == "TokenHub/glm-5.3", str(m.get("upstream")))
st, _, body = get("/v1/models/definitely-not-a-model")
check("未知模型返回 404", st == 404, str(st))

print("\n=== 3. 切换模型确实生效（同一问题，不同模型）===")
answers = {}
for mid in ["deepseek-v4-flash", "glm-5.3", "kimi-k3", "hy3"]:
    st, h, r = ask(mid, sid=f"model-{mid}")
    txt = (r["choices"][0]["message"]["content"] or "") if isinstance(r, dict) and "choices" in r else ""
    answers[mid] = txt
    check(f"{mid} 正常回答", st == 200 and bool(txt.strip()))
    check(f"{mid} 响应头回显上游模型", h.get("x-orcaterm-model-id") == mid,
          str(h.get("x-orcaterm-model")))
uniq = len(set(answers.values()))
check("不同模型给出不同回答（切换真的生效）", uniq >= 3, f"{uniq} 种不同回答")

print("\n=== 4. 兼容别名 orcaterm-assistant ===")
st, h, r = ask("orcaterm-assistant", sid="alias-check")
txt = (r["choices"][0]["message"]["content"] or "") if isinstance(r, dict) and "choices" in r else ""
check("别名仍可用", st == 200 and bool(txt.strip()))
check("别名落到默认模型", h.get("x-orcaterm-model-id") == "deepseek-v4-flash",
      str(h.get("x-orcaterm-model-id")))
st, _, body = ask("nope-not-real", sid="bad-model")
check("未知模型被拒绝", st == 404, f"status={st}")

print("\n=== 5. tools 回归（不受模型改动影响）===")
TOOLS = [{"type": "function", "function": {
    "name": "fetch", "description": "抓取网页",
    "parameters": {"type": "object", "properties": {"url": {"type": "string"}}, "required": ["url"]}}}]
SID = {"X-Session-Id": "model-tools-regress"}
st, h, d2 = post({"model": "glm-5.3", "tools": TOOLS,
                  "messages": [{"role": "user", "content": "请调用 fetch 工具抓取 https://example.com"}]}, SID)
ch = d2["choices"][0] if isinstance(d2, dict) and "choices" in d2 else {}
check("glm-5.3 下仍能返回 tool_calls", ch.get("finish_reason") == "tool_calls",
      str(ch.get("finish_reason")))
tc = (ch.get("message", {}) or {}).get("tool_calls", [{}])[0]
if tc:
    st, h, d3 = post({"model": "glm-5.3", "tools": TOOLS, "messages": [
        {"role": "user", "content": "请调用 fetch 工具抓取 https://example.com"},
        {"role": "assistant", "content": None, "tool_calls": [tc]},
        {"role": "tool", "tool_call_id": tc["id"], "name": tc["function"]["name"],
         "content": "MARKER-MODEL-1 标题是【青柠模型页】"}]}, SID)
    body3 = d3["choices"][0]["message"]["content"] if isinstance(d3, dict) and "choices" in d3 else ""
    check("glm-5.3 下工具结果被消费", "青柠" in (body3 or ""))

print("\n" + ("全部通过 ✅" if not fail else f"失败 {len(fail)} 项: {fail}"))
sys.exit(1 if fail else 0)
