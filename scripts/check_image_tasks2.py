import json
import os

d = json.load(open("/opt/gptGrok2api/data/image_tasks.json", encoding="utf-8"))
tasks = d.get("tasks") or []
print("tasks:", len(tasks))
for item in tasks[-5:]:
    print("---")
    print(json.dumps({k: item.get(k) for k in ("id", "prompt", "model", "status", "error", "created_at", "updated_at", "account_email", "provider", "size", "n")}, ensure_ascii=False)[:500])

idx = json.load(open("/opt/gptGrok2api/data/image_index.json", encoding="utf-8"))
items = idx.get("items") or []
print("\nindex items:", len(items))
for item in items[-5:]:
    print("---")
    print(json.dumps(item, ensure_ascii=False)[:400])
