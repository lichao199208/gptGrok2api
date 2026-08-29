import json
d = json.load(open('/opt/gptGrok2api/data/register.json'))
provs = (d.get('mail') or {}).get('providers') or []
for i, p in enumerate(provs):
    t = p.get('type')
    accounts = p.get('accounts') or ''
    lines = [ln for ln in accounts.splitlines() if ln.strip()]
    print(f"[{i}] {t} enable={p.get('enable')} id={p.get('id')} accounts_lines={len(lines)} pool_batch={p.get('pool_batch')}")
