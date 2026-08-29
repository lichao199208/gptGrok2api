import sys
import json
import re
import base64
import urllib3
import requests
from urllib.parse import urlsplit, urlunsplit

urllib3.disable_warnings()

EMAIL = sys.argv[1] if len(sys.argv) > 1 else "ChunkyButtotm@mail.com"
PASSWORD = sys.argv[2] if len(sys.argv) > 2 else "WMNukRC0Zng"
PROXY = sys.argv[3] if len(sys.argv) > 3 else "http://127.0.0.1:40080"

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"

s = requests.Session()
s.verify = False
s.proxies = {"http": PROXY, "https": PROXY}
s.headers.update({"User-Agent": UA, "Accept-Language": "en-US,en;q=0.9"})


def log(name, msg):
    print(f"[{name}] {msg}")


s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
s.post("https://www.mail.com/consentpage", data={"redirectUrl": "https://www.mail.com/"}, timeout=30, allow_redirects=False)
s.get("https://www.mail.com/privacy?referrer=https%3A%2F%2Fwww.mail.com%2F", timeout=30, allow_redirects=True)
r_home = s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
home_text = r_home.text
m = re.search(r'name="statistics" value="([^"]+)"', home_text)
statistics = m.group(1) if m else ""
form_fields = {}
for field in ["ibaInfo", "service", "uasServiceID", "successURL", "loginFailedURL", "loginErrorURL", "edition", "lang", "usertype"]:
    mm = re.search(r'name="%s" value="([^"]*)"' % field, home_text)
    form_fields[field] = mm.group(1) if mm else ""
print("statistics len:", len(statistics))
data = dict(form_fields)
data["statistics"] = statistics
data["username"] = EMAIL
data["password"] = PASSWORD

r = s.post("https://login.mail.com/login", data=data, timeout=40, allow_redirects=False,
           headers={"Referer": "https://www.mail.com/", "Origin": "https://www.mail.com"})
loc = r.headers.get("Location") or ""
log("login", f"status={r.status_code} location={loc[:300]}")
log("login", f"set-cookie={r.headers.get('set-cookie', '')[:200]}")
log("login", f"body={r.text[:300]}")

if loc:
    # follow the redirect to see what page it is
    try:
        r2 = s.get(loc if loc.startswith("http") else "https://login.mail.com" + loc, timeout=40, allow_redirects=True)
        log("follow", f"status={r2.status_code} final={r2.url[:250]}")
        log("follow", f"title={re.search(r'<title>(.*?)</title>', r2.text, re.S).group(1)[:120] if '<title>' in r2.text else 'no-title'}")
        # look for error markers
        body = r2.text
        for marker in ("wrong", "invalid", "error", "verify", "verification", "captcha", "security", "account", "blocked", "suspended", "2FA", "two-factor", "禁", "验证"):
            idx = body.lower().find(marker.lower())
            if idx != -1:
                log("follow", f"marker '{marker}': ...{re.sub(chr(60)+'[^'+chr(62)+']*'+chr(62), ' ', body[max(0,idx-80):idx+160])[:220]}")
        log("follow", f"body_head={body[:200]!r}")
    except Exception as e:
        log("follow", f"ERR {type(e).__name__}: {e}")
