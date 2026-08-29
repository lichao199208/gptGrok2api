#!/usr/bin/env python3
"""Add temporary diagnostics to openai_register.py (live copy in container)."""
PATH = "/opt/gptGrok2api/services/register/openai_register.py.live"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

# 1. register(): log otp_final_url + callback_code decision
old = """                otp_final_url = self._validate_otp(code, index)
                callback_params = extract_oauth_callback_params_from_url(otp_final_url)
                callback_code = str((callback_params or {}).get("code") or "").strip()
                if callback_code:
                    self.platform_auth_code = callback_code

                if self.platform_auth_code:"""
new = """                otp_final_url = self._validate_otp(code, index)
                callback_params = extract_oauth_callback_params_from_url(otp_final_url)
                callback_code = str((callback_params or {}).get("code") or "").strip()
                step(index, f"DIAG otp_final_url={otp_final_url[:200]} callback_code={'yes' if callback_code else 'no'}", "yellow")
                if callback_code:
                    self.platform_auth_code = callback_code

                if self.platform_auth_code:"""
if old in src:
    src = src.replace(old, new, 1)
    print("diag register() +")
else:
    print("register anchor not found")

# 2. _create_account: dump detail on 400
old2 = """            data = _response_json(resp) if resp is not None else {}
            if data.get("message") == "Failed to create account. Please try again.":
                step(index, "创建账号失败提示: 邮箱域名很可能因滥用被封禁，请更换邮箱域名", "yellow")
            detail = f", detail={json.dumps(data, ensure_ascii=False)}" if data else ""
            raise RuntimeError(error or f"create_account_http_{getattr(resp, 'status_code', 'unknown')}{detail}")"""
new2 = """            data = _response_json(resp) if resp is not None else {}
            if data.get("message") == "Failed to create account. Please try again.":
                step(index, "创建账号失败提示: 邮箱域名很可能因滥用被封禁，请更换邮箱域名", "yellow")
            detail = f", detail={json.dumps(data, ensure_ascii=False)}" if data else ""
            if isinstance(data, dict) and isinstance(data.get("error"), dict) and data["error"].get("code") == "invalid_auth_step":
                err = data["error"]
                cookie_names = []
                try:
                    jar = getattr(self.session.cookies, "jar", self.session.cookies)
                    for c in list(jar):
                        d = str(getattr(c, "domain", "") or "")
                        if "auth.openai.com" in d:
                            cookie_names.append(str(getattr(c, "name", "") or ""))
                except Exception:
                    pass
                step(
                    index,
                    f"DIAG invalid_auth_step: redirect_uri={err.get('redirect_uri')} "
                    f"platform_auth_code={'yes' if self.platform_auth_code else 'no'} "
                    f"last_otp_continue_url={str(getattr(self, 'last_otp_continue_url', '') or '')[:120]} "
                    f"auth_cookies={cookie_names} "
                    f"final_url={str(getattr(self, '_platform_authorize_final_url', '') or '')[:120]}",
                    "yellow",
                )
            raise RuntimeError(error or f"create_account_http_{getattr(resp, 'status_code', 'unknown')}{detail}")"""
if old2 in src:
    src = src.replace(old2, new2, 1)
    print("diag _create_account +")
else:
    print("create_account anchor not found")

with open(PATH, "w", encoding="utf-8") as f:
    f.write(src)
import py_compile
py_compile.compile(PATH, doraise=True)
print("OK")
