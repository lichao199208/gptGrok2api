import json
import urllib3
import requests

urllib3.disable_warnings()

r = requests.post('http://127.0.0.1:3000/api/register',
                  headers={"Authorization": "Bearer Lfz5201314", "Content-Type": "application/json"},
                  json={"total": 10, "mode": "total"},
                  timeout=30, verify=False)
print("POST:", r.status_code)
reg = r.json().get('register') or {}
print("api total=", reg.get('total'), "enabled=", reg.get('enabled'), "mode=", reg.get('mode'))
d = json.load(open('/opt/gptGrok2api/data/register.json'))
print("disk total=", d.get('total'))
