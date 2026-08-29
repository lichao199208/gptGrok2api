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

EMAIL = sys.argv[1] if len(sys.argv) > 1 else "ChunkyButtotm@mail.com"
PASSWORD = sys.argv[2] if len(sys.argv) > 2 else "WMNukRC0Zng"
PROXY = sys.argv[3] if len(sys.argv) > 3 else "http://root:lichao@64.83.18.19:7890"

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"

s = requests.Session()
s.verify = False
s.proxies = {"http": PROXY, "https": PROXY}
s.headers.update({"User-Agent": UA, "Accept-Language": "en-US,en;q=0.9"})


def basic(client_id):
    return "Basic " + base64.b64encode(f"{client_id}:*******".encode()).decode()


def login():
    s.get("https://www.mail.com/", timeout=40, allow_redirects=True)
    s.post("https://www.mail.com/consentpage", data={"redirectUrl": "https://www.mail.com/"}, timeout=40, allow_redirects=False)
    s.get("https://www.mail.com/privacy?referrer=https%3A%2F%2Fwww.mail.com%2F", timeout=40, allow_redirects=True)
    r_home = s.get("https://www.mail.com/", timeout=40, allow_redirects=True)
    home_text = r_home.text
    m = re.search(r'name="statistics" value="([^"]+)"', home_text)
    statistics = m.group(1) if m else ""
    form_fields = {}
    for field in ["ibaInfo", "service", "uasServiceID", "successURL", "loginFailedURL", "loginErrorURL", "edition", "lang", "usertype"]:
        mm = re.search(r'name="%s" value="([^"]*)"' % field, home_text)
        form_fields[field] = mm.group(1) if mm else ""
    data = dict(form_fields)
    data["statistics"] = statistics
    data["username"] = EMAIL
    data["password"] = PASSWORD
    r = s.post("https://login.mail.com/login", data=data, timeout=40, allow_redirects=False,
               headers={"Referer": "https://www.mail.com/", "Origin": "https://www.mail.com"})
    loc = r.headers.get("Location") or ""
    if "ott=" not in loc:
        print("LOGIN FAIL:", r.status_code, loc[:200], r.text[:100])
        return None
    parts = urlsplit(loc)
    s.get(urlunsplit((parts.scheme, parts.netloc, "/login", parts.query, parts.fragment)), timeout=40, allow_redirects=False)
    halogin = urlunsplit((parts.scheme, parts.netloc, "/halogin", parts.query, parts.fragment)) + "&tz=0"
    r3 = s.get(halogin, timeout=40, allow_redirects=False)
    m = re.search(r"[?&]sid=([^&]+)", r3.headers.get("Location") or "")
    sid = m.group(1) if m else ""
    token_url = f"https://oauthbridge.{parts.netloc}/navigator/oauth2/token?sid={sid}"
    r4 = s.post(token_url,
                data={"grant_type": "urn:mam:oauth:grant-type:spa",
                      "scope": "mail_account_r mail_mailbox_w webmailer_setting_r webmailer_setting_w mail_confix_w"},
                timeout=40,
                headers={"Content-Type": "application/x-www-form-urlencoded",
                         "Authorization": basic("mailcom_mailsidebar_passport_live"),
                         "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
                         "Origin": "https://webmailer.mail.com",
                         "Referer": "https://webmailer.mail.com/"})
    td = r4.json()
    token = td.get("access_token") or ""
    if not token:
        print("TOKEN FAIL:", r4.status_code, td)
    return token


def api(token, method, path, headers=None, json_body=None):
    h = {"Authorization": "Bearer " + token, "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335", "User-Agent": UA}
    if headers:
        h.update(headers)
    return s.request(method, "https://settings-cats.mail.com" + path, headers=h, json=json_body, timeout=40)


def main():
    token = login()
    if not token:
        return
    print("TOKEN OK:", token[:25], "...")
    r = api(token, "GET", "/mailaccount/primary/emailAddresses?absoluteURI=false&q.state.in=ACTIVE&q.type.in=MANAGED%2CDOMAIN_HOSTING",
            headers={"Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json"})
    print("LIST:", r.status_code)
    if r.status_code == 200:
        items = r.json().get("mailaddresslist") or []
        deletable = [i for i in items if i.get("deletable")]
        print("  active:", len(items), "deletable:", len(deletable))
        if len(deletable) >= 9:
            oldest = min(deletable, key=lambda i: str(i.get("entryDate") or ""))
            enc = oldest["address"].replace("@", "%40")
            rd = api(token, "POST", f"/mailaccount/primary/emailAddressesRemovals/{enc}/removals?absoluteURI=false",
                     headers={"Accept": "text/plain;charset=UTF-8", "Content-Type": "text/plain;charset=UTF-8"})
            print("DELETE oldest:", oldest["address"], "->", rd.status_code)
    # create one
    cand = "".join(random.choices(string.ascii_lowercase + string.digits, k=10)) + "@humanoid.net"
    rv = api(token, "POST", "/mailaccount/emailAddressValidations?absoluteURI=false",
             headers={"Content-Type": "application/vnd.ui.trinity.email-address-validation-request+json",
                      "Accept": "application/vnd.ui.trinity.email-address-validation-response+json"},
             json_body=[cand])
    print("VALIDATE:", cand, rv.status_code)
    if rv.status_code == 200:
        rc = api(token, "POST", "/mailaccount/primary/emailAddresses?absoluteURI=false",
                 headers={"Content-Type": "application/vnd.ui.trinity.minimalmailaddress-v3+json",
                          "Accept": "application/vnd.ui.trinity.minimalmailaddress-v3+json"},
                 json_body={"address": cand, "deletable": True, "pgpEnabled": False,
                            "defaultSenderAddress": False, "defaultReceiverAddress": False, "state": "ACTIVE"})
        print("CREATE:", cand, "->", rc.status_code)
        if rc.status_code == 201:
            enc = cand.replace("@", "%40")
            rd = api(token, "POST", f"/mailaccount/primary/emailAddressesRemovals/{enc}/removals?absoluteURI=false",
                     headers={"Accept": "text/plain;charset=UTF-8", "Content-Type": "text/plain;charset=UTF-8"})
            print("CLEANUP:", rd.status_code)


main()
