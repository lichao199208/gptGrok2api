import sys
import json
import re
import base64
import urllib3
import requests
from urllib.parse import urlsplit, urlunsplit

urllib3.disable_warnings()

EMAIL = sys.argv[1] if len(sys.argv) > 1 else ""
PASSWORD = sys.argv[2] if len(sys.argv) > 2 else ""
PROXY = sys.argv[3] if len(sys.argv) > 3 else ""

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"

s = requests.Session()
s.verify = False
if PROXY:
    s.proxies = {"http": PROXY, "https": PROXY}
s.headers.update({"User-Agent": UA, "Accept-Language": "en-US,en;q=0.9"})


def basic(client_id):
    return "Basic " + base64.b64encode(f"{client_id}:*******".encode()).decode()


def login():
    s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
    s.post("https://www.mail.com/consentpage", data={"redirectUrl": "https://www.mail.com/"}, timeout=30, allow_redirects=False)
    s.get("https://www.mail.com/privacy?referrer=https%3A%2F%2Fwww.mail.com%2F", timeout=30, allow_redirects=True)
    r_home = s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
    home_text = r_home.text
    m = re.search(r'name="statistics" value="([^"]+)"', home_text)
    statistics = m.group(1) if m else ""
    form_fields = {}
    for field in ["ibaInfo", "service", "uasServiceID", "successURL", "loginFailedURL", "loginErrorURL", "edition", "lang", "usertype"]:
        m = re.search(r'name="%s" value="([^"]*)"' % field, home_text)
        form_fields[field] = m.group(1) if m else ""
    data = dict(form_fields)
    data["statistics"] = statistics
    data["username"] = EMAIL
    data["password"] = PASSWORD
    r = s.post("https://login.mail.com/login", data=data, timeout=40, allow_redirects=False,
               headers={"Referer": "https://www.mail.com/", "Origin": "https://www.mail.com"})
    loc = r.headers.get("Location") or ""
    if "ott=" not in loc:
        raise RuntimeError(f"login failed: {r.status_code} {loc[:200]}")
    parts = urlsplit(loc)
    halogin_url = urlunsplit((parts.scheme, parts.netloc, "/halogin", parts.query, parts.fragment)) + "&tz=0"
    r3 = s.get(halogin_url, timeout=40, allow_redirects=False)
    m = re.search(r"[?&]sid=([^&]+)", r3.headers.get("Location") or "")
    sid = m.group(1) if m else ""
    token_url = f"https://oauthbridge.{parts.netloc}/navigator/oauth2/token?sid={sid}"
    r4 = s.post(token_url,
                data={"grant_type": "urn:mam:oauth:grant-type:spa",
                      "scope": "mail_account_r mail_mailbox_w webmailer_setting_r webmailer_setting_w mail_confix_w"},
                timeout=40,
                headers={
                    "Content-Type": "application/x-www-form-urlencoded",
                    "Authorization": basic("mailcom_mailsidebar_passport_live"),
                    "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
                    "Origin": "https://webmailer.mail.com",
                    "Referer": "https://webmailer.mail.com/",
                })
    td = r4.json()
    token = td.get("access_token") or ""
    if not token:
        raise RuntimeError(f"token failed: {r4.status_code} {td}")
    return token


def api(token, path):
    headers = {
        "Authorization": "Bearer " + token,
        "Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json",
        "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
        "User-Agent": UA,
    }
    r = s.get("https://settings-cats.mail.com" + path, headers=headers, timeout=40)
    return r


token = login()
# full list without state filter
r = api(token, "/mailaccount/primary/emailAddresses?absoluteURI=false")
print("status", r.status_code)
data = r.json()
items = data.get("mailaddresslist") or []
print("TOTAL:", len(items))
actives = [i for i in items if i.get("state") == "ACTIVE"]
inactives = [i for i in items if i.get("state") != "ACTIVE"]
print("ACTIVE:", len(actives), "INACTIVE:", len(inactives))
print("--- ACTIVE ---")
for i in actives:
    print(json.dumps({k: i.get(k) for k in ("address", "state", "deletable", "defaultSenderAddress", "entryDate")}, ensure_ascii=False))
print("--- INACTIVE ---")
for i in inactives:
    print(json.dumps({k: i.get(k) for k in ("address", "state", "deletable", "entryDate", "exitDate")}, ensure_ascii=False))
print("--- links on first INACTIVE ---")
if inactives:
    print(json.dumps(inactives[0].get("_links", {}), ensure_ascii=False)[:1000])
print("--- keys ---")
print(list(items[0].keys()) if items else "none")
