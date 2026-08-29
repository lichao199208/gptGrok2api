from __future__ import annotations

import asyncio
import re
import time
import uuid
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import parse_qs, urlparse

from common.browser import browser_kwargs

_ALLOWED_HOSTS = {
    "accounts.x.ai",
    "auth.x.ai",
    "auth.grok.com",
    "auth.grokusercontent.com",
    "auth.grokipedia.com",
    "grok.com",
}
_COOKIE_DOMAINS = ("x.ai", "grok.com", "grokusercontent.com", "grokipedia.com")
_SESSION_TTL_SECONDS = 15 * 60


class XaiBrowserFlowError(RuntimeError):
    def __init__(self, message: str, *, stage: str, retryable: bool = False):
        super().__init__(message)
        self.stage = str(stage or "browser")
        self.retryable = bool(retryable)


@dataclass
class BrowserSession:
    session_id: str
    browser: Any
    page: Any
    signup_url: str
    created_at: float = field(default_factory=time.monotonic)
    touched_at: float = field(default_factory=time.monotonic)
    lock: asyncio.Lock = field(default_factory=asyncio.Lock)

    async def close(self) -> None:
        try:
            await self.browser.close()
        except Exception:
            pass


_sessions: dict[str, BrowserSession] = {}
_sessions_lock = asyncio.Lock()


def _allowed_url(value: object, *, stage: str) -> str:
    url = str(value or "").strip()
    parsed = urlparse(url)
    if parsed.scheme != "https" or (parsed.hostname or "").lower() not in _ALLOWED_HOSTS:
        raise XaiBrowserFlowError("xAI returned an unsafe browser navigation target", stage=stage)
    return url


async def _cookie_snapshot(page: Any) -> list[dict[str, Any]]:
    cookies = await page.context.cookies()
    result: list[dict[str, Any]] = []
    for cookie in cookies:
        domain = str(cookie.get("domain") or "").lower().lstrip(".")
        if not any(domain == suffix or domain.endswith(f".{suffix}") for suffix in _COOKIE_DOMAINS):
            continue
        result.append(
            {
                "name": str(cookie.get("name") or ""),
                "value": str(cookie.get("value") or ""),
                "domain": str(cookie.get("domain") or ""),
                "path": str(cookie.get("path") or "/"),
                "secure": bool(cookie.get("secure", True)),
                "httpOnly": bool(cookie.get("httpOnly", False)),
                "sameSite": str(cookie.get("sameSite") or ""),
                "expires": cookie.get("expires"),
            }
        )
    return result


def _cookie_value(cookies: list[dict[str, Any]], *names: str) -> str:
    preferred = sorted(
        cookies,
        key=lambda item: (
            1 if str(item.get("domain") or "").lstrip(".") == "x.ai" else 0,
            1 if str(item.get("path") or "/") == "/" else 0,
        ),
        reverse=True,
    )
    for name in names:
        for cookie in preferred:
            if cookie.get("name") == name and cookie.get("value"):
                return str(cookie["value"])
    return ""


async def _session(session_id: str) -> BrowserSession:
    async with _sessions_lock:
        current = _sessions.get(str(session_id or "").strip())
    if current is None:
        raise XaiBrowserFlowError("xAI browser session does not exist or has expired", stage="browser_session")
    current.touched_at = time.monotonic()
    return current


async def cleanup_expired_sessions() -> int:
    cutoff = time.monotonic() - _SESSION_TTL_SECONDS
    async with _sessions_lock:
        expired = [item for item in _sessions.values() if item.touched_at < cutoff]
        for item in expired:
            _sessions.pop(item.session_id, None)
    for item in expired:
        await item.close()
    return len(expired)


async def start_session(*, signup_url: str, proxy: str = "", timeout_s: float = 120) -> dict[str, Any]:
    import cloakbrowser

    await cleanup_expired_sessions()
    safe_signup_url = _allowed_url(signup_url, stage="browser_start")
    kwargs = browser_kwargs("XAI", proxy or None)
    browser = await cloakbrowser.launch_async(**kwargs)
    try:
        page = await browser.new_page()
        await page.goto(
            safe_signup_url,
            wait_until="domcontentloaded",
            timeout=max(15_000, min(180_000, int(timeout_s * 1000))),
        )
        session_id = f"xai-{uuid.uuid4().hex}"
        current = BrowserSession(session_id=session_id, browser=browser, page=page, signup_url=safe_signup_url)
        async with _sessions_lock:
            _sessions[session_id] = current
        return {"ok": True, "session_id": session_id, "url": page.url}
    except Exception:
        await browser.close()
        raise


async def _pause(page: Any, milliseconds: int) -> None:
    try:
        await page.wait_for_timeout(milliseconds)
    except Exception:
        await asyncio.sleep(milliseconds / 1000)


async def _body_text(page: Any) -> str:
    try:
        return str(await page.locator("body").inner_text(timeout=3_000) or "")
    except Exception:
        return ""


async def _first_visible(page: Any, selectors: tuple[str, ...]) -> Any | None:
    for selector in selectors:
        try:
            locator = page.locator(selector)
            count = await locator.count()
            for index in range(count):
                candidate = locator.nth(index)
                if await candidate.is_visible():
                    return candidate
        except Exception:
            continue
    return None


async def _fill_first(page: Any, selectors: tuple[str, ...], value: str) -> bool:
    field = await _first_visible(page, selectors)
    if field is None:
        return False
    await field.fill(str(value))
    return True


async def _set_native_input_value(page: Any, selector: str, value: str) -> bool:
    return bool(
        await page.evaluate(
            """([selector, value]) => {
              const field = document.querySelector(selector);
              if (!(field instanceof HTMLInputElement)) return false;
              const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set;
              if (setter) setter.call(field, value); else field.value = value;
              field.dispatchEvent(new Event('input', {bubbles: true}));
              field.dispatchEvent(new Event('change', {bubbles: true}));
              return true;
            }""",
            [selector, str(value)],
        )
    )


async def _click_submit(page: Any, names: tuple[str, ...]) -> bool:
    if await _click_any(page, names):
        return True
    button = await _first_visible(page, ('button[type="submit"]', 'input[type="submit"]'))
    if button is None:
        return False
    await button.click(timeout=5_000, force=True)
    return True


async def _wait_for_email_verification(page: Any, timeout_s: float = 30) -> None:
    deadline = asyncio.get_running_loop().time() + timeout_s
    while asyncio.get_running_loop().time() < deadline:
        body = await _body_text(page)
        if re.search(r"验证您的邮箱|验证码|Verify your email|verification code", body, re.I):
            return
        if await _first_visible(
            page,
            ('input[autocomplete="one-time-code"]', 'input[name="code"]', 'input[inputmode="numeric"]'),
        ):
            return
        if re.search(r"邮箱.*已被使用|无法注册|请求过于频繁|already.*email|too many", body, re.I):
            raise XaiBrowserFlowError(body[:300], stage="send_email")
        await _pause(page, 250)
    raise XaiBrowserFlowError("xAI 中文邮箱验证页面未出现", stage="send_email", retryable=True)


async def send_email(session_id: str, email: str) -> dict[str, Any]:
    current = await _session(session_id)
    async with current.lock:
        page = current.page
        await _click_any(page, ("全部拒绝", "接受全部", "Reject all", "Accept all"))
        email_selectors = ('input[type="email"]', 'input[name="email"]', 'input[autocomplete="email"]')
        email_field = await _first_visible(page, email_selectors)
        clicked_toggle = False
        deadline = asyncio.get_running_loop().time() + 20
        while email_field is None and asyncio.get_running_loop().time() < deadline:
            if not clicked_toggle:
                if await _click_any(page, ("使用邮箱注册", "Sign up with email")):
                    clicked_toggle = True
            await _pause(page, 500)
            email_field = await _first_visible(page, email_selectors)
        if email_field is None:
            try:
                await page.screenshot(path=f"/tmp/xai-sendemail-{current.session_id}.png", full_page=True)
            except Exception:
                pass
            raise XaiBrowserFlowError("未找到注册邮箱输入框", stage="send_email")
        await email_field.fill(str(email).strip())
        if not await _click_submit(page, ("注册", "继续", "Sign up", "Continue")):
            raise XaiBrowserFlowError("未找到邮箱注册提交按钮", stage="send_email")
        await _wait_for_email_verification(page)
        return {"ok": True, "grpc_status": "0", "url": page.url}


async def _fill_verification_code(page: Any, code: str) -> bool:
    normalized = re.sub(r"[^A-Za-z0-9]", "", str(code or ""))
    selectors = (
        'input[autocomplete="one-time-code"]',
        'input[name="code"]',
        'input[inputmode="numeric"]',
        'input[inputmode="text"]',
        'input[type="text"]',
    )
    for selector in selectors:
        visible: list[Any] = []
        try:
            locator = page.locator(selector)
            for index in range(await locator.count()):
                field = locator.nth(index)
                if await field.is_visible():
                    visible.append(field)
        except Exception:
            continue
        if not visible:
            continue
        if len(visible) == 1:
            await visible[0].fill(normalized)
            return True
        for field, character in zip(visible, normalized):
            await field.fill(character)
        return len(visible) >= len(normalized)
    return False


async def _wait_for_profile_form(page: Any, timeout_s: float = 30) -> None:
    deadline = asyncio.get_running_loop().time() + timeout_s
    last_body = ""
    while asyncio.get_running_loop().time() < deadline:
        if await _first_visible(page, ('input[name="givenName"]', 'input[autocomplete="given-name"]')):
            return
        last_body = await _body_text(page)
        if re.search(r"验证码.*(无效|错误|过期)|invalid.*code|expired.*code", last_body, re.I):
            raise XaiBrowserFlowError(last_body[:300], stage="verify_email")
        await _pause(page, 250)
    detail = re.sub(r"\s+", " ", last_body).strip()[:240]
    raise XaiBrowserFlowError(
        f"邮箱验证码提交后未进入资料页 (url={page.url}){': ' + detail if detail else ''}",
        stage="verify_email",
        retryable=True,
    )


async def _confirm_email_submission_if_needed(page: Any) -> bool:
    profile_selectors = ('input[name="givenName"]', 'input[autocomplete="given-name"]')
    confirm_names = ("确认邮箱", "验证邮箱", "Confirm email", "Verify email")
    for attempt in range(20):
        if await _first_visible(page, profile_selectors):
            return False
        if attempt >= 4 and await _click_any(page, confirm_names):
            return True
        await _pause(page, 250)
    return False


async def verify_email(session_id: str, email: str, code: str) -> dict[str, Any]:
    current = await _session(session_id)
    async with current.lock:
        del email
        page = current.page
        if not await _fill_verification_code(page, code):
            raise XaiBrowserFlowError("未找到邮箱验证码输入框", stage="verify_email")
        await _confirm_email_submission_if_needed(page)
        try:
            await _wait_for_profile_form(page)
        except Exception:
            try:
                await page.screenshot(path=f"/tmp/xai-verify-{current.session_id}.png", full_page=True)
            except Exception:
                pass
            raise
        return {"ok": True, "grpc_status": "0", "url": page.url}


async def _inject_turnstile_token(page: Any, token: str) -> bool:
    if not token:
        return False
    return bool(
        await page.evaluate(
            """token => {
              const fields = [...document.querySelectorAll(
                'input[name="cf-turnstile-response"], textarea[name="cf-turnstile-response"], input[name="g-recaptcha-response"]'
              )];
              if (!fields.length) {
                const field = document.createElement('input');
                field.type = 'hidden';
                field.name = 'cf-turnstile-response';
                (document.querySelector('form') || document.body).appendChild(field);
                fields.push(field);
              }
              for (const field of fields) {
                const proto = field instanceof HTMLTextAreaElement
                  ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
                const setter = Object.getOwnPropertyDescriptor(proto, 'value')?.set;
                if (setter) setter.call(field, token); else field.value = token;
                field.dispatchEvent(new Event('input', {bubbles: true}));
                field.dispatchEvent(new Event('change', {bubbles: true}));
              }
              return fields.length > 0;
            }""",
            token,
        )
    )


async def _solve_turnstile_in_page(page: Any, fallback_token: str, timeout_s: float = 35) -> str:
    from turnstile.solve import _wait_for_turnstile_token

    deadline = time.monotonic() + max(5.0, float(timeout_s))
    token, clicks, error = await _wait_for_turnstile_token(page, deadline)
    if token:
        return token
    if fallback_token and await _inject_turnstile_token(page, fallback_token):
        return fallback_token
    raise XaiBrowserFlowError(
        f"同一注册浏览器未完成 Turnstile（clicks={clicks}, error={error or 'none'}）",
        stage="turnstile",
        retryable=True,
    )


async def _signup_succeeded(page: Any) -> bool:
    parsed = urlparse(str(page.url or ""))
    body = await _body_text(page)
    if parsed.path.startswith("/account") and "/sign-" not in parsed.path:
        return True
    return bool(re.search(r"管理您的账户|欢迎|Manage your account|Welcome", body, re.I)) and not bool(
        re.search(r"完成注册|Complete your sign up", body, re.I)
    )


async def _form_diagnostics(page: Any) -> str:
    try:
        state = await page.evaluate(
            """() => {
              const submit = document.querySelector('button[type="submit"], input[type="submit"]');
              const token = document.querySelector('[name="cf-turnstile-response"], [name="g-recaptcha-response"]');
              const fields = ['givenName', 'familyName', 'password'].map(name => {
                const field = document.querySelector(`input[name="${name}"]`);
                return `${name}:${field ? field.value.length : -1}:${field?.getAttribute('aria-invalid') || ''}`;
              });
              return {
                submitDisabled: Boolean(submit?.disabled),
                tokenLength: token?.value?.length || 0,
                fields,
                invalidCount: document.querySelectorAll(':invalid').length,
              };
            }"""
        )
    except Exception:
        return "diagnostics=unavailable"
    return (
        f"submit_disabled={bool((state or {}).get('submitDisabled'))}, "
        f"turnstile_length={int((state or {}).get('tokenLength') or 0)}, "
        f"fields={','.join((state or {}).get('fields') or [])}, "
        f"invalid={int((state or {}).get('invalidCount') or 0)}"
    )


async def _wait_for_signup_success(
    page: Any,
    turnstile_token: str,
    profile: tuple[str, str, str],
    timeout_s: float = 60,
) -> None:
    deadline = asyncio.get_running_loop().time() + timeout_s
    retry_at = {12, 28, 44}
    iteration = 0
    last_body = ""
    while asyncio.get_running_loop().time() < deadline:
        if await _signup_succeeded(page):
            return
        last_body = await _body_text(page)
        if re.search(r"邮箱.*已被使用|账户.*已存在|already exists|already in use", last_body, re.I):
            raise XaiBrowserFlowError(last_body[:300], stage="create_account")
        if iteration in retry_at and re.search(r"完成注册|Complete your sign up", last_body, re.I):
            given_name, family_name, password = profile
            await _fill_first(page, ('input[name="givenName"]',), given_name)
            await _fill_first(page, ('input[name="familyName"]',), family_name)
            await _fill_first(page, ('input[name="password"]', 'input[type="password"]'), password)
            await _set_native_input_value(page, 'input[name="password"]', password)
            await _solve_turnstile_in_page(page, turnstile_token, timeout_s=20)
            await _pause(page, 400)
            await _click_submit(page, ("完成注册", "创建账户", "继续", "Complete sign up", "Create account", "Continue"))
        iteration += 1
        await _pause(page, 750)
    detail = re.sub(r"\s+", " ", last_body).strip()[:240]
    diagnostics = await _form_diagnostics(page)
    raise XaiBrowserFlowError(
        f"点击“完成注册”后未进入账户页 ({diagnostics}){': ' + detail if detail else ''}",
        stage="create_account",
        retryable=True,
    )


async def signup(
    session_id: str,
    *,
    email: str,
    password: str,
    code: str,
    given_name: str,
    family_name: str,
    turnstile_token: str,
    action_id: str,
    next_router_state_tree: str,
) -> dict[str, Any]:
    current = await _session(session_id)
    async with current.lock:
        del email, code, action_id, next_router_state_tree
        page = current.page
        fields = (
            (("input[name=\"givenName\"]", 'input[autocomplete="given-name"]'), given_name, "名字"),
            (("input[name=\"familyName\"]", 'input[autocomplete="family-name"]'), family_name, "姓氏"),
            (("input[name=\"password\"]", 'input[type="password"]', 'input[autocomplete="new-password"]'), password, "密码"),
        )
        for selectors, value, label in fields:
            if not await _fill_first(page, selectors, value):
                raise XaiBrowserFlowError(f"未找到{label}输入框", stage="create_account")
        await _set_native_input_value(page, 'input[name="password"]', password)
        await _pause(page, 1_000)
        await _solve_turnstile_in_page(page, turnstile_token)
        await _pause(page, 500)
        if not await _click_submit(
            page,
            ("完成注册", "创建账户", "继续", "提交", "Complete sign up", "Create account", "Continue", "Submit"),
        ):
            raise XaiBrowserFlowError("未找到“完成注册”按钮", stage="create_account")
        try:
            await _wait_for_signup_success(page, turnstile_token, (given_name, family_name, password))
        except Exception:
            try:
                await page.screenshot(path=f"/tmp/xai-signup-{current.session_id}.png", full_page=True)
            except Exception:
                pass
            raise
        cookies = await _cookie_snapshot(page)
        sso = _cookie_value(cookies, "sso", "sso-rw")
        if not sso:
            raise XaiBrowserFlowError("注册已进入账户页，但浏览器中没有 xAI SSO Cookie", stage="cookie_setter", retryable=True)
        return {
            "ok": True,
            "sso": sso,
            "sso_rw": _cookie_value(cookies, "sso-rw"),
            "cookies": cookies,
            "redirect_url": page.url,
            "setter_hops": 0,
        }


async def _click_any(page: Any, names: tuple[str, ...]) -> bool:
    for name in names:
        try:
            button = page.get_by_role("button", name=name, exact=True)
            if await button.count() and await button.first.is_visible():
                await button.first.click(timeout=2_000)
                return True
        except Exception:
            pass
        try:
            locator = page.locator(
                f'button:has-text("{name}"), [role="button"]:has-text("{name}"), '
                f'input[type="submit"][value="{name}"]'
            )
            if await locator.count() and await locator.first.is_visible():
                await locator.first.click(timeout=2_000)
                return True
        except Exception:
            pass
    return False


def _device_authorization_outcome(url: str, body: str) -> tuple[str, str]:
    """Classify the device result without treating a denied done page as success."""
    parsed = urlparse(url)
    query_error = parse_qs(parsed.query).get("error", [""])[0].strip().lower()
    if query_error:
        return "failed", f"device_verify_failed:{query_error}"

    normalized_body = str(body or "").lower()
    if any(
        marker in normalized_body
        for marker in (
            "access denied",
            "authorization denied",
            "device authorization was denied",
            "request was denied",
            "拒绝访问",
            "授权已拒绝",
            "未获授权",
        )
    ):
        return "failed", "device_verify_failed:access_denied"

    if "/oauth2/device/done" in parsed.path or any(
        marker in normalized_body
        for marker in (
            "device authorized",
            "device has been authorized",
            "you can close this window",
            "已授权",
            "设备已授权",
            "可以关闭",
        )
    ):
        return "confirmed", "confirmed"
    return "pending", ""


async def authorize_device(
    session_id: str,
    *,
    verification_url: str,
    user_code: str,
    timeout_s: float,
) -> dict[str, Any]:
    current = await _session(session_id)
    async with current.lock:
        target = _allowed_url(verification_url, stage="device_consent")
        goto_error = ""
        for _goto_attempt in range(3):
            try:
                await current.page.goto(target, wait_until="domcontentloaded", timeout=60_000)
                goto_error = ""
                break
            except Exception as _exc:
                _msg = str(_exc)
                if "ERR_ABORTED" not in _msg:
                    raise
                goto_error = _msg
                await _pause(current.page, 1500)
        if goto_error:
            try:
                await current.page.wait_for_load_state("domcontentloaded", timeout=10_000)
            except Exception:
                pass
        code_input = current.page.locator(
            'input[name="user_code"], input[autocomplete="one-time-code"], input[inputmode="text"]'
        )
        if await code_input.count() and await code_input.first.is_visible():
            field = code_input.first
            value = ""
            try:
                value = await field.input_value()
            except Exception:
                pass
            if user_code.replace("-", "") not in value.replace("-", ""):
                await field.fill("")
                await field.press_sequentially(user_code)
            await _click_any(current.page, ("Continue", "Next", "Submit", "Verify", "继续", "下一步", "提交", "验证"))

        deadline = asyncio.get_running_loop().time() + max(15.0, float(timeout_s))
        consent_clicked = False
        reason = ""
        while asyncio.get_running_loop().time() < deadline:
            url = current.page.url
            parsed = urlparse(url)
            host = (parsed.hostname or "").lower()
            if parsed.scheme != "https" or host not in _ALLOWED_HOSTS:
                reason = "unsafe_page"
                break
            try:
                body = (await current.page.locator("body").inner_text(timeout=3_000)).lower()
            except Exception:
                body = ""
            try:
                title = (await current.page.title()).lower()
            except Exception:
                title = ""
            outcome, outcome_reason = _device_authorization_outcome(url, body)
            if outcome == "failed":
                reason = outcome_reason
                break
            if outcome == "confirmed":
                cookies = await _cookie_snapshot(current.page)
                return {"ok": True, "reason": "confirmed", "final_url": url, "cookies": cookies}
            if "rate limit" in body or "too many requests" in body:
                reason = "rate_limited"
                break
            if ("attention required" in title and "cloudflare" in title) or "cloudflare ray id" in body:
                reason = "challenge_required"
                break
            if any(marker in body for marker in ("continue with email", "sign in with email", "使用邮箱登录")):
                reason = "login_required_session_lost"
                break
            if await _click_any(current.page, ("Reject all", "Reject All", "Accept all", "Accept All", "全部拒绝", "拒绝全部")):
                await current.page.wait_for_timeout(400)
                continue
            if "/oauth2/device/consent" in parsed.path or any(
                marker in body for marker in ("authorize grok", "allow access", "wants to access", "授权 grok", "请求访问")
            ):
                if not consent_clicked and await _click_any(
                    current.page,
                    ("Allow", "Authorize", "Approve", "Continue", "允许", "授权", "同意", "继续"),
                ):
                    consent_clicked = True
                    await current.page.wait_for_timeout(900)
                    continue
                await current.page.wait_for_timeout(500)
                continue
            if await _click_any(
                current.page,
                ("Allow", "Authorize", "Approve", "Confirm", "Continue", "允许", "授权", "确认", "继续"),
            ):
                await current.page.wait_for_timeout(800)
                continue
            await current.page.wait_for_timeout(700)
        raise XaiBrowserFlowError(
            f"xAI browser device authorization failed: {reason or 'confirmation_timeout'}",
            stage="approve",
            retryable=reason in {"", "challenge_required", "rate_limited"},
        )


async def close_session(session_id: str) -> bool:
    async with _sessions_lock:
        current = _sessions.pop(str(session_id or "").strip(), None)
    if current is None:
        return False
    await current.close()
    return True


async def close_all_sessions() -> None:
    async with _sessions_lock:
        current = list(_sessions.values())
        _sessions.clear()
    for item in current:
        await item.close()


__all__ = [
    "XaiBrowserFlowError",
    "authorize_device",
    "cleanup_expired_sessions",
    "close_all_sessions",
    "close_session",
    "send_email",
    "signup",
    "start_session",
    "verify_email",
]
