import json
import urllib3
import requests

urllib3.disable_warnings()

r = requests.get('http://127.0.0.1:3000/api/register', headers={"Authorization": "Bearer Lfz5201314"}, timeout=30, verify=False)
reg = r.json().get('register') or {}
provs = (reg.get('mail') or {}).get('providers') or []

# keep only the first mailcom_mother (id mailcom_mother-ew6cw2ehufbm), drop duplicates
seen = set()
kept = []
for p in provs:
    if isinstance(p, dict) and p.get('type') == 'mailcom_mother':
        pid = p.get('id') or ''
        if pid in seen:
            print("dropping duplicate:", pid)
            continue
        seen.add(pid)
    kept.append(p)

rr = requests.post('http://127.0.0.1:3000/api/register',
                   headers={"Authorization": "Bearer Lfz5201314", "Content-Type": "application/json"},
                   json={"mail": {"providers": kept}}, timeout=30, verify=False)
print("POST:", rr.status_code)
out = rr.json().get('register') or {}
mc = [p for p in (out.get('mail') or {}).get('providers') or [] if p.get('type') == 'mailcom_mother']
print("mailcom_mother entries now:", len(mc))
for p in mc:
    print("  id:", p.get('id'), "accounts_count:", p.get('accounts_count'), "proxies:", p.get('proxies'))
