#!/usr/bin/env python3
"""Extend register_service.py: redact + merge mailcom_mother accounts."""
import py_compile

PATH = "/opt/gptGrok2api/services/register_service.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

original = src

# ---- 1. module-level helpers after _merge_outlook_pool / _outlook_credential_changed ----
helper_anchor = "def _outlook_credential_changed(old: dict | None, new: dict) -> bool:"
helper_code = '''def _serialize_mailcom_accounts(accounts: list[dict]) -> str:
    return "\\n".join(
        f"{str(item.get('email') or '').strip()}----{str(item.get('password') or '').strip()}"
        for item in accounts
        if str(item.get("email") or "").strip() and str(item.get("password") or "").strip()
    )


def _merge_mailcom_pool(old_text: str, new_text: str) -> str:
    """合并 mail.com 母号池：按邮箱去重，新导入的同名邮箱覆盖旧密码。"""
    merged: dict[str, dict] = {}
    for credential in mail_provider.parse_mailcom_mother_accounts(old_text or ""):
        merged[credential["email"].strip().lower()] = credential
    for credential in mail_provider.parse_mailcom_mother_accounts(new_text or ""):
        merged[credential["email"].strip().lower()] = credential
    return _serialize_mailcom_accounts(list(merged.values()))


def _mailcom_credential_changed(old: dict | None, new: dict) -> bool:
    if not old:
        return True
    return str(old.get("password") or "") != str(new.get("password") or "")


'''
if "def _serialize_mailcom_accounts" not in src:
    src = src.replace(helper_anchor, helper_code + helper_anchor, 1)
    print("added mailcom helpers")
else:
    print("mailcom helpers already present")

# ---- 2. _redact_outlook_pools -> also redact mailcom_mother accounts ----
redact_anchor = '''        for index, provider in enumerate(providers):
            if not isinstance(provider, dict) or provider.get("type") != "outlook_token":
                continue
            provider_id = _provider_id(provider) or f"outlook-{index}"
            provider["id"] = provider_id
            pool_text = str(provider.get("mailboxes") or "")'''
redact_new = '''        for index, provider in enumerate(providers):
            if not isinstance(provider, dict) or provider.get("type") != "outlook_token":
                continue
            provider_id = _provider_id(provider) or f"outlook-{index}"
            provider["id"] = provider_id
            pool_text = str(provider.get("mailboxes") or "")'''
if redact_anchor in src:
    # insert mailcom redaction right after the outlook loop (after the for loop body)
    redact_loop_end = '''                    updated_at: item.get("updated_at") or "",
                }
                for item in mail_provider.outlook_token_pool_failures(
                    credentials,
                    since=batch_started_at,
                )
            ]
'''
    mailcom_redact = '''
        for index, provider in enumerate(providers):
            if not isinstance(provider, dict) or provider.get("type") != "mailcom_mother":
                continue
            provider_id = _provider_id(provider) or f"mailcom-{index}"
            provider["id"] = provider_id
            accounts = mail_provider.parse_mailcom_mother_accounts(str(provider.get("accounts") or ""))
            provider["accounts"] = ""
            provider["accounts_count"] = len(accounts)
            provider["accounts_preview"] = [self._mask_email(item["email"]) for item in accounts]
'''
    if "accounts_count" not in src:
        src = src.replace(redact_loop_end, redact_loop_end + mailcom_redact, 1)
        print("added mailcom redaction")
    else:
        print("mailcom redaction already present")
else:
    print("redact anchor not found")

# ---- 3. _merge_outlook_pools -> also merge mailcom_mother accounts ----
# extend the merge function: after the outlook loop, add mailcom loop
merge_anchor = '''            for key in ("mailboxes_count", "mailboxes_base_count", "mailboxes_alias_count", "mailboxes_preview", "mailboxes_stats", "mailboxes_parse_stats", "mailboxes_failed"):
                provider.pop(key, None)

    def _prune_unused_outlook_pools(self) -> int:'''
merge_new = '''            for key in ("mailboxes_count", "mailboxes_base_count", "mailboxes_alias_count", "mailboxes_preview", "mailboxes_stats", "mailboxes_parse_stats", "mailboxes_failed"):
                provider.pop(key, None)
        # mail.com 母号池：同样按邮箱合并去重（accounts 为只写导入框）
        old_mailcom_by_id = {
            _provider_id(provider): provider
            for provider in old_providers
            if isinstance(provider, dict) and provider.get("type") == "mailcom_mother" and _provider_id(provider)
        }
        old_mailcom_by_order = [
            provider
            for provider in old_providers
            if isinstance(provider, dict) and provider.get("type") == "mailcom_mother"
        ]
        mailcom_index = 0
        for index, provider in enumerate(mail["providers"]):
            if not isinstance(provider, dict):
                continue
            if provider.get("type") != "mailcom_mother":
                continue
            provider_id = _provider_id(provider)
            old = old_mailcom_by_id.get(provider_id) or {}
            if not old and index < len(old_providers) and isinstance(old_providers[index], dict) and old_providers[index].get("type") == "mailcom_mother":
                old = old_providers[index]
            if not old and mailcom_index < len(old_mailcom_by_order):
                old = old_mailcom_by_order[mailcom_index]
            mailcom_index += 1
            old_text = str(old.get("accounts") or "") if old.get("type") == "mailcom_mother" else ""
            new_text = str(provider.get("accounts") or "")
            if new_text.strip():
                provider["accounts"] = _merge_mailcom_pool(old_text, new_text)
            elif old_text:
                provider["accounts"] = _merge_mailcom_pool(old_text, "")
            else:
                provider["accounts"] = ""
            for key in ("accounts_count", "accounts_preview"):
                provider.pop(key, None)

    def _prune_unused_outlook_pools(self) -> int:'''
if "old_mailcom_by_id" not in src:
    if merge_anchor in src:
        src = src.replace(merge_anchor, merge_new, 1)
        print("added mailcom merge")
    else:
        print("merge anchor not found")
else:
    print("mailcom merge already present")

if src == original:
    print("NO CHANGES")
else:
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    py_compile.compile(PATH, doraise=True)
    print("PATCH OK + syntax valid")
