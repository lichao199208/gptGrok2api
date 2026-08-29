import json
d = json.load(open('/opt/gptGrok2api/data/register.json'))
print('proxy:', d.get('proxy'))
print('api_use_register_proxy:', (d.get('mail') or {}).get('api_use_register_proxy'))
