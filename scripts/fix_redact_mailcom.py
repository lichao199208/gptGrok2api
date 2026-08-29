#!/usr/bin/env python3
"""Insert mailcom_mother redaction into _redact_outlook_pools."""
import py_compile

PATH = "/opt/gptGrok2api/services/register_service.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

anchor = '''                for item in mail_provider.outlook_token_pool_failures(
                    credentials,
                    since=batch_started_at,
                )
            ]

    def _drop_mail_proxy(self) -> None:'''
replacement = '''                for item in mail_provider.outlook_token_pool_failures(
                    credentials,
                    since=batch_started_at,
                )
            ]
        for index, provider in enumerate(providers):
            if not isinstance(provider, dict) or provider.get("type") != "mailcom_mother":
                continue
            provider_id = _provider_id(provider) or f"mailcom-{index}"
            provider["id"] = provider_id
            accounts = mail_provider.parse_mailcom_mother_accounts(str(provider.get("accounts") or ""))
            provider["accounts"] = ""
            provider["accounts_count"] = len(accounts)
            provider["accounts_preview"] = [self._mask_email(item["email"]) for item in accounts]

    def _drop_mail_proxy(self) -> None:'''

if "mailcom-{index}" in src:
    print("already present")
elif anchor in src:
    src = src.replace(anchor, replacement, 1)
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    py_compile.compile(PATH, doraise=True)
    print("redaction added + syntax ok")
else:
    print("anchor not found")
