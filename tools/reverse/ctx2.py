#!/usr/bin/env python
# 提取关键字上下文，按 400 字符换行保存，便于阅读
import sys, os

path = sys.argv[1]
outdir = sys.argv[2]
keys = sys.argv[3:]
os.makedirs(outdir, exist_ok=True)
src = open(path, encoding="utf-8", errors="replace").read()

for k in keys:
    idxs = []
    i = -1
    while True:
        i = src.find(k, i + 1)
        if i < 0:
            break
        idxs.append(i)
    fn = os.path.join(outdir, "ctx_" + "".join(ch if ch.isalnum() or ch in "_-" else "_" for ch in k) + ".txt")
    with open(fn, "w", encoding="utf-8") as f:
        f.write(f"### [{k}] hits={len(idxs)} in {os.path.basename(path)}\n")
        for i in idxs[:8]:
            lo, hi = max(0, i - 2000), min(len(src), i + 3000)
            seg = src[lo:hi]
            wrapped = "\n".join(seg[j:j + 380] for j in range(0, len(seg), 380))
            f.write(f"\n===== @{i} =====\n{wrapped}\n")
    print(f"{k}: {len(idxs)} -> {fn}")
