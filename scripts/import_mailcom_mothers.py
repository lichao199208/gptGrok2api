#!/usr/bin/env python3
"""Merge mailcom_mother provider entry into /opt/gptGrok2api/data/register.json."""
import json
import random
import string

REGISTER = "/opt/gptGrok2api/data/register.json"
MOTHERS = "/opt/gptGrok2api/scripts/mailcom_mothers.txt"

with open(REGISTER, "r", encoding="utf-8") as f:
    config = json.load(f)

with open(MOTHERS, "r", encoding="utf-8") as f:
    accounts_text = f.read().strip()

mail = config.get("mail")
if not isinstance(mail, dict):
    mail = {}
    config["mail"] = mail
providers = mail.get("providers")
if not isinstance(providers, list):
    providers = []
    mail["providers"] = providers

entry_id = "mailcom_mother-" + "".join(random.choices(string.ascii_lowercase + string.digits, k=12))
entry = {
    "id": entry_id,
    "enable": True,
    "type": "mailcom_mother",
    "label": "Mail.com母号",
    "accounts": accounts_text,
    "imap_host": "imap.mail.com",
    "domains": [
        "mail.com", "europe.com", "email.com", "usa.com", "dr.com", "clubmember.org",
        "salesperson.net", "housemail.com", "worker.com", "humanoid.net", "inorbit.com",
        "brew-meister.com", "toke.com", "hot-shot.com", "homemail.com", "reborn.com",
        "pacific-ocean.com", "deliveryman.com", "umpire.com", "computer4u.com", "webname.com",
        "e-mail.com", "email.net", "emailaccount.com", "emailengine.net", "emailengine.org",
    ],
    "max_active": 9,
    "proxy": "http://privoxy:8118",
}

# remove existing mailcom_mother entries
providers = [p for p in providers if not (isinstance(p, dict) and p.get("type") == "mailcom_mother")]
# put mailcom_mother first
providers.insert(0, entry)
mail["providers"] = providers

with open(REGISTER, "w", encoding="utf-8") as f:
    json.dump(config, f, ensure_ascii=False, indent=2)

print("providers now:", [(p.get("type"), p.get("enable")) for p in providers])
print("mailcom accounts lines:", len(accounts_text.splitlines()))
