import re

lines = []
with open('mailcom_domains.txt', 'r', encoding='utf-8') as f:
    for ln in f:
        d = ln.strip().lstrip('@').strip().lower()
        if not d:
            continue
        # domain validation: letters/digits/hyphens + dots
        if re.fullmatch(r'[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*', d):
            lines.append(d)
        else:
            print("SKIP invalid:", repr(ln.strip()))

unique = []
seen = set()
for d in lines:
    if d not in seen:
        seen.add(d)
        unique.append(d)
print("total unique domains:", len(unique))
with open('mailcom_domains_clean.txt', 'w', encoding='utf-8') as f:
    f.write('\n'.join(unique) + '\n')
print("first 10:", unique[:10])
print("last 5:", unique[-5:])
