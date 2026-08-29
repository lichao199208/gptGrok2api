import json
import urllib3
import requests

urllib3.disable_warnings()

# fetch current, set pool_batch on mailcom_mother provider, push back
r = requests.get('http://127.0.0.1:3000/api/register', headers={"Authorization": "Bearer Lfz5201314"}, timeout=30, verify=False)
reg = r.json().get('register') or {}
provs = (reg.get('mail') or {}).get('providers') or []
changed = False
for p in provs:
    if isinstance(p, dict) and p.get('type') == 'mailcom_mother':
        if p.get('pool_batch') is None:
            p['pool_batch'] = 3
            changed = True
        p.pop('accounts_count', None)
        p.pop('accounts_preview', None)
        break
if changed:
    rr = requests.post('http://127.0.0.1:3000/api/register',
                       headers={"Authorization": "Bearer Lfz5201314", "Content-Type": "application/json"},
                       json={"mail": {"providers": provs}}, timeout=30, verify=False)
    print("POST:", rr.status_code)
    out = rr.json().get('register') or {}
    for p in (out.get('mail') or {}).get('providers') or []:
        if p.get('type') == 'mailcom_mother':
            print("pool_batch now:", p.get('pool_batch'), "accounts_count:", p.get('accounts_count'))
else:
    print("no change needed")
