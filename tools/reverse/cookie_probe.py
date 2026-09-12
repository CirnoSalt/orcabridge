#!/usr/bin/env python
"""诊断 OrcaTerm 登录态：.cookies 与 WebView cookie DB 里的 token 是否过期。

用途：代理报 upstream_auth_error / "Invalid or expired AccessToken" 时，
先跑这个脚本判断是"真的过期"还是"代理读错了凭据"。

用法：
    .venv-cdp/Scripts/python.exe tools/reverse/cookie_probe.py
"""
import base64
import datetime
import json
import os
import sqlite3
import time

BASE = os.path.join(os.environ["LOCALAPPDATA"], "com.orcaterm-desktop.app")
COOKIES = os.path.join(BASE, ".cookies")
WEBVIEW_DB = os.path.join(BASE, "EBWebView", "Default", "Network", "Cookies")
CHROMIUM_EPOCH = 11644473600  # 1601-01-01 -> 1970-01-01 秒差


def jwt_exp(token):
    """从 JWT 里取 exp（不校验签名，仅诊断）。"""
    try:
        seg = token.split(".")[1]
        seg += "=" * (-len(seg) % 4)
        return json.loads(base64.urlsafe_b64decode(seg)).get("exp")
    except Exception:  # noqa: BLE001
        return None


def describe(token, now):
    exp = jwt_exp(token)
    if exp is None:
        return "非 JWT 或无法解析"
    when = datetime.datetime.fromtimestamp(exp)
    if exp < now:
        return f"EXPIRED at {when}（{int((now - exp) / 60)} 分钟前）"
    return f"valid until {when}（剩 {int((exp - now) / 60)} 分钟）"


def from_cookies_file(now):
    print(f"=== {COOKIES}")
    if not os.path.exists(COOKIES):
        print("  文件不存在")
        return
    mtime = datetime.datetime.fromtimestamp(os.path.getmtime(COOKIES))
    print(f"  mtime: {mtime}")
    for item in json.load(open(COOKIES, encoding="utf-8")):
        raw = (item.get("raw_cookie") or "").split(";")[0].strip()
        if "=" not in raw:
            continue
        name, value = raw.split("=", 1)
        if name not in ("ot_session", "sid"):
            continue
        dom = item.get("domain") or {}
        host = dom.get("HostOnly") or dom.get("Suffix") or ""
        note = describe(value, now) if name == "ot_session" else f"len={len(value)}"
        print(f"  {name:<11} host={host:<32} {note}")


def from_webview(now):
    print(f"=== {WEBVIEW_DB}")
    if not os.path.exists(WEBVIEW_DB):
        print("  不存在")
        return
    try:
        uri = "file:///" + WEBVIEW_DB.replace("\\", "/") + "?mode=ro"
        con = sqlite3.connect(uri, uri=True, timeout=3)
    except sqlite3.Error as exc:
        print(f"  打开失败（可能被应用独占锁定）: {exc}")
        return
    rows = con.execute(
        "select host_key, name, value, expires_utc from cookies "
        "where name in ('ot_session','sid') order by expires_utc desc"
    ).fetchall()
    if not rows:
        print("  没有 ot_session / sid 记录")
    seen = set()
    for host, name, value, exp_utc in rows:
        if isinstance(value, bytes):
            value = value.decode("utf-8", "replace")
        exp = exp_utc / 1e6 - CHROMIUM_EPOCH if exp_utc else 0
        when = datetime.datetime.fromtimestamp(exp) if exp > 0 else None
        tag = "session cookie" if not when else ("EXPIRED" if when.timestamp() < now else "有效")
        if name == "ot_session":
            tag = describe(value, now)
        # 同一 host+name 只报最新一条
        if (host, name) in seen:
            continue
        seen.add((host, name))
        print(f"  {name:<11} host={host:<32} {tag}")


def main():
    now = time.time()
    print("now =", datetime.datetime.fromtimestamp(now))
    from_cookies_file(now)
    from_webview(now)
    print()
    print("提示：若两处 ot_session 都已过期，必须先在 OrcaTerm 里重新登录，代理无法自行续期。")


if __name__ == "__main__":
    main()
