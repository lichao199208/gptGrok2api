#!/usr/bin/env python3
"""Add proxies list to mailcom_mother entry in register.json."""
import json

REGISTER = "/opt/gptGrok2api/data/register.json"
with open(REGISTER, "r", encoding="utf-8") as f:
    config = json.load(f)

mail = config.get("mail") or {}
for provider in mail.get("providers") or []:
    if isinstance(provider, dict) and provider.get("type") == "mailcom_mother":
        provider["proxy"] = "http://privoxy:8118"
        provider["proxies"] = [
            "http://privoxy:8118",
            "http://root:lichao@64.83.18.19:7890",
        ]
        print("proxies set for", provider.get("id"))
        break

with open(REGISTER, "w", encoding="utf-8") as f:
    json.dump(config, f, ensure_ascii=False, indent=2)
print("done")
