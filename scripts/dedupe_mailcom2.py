import json
import urllib3
import requests

urllib3.disable_warnings()

r = requests.get('http://127.0.0.1:3000/api/register', headers={"Authorization": "Bearer Lfz5201314"}, timeout=30, verify=False)
reg = r.json().get('register') or {}
provs = (reg.get('mail') or {}).get('providers') or []

kept = []
seen_mailcom = False
for p in provs:
    if isinstance(p, dict) and p.get('type') == 'mailcom_mother':
        if seen_mailcom:
            print("dropping duplicate mailcom_mother:", p.get('id'))
            continue
        seen_mailcom = True
    kept.append(p)

rr = requests.post('http://127.0.0.1:3000/api/register',
                   headers={"Authorization": "Bearer Lfz5201314", "Content-Type": "application/json"},
                   json={"mail": {"providers": kept}}, timeout=30, verify=False)
print("POST:", rr.status_code)
out = rr.json().get('register') or {}
mc = [p for p in (out.get('mail') or {}).get('providers') or [] if p.get('type') == 'mailcom_mother']
print("mailcom_mother entries now:", len(mc))
for p in mc:
    print("  id:", p.get('id'), "accounts_count:", p.get('accounts_count'), "pool_batch:", p.get('pool_batch'), "proxies:", p.get('proxies'))
