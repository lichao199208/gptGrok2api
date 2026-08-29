#!/usr/bin/env python3
"""Fix BOM in accounts file + parser, re-import register.json."""
import json

# 1. rewrite mothers file without BOM
src_file = "/opt/gptGrok2api/scripts/mailcom_mothers.txt"
with open(src_file, "rb") as f:
    raw = f.read()
text = raw.decode("utf-8-sig")
lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
with open(src_file, "w", encoding="utf-8") as f:
    f.write("\n".join(lines) + "\n")
print("mothers file rewritten:", len(lines), "lines")

# 2. fix parser: strip BOM/zero-width from email in mail_provider.py
PATH = "/opt/gptGrok2api/services/register/mail_provider.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

old = '        email = str(email or "").strip()\n        password = str(password or "").strip()\n        if not email or not password or "@" not in email:\n            continue'
new = '        email = str(email or "").strip().lstrip("\\ufeff").strip()\n        password = str(password or "").strip()\n        if not email or not password or "@" not in email:\n            continue'
if old in src:
    src = src.replace(old, new, 1)
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    import py_compile
    py_compile.compile(PATH, doraise=True)
    print("parser BOM fix applied")
else:
    print("parser anchor not found")

# 3. re-import mothers into register.json (fresh entry, BOM-free)
REGISTER = "/opt/gptGrok2api/data/register.json"


def _load_domains() -> list[str]:
    path = "/opt/gptGrok2api/scripts/mailcom_domains_clean.txt"
    try:
        with open(path, "r", encoding="utf-8") as f:
            return [ln.strip() for ln in f if ln.strip()]
    except Exception:
        return ["mail.com", "europe.com", "email.com", "usa.com", "dr.com", "clubmember.org",
                "salesperson.net", "housemail.com", "worker.com", "humanoid.net"]
with open(REGISTER, "r", encoding="utf-8") as f:
    config = json.load(f)
with open(src_file, "r", encoding="utf-8") as f:
    accounts_text = f.read().strip()

mail = config.get("mail")
providers = [p for p in mail.get("providers") or [] if not (isinstance(p, dict) and p.get("type") == "mailcom_mother")]
for p in providers:
    if isinstance(p, dict) and p.get("type") == "mailcom_mother":
        continue
entry = {
    "id": "mailcom_mother-ew6cw2ehufbm",
    "enable": True,
    "type": "mailcom_mother",
    "label": "Mail.com母号",
    "accounts": accounts_text,
    "imap_host": "imap.mail.com",
    "domains": _load_domains(),
    "max_active": 9,
    "proxy": "http://privoxy:8118",
}
providers.insert(0, entry)
mail["providers"] = providers
with open(REGISTER, "w", encoding="utf-8") as f:
    json.dump(config, f, ensure_ascii=False, indent=2)
first_line = accounts_text.splitlines()[0]
print("register.json updated. first account:", repr(first_line[:40]))
