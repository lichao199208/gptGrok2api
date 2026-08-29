import sys
import json
import re
import urllib3
import requests

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


# 1. consent
r = s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
log("home", f"status={r.status_code} url={r.url}")
consent_post = s.post("https://www.mail.com/consentpage", data={"redirectUrl": "https://www.mail.com/"}, timeout=30, allow_redirects=False)
log("consent", f"status={consent_post.status_code} url={consent_post.url} loc={consent_post.headers.get('Location')}")
# follow the post-consent navigation like the browser does
r_privacy = s.get("https://www.mail.com/privacy?referrer=https%3A%2F%2Fwww.mail.com%2F", timeout=30, allow_redirects=True)
log("privacy", f"status={r_privacy.status_code} url={r_privacy.url}")
r_home = s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
log("home2", f"status={r_home.status_code} url={r_home.url} len={len(r_home.text)}")
home_text = r_home.text
# grab the statistics blob and form fields from the homepage
m = re.search(r'name="statistics" value="([^"]+)"', home_text)
statistics = m.group(1) if m else ""
log("statistics", f"len={len(statistics)}")
form_fields = {}
for field in ["service", "uasServiceID", "successURL", "loginFailedURL", "loginErrorURL", "edition", "lang", "usertype"]:
    m = re.search(r'name="%s" value="([^"]*)"' % field, home_text)
    form_fields[field] = m.group(1) if m else ""
    log("field", f"{field}={form_fields[field][:80]}")
iba_m = re.search(r'name="ibaInfo" value="([^"]*)"', home_text)
form_fields["ibaInfo"] = iba_m.group(1) if iba_m else "abd=false"
if not statistics:
    log("statistics", "NOT FOUND - abort")
    sys.exit(1)

# 2. login POST
data = dict(form_fields)
data["statistics"] = statistics
data["username"] = EMAIL
data["password"] = PASSWORD
log("login", "POSTing credentials...")
r = s.post("https://login.mail.com/login", data=data, timeout=40, allow_redirects=False,
           headers={"Referer": "https://www.mail.com/", "Origin": "https://www.mail.com"})
log("login", f"status={r.status_code} location={r.headers.get('Location')}")
loc = r.headers.get("Location") or ""
if not loc or r.status_code not in (301, 302, 303, 307, 308):
    log("login", f"FAILED body={r.text[:400]}")
    sys.exit(2)
# 3. GET navigator /login with ott
r2 = s.get(loc, timeout=40, allow_redirects=False)
log("navigator-login", f"status={r2.status_code} url={r2.url[:200]} len={len(r2.content)}")
# 4. GET /halogin with ott + tz (same host, path /halogin instead of /login)
from urllib.parse import urlsplit, urlunsplit, urlencode, parse_qsl
parts = urlsplit(loc)
halogin_url = urlunsplit((parts.scheme, parts.netloc, "/halogin", parts.query, parts.fragment))
halogin_url = halogin_url + ("&" if "?" in halogin_url else "?") + "tz=0"
r3 = s.get(halogin_url, timeout=40, allow_redirects=False)
log("halogin", f"status={r3.status_code} location={r3.headers.get('Location')}")
sid_loc = r3.headers.get("Location") or ""
m = re.search(r"[?&]sid=([^&]+)", sid_loc)
sid = m.group(1) if m else ""
if not sid:
    log("halogin", f"FAILED body={r3.text[:400]}")
    sys.exit(3)
log("sid", sid[:60] + "...")
# 5. OAuth bridge token
token_url = "https://oauthbridge.navigator-lxa.mail.com/navigator/oauth2/token?sid=" + sid
scopes = "mail_account_r mail_mailbox_w webmailer_setting_r webmailer_setting_w mail_confix_w"
r4 = s.post(token_url, data={"grant_type": "urn:mam:oauth:grant-type:spa", "scope": scopes}, timeout=40,
            headers={"Content-Type": "application/x-www-form-urlencoded"})
log("oauthbridge", f"status={r4.status_code}")
try:
    token_data = r4.json()
    log("oauthbridge", "body keys: %s" % list(token_data.keys()))
    access_token = token_data.get("access_token") or ""
    if access_token:
        log("token", f"access_token={access_token[:40]}... expires_in={token_data.get('expires_in')}")
        with open("/tmp/mailcom_token.json", "w") as f:
            json.dump({"access_token": access_token, "data": token_data}, f)
        # 6. test settings API
        headers = {
            "Authorization": "Bearer " + access_token,
            "Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json",
            "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
        }
        r5 = requests.get("https://settings-cats.mail.com/mailaccount/primary/emailAddresses?absoluteURI=false",
                          headers=headers, timeout=40, verify=False,
                          proxies=s.proxies if PROXY else None)
        log("settings", f"status={r5.status_code} body={r5.text[:1500]}")
    else:
        log("token", "NO access_token: %s" % json.dumps(token_data)[:600])
except Exception as e:
    log("oauthbridge", f"parse error {type(e).__name__}: {e} body={r4.text[:400]}")
