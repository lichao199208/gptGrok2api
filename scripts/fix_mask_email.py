#!/usr/bin/env python3
import py_compile

PATH = "/opt/gptGrok2api/services/register/mail_provider.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

old_call = "        masked = _mask_email(email)"
new_call = "        masked = _mailcom_mask_email(email)"
if old_call in src:
    src = src.replace(old_call, new_call, 1)
    print("replaced _mask_email call")
else:
    print("call not found")

helper = '''

def _mailcom_mask_email(email: str) -> str:
    local, sep, domain = str(email or "").partition("@")
    if not sep:
        return "***"
    masked = (local[:2] + "***" + local[-1:]) if len(local) > 2 else (local[:1] + "***")
    return f"{masked}@{domain}"
'''
if "def _mailcom_mask_email" not in src:
    src = src.replace("def _mailcom_next_account(accounts: list[dict[str, str]]) -> dict[str, str]:",
                      "def _mailcom_mask_email(email: str) -> str:\n    local, sep, domain = str(email or '').partition('@')\n    if not sep:\n        return '***'\n    masked = (local[:2] + '***' + local[-1:]) if len(local) > 2 else (local[:1] + '***')\n    return f'{masked}@{domain}'\n\n\ndef _mailcom_next_account(accounts: list[dict[str, str]]) -> dict[str, str]:", 1)
    print("added helper")

with open(PATH, "w", encoding="utf-8") as f:
    f.write(src)
py_compile.compile(PATH, doraise=True)
print("OK")
