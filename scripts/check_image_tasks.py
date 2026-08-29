import json
import os

# check image tasks
for name in ["image_tasks.json", "image_index.json"]:
    p = "/opt/gptGrok2api/data/" + name
    if os.path.exists(p):
        try:
            d = json.load(open(p, encoding="utf-8"))
            print(f"=== {name} ===")
            if isinstance(d, list):
                print("  entries:", len(d))
                for item in d[-3:]:
                    print("  ", json.dumps({k: item.get(k) for k in ("id", "prompt", "model", "status", "error", "created_at", "updated_at")}, ensure_ascii=False)[:400])
            elif isinstance(d, dict):
                keys = list(d.keys())[:10]
                print("  keys:", keys)
        except Exception as e:
            print(name, "ERR", e)
