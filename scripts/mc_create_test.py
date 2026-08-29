import sys
import json
import re
import base64
import random
import string
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


def login():
    s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
    s.post("https://www.mail.com/consentpage", data={"redirectUrl": "https://www.mail.com/"}, timeout=30, allow_redirects=False)
    s.get("https://www.mail.com/privacy?referrer=https%3A%2F%2Fwww.mail.com%2F", timeout=30, allow_redirects=True)
    r_home = s.get("https://www.mail.com/", timeout=30, allow_redirects=True)
    home_text = r_home.text
    m = re.search(r'name="statistics" value="([^"]+)"', home_text)
    statistics = m.group(1) if m else ""
    if not statistics:
        raise RuntimeError("no statistics blob")
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
    if not sid:
        raise RuntimeError(f"halogin failed: {r3.status_code}")
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


def api(token, method, path, headers=None, body=None, json_body=None):
    h = {
        "Authorization": "Bearer " + token,
        "Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json",
        "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
        "User-Agent": UA,
    }
    if headers:
        h.update(headers)
    r = s.request(method, "https://settings-cats.mail.com" + path, headers=h, data=body, json=json_body, timeout=40)
    return r


DOMAINS = ["humanoid.net", "salesperson.net", "dr.com", "mail.com", "europe.com", "email.com", "usa.com", "clubmember.org",
           "housemail.com", "worker.com", "e-mail.com", "email.net", "emailaccount.com", "emailengine.net", "emailengine.org"]


def random_local():
    length = random.randint(8, 12)
    return "".join(random.choice(string.ascii_lowercase + string.digits) for _ in range(length))


def main():
    token = login()
    log("token", token[:30] + "...")
    # list current addresses
    r = api(token, "GET", "/mailaccount/primary/emailAddresses?absoluteURI=false&q.state.in=ACTIVE&q.type.in=MANAGED%2CDOMAIN_HOSTING",
            headers={"Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json"})
    log("list", f"status={r.status_code}")
    data = r.json() if r.status_code == 200 else {}
    items = data.get("mailaddresslist") or []
    existing = {item["address"].lower() for item in items}
    log("existing", f"{len(items)} addresses, main={[i['address'] for i in items if not i.get('deletable')]}")

    # find a valid domain
    chosen = None
    validated_domains = []
    for domain in DOMAINS:
        candidate = f"{random_local()}@{domain}"
        rv = api(token, "POST", "/mailaccount/emailAddressValidations?absoluteURI=false",
                 headers={"Content-Type": "application/vnd.ui.trinity.email-address-validation-request+json",
                          "Accept": "application/vnd.ui.trinity.email-address-validation-response+json"},
                 json_body=[candidate])
        if rv.status_code == 200:
            validated_domains.append(domain)
            log("validate", f"{candidate} OK")
            if chosen is None:
                chosen = candidate
        else:
            log("validate-fail", f"{candidate} status={rv.status_code} body={rv.text[:150]}")
    if not chosen:
        raise RuntimeError("no domain validated")
    log("validated-domains", json.dumps(validated_domains))

    # create (try each validated domain until one succeeds)
    created = None
    for domain in validated_domains:
        cand = f"{random_local()}@{domain}"
        rc = api(token, "POST", "/mailaccount/primary/emailAddresses?absoluteURI=false",
                 headers={"Content-Type": "application/vnd.ui.trinity.minimalmailaddress-v3+json",
                          "Accept": "application/vnd.ui.trinity.minimalmailaddress-v3+json"},
                 json_body={"address": cand, "deletable": True, "pgpEnabled": False,
                            "defaultSenderAddress": False, "defaultReceiverAddress": False, "state": "ACTIVE"})
        log("create", f"{cand} status={rc.status_code} body={rc.text[:250]}")
        if rc.status_code == 201:
            created = cand
            break
    if not created:
        raise RuntimeError("all creates failed")

    # verify in list
    r = api(token, "GET", "/mailaccount/primary/emailAddresses?absoluteURI=false&q.state.in=ACTIVE&q.type.in=MANAGED%2CDOMAIN_HOSTING",
            headers={"Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json"})
    items2 = (r.json().get("mailaddresslist") or []) if r.status_code == 200 else []
    found = [i for i in items2 if i["address"].lower() == created.lower()]
    log("verify", f"in list: {bool(found)} state={found[0]['state'] if found else 'N/A'}")

    # delete (cleanup)
    if found and found[0].get("deletable"):
        enc = created.replace("@", "%40")
        rd = api(token, "POST", f"/mailaccount/primary/emailAddressesRemovals/{enc}/removals?absoluteURI=false",
                 headers={"Accept": "text/plain;charset=UTF-8", "Content-Type": "text/plain;charset=UTF-8"}, body="")
        log("delete", f"status={rd.status_code} body={rd.text[:120]}")
    else:
        log("delete", "skipped (not deletable or not found)")

    # final list check
    r = api(token, "GET", "/mailaccount/primary/emailAddresses?absoluteURI=false&q.state.in=ACTIVE&q.type.in=MANAGED%2CDOMAIN_HOSTING",
            headers={"Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json"})
    items3 = (r.json().get("mailaddresslist") or []) if r.status_code == 200 else []
    log("final", f"{len(items3)} addresses, created-remains: {any(i['address'].lower() == created.lower() for i in items3)}")


main()
