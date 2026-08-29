with open('/opt/gptGrok2api/scripts/mailcom_domains_clean.txt', 'r', encoding='utf-8') as f:
    domains = [ln.strip() for ln in f if ln.strip()]

lines = []
for i in range(0, len(domains), 6):
    chunk = domains[i:i+6]
    lines.append("        " + ", ".join(f"'{d}'" for d in chunk) + ",")

ts = "\n".join(lines)
print(ts)
with open('/opt/gptGrok2api/scripts/mailcom_domains_ts.txt', 'w', encoding='utf-8') as f:
    f.write(ts)
print("---", len(domains), "domains")
