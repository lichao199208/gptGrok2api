import json
import sys
import time
import urllib3
import requests

urllib3.disable_warnings()

BASE = "http://127.0.0.1:3000"
KEY = "Lfz5201314"
H = {"Authorization": "Bearer " + KEY, "Content-Type": "application/json"}


def get_config():
    return requests.get(BASE + "/api/register", headers={"Authorization": "Bearer " + KEY}, timeout=30, verify=False).json()["register"]


def post(path, body=None):
    return requests.post(BASE + path, headers=H, json=body or {}, timeout=30, verify=False)


cfg = get_config()
print("before: mode=%s total=%s enabled=%s target=%s" % (cfg.get("mode"), cfg.get("total"), cfg.get("enabled"), cfg.get("target")))

# set a tiny task
r = post("/api/register", {"total": 1, "mode": "total"})
print("set total=1:", r.status_code)
r = post("/api/register/start")
print("start:", r.status_code, r.text[:200])

# poll logs
seen = set()
for i in range(90):
    time.sleep(2)
    cfg = get_config()
    stats = cfg.get("stats") or {}
    logs = cfg.get("logs") or []
    for log in logs:
        text = str(log.get("text") or "")
        key = (log.get("time") or "", text[:60])
        if key in seen:
            continue
        seen.add(key)
        print(f"[{log.get('time')}] {log.get('level')} {text[:220]}")
    running = stats.get("running")
    done = stats.get("done")
    success = stats.get("success")
    fail = stats.get("fail")
    if i % 10 == 0:
        print(f"  ... poll {i}: running={running} done={done} success={success} fail={fail}")
    if running == 0 and done is not None and done > 0:
        break
    if running == 0 and done is not None and i > 3 and done == 0 and stats.get("started_at") and cfg.get("stats", {}).get("finished_at"):
        break

print("=== FINAL STATS ===")
stats = get_config().get("stats") or {}
for k in ("running", "done", "success", "fail", "avg_seconds", "success_rate", "current_available", "estimated_available"):
    print(f"  {k}: {stats.get(k)}")

# stop + restore
post("/api/register/stop")
r = post("/api/register", {"total": 100, "mode": "total"})
print("restored total=100:", r.status_code)
