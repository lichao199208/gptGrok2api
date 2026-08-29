#!/usr/bin/env python3
"""Patch /opt/gptGrok2api/services/register/mail_provider.py to add MailComMotherProvider."""
import re
import sys

PATH = "/opt/gptGrok2api/services/register/mail_provider.py"

with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

original = src

# 1. add base64 import
if "import base64" not in src:
    src = src.replace("import hashlib\nimport imaplib", "import base64\nimport hashlib\nimport imaplib", 1)

# 2. constants after OUTLOOK_REFRESHED_CREDENTIAL_RESET_STATES
constants = '''
MAILCOM_DEFAULT_DOMAINS = [
    "mail.com", "europe.com", "email.com", "usa.com", "dr.com", "clubmember.org",
    "salesperson.net", "housemail.com", "worker.com", "humanoid.net", "inorbit.com",
    "brew-meister.com", "toke.com", "hot-shot.com", "homemail.com", "reborn.com",
    "pacific-ocean.com", "deliveryman.com", "umpire.com", "computer4u.com", "webname.com",
    "e-mail.com", "email.net", "emailaccount.com", "emailengine.net", "emailengine.org",
]
MAILCOM_IMAP_HOST = "imap.mail.com"
MAILCOM_SETTINGS_BASE = "https://settings-cats.mail.com"
MAILCOM_SPA_CLIENT = "mailcom_mailsidebar_passport_live"
MAILCOM_TOKEN_SCOPES = "mail_account_r mail_mailbox_w webmailer_setting_r webmailer_setting_w mail_confix_w"
MAILCOM_TOKEN_TTL_SECONDS = 7200
_mailcom_token_cache: dict[str, tuple[str, float]] = {}
_mailcom_token_lock = Lock()
_mailcom_account_index = 0
'''
anchor = "OUTLOOK_REFRESHED_CREDENTIAL_RESET_STATES = OUTLOOK_RETRYABLE_STATES | OUTLOOK_INVALID_STATES"
if "MAILCOM_DEFAULT_DOMAINS" not in src:
    src = src.replace(anchor, anchor + "\n" + constants, 1)

# 3. provider class + helpers before `def _entries(`
provider_code = '''

def parse_mailcom_mother_accounts(text: str) -> list[dict[str, str]]:
    """解析 mail.com 母号池：每行 email----password（也兼容 email:password / email password）。"""
    result: list[dict[str, str]] = []
    for raw_line in str(text or "").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        email = ""
        password = ""
        if "----" in line:
            email, _, password = line.partition("----")
        elif "--" in line:
            email, _, password = line.partition("--")
        elif ":" in line:
            email, _, password = line.partition(":")
        else:
            parts = line.split(None, 1)
            if len(parts) == 2:
                email, password = parts
        email = str(email or "").strip()
        password = str(password or "").strip()
        if not email or not password or "@" not in email:
            continue
        result.append({"email": email, "password": password})
    return result


def _random_mailcom_local() -> str:
    length = random.randint(8, 12)
    return "".join(random.choices(string.ascii_lowercase + string.digits, k=length))


def _mailcom_next_account(accounts: list[dict[str, str]]) -> dict[str, str]:
    global _mailcom_account_index
    if not accounts:
        return {}
    with _mailcom_token_lock:
        account = dict(accounts[_mailcom_account_index % len(accounts)])
        _mailcom_account_index = (_mailcom_account_index + 1) % len(accounts)
    return account


def _mailcom_cached_token(email: str) -> str:
    with _mailcom_token_lock:
        cached = _mailcom_token_cache.get(email)
        if cached and time.monotonic() < cached[1]:
            return cached[0]
    return ""


def _mailcom_cache_token(email: str, token: str) -> None:
    with _mailcom_token_lock:
        _mailcom_token_cache[email] = (token, time.monotonic() + MAILCOM_TOKEN_TTL_SECONDS - 100)


class MailComMotherError(RuntimeError):
    pass


class MailComMotherProvider(BaseMailProvider):
    """mail.com 母号 provider：登录母号换取 Bearer token，通过 settings API 创建附加地址（子号），
    子号收到的验证码用母号 IMAP 读取。

    免费 mail.com 账号的激活子号配额为 10（含主地址，可删子号 9 个）；
    配额满时自动删除最旧的 deletable 子号后再创建新子号。
    每个 create_mailbox() 都会生成一个全新的随机子号，因此注册遇到“邮箱已使用”时
    上层自动重试即可拿到新的子号。
    """

    name = "mailcom_mother"

    def __init__(self, entry: dict, conf: dict):
        super().__init__(conf, str(entry.get("provider_ref") or ""))
        self.label = str(entry.get("label") or self.provider_ref or "Mail.com母号")
        self.accounts = parse_mailcom_mother_accounts(entry.get("accounts") or entry.get("pool") or "")
        self.imap_host = str(entry.get("imap_host") or MAILCOM_IMAP_HOST).strip() or MAILCOM_IMAP_HOST
        self.domains = _normalize_string_list(entry.get("domains")) or list(MAILCOM_DEFAULT_DOMAINS)
        self.max_active = max(1, int(entry.get("max_active") or 9))
        self.proxy = str(entry.get("proxy") or conf.get("proxy") or "").strip()
        session_conf = {**conf, "proxy": self.proxy}
        self.session = _create_session(session_conf)
        self.session.headers.update({"Accept-Language": "en-US,en;q=0.9"})
        self._imap: imaplib.IMAP4_SSL | None = None
        self._imap_key = ""

    def close(self) -> None:
        self._close_imap()
        try:
            self.session.close()
        except Exception:
            pass

    def _close_imap(self) -> None:
        imap = self._imap
        self._imap = None
        self._imap_key = ""
        if imap is None:
            return
        try:
            imap.logout()
        except Exception:
            pass

    # ---------- 登录 ----------
    def _login(self, account: dict[str, str]) -> str:
        email = str(account.get("email") or "").strip()
        cached = _mailcom_cached_token(email)
        if cached:
            return cached
        token = self._login_fresh(account)
        _mailcom_cache_token(email, token)
        return token

    def _login_fresh(self, account: dict[str, str]) -> str:
        email = str(account.get("email") or "").strip()
        password = str(account.get("password") or "").strip()
        masked = _mask_email(email)
        timeout = max(10.0, float(self.conf.get("request_timeout") or 30))
        s = self.session
        # 1) consent 流（www.mail.com 需要先通过隐私页拿到表单与 cookies）
        s.get("https://www.mail.com/", timeout=timeout, allow_redirects=True)
        s.post("https://www.mail.com/consentpage", data={"redirectUrl": "https://www.mail.com/"}, timeout=timeout, allow_redirects=False)
        s.get("https://www.mail.com/privacy?referrer=https%3A%2F%2Fwww.mail.com%2F", timeout=timeout, allow_redirects=True)
        r_home = s.get("https://www.mail.com/", timeout=timeout, allow_redirects=True)
        home_text = r_home.text
        match = re.search(r'name="statistics" value="([^"]+)"', home_text)
        if not match:
            raise MailComMotherError(f"mail.com 登录表单解析失败（可能被风控拦截），母号 {masked}")
        form_fields: dict[str, str] = {}
        for field in ("ibaInfo", "service", "uasServiceID", "successURL", "loginFailedURL", "loginErrorURL", "edition", "lang", "usertype"):
            m = re.search(r'name="%s" value="([^"]*)"' % field, home_text)
            form_fields[field] = m.group(1) if m else ""
        data = dict(form_fields)
        data["statistics"] = match.group(1)
        data["username"] = email
        data["password"] = password
        # 2) 提交登录表单 → 303 带 ott
        r = s.post(
            "https://login.mail.com/login",
            data=data,
            timeout=timeout,
            allow_redirects=False,
            headers={"Referer": "https://www.mail.com/", "Origin": "https://www.mail.com"},
        )
        loc = r.headers.get("Location") or ""
        if r.status_code not in (301, 302, 303, 307, 308) or "ott=" not in loc:
            raise MailComMotherError(
                f"mail.com 登录失败（HTTP {r.status_code}，可能母号密码错误或被风控），母号 {masked}"
            )
        parts = urlsplit(loc)
        # 3) 访问 navigator /login（与浏览器一致）
        s.get(urlunsplit((parts.scheme, parts.netloc, "/login", parts.query, parts.fragment)), timeout=timeout, allow_redirects=False)
        # 4) halogin 换 sid
        halogin = urlunsplit((parts.scheme, parts.netloc, "/halogin", parts.query, parts.fragment)) + "&tz=0"
        r3 = s.get(halogin, timeout=timeout, allow_redirects=False)
        m = re.search(r"[?&]sid=([^&]+)", r3.headers.get("Location") or "")
        sid = m.group(1) if m else ""
        if not sid:
            raise MailComMotherError(f"mail.com halogin 未返回 sid（HTTP {r3.status_code}），母号 {masked}")
        # 5) OAuth bridge 换 Bearer token
        token_url = f"https://oauthbridge.{parts.netloc}/navigator/oauth2/token?sid={sid}"
        basic = "Basic " + base64.b64encode(f"{MAILCOM_SPA_CLIENT}:*******".encode()).decode()
        r4 = s.post(
            token_url,
            data={"grant_type": "urn:mam:oauth:grant-type:spa", "scope": MAILCOM_TOKEN_SCOPES},
            timeout=timeout,
            headers={
                "Content-Type": "application/x-www-form-urlencoded",
                "Authorization": basic,
                "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
                "Origin": "https://webmailer.mail.com",
                "Referer": "https://webmailer.mail.com/",
            },
        )
        try:
            token_data = r4.json()
        except Exception:
            token_data = {}
        token = str(token_data.get("access_token") or "").strip()
        if not token:
            detail = str(token_data.get("error_description") or token_data.get("error") or r4.text[:200])
            raise MailComMotherError(f"mail.com 换取 OAuth token 失败（HTTP {r4.status_code}）：{detail}，母号 {masked}")
        return token

    # ---------- settings API ----------
    def _api(self, token: str, method: str, path: str, *, headers: dict | None = None, data=None, json_body=None):
        h = {
            "Authorization": "Bearer " + token,
            "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
            "User-Agent": self.conf.get("user_agent") or "chatgpt2api",
        }
        if headers:
            h.update(headers)
        return self.session.request(
            method.upper(),
            MAILCOM_SETTINGS_BASE + path,
            headers=h,
            data=data,
            json=json_body,
            timeout=max(10.0, float(self.conf.get("request_timeout") or 30)),
        )

    def _list_active(self, token: str) -> list[dict[str, Any]]:
        r = self._api(
            token,
            "GET",
            "/mailaccount/primary/emailAddresses?absoluteURI=false&q.state.in=ACTIVE&q.type.in=MANAGED%2CDOMAIN_HOSTING",
            headers={"Accept": "application/vnd.ui.trinity.mailaddress.list-v5+json"},
        )
        if r.status_code != 200:
            raise MailComMotherError(f"mail.com 获取地址列表失败：HTTP {r.status_code} {r.text[:200]}")
        try:
            data = r.json()
        except Exception:
            data = {}
        items = data.get("mailaddresslist")
        return items if isinstance(items, list) else []

    def _delete_address(self, token: str, address: str) -> None:
        enc = str(address).replace("@", "%40")
        r = self._api(
            token,
            "POST",
            f"/mailaccount/primary/emailAddressesRemovals/{enc}/removals?absoluteURI=false",
            headers={"Accept": "text/plain;charset=UTF-8", "Content-Type": "text/plain;charset=UTF-8"},
            data="",
        )
        if r.status_code != 204:
            raise MailComMotherError(f"删除 mail.com 子号失败：{address} HTTP {r.status_code} {r.text[:160]}")

    def _create_address(self, token: str, candidate: str) -> str:
        """返回 "ok" / "quota" / "domain"。"""
        rv = self._api(
            token,
            "POST",
            "/mailaccount/emailAddressValidations?absoluteURI=false",
            headers={
                "Content-Type": "application/vnd.ui.trinity.email-address-validation-request+json",
                "Accept": "application/vnd.ui.trinity.email-address-validation-response+json",
            },
            json_body=[candidate],
        )
        if rv.status_code != 200:
            return "domain"
        rc = self._api(
            token,
            "POST",
            "/mailaccount/primary/emailAddresses?absoluteURI=false",
            headers={
                "Content-Type": "application/vnd.ui.trinity.minimalmailaddress-v3+json",
                "Accept": "application/vnd.ui.trinity.minimalmailaddress-v3+json",
            },
            json_body={
                "address": candidate,
                "deletable": True,
                "pgpEnabled": False,
                "defaultSenderAddress": False,
                "defaultReceiverAddress": False,
                "state": "ACTIVE",
            },
        )
        if rc.status_code == 201:
            return "ok"
        if rc.status_code == 409:
            return "quota"
        if rc.status_code in (400, 415):
            return "domain"
        raise MailComMotherError(f"创建 mail.com 子号失败：{candidate} HTTP {rc.status_code} {rc.text[:200]}")

    def _delete_oldest_active(self, token: str) -> None:
        actives = self._list_active(token)
        deletable = [item for item in actives if bool(item.get("deletable"))]
        if not deletable:
            raise MailComMotherError("mail.com 母号没有可删除的子号来释放配额")
        oldest = min(deletable, key=lambda item: str(item.get("entryDate") or ""))
        self._delete_address(token, oldest["address"])

    def _claim_address(self, account: dict[str, str]) -> str:
        token = self._login(account)
        actives = self._list_active(token)
        deletable = [item for item in actives if bool(item.get("deletable"))]
        if len(deletable) >= self.max_active:
            self._delete_oldest_active(token)
        for _ in range(3):
            candidate = f"{_random_mailcom_local()}@{_next_domain(self.domains)}"
            result = self._create_address(token, candidate)
            if result == "ok":
                return candidate
            if result == "quota":
                self._delete_oldest_active(token)
                continue
        raise MailComMotherError(f"[{self.label}] mail.com 子号创建失败（域名均不可用或配额异常）")

    # ---------- provider 接口 ----------
    def create_mailbox(self, username: str | None = None) -> dict[str, Any]:
        if not self.accounts:
            raise RuntimeError(f"[{self.label}] Mail.com 母号池为空，请先在邮箱配置中导入 email----password 母号账号")
        last_error = ""
        for _ in range(min(len(self.accounts), 3)):
            account = _mailcom_next_account(self.accounts)
            try:
                address = self._claim_address(account)
            except MailComMotherError as error:
                last_error = str(error)
                continue
            return {
                "provider": self.name,
                "provider_ref": self.provider_ref,
                "address": address,
                "login_email": account["email"],
                "password": account.get("password", ""),
                "imap_host": self.imap_host,
                "label": self.label,
                "mother_email": account["email"],
            }
        raise RuntimeError(last_error or f"[{self.label}] mail.com 母号创建子号失败")

    def _imap_connect(self, mailbox: dict[str, Any]) -> imaplib.IMAP4_SSL:
        key = str(mailbox.get("login_email") or mailbox.get("mother_email") or "").strip()
        if self._imap is not None and self._imap_key == key:
            return self._imap
        self._close_imap()
        try:
            imap = imaplib.IMAP4_SSL(self.imap_host)
            imap.socket().settimeout(max(10.0, float(self.conf.get("request_timeout") or 30)))
            imap.login(key, str(mailbox.get("password") or ""))
            status, _ = imap.select("INBOX")
            if status != "OK":
                raise RuntimeError("IMAP select INBOX 失败")
        except Exception as exc:
            raise MailComMotherError(f"mail.com IMAP 登录失败：{type(exc).__name__}: {exc}") from exc
        self._imap = imap
        self._imap_key = key
        return imap

    def _parse_imap_message(self, mailbox: dict[str, Any], raw: bytes) -> dict[str, Any]:
        message = message_from_bytes(raw, policy=policy.default)
        try:
            received = _parse_received_at(parsedate_to_datetime(str(message.get("Date") or "")))
        except Exception:
            received = None
        plain: list[str] = []
        html: list[str] = []
        for part in (message.walk() if message.is_multipart() else [message]):
            if part.get_content_maintype() == "multipart":
                continue
            try:
                payload = part.get_content()
            except Exception:
                continue
            if not payload:
                continue
            if part.get_content_type() == "text/html":
                html.append(str(payload))
            else:
                plain.append(str(payload))

        def _decode(value: str | None) -> str:
            if not value:
                return ""
            try:
                return str(make_header(decode_header(value)))
            except Exception:
                return value

        return {
            "provider": self.name,
            "mailbox": str(mailbox.get("address") or ""),
            "message_id": _decode(str(message.get("Message-ID") or "")),
            "subject": _decode(str(message.get("Subject") or "")),
            "sender": _decode(str(message.get("From") or "")),
            "to": _decode(str(message.get("To") or "")),
            "delivered_to": _decode(str(message.get("Delivered-To") or "")),
            "text_content": "\\n".join(plain).strip(),
            "html_content": "\\n".join(html).strip(),
            "received_at": received,
            "raw": None,
        }

    def fetch_latest_message(self, mailbox: dict[str, Any]) -> dict[str, Any] | None:
        address = str(mailbox.get("address") or "").strip()
        if not address:
            return None
        imap = self._imap_connect(mailbox)
        try:
            status, data = imap.search(None, 'TO "%s"' % address)
        except Exception as exc:
            raise MailComMotherError(f"mail.com IMAP 查询失败：{type(exc).__name__}: {exc}") from exc
        if status != "OK" or not data or not data[0]:
            return None
        ids = data[0].split()
        for msg_id in reversed(ids[-10:]):
            try:
                status2, fetched = imap.fetch(msg_id, "(BODY.PEEK[])")
            except Exception:
                continue
            if status2 != "OK" or not fetched:
                continue
            raw_payload = b""
            for part in fetched:
                if isinstance(part, tuple) and isinstance(part[1], bytes):
                    raw_payload = part[1]
                    break
            if raw_payload:
                return self._parse_imap_message(mailbox, raw_payload)
        return None


'''

anchor_entries = "def _entries(mail_config: dict) -> list[dict]:"
if "class MailComMotherProvider" not in src:
    src = src.replace(anchor_entries, provider_code + "\n" + anchor_entries, 1)

# 4. factory branch
factory_anchor = '    if entry["type"] == "outlook_token":\n        return OutlookTokenProvider(entry, conf)'
if 'entry["type"] == "mailcom_mother"' not in src:
    src = src.replace(
        factory_anchor,
        factory_anchor + '\n    if entry["type"] == "mailcom_mother":\n        return MailComMotherProvider(entry, conf)',
        1,
    )

if src == original:
    print("NO CHANGES APPLIED")
    sys.exit(1)

with open(PATH, "w", encoding="utf-8") as f:
    f.write(src)

import py_compile
try:
    py_compile.compile(PATH, doraise=True)
    print("PATCH OK + syntax valid")
except Exception as e:
    print("SYNTAX ERROR:", e)
    sys.exit(1)
