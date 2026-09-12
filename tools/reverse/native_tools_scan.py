#!/usr/bin/env python
"""扫描客户端 bundle，导出 OrcaTerm 原生工具名候选（刷新 nativeToolNames 用）。

背景：上游原生工具表由**服务端**下发，客户端要么不落盘、要么只在 V8 code cache 里留字符串。
所以代理里的 allowlist 只能是一个快照 —— 这个脚本让"刷新快照"变成一条命令，
而不是靠记忆去挑名字。

做法：取客户端 WebView 的 Code Cache，抽 ASCII 串，按"动词_名词"模式收集候选，
再按一份**明确的排除规则**去掉 DOM/事件/UI 内部名。

用法：
    .venv-cdp/Scripts/python.exe tools/reverse/native_tools_scan.py            # 打印候选
    .venv-cdp/Scripts/python.exe tools/reverse/native_tools_scan.py --go       # 打印可粘贴的 Go 条目
"""
import glob
import os
import re
import sys

CACHE = os.path.join(
    os.environ.get("LOCALAPPDATA", ""),
    "com.orcaterm-desktop.app", "EBWebView", "Default", "Code Cache", "js",
)

# 工具名形如 verb_noun；动词表覆盖客户端里出现过的全部前缀。
VERBS = (
    "list get query read write create delete update execute run submit remote "
    "search select switch connect attach detach start stop apply check send open close"
).split()

CANDIDATE = re.compile(r"\b(?:" + "|".join(VERBS) + r")_[a-z0-9_]{2,}\b")

# 明确不是工具的名字，附理由，避免下次有人"顺手加回去"。
EXCLUDE = {
    # 前端组件/状态机内部标识
    "list__group", "list__item", "list__label", "list__separate", "list__status",
    "list__submenu", "list__task", "list_item", "search__inner", "check__label",
    # 事件/埋点名
    "check_failed", "check_timeout", "create_command_fail", "create_command_success",
    "create_group_success_in_whitelist", "create_group_success_not_in_whitelist",
    "execute_input", "execute_result", "query_version_error",
    "delete_local_combine", "delete_remote_combine",
    # 窗口 / 标签页 / 面板
    "create_webview_window", "get_tab_pages", "get_active_tab_page", "get_all_windows",
    "connect_modal", "connect_config_id", "connect_config_name", "connect_list",
    # 安装文档跳转
    "check_install", "check_tat_install_doc",
    # 流式解析/渲染细节
    "get_offset", "get_offseta", "get_options", "get_optionsa", "get_rolea",
    "get_session_rolea", "get_details", "get_detailsaif", "get_messages",
    "get_payload", "get_buffer_size", "get_trailing_bytes", "read_before",
}


def collect():
    if not os.path.isdir(CACHE):
        print(f"缓存目录不存在: {CACHE}", file=sys.stderr)
        print("请先启动 OrcaTerm 并进过一次 AI 会话。", file=sys.stderr)
        return []
    names = set()
    for path in glob.glob(os.path.join(CACHE, "*")):
        if not os.path.isfile(path):
            continue
        try:
            data = open(path, "rb").read()
        except OSError:
            continue
        for chunk in re.findall(rb"[ -~]{3,}", data):
            names.update(CANDIDATE.findall(chunk.decode("ascii", "replace")))
    return sorted(n for n in names if n not in EXCLUDE)


def main():
    names = collect()
    if not names:
        return 1
    if "--go" in sys.argv:
        print("\t// 由 native_tools_scan.py 扫描客户端 Code Cache 得到；")
        print("\t// 带 (实测) 标记的名字在真机请求里被上游实际请求过。")
        for name in names:
            print(f'\t"{name}": true,')
    else:
        print(f"候选原生工具名 {len(names)} 个（已排除 {len(EXCLUDE)} 个 UI/事件内部名）：")
        for name in names:
            print("  " + name)
        print()
        print("与 main.go 的 nativeToolNames 比对后手工增删；--go 输出可粘贴的 Go 条目。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
