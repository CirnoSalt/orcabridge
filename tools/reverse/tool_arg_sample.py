#!/usr/bin/env python
"""真机采样：上游原生工具调用的**参数形状**。

背景：OrcaBridge 想做"上游原生工具 → 客户端声明工具"的语义映射，
但上游的 tool schema 由服务端下发，客户端 bundle 里只留工具名字符串，
拿不到参数表。所以只能真机把调用打出来，看 args 实际长什么样。

做法：对每个场景声明一批原生工具 + 一句指令，请求之后立刻读
GET /debug/last，从 raw 里取 data.tool.{name,args}。
代理不会执行这些调用，只观察形状，安全。

用法：
    .venv-cdp/Scripts/python.exe tools/reverse/tool_arg_sample.py [port] [场景序号...]

输出可直接用于维护 main.go 里的工具映射表。
"""
import json
import sys
import urllib.error
import urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8083"
BASE = f"http://localhost:{PORT}"

# (标签, 指令, 声明的原生工具)
SCENARIOS = [
    ("终端-执行命令", "请执行 pwd 命令，告诉我当前目录",
     ["list_terminals", "get_active_terminal", "select_connect_config",
      "execute_terminal_command", "get_terminal_output"]),
    ("命令-execute_command", "请执行命令 uname -a",
     ["execute_command"]),
    ("命令-run_commands", "请运行命令 whoami",
     ["run_commands", "execute_command"]),
    ("读文件", "请读取 /etc/hostname 的内容",
     ["remote_read", "read_cloud_space_file"]),
    ("写文件", "请把 hello 写入 /tmp/orcabridge_probe.txt",
     ["remote_write", "create_file"]),
    ("搜索", "请在 /var/log 目录下搜索包含 error 的行",
     ["remote_grep", "remote_glob"]),
    ("列目录", "请列出 /tmp 目录下有哪些文件",
     ["remote_glob", "read_cloud_space_file_list"]),
    ("抓取网页", "请抓取 https://example.com 的内容",
     ["fetch"]),
    # 下面几个带上终端链路相关的工具，避免上游先要 list_terminals 而 502，
    # 目的是把 remote_write / remote_edit / remote_glob 的真实 args 打出来。
    ("写文件(带链路)", "请把 hello 这行文本写入 /tmp/orcabridge_probe.txt，不要做别的",
     ["remote_write", "list_terminals", "get_active_terminal",
      "select_connect_config", "execute_terminal_command"]),
    ("改文件(带链路)", "请把 /tmp/orcabridge_probe.txt 里的 hello 改成 world",
     ["remote_edit", "remote_read", "list_terminals", "get_active_terminal",
      "select_connect_config", "execute_terminal_command"]),
    ("列目录(带链路)", "请列出 /tmp 目录下有哪些文件",
     ["remote_glob", "list_terminals", "get_active_terminal",
      "select_connect_config", "execute_terminal_command"]),
]


def post(path, payload, timeout=240):
    request = urllib.request.Request(
        BASE + path, data=json.dumps(payload, ensure_ascii=False).encode(), method="POST")
    request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, response.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as error:  # noqa: F821
        return error.code, error.read().decode("utf-8", "replace")


def get(path, timeout=30):
    with urllib.request.urlopen(BASE + path, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8", "replace"))


def tool_of(debug):
    """从 /debug/last 的 raw 里取 data.tool。raw 是字符串化的 JSON。"""
    raw = debug.get("raw")
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except json.JSONDecodeError:
            return None
    if not isinstance(raw, dict):
        return None
    data = raw.get("data") or {}
    return data.get("tool")


selected = sys.argv[2:]
print(f"采样目标: {BASE}  （共 {len(SCENARIOS)} 个场景）\n")
for index, (label, prompt, tools) in enumerate(SCENARIOS, 1):
    if selected and str(index) not in selected:
        continue
    print(f"--- [{index}] {label} ---")
    print(f"    指令: {prompt}")
    print(f"    声明: {', '.join(tools)}")
    status, raw = post("/v1/chat/completions", {
        "model": "deepseek-v4-flash",
        "messages": [{"role": "user", "content": prompt}],
        "tools": [{"type": "function",
                   "function": {"name": n, "description": n + " tool",
                                "parameters": {"type": "object", "properties": {}}}}
                  for n in tools],
    })
    if status != 200:
        print(f"    HTTP {status}: {raw[:300]}")
        continue
    try:
        body = json.loads(raw)
        calls = ((body.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") or []
    except json.JSONDecodeError:
        calls = []
    if not calls:
        answer = ""
        try:
            answer = (json.loads(raw)["choices"][0]["message"].get("content") or "")[:160]
        except (KeyError, IndexError, TypeError, json.JSONDecodeError):
            pass
        print("    本轮未发起工具调用（模型行为）", answer)
        print()
        continue
    for call in calls:
        print(f"    -> 代理发出: {call['function']['name']}"
              f"({call['function']['arguments']})")
    probe = tool_of(get("/debug/last"))
    if probe:
        print(f"    <= 上游下发: name={probe.get('name')} type={probe.get('type')} "
              f"mcpServer={probe.get('mcpServer')}")
        print(f"       args={json.dumps(probe.get('args'), ensure_ascii=False)}")
        if probe.get("description"):
            print(f"       desc={probe['description'][:120]}")
    else:
        print("    （/debug/last 里没有 tool 字段）")
    print()
