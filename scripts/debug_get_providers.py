import json
import urllib3
import requests

urllib3.disable_warnings()

r = requests.get('http://127.0.0.1:3000/api/register', headers={"Authorization": "Bearer Lfz5201314"}, timeout=30, verify=False)
reg = r.json().get('register') or {}
provs = (reg.get('mail') or {}).get('providers') or []
print("GET providers count:", len(provs))
for i, p in enumerate(provs):
    print(f"[{i}] type={p.get('type')} id={p.get('id')} enable={p.get('enable')} accounts_count={p.get('accounts_count')}")
