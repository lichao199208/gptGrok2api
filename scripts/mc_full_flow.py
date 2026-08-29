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


def log(name, msg):
    print(f"[{name}] {msg}")


def basic(client_id):
    return "Basic " + base64.b64encode(f"{client_id}:*******".encode()).decode()


# 1. consent
s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
s.post("https://www.mail.com/consentpage", data={"redirectUrl": "https://www.mail.com/"}, timeout=30, allow_redirects=False)
s.get("https://www.mail.com/privacy?referrer=https%3A%2F%2Fwww.mail.com%2F", timeout=30, allow_redirects=True)
r_home = s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
home_text = r_home.text

m = re.search(r'name="statistics" value="([^"]+)"', home_text)
statistics = m.group(1) if m else ""
if not statistics:
    log("fatal", "no statistics blob")
    sys.exit(1)
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
if r.status_code not in (301, 302, 303, 307, 308) or "ott=" not in loc:
    log("login", f"FAILED status={r.status_code} loc={loc[:200]}")
    sys.exit(2)
log("ott", loc.split("ott=")[-1][:36])

parts = urlsplit(loc)
navigator_host = parts.netloc
halogin_url = urlunsplit((parts.scheme, parts.netloc, "/halogin", parts.query, parts.fragment)) + "&tz=0"
r3 = s.get(halogin_url, timeout=40, allow_redirects=False)
sid_loc = r3.headers.get("Location") or ""
m = re.search(r"[?&]sid=([^&]+)", sid_loc)
sid = m.group(1) if m else ""
if not sid:
    log("halogin", f"FAILED status={r3.status_code} body={r3.text[:300]}")
    sys.exit(3)
log("sid", sid[:50] + "...")

# 2. oauthbridge token with various clients/scopes
token_url = f"https://oauthbridge.{navigator_host}/navigator/oauth2/token?sid={sid}"
clients = [
    ("mailcom_mailsidebar_passport_live", "https://webmailer.mail.com/"),
    ("mailcom_webmailermailroot_live", "https://webmailer.mail.com/"),
    ("mailcom_lps_live", f"https://lps.{navigator_host}/"),
]
scopes = ["mail_account_r mail_mailbox_w webmailer_setting_r webmailer_setting_w mail_confix_w",
          "mail_account_r",
          "mail_account_r mail_mailbox_w mail_confix_r mail_confix_w"]
found_token = ""
for client_id, referer in clients:
    origin = referer.rstrip("/")
    for scope in scopes:
        r4 = s.post(token_url,
                    data={"grant_type": "urn:mam:oauth:grant-type:spa", "scope": scope},
                    timeout=40,
                    headers={
                        "Content-Type": "application/x-www-form-urlencoded",
                        "Authorization": basic(client_id),
                        "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
                        "Origin": origin,
                        "Referer": referer,
                    })
        try:
            td = r4.json()
        except Exception:
            td = {}
        if r4.status_code == 200 and td.get("access_token"):
            log("token", f"client={client_id} scope={scope} OK expires_in={td.get('expires_in')}")
            found_token = td["access_token"]
            with open("/tmp/mailcom_token.json", "w") as f:
                json.dump({"access_token": found_token, "client": client_id, "scope": scope}, f)
            break
        else:
            log("token-fail", f"client={client_id} scope={scope[:30]} status={r4.status_code} err={td.get('error_description') or td.get('error') or r4.text[:120]}")
    if found_token:
        break

if not found_token:
    log("fatal", "no token obtained")
    sys.exit(4)

# 3. settings API tests
for api_host in ["settings-cats.mail.com", f"settings-{navigator_host.split('-')[1]}.mail.com" if "-" in navigator_host else ""]:
    if not api_host:
        continue
    for path in ["/mailaccount/primary/emailAddresses?absoluteURI=false",
                 "/mailaccount/primary/emailAddresses?absoluteURI=false&q.state.in=ACTIVE&q.type.in=MANAGED%2CDOMAIN_HOSTING"]:
        headers = {
            "Authorization": "Bearer " + found_token,
            "Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json",
            "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
            "User-Agent": UA,
        }
        try:
            r5 = s.get(f"https://{api_host}{path}", headers=headers, timeout=40)
            log("settings", f"host={api_host} status={r5.status_code} body={r5.text[:1200]}")
        except Exception as e:
            log("settings-err", f"host={api_host} {type(e).__name__}: {e}")
