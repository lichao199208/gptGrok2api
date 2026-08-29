import json
import time
import urllib3
import requests

urllib3.disable_warnings()

BASE = "http://127.0.0.1:3000"
KEY = "Lfz5201314"
H = {"Authorization": "Bearer " + KEY, "Content-Type": "application/json"}


def get_config():
    return requests.get(BASE + "/api/register", headers={"Authorization": "Bearer " + KEY}, timeout=30, verify=False).json()["register"]


cfg = get_config()
print("before: total=%s enabled=%s" % (cfg.get("total"), cfg.get("enabled")))

requests.post(BASE + "/api/register", headers=H, json={"total": 1, "mode": "total"}, timeout=30, verify=False)
r = requests.post(BASE + "/api/register/start", headers=H, json={}, timeout=30, verify=False)
print("start:", r.status_code)

seen = set()
for i in range(120):
    time.sleep(2)
    cfg = get_config()
    stats = cfg.get("stats") or {}
    for log in cfg.get("logs") or []:
        text = str(log.get("text") or "")
        key = (log.get("time") or "", text[:80])
        if key in seen:
            continue
        seen.add(key)
        print(f"[{log.get('time')}] {log.get('level')} {text[:260]}")
    if stats.get("running") == 0 and stats.get("done") is not None and stats.get("done", 0) > 0:
        break
    if stats.get("running") == 0 and stats.get("finished_at"):
        break

print("=== FINAL ===")
stats = get_config().get("stats") or {}
print("success:", stats.get("success"), "fail:", stats.get("fail"), "done:", stats.get("done"))
requests.post(BASE + "/api/register/stop", headers=H, json={}, timeout=30, verify=False)
requests.post(BASE + "/api/register", headers=H, json={"total": 10, "mode": "total"}, timeout=30, verify=False)
print("restored total=10")
