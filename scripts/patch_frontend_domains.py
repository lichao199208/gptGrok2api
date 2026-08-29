#!/usr/bin/env python3
"""Update defaultProvider mailcom_mother domains to the full 200-domain list."""
PATH = "/opt/gptGrok2api/web-vue/src/views/register/registerProviderView.ts"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

with open("/opt/gptGrok2api/scripts/mailcom_domains_ts.txt", "r", encoding="utf-8") as f:
    ts_domains = f.read().strip()

# find the defaultProvider mailcom_mother case
old_anchor = "        domains: ['mail.com', 'europe.com', 'email.com', 'usa.com', 'dr.com', 'clubmember.org', 'salesperson.net', 'housemail.com', 'worker.com', 'humanoid.net'],"
new_anchor = "        domains: [\n" + ts_domains + "\n        ],"

if old_anchor in src:
    src = src.replace(old_anchor, new_anchor, 1)
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    print("defaultProvider domains updated to full list")
elif "        domains: [" in src and "'mail.com', 'europe.com', 'email.com', 'usa.com', 'dr.com', 'clubmember.org', 'salesperson.net', 'housemail.com', 'worker.com', 'humanoid.net']" in src:
    print("need different anchor")
else:
    print("anchor not found")
