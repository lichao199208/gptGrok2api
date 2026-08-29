import json
import urllib3
import requests

urllib3.disable_warnings()

r = requests.get('http://127.0.0.1:3000/api/register', headers={"Authorization": "Bearer Lfz5201314"}, timeout=30, verify=False)
reg = r.json().get('register') or {}
provs = (reg.get('mail') or {}).get('providers') or []
for p in provs:
    if p.get('type') == 'mailcom_mother':
        print("mailcom_mother:")
        print("  enabled:", p.get('enable'))
        print("  proxy:", p.get('proxy'))
        print("  proxies:", p.get('proxies'))
        print("  accounts_count:", p.get('accounts_count'))
        print("  pool_batch:", p.get('pool_batch'), "max_active:", p.get('max_active'))
        print("  accounts redacted:", repr(p.get('accounts')) == "''")
        break
print("register enabled:", reg.get('enabled'), "total:", reg.get('total'))
