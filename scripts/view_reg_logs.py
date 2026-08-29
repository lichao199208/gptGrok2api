import json

with open('/opt/gptGrok2api/data/register.json', 'r', encoding='utf-8') as f:
    cfg = json.load(f)

logs = cfg.get('logs') or []
# print the last registration attempt sequence
print("total logs:", len(logs))
for log in logs[-30:]:
    print(f"[{log.get('time')}] {log.get('level')} {str(log.get('text'))[:250]}")
