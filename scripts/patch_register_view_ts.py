#!/usr/bin/env python3
"""Patch web-vue registerProviderView.ts for mailcom_mother."""
import py_compile

PATH = "/opt/gptGrok2api/web-vue/src/views/register/registerProviderView.ts"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

original = src

# 1. providerTypeOptions
anchor = "  { value: 'outlook_token', label: 'Microsoft 邮箱凭据池' },\n]"
new = "  { value: 'outlook_token', label: 'Microsoft 邮箱凭据池' },\n  { value: 'mailcom_mother', label: 'Mail.com 母号（自动子号）' },\n]"
if "mailcom_mother" not in src:
    src = src.replace(anchor, new, 1)
    print("providerTypeOptions +")
else:
    print("providerTypeOptions exists")

# 2. providerTypeKeys
anchor2 = "  outlook_token: ['mailboxes', 'mode', 'imap_host', 'message_limit', 'alias_enabled', 'alias_per_email', 'alias_prefix', 'alias_include_original'],\n}"
new2 = "  outlook_token: ['mailboxes', 'mode', 'imap_host', 'message_limit', 'alias_enabled', 'alias_per_email', 'alias_prefix', 'alias_include_original'],\n  mailcom_mother: ['accounts', 'imap_host', 'domains', 'max_active', 'proxy'],\n}"
if "mailcom_mother: ['accounts'" not in src:
    src = src.replace(anchor2, new2, 1)
    print("providerTypeKeys +")
else:
    print("providerTypeKeys exists")

# 3. providerLocalOnlyKeys
anchor3 = "  outlook_token: ['mailboxes_count', 'mailboxes_base_count', 'mailboxes_alias_count', 'mailboxes_preview', 'mailboxes_stats', 'mailboxes_parse_stats', 'mailboxes_failed'],\n}"
new3 = "  outlook_token: ['mailboxes_count', 'mailboxes_base_count', 'mailboxes_alias_count', 'mailboxes_preview', 'mailboxes_stats', 'mailboxes_parse_stats', 'mailboxes_failed'],\n  mailcom_mother: ['accounts_count', 'accounts_preview'],\n}"
if "mailcom_mother: ['accounts_count'" not in src:
    src = src.replace(anchor3, new3, 1)
    print("providerLocalOnlyKeys +")
else:
    print("providerLocalOnlyKeys exists")

# 4. defaultProvider case: after outlook_token default
anchor4 = "    case 'outlook_token':\n      return {\n        ...base,\n        mailboxes: '',"
outlook_case_end = "        alias_include_original: true,\n      }"
new_case = """    case 'mailcom_mother':
      return {
        ...base,
        accounts: '',
        imap_host: 'imap.mail.com',
        domains: ['mail.com', 'europe.com', 'email.com', 'usa.com', 'dr.com', 'clubmember.org', 'salesperson.net', 'housemail.com', 'worker.com', 'humanoid.net'],
        max_active: 9,
        proxy: 'http://privoxy:8118',
      }
"""
if "case 'mailcom_mother':" not in src:
    # find the end of the outlook_token case: the closing "}\n    case " right after outlook default
    idx = src.find(anchor4)
    if idx != -1:
        end_idx = src.find("      }\n", idx)
        # ensure that's the outlook case end
        if end_idx != -1:
            insert_at = end_idx + len("      }\n")
            src = src[:insert_at] + new_case + src[insert_at:]
            print("defaultProvider case +")
        else:
            print("outlook case end not found")
    else:
        print("outlook default case not found")
else:
    print("defaultProvider case exists")

# 5. providerRequirementMessages case
anchor5 = "    case 'outlook_token': {"
req_new = """    case 'mailcom_mother': {
      const savedCount = Number(provider.accounts_count || 0)
      if (savedCount <= 0 && !isFilled(provider.accounts)) missing.push('Mail.com 母号池')
      break
    }
    case 'outlook_token': {"""
if "case 'mailcom_mother': {" not in src:
    if anchor5 in src:
        src = src.replace(anchor5, req_new, 1)
        print("requirementMessages +")
    else:
        print("outlook req case not found")
else:
    print("requirementMessages exists")

if src == original:
    print("NO CHANGES")
else:
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    print("PATCH OK")
