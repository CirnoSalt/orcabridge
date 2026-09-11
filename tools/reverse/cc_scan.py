#!/usr/bin/env python
# 从 V8 Code Cache 中按字节搜索关键字并打印上下文
import sys, os, re

CACHE = os.path.join(os.environ["LOCALAPPDATA"], "com.orcaterm-desktop.app",
                     "EBWebView", "Default", "Code Cache", "js")

KEYS = sys.argv[1:] or ["approvedTools", "tool_result", "toolCallId", "uiServers", "selectedUITools"]

def printable(b):
    # 保留 ASCII 可打印与 UTF-8 中文
    return b.decode("utf-8", "replace")

for fn in sorted(os.listdir(CACHE)):
    path = os.path.join(CACHE, fn)
    if not os.path.isfile(path):
        continue
    data = open(path, "rb").read()
    hits = {}
    for k in KEYS:
        kb = k.encode()
        idxs = []
        i = -1
        while True:
            i = data.find(kb, i + 1)
            if i < 0:
                break
            idxs.append(i)
        if idxs:
            hits[k] = idxs
    if not hits:
        continue
    print("=" * 78)
    print(f"### {fn}  (size={len(data)})")
    for k, idxs in hits.items():
        print(f"\n----- [{k}] x{len(idxs)} -----")
        for i in idxs[:8]:
            lo = max(0, i - 900)
            hi = min(len(data), i + 1200)
            txt = printable(data[lo:hi])
            # 只保留可打印密度较高的行
            txt = re.sub(r'[^\x09\x0a\x0d\x20-\x7e\u2e80-\uffff]', '·', txt)
            print(f"\n<<< off={i} ({fn}) >>>")
            print(txt)
