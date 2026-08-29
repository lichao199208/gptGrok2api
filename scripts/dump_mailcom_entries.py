import json
d = json.load(open('/opt/gptGrok2api/data/register.json'))
provs = (d.get('mail') or {}).get('providers') or []
for i, p in enumerate(provs):
    if p.get('type') == 'mailcom_mother':
        print(f"=== [{i}] id={p.get('id')} ===")
        for k, v in p.items():
            if k == 'accounts':
                lines = [ln for ln in (v or '').splitlines() if ln.strip()]
                print(f"  accounts: {len(lines)} lines; first3={lines[:3]}")
            else:
                print(f"  {k}: {json.dumps(v, ensure_ascii=False)[:150]}")
