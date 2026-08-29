import json
import urllib3
import requests

urllib3.disable_warnings()

d = json.load(open('/opt/gptGrok2api/data/register.json'))
print('before: total=', d.get('total'), 'enabled=', d.get('enabled'), 'mode=', d.get('mode'))

# restore total=10 (user's last known setting), keep enabled=False
d['total'] = 10
with open('/opt/gptGrok2api/data/register.json', 'w', encoding='utf-8') as f:
    json.dump(d, f, ensure_ascii=False, indent=2)
print('after: total=', d.get('total'))

# confirm via API
r = requests.get('http://127.0.0.1:3000/api/register', headers={"Authorization": "Bearer Lfz5201314"}, timeout=30, verify=False)
reg = r.json().get('register') or {}
print('api total=', reg.get('total'), 'enabled=', reg.get('enabled'))
