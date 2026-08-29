#!/usr/bin/env python3
PATH = "/opt/gptGrok2api/web-vue/src/api/register.ts"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

anchor = "  mode?: 'graph' | 'imap' | 'auto' | string\n  imap_host?: string\n  message_limit?: number"
new = "  mode?: 'graph' | 'imap' | 'auto' | string\n  imap_host?: string\n  message_limit?: number\n  accounts?: string\n  accounts_count?: number\n  accounts_preview?: string[]\n  domains?: string[]\n  max_active?: number\n  proxy?: string"

if "accounts_count?: number" in src:
    print("already present")
elif anchor in src:
    src = src.replace(anchor, new, 1)
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    print("type fields added")
else:
    print("anchor not found")
