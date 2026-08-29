import json
import urllib3
import requests

urllib3.disable_warnings()

r = requests.get('http://127.0.0.1:3000/api/register', headers={"Authorization": "Bearer Lfz5201314"}, timeout=30, verify=False)
reg = r.json().get('register') or {}
for p in (reg.get('mail') or {}).get('providers') or []:
    if p.get('type') == 'mailcom_mother':
        doms = p.get('domains') or []
        print("mailcom_mother domains:", len(doms))
        print("  sample:", doms[0:6], "...", doms[-4:])
        print("  enabled:", p.get('enable'), "accounts_count:", p.get('accounts_count'))
        break
print("register total:", reg.get('total'), "enabled:", reg.get('enabled'))
