import json
import urllib3
import requests

urllib3.disable_warnings()

with open('/opt/gptGrok2api/scripts/mailcom_domains_clean.txt', 'r', encoding='utf-8') as f:
    domains = [ln.strip() for ln in f if ln.strip()]

r = requests.get('http://127.0.0.1:3000/api/register', headers={"Authorization": "Bearer Lfz5201314"}, timeout=30, verify=False)
reg = r.json().get('register') or {}
provs = (reg.get('mail') or {}).get('providers') or []
for p in provs:
    if isinstance(p, dict) and p.get('type') == 'mailcom_mother':
        p['domains'] = domains
        p.pop('accounts_count', None)
        p.pop('accounts_preview', None)
        break

rr = requests.post('http://127.0.0.1:3000/api/register',
                   headers={"Authorization": "Bearer Lfz5201314", "Content-Type": "application/json"},
                   json={"mail": {"providers": provs}}, timeout=30, verify=False)
print("POST:", rr.status_code)
out = rr.json().get('register') or {}
for p in (out.get('mail') or {}).get('providers') or []:
    if p.get('type') == 'mailcom_mother':
        doms = p.get('domains') or []
        print("mailcom_mother domains now:", len(doms), "first5:", doms[:5])
