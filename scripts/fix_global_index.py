#!/usr/bin/env python3
import py_compile

PATH = "/opt/gptGrok2api/services/register/mail_provider.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

old = """    def _login_with_rotation(self, account: dict[str, str]) -> str:
        email = str(account.get("email") or "").strip()
        cached = _mailcom_cached_token(email)
        if cached:
            return cached
        if not self.proxies:
            raise MailComMotherError("mail.com 未配置任何出口代理（proxy / proxies 为空）")
        now = time.monotonic()
        with _mailcom_token_lock:
            start = _mailcom_proxy_index % len(self.proxies)"""
new = """    def _login_with_rotation(self, account: dict[str, str]) -> str:
        global _mailcom_proxy_index
        email = str(account.get("email") or "").strip()
        cached = _mailcom_cached_token(email)
        if cached:
            return cached
        if not self.proxies:
            raise MailComMotherError("mail.com 未配置任何出口代理（proxy / proxies 为空）")
        now = time.monotonic()
        with _mailcom_token_lock:
            start = _mailcom_proxy_index % len(self.proxies)"""

if old in src:
    src = src.replace(old, new, 1)
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    py_compile.compile(PATH, doraise=True)
    print("fixed + syntax ok")
else:
    print("anchor not found")
