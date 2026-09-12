#!/usr/bin/env python
# 真实上游模型 canary。需要已登录 OrcaTerm；不作为无网络 CI 测试。
import json
import sys
import urllib.error
import urllib.request
import uuid

PORT = sys.argv[1] if len(sys.argv) > 1 else "8083"
BASE = f"http://localhost:{PORT}"
QUESTION = "用一句话介绍杭州，不要超过 20 字。"
failures = []


def request(path, payload=None, headers=None):
    data = None if payload is None else json.dumps(payload, ensure_ascii=False).encode()
    req = urllib.request.Request(BASE + path, data=data, method="POST" if data else "GET")
    if data:
        req.add_header("Content-Type", "application/json")
    for key, value in (headers or {}).items():
        req.add_header(key, value)
    try:
        response = urllib.request.urlopen(req, timeout=240)
        raw = response.read().decode("utf-8", "replace")
        return response.status, {k.lower(): v for k, v in response.headers.items()}, json.loads(raw)
    except urllib.error.HTTPError as error:
        raw = error.read().decode("utf-8", "replace")
        try:
            body = json.loads(raw)
        except json.JSONDecodeError:
            body = raw
        return error.code, {k.lower(): v for k, v in error.headers.items()}, body


def check(name, condition, detail=""):
    print(("  [OK]   " if condition else "  [FAIL] ") + name, detail)
    if not condition:
        failures.append(name)


def ask(model, question=QUESTION):
    return request("/v1/chat/completions", {
        "model": model, "messages": [{"role": "user", "content": question}],
    }, {"X-Session-Id": "model-" + uuid.uuid4().hex})


def content_of(body):
    try:
        return body["choices"][0]["message"].get("content") or ""
    except (KeyError, IndexError, TypeError):
        return ""


print("=== 1. 模型目录 ===")
status, _, body = request("/v1/models")
data = body.get("data", []) if isinstance(body, dict) else []
ids = [model.get("id") for model in data]
unique = [model for model in data if not model.get("alias_of")]
current = [model for model in unique if not model.get("legacy")]
legacy = [model for model in unique if model.get("legacy")]
check("状态 200", status == 200, f"status={status}")
check("17 个唯一上游模型", len(unique) == 17, f"actual={len(unique)}")
check("8 current + 9 legacy", len(current) == 8 and len(legacy) == 9,
      f"current={len(current)} legacy={len(legacy)}")
check("每项都有 upstream", all(model.get("upstream") for model in data))
check("默认模型唯一", sum(1 for model in data if model.get("default")) == 1)

print("\n=== 2. 单模型与兼容 alias retrieve ===")
status, _, model = request("/v1/models/glm-5.3")
check("glm-5.3 可获取", status == 200 and model.get("upstream") == "TokenHub/glm-5.3")
alias = body.get("note", {}).get("alias") if isinstance(body, dict) else ""
if alias:
    status, _, alias_model = request("/v1/models/" + alias)
    check("兼容 alias 可获取", status == 200 and alias_model.get("alias_of"), str(alias_model)[:180])
status, _, _ = request("/v1/models/definitely-not-a-model")
check("未知模型返回 404", status == 404, f"status={status}")

print("\n=== 3. 当前模型抽样 ===")
answers = {}
for model_id in ["deepseek-v4-flash", "glm-5.3", "kimi-k3", "hy3"]:
    status, headers, response = ask(model_id)
    answer = content_of(response)
    answers[model_id] = answer
    check(f"{model_id} 正常回答", status == 200 and bool(answer.strip()))
    check(f"{model_id} 模型头正确", headers.get("x-orcaterm-model-id") == model_id,
          str(headers.get("x-orcaterm-model-id")))
# 自然语言回答是否不同只作为观察信号，不作为协议失败条件。
print("  [INFO] 不同回答数量:", len(set(answers.values())))

print("\n=== 4. 兼容 alias 与未知模型 ===")
status, headers, response = ask(alias or "orcaterm-assistant")
check("alias 可用于聊天", status == 200 and bool(content_of(response).strip()))
status, _, response = ask("nope-not-real")
error_code = response.get("error", {}).get("code") if isinstance(response, dict) else ""
check("未知模型被拒绝", status == 404 and error_code == "model_not_found",
      f"status={status} code={error_code}")

print("\n=== 5. 原生工具抽样 ===")
tools = [{"type": "function", "function": {
    "name": "fetch", "description": "抓取网页",
    "parameters": {"type": "object", "properties": {"url": {"type": "string"}},
                   "required": ["url"]}}}]
status, _, response = request("/v1/chat/completions", {
    "model": "glm-5.3", "tools": tools,
    "messages": [{"role": "user", "content": "请调用 fetch 工具抓取 https://example.com"}],
}, {"X-Session-Id": "model-tool-" + uuid.uuid4().hex})
choice = response.get("choices", [{}])[0] if isinstance(response, dict) else {}
check("glm-5.3 返回 tool_calls", status == 200 and choice.get("finish_reason") == "tool_calls",
      str(choice.get("finish_reason")))

print("\n" + ("全部通过 ✅" if not failures else f"失败 {len(failures)} 项: {failures}"))
sys.exit(1 if failures else 0)
