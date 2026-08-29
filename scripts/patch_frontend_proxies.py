#!/usr/bin/env python3
"""Frontend: add proxies/pool_batch for mailcom_mother."""
import json

# 1. RegisterProvider type
p1 = "/opt/gptGrok2api/web-vue/src/api/register.ts"
with open(p1, "r", encoding="utf-8") as f:
    src = f.read()
anchor = "  domains?: string[]\n  max_active?: number\n  proxy?: string"
new = "  domains?: string[]\n  max_active?: number\n  pool_batch?: number\n  proxy?: string\n  proxies?: string[]"
if "pool_batch?: number" in src:
    print("type ok")
elif anchor in src:
    src = src.replace(anchor, new, 1)
    with open(p1, "w", encoding="utf-8") as f:
        f.write(src)
    print("type patched")
else:
    print("type anchor not found")

# 2. providerTypeKeys
p2 = "/opt/gptGrok2api/web-vue/src/views/register/registerProviderView.ts"
with open(p2, "r", encoding="utf-8") as f:
    src = f.read()
anchor2 = "  mailcom_mother: ['accounts', 'imap_host', 'domains', 'max_active', 'proxy'],"
new2 = "  mailcom_mother: ['accounts', 'imap_host', 'domains', 'max_active', 'pool_batch', 'proxy', 'proxies'],"
if "pool_batch" in src:
    print("keys ok")
elif anchor2 in src:
    src = src.replace(anchor2, new2, 1)
    with open(p2, "w", encoding="utf-8") as f:
        f.write(src)
    print("keys patched")
else:
    print("keys anchor not found")

# 3. defaultProvider case
anchor3 = "        domains: ['mail.com', 'europe.com', 'email.com', 'usa.com', 'dr.com', 'clubmember.org', 'salesperson.net', 'housemail.com', 'worker.com', 'humanoid.net'],\n        max_active: 9,\n        proxy: 'http://privoxy:8118',"
new3 = "        domains: ['mail.com', 'europe.com', 'email.com', 'usa.com', 'dr.com', 'clubmember.org', 'salesperson.net', 'housemail.com', 'worker.com', 'humanoid.net'],\n        max_active: 9,\n        pool_batch: 3,\n        proxy: 'http://privoxy:8118',\n        proxies: ['http://privoxy:8118'],"
if "pool_batch: 3" in src:
    print("default ok")
elif anchor3 in src:
    src = src.replace(anchor3, new3, 1)
    with open(p2, "w", encoding="utf-8") as f:
        f.write(src)
    print("default patched")
else:
    print("default anchor not found")
