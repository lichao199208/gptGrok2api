import json
import urllib3
import requests

urllib3.disable_warnings()

BASE = "http://127.0.0.1:3000"
KEY = "Lfz5201314"

r = requests.get(BASE + "/api/register", headers={"Authorization": "Bearer " + KEY}, timeout=30, verify=False)
print("status:", r.status_code)
data = r.json()
reg = data.get("register") if isinstance(data.get("register"), dict) else data
print("reg keys:", list(reg.keys())[:30])
mail = reg.get("mail") or {}
print("mail keys:", list(mail.keys())[:20])
provs = mail.get("providers") or []
print("providers count:", len(provs))
for p in provs:
    t = p.get("type")
    print(f"--- {t} enable={p.get('enable')} label={p.get('label')}")
    if t == "mailcom_mother":
        print("  id:", p.get("id"))
        print("  imap_host:", p.get("imap_host"))
        print("  max_active:", p.get("max_active"))
        print("  proxy:", p.get("proxy"))
        print("  accounts(raw):", repr(p.get("accounts"))[:100])
        print("  accounts_count:", p.get("accounts_count"))
        print("  accounts_preview:", p.get("accounts_preview"))
        doms = p.get("domains") or []
        print("  domains:", len(doms), doms[:6], "...")
