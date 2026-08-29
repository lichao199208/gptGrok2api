#!/usr/bin/env python3
"""Rewrite MailComMotherProvider with proxy rotation + sub-address pool."""
import py_compile

PATH = "/opt/gptGrok2api/services/register/mail_provider.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

start_marker = "class MailComMotherProvider(BaseMailProvider):"
end_marker = "\ndef _entries(mail_config: dict) -> list[dict]:"

idx_start = src.find(start_marker)
idx_end = src.find(end_marker)
if idx_start == -1 or idx_end == -1:
    print("markers not found")
    raise SystemExit(1)

new_class = '''class MailComMotherProvider(BaseMailProvider):
    """mail.com 母号 provider：登录母号换取 Bearer token，通过 settings API 创建附加地址（子号），
    子号收到的验证码用母号 IMAP 读取。

    免费 mail.com 账号的激活子号配额为 10（含主地址，可删子号 9 个）；
    配额满时自动删除最旧的 deletable 子号后再创建新子号。

    防风控设计：
    - 多出口代理轮换：login.mail.com 对出口 IP 有登录频率风控（返回 302 assistance），
      被标记的代理自动冷却 10 分钟后重试，并切换到下一个可用代理。
    - 子号池批量预生成：每次登录批量创建 batch_size 个子号并缓存，注册时优先从池中
      领取，极大降低登录频率。
    """

    name = "mailcom_mother"

    def __init__(self, entry: dict, conf: dict):
        super().__init__(conf, str(entry.get("provider_ref") or ""))
        self.label = str(entry.get("label") or self.provider_ref or "Mail.com母号")
        self.accounts = parse_mailcom_mother_accounts(entry.get("accounts") or entry.get("pool") or "")
        self.imap_host = str(entry.get("imap_host") or MAILCOM_IMAP_HOST).strip() or MAILCOM_IMAP_HOST
        self.domains = _normalize_string_list(entry.get("domains")) or list(MAILCOM_DEFAULT_DOMAINS)
        self.max_active = max(1, int(entry.get("max_active") or 9))
        self.batch_size = max(1, min(6, int(entry.get("pool_batch") or 3)))
        self.pool_enabled = True
        self.proxies = _mailcom_proxy_list(entry, conf)
        session_conf = {**conf, "proxy": self.proxies[0] if self.proxies else ""}
        self.session = _create_session(session_conf)
        self.session.headers.update({"Accept-Language": "en-US,en;q=0.9"})
        self._active_session = None
        self._active_proxy = ""
        self._imap: imaplib.IMAP4_SSL | None = None
        self._imap_key = ""

    def close(self) -> None:
        self._close_imap()
        try:
            self.session.close()
        except Exception:
            pass
        if self._active_session is not None and self._active_session is not self.session:
            try:
                self._active_session.close()
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

    # ---------- 出口代理 ----------
    def _api_session(self):
        return self._active_session if self._active_session is not None else self.session

    def _mark_proxy_blocked(self, proxy: str) -> None:
        with _mailcom_token_lock:
            _mailcom_proxy_blocked_until[proxy] = time.monotonic() + MAILCOM_PROXY_COOLDOWN_SECONDS

    def _login_with_rotation(self, account: dict[str, str]) -> str:
        email = str(account.get("email") or "").strip()
        cached = _mailcom_cached_token(email)
        if cached:
            return cached
        if not self.proxies:
            raise MailComMotherError("mail.com 未配置任何出口代理（proxy / proxies 为空）")
        now = time.monotonic()
        with _mailcom_token_lock:
            start = _mailcom_proxy_index % len(self.proxies)
        last_error = ""
        for offset in range(len(self.proxies)):
            proxy = self.proxies[(start + offset) % len(self.proxies)]
            with _mailcom_token_lock:
                blocked_until = _mailcom_proxy_blocked_until.get(proxy, 0)
                if blocked_until > now:
                    continue
                _mailcom_proxy_index = (_mailcom_proxy_index + 1) % len(self.proxies)
            try:
                token, session = self._login_fresh(account, proxy)
            except _MailComProxyBlocked as error:
                last_error = str(error)
                self._mark_proxy_blocked(proxy)
                continue
            except MailComMotherError as error:
                # 非代理类失败（密码错误等）直接抛出，不轮换
                raise
            except Exception as error:
                last_error = f"{proxy}: {type(error).__name__}: {error}"
                continue
            self._active_proxy = proxy
            self._active_session = session
            _mailcom_cache_token(email, token)
            return token
        raise MailComMotherError(last_error or "mail.com 所有出口代理均被风控或不可用，请等待冷却或更换代理")

    def _login_fresh(self, account: dict[str, str], proxy: str) -> tuple[str, object]:
        email = str(account.get("email") or "").strip()
        password = str(account.get("password") or "").strip()
        masked = _mailcom_mask_email(email)
        session = _create_session({**self.conf, "proxy": proxy, "user_agent": self.conf.get("user_agent") or "chatgpt2api"})
        session.headers.update({"Accept-Language": "en-US,en;q=0.9"})
        timeout = max(10.0, float(self.conf.get("request_timeout") or 30))
        try:
            # 1) consent 流
            session.get("https://www.mail.com/", timeout=timeout, allow_redirects=True)
            session.post("https://www.mail.com/consentpage", data={"redirectUrl": "https://www.mail.com/"}, timeout=timeout, allow_redirects=False)
            session.get("https://www.mail.com/privacy?referrer=https%3A%2F%2Fwww.mail.com%2F", timeout=timeout, allow_redirects=True)
            r_home = session.get("https://www.mail.com/", timeout=timeout, allow_redirects=True)
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
            # 2) 登录表单
            r = session.post(
                "https://login.mail.com/login",
                data=data,
                timeout=timeout,
                allow_redirects=False,
                headers={"Referer": "https://www.mail.com/", "Origin": "https://www.mail.com"},
            )
            loc = r.headers.get("Location") or ""
            if r.status_code in (301, 302, 303, 307, 308) and "ott=" in loc:
                parts = urlsplit(loc)
                session.get(urlunsplit((parts.scheme, parts.netloc, "/login", parts.query, parts.fragment)), timeout=timeout, allow_redirects=False)
                halogin = urlunsplit((parts.scheme, parts.netloc, "/halogin", parts.query, parts.fragment)) + "&tz=0"
                r3 = session.get(halogin, timeout=timeout, allow_redirects=False)
                m = re.search(r"[?&]sid=([^&]+)", r3.headers.get("Location") or "")
                sid = m.group(1) if m else ""
                if not sid:
                    raise MailComMotherError(f"mail.com halogin 未返回 sid（HTTP {r3.status_code}），母号 {masked}")
                token_url = f"https://oauthbridge.{parts.netloc}/navigator/oauth2/token?sid={sid}"
                basic = "Basic " + base64.b64encode(f"{MAILCOM_SPA_CLIENT}:*******".encode()).decode()
                r4 = session.post(
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
                return token, session
            if r.status_code in (301, 302, 303, 307, 308):
                body_hint = str(r.text or "")[:200]
                if "support.mail.com" in loc or "assistance" in body_hint:
                    raise _MailComProxyBlocked(f"出口 {proxy} 被 mail.com 风控（assistance 跳转）")
                raise MailComMotherError(
                    f"mail.com 登录失败（HTTP {r.status_code}，跳转 {loc[:120]}，可能母号密码错误或被风控），母号 {masked}"
                )
            if r.status_code == 403:
                raise _MailComProxyBlocked(f"出口 {proxy} 被 mail.com 拒绝（HTTP 403）")
            raise MailComMotherError(f"mail.com 登录失败（HTTP {r.status_code}，可能母号密码错误或被风控），母号 {masked}")
        except _MailComProxyBlocked:
            raise
        except MailComMotherError:
            raise
        except Exception as error:
            raise MailComMotherError(f"mail.com 登录异常（{type(error).__name__}），母号 {masked}") from error
        finally:
            pass

    # ---------- settings API ----------
    def _api(self, token: str, method: str, path: str, *, headers: dict | None = None, data=None, json_body=None):
        h = {
            "Authorization": "Bearer " + token,
            "x-ui-app": "mailcom.mailset-compose/1.0.5-build.335",
            "User-Agent": self.conf.get("user_agent") or "chatgpt2api",
        }
        if headers:
            h.update(headers)
        return self._api_session().request(
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

    def _claim_batch(self, account: dict[str, str], count: int) -> list[str]:
        """登录一次并批量创建 count 个子号。"""
        token = self._login_with_rotation(account)
        actives = self._list_active(token)
        deletable = [item for item in actives if bool(item.get("deletable"))]
        free = max(0, self.max_active - len(deletable))
        need = count - free
        if need > 0:
            ordered = sorted(deletable, key=lambda item: str(item.get("entryDate") or ""))
            for index in range(min(need, len(ordered))):
                self._delete_address(token, ordered[index]["address"])
        created: list[str] = []
        for _ in range(count):
            for _attempt in range(3):
                candidate = f"{_random_mailcom_local()}@{_next_domain(self.domains)}"
                result = self._create_address(token, candidate)
                if result == "ok":
                    created.append(candidate)
                    break
                if result == "quota":
                    self._delete_oldest_active(token)
                    continue
        if not created:
            raise MailComMotherError(f"[{self.label}] mail.com 子号创建失败（域名均不可用或配额异常）")
        return created

    # ---------- provider 接口 ----------
    def _mailbox_dict(self, mother_email: str, address: str) -> dict[str, Any]:
        account = next(
            (item for item in self.accounts if str(item.get("email") or "").lower() == str(mother_email or "").lower()),
            {"email": mother_email, "password": ""},
        )
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

    def create_mailbox(self, username: str | None = None) -> dict[str, Any]:
        if not self.accounts:
            raise RuntimeError(f"[{self.label}] Mail.com 母号池为空，请先在邮箱配置中导入 email----password 母号账号")
        if self.pool_enabled:
            popped = _mailcom_pool_pop()
            if popped:
                mother, address = popped
                return self._mailbox_dict(mother, address)
        last_error = ""
        for _ in range(min(len(self.accounts), 3)):
            account = _mailcom_next_account(self.accounts)
            try:
                created = self._claim_batch(account, self.batch_size)
            except MailComMotherError as error:
                last_error = str(error)
                continue
            mother = account["email"]
            if self.pool_enabled and len(created) > 1:
                _mailcom_pool_push(mother, created[1:])
            return self._mailbox_dict(mother, created[0])
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


class _MailComProxyBlocked(RuntimeError):
    """出口代理被 mail.com 风控，需轮换。"""

'''

src = src[:idx_start] + new_class + src[idx_end + 1:]

# add pool/proxy module state before the class (after _mailcom_cache_token)
state_anchor = "class MailComMotherError(RuntimeError):"
state_code = '''# 子号池 / 代理轮换的进程内状态
_mailcom_subaddress_pool: dict[str, list[str]] = {}
_mailcom_pool_lock = Lock()
_mailcom_proxy_blocked_until: dict[str, float] = {}
_mailcom_proxy_index = 0
MAILCOM_BATCH_SIZE = 3
MAILCOM_PROXY_COOLDOWN_SECONDS = 600


def _mailcom_proxy_list(entry: dict, conf: dict) -> list[str]:
    """代理优先级：entry.proxies（多行列表）> entry.proxy > conf.proxy。"""
    proxies: list[str] = []
    candidates = _normalize_string_list(entry.get("proxies")) or [entry.get("proxy") or conf.get("proxy") or ""]
    for value in candidates:
        value = str(value or "").strip()
        if value and value not in proxies:
            proxies.append(value)
    return proxies


def _mailcom_pool_pop() -> tuple[str, str] | None:
    """从子号池取一个就绪子号，返回 (mother_email, address)。"""
    with _mailcom_pool_lock:
        for mother, addresses in list(_mailcom_subaddress_pool.items()):
            while addresses:
                address = addresses.pop(0)
                if str(address or "").strip():
                    return mother, str(address).strip()
            _mailcom_subaddress_pool.pop(mother, None)
    return None


def _mailcom_pool_push(mother: str, addresses: list[str]) -> None:
    with _mailcom_pool_lock:
        pool = _mailcom_subaddress_pool.setdefault(str(mother or "").strip(), [])
        for address in addresses:
            address = str(address or "").strip()
            if address and address not in pool:
                pool.append(address)


'''
if "_mailcom_subaddress_pool" not in src:
    src = src.replace(state_anchor, state_code + state_anchor, 1)
    print("pool/proxy state added")
else:
    print("pool/proxy state exists")

with open(PATH, "w", encoding="utf-8") as f:
    f.write(src)
py_compile.compile(PATH, doraise=True)
print("class rewritten + syntax ok")
