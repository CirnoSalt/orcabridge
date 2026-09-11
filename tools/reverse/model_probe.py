#!/usr/bin/env python
# 探测上游支持哪些模型 ID：逐个发真实请求，对比回答差异
# 用法: python model_probe.py [--question "..." ]
import json, os, uuid, base64, hashlib, urllib.request, urllib.error, sys

COOKIES = os.path.join(os.environ["LOCALAPPDATA"], "com.orcaterm-desktop.app", ".cookies")
UP = "https://lightai.cloud.tencent.com"
UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36")

# 取自前端 AI chunk（module 95201）的模型表
MODELS = [
    ("Deepseek-V4-Pro",   "TokenHub/deepseek-v4-pro"),
    ("DeepSeek-V4-Flash", "TokenHub/deepseek-v4-flash"),
    ("Hy3",               "Hunyuan3/hy3"),
    ("Hy4-preview",       "Hunyuan3/hy4-preview"),
    ("Kimi-K3",           "TokenHub/kimi-k3"),
    ("GLM-5.2",           "TokenHub/glm-5.2"),
    ("GLM-5.3",           "TokenHub/glm-5.3"),
    ("GLM-5.3-Flash",     "TokenHub/glm-5.3-flash"),
]
# 对照组：不存在的模型（用于判断 model 字段是否真的被采纳）
CONTROLS = [
    ("[对照] 不存在的模型", "TokenHub/definitely-not-a-model"),
    ("[对照] value 形式",   "deepseek-v4-flash"),
]

QUESTION = "用一句话介绍杭州，不要超过 20 字。"


def creds():
    arr = json.load(open(COOKIES, encoding="utf-8"))
    sid = ot = None
    for it in arr:
        d = it.get("domain") or {}
        dom = d.get("HostOnly") or d.get("Suffix") or ""
        raw = (it.get("raw_cookie") or "").split(";")[0].strip()
        if "=" not in raw:
            continue
        k, v = raw.split("=", 1)
        if "lightai.cloud.tencent.com" in dom and k == "sid":
            sid = v
        if "orcaterm.cloud.tencent.com" in dom and k == "ot_session":
            ot = v
    return sid, ot


def jwt_uid(jwt):
    s = jwt.split(".")[1]; s += "=" * (-len(s) % 4)
    return json.loads(base64.urlsafe_b64decode(s)).get("userId")


def ask(model, question):
    sid, ot = creds()
    uid = str(jwt_uid(ot))
    env = {"_format": "acp-prompt", "version": 1, "prompt": [{"type": "text", "text": question}]}
    body = {
        "conversationId": "cid-" + str(uuid.uuid4()) + "-orcaterm",
        "user": {"id": uid, "setting": {
            "mcpServers": ["mcp-server-orcaterm-oauth"],
            "uiServers": ["ui-tools-orcaterm-explorer"],
            "tools": [], "approvedTools": [], "approvedMCPTools": [],
            "enableAutoSubtaskExecution": False, "env": [],
            "model": model, "modelDesc": {},
        }},
        "input": {"type": "start", "input": json.dumps(env, ensure_ascii=False),
                  "id": str(uuid.uuid4()), "emphasisPrompt": "", "files": []},
        "stream": False,
    }
    r = urllib.request.Request(UP + "/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true",
                               data=json.dumps(body, ensure_ascii=False).encode(), method="POST")
    for k, v in [
        ("Content-Type", "application/json"), ("Accept", "text/event-stream"),
        ("Authorization", "Bearer " + ot),
        ("Cookie", f"ot_session={ot}; userAccountLoginMethod=oauth; userAccountType=wechat; userId={uid}"),
        ("X-Product", "orcaterm"), ("X-Seq-Id", str(uuid.uuid4())),
        ("X-Csrfcode", ""), ("X-Referer", "http://tauri.localhost/"),
        ("Origin", "http://tauri.localhost"), ("User-Agent", UA),
    ]:
        r.add_header(k, v)
    try:
        raw = urllib.request.urlopen(r, timeout=180).read().decode("utf-8", "replace")
        j = json.loads(raw)
        return j.get("code"), (j.get("data") or {})
    except urllib.error.HTTPError as e:
        return e.code, {"error": e.read().decode("utf-8", "replace")[:200]}
    except Exception as e:
        return -1, {"error": repr(e)[:200]}


def main():
    if "--question" in sys.argv:
        q = sys.argv[sys.argv.index("--question") + 1]
    else:
        q = QUESTION
    print(f"提问: {q}\n")
    seen = {}
    for label, mid in MODELS + CONTROLS:
        code, d = ask(mid, q)
        tc = (d.get("taskCompletion") or "").strip()
        th = (d.get("thinking") or "").strip()
        h = hashlib.md5((tc + "|" + th).encode("utf-8")).hexdigest()[:8]
        seen.setdefault(h, []).append(label)
        print(f"{label:22s} model={mid:34s} code={code} action={d.get('action')}")
        print(f"    回答: {tc[:80]}")
        if d.get("error"):
            print(f"    错误: {str(d['error'])[:160]}")
        print(f"    指纹: {h}")
    print("\n=== 相同指纹归组（同一指纹=回答完全一致，可能未真正切换）===")
    for h, labels in seen.items():
        print(f"  {h}: {labels}")


if __name__ == "__main__":
    main()
