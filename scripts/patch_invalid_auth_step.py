#!/usr/bin/env python3
"""Patch openai_register.py: convert invalid_auth_step into auto-regen (email-used)."""
import py_compile

PATH = "/opt/gptGrok2api/services/register/openai_register.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

original = src

# 1. helper after OpenAIEmailAlreadyRegistered class
anchor_class = '        super().__init__(message)\n\n\nclass OpenAIMailboxDeliveryTimeout(RuntimeError):'
helper = '''        super().__init__(message)


def _is_openai_invalid_auth_step(error: Exception | str | None) -> bool:
    """create_account 返回 invalid_auth_step：OpenAI 判定当前会话处于登录态而非注册态。

    通常意味着该邮箱已被判为已有账号（或在 OTP 验证后授权上下文过期）。
    上层将其视为“邮箱已使用”，自动更换子号重试。
    """
    text = str(error or "").strip().lower()
    return "invalid_auth_step" in text or "invalid authorization step" in text


class OpenAIMailboxDeliveryTimeout(RuntimeError):'''
if "def _is_openai_invalid_auth_step" not in src:
    if anchor_class in src:
        src = src.replace(anchor_class, helper, 1)
        print("helper +")
    else:
        print("helper anchor not found")
else:
    print("helper exists")

# 2. PlatformRegistrar.register except: convert invalid_auth_step BEFORE deactivated check
anchor_platform = """        except Exception as error:
            if _is_openai_account_deactivated_error(error):
                mail_provider.mark_mailbox_result(mailbox, success=True)
                raise OpenAIEmailAlreadyRegistered(
                    email,
                    reason="account_deactivated",
                ) from error
            mail_provider.mark_mailbox_result(mailbox, success=False, error=error)
            raise"""
new_platform = """        except Exception as error:
            if _is_openai_invalid_auth_step(error):
                step(index, f"{email} 授权状态异常（invalid_auth_step），判定为邮箱已使用，自动更换子号", "yellow")
                mail_provider.mark_mailbox_result(mailbox, success=True)
                raise OpenAIEmailAlreadyRegistered(email, reason="invalid_auth_step") from error
            if _is_openai_account_deactivated_error(error):
                mail_provider.mark_mailbox_result(mailbox, success=True)
                raise OpenAIEmailAlreadyRegistered(
                    email,
                    reason="account_deactivated",
                ) from error
            mail_provider.mark_mailbox_result(mailbox, success=False, error=error)
            raise"""
if "_is_openai_invalid_auth_step(error):" not in src:
    if anchor_platform in src:
        src = src.replace(anchor_platform, new_platform, 1)
        print("platform register except +")
    else:
        print("platform except anchor not found")
else:
    print("platform except exists")

# 3. reference flow register except: same conversion
anchor_ref = """        except OpenAIEmailAlreadyRegistered:
            raise
        except Exception as error:
            if _is_openai_account_deactivated_error(error):
                mail_provider.mark_mailbox_result(mailbox, success=True)
                raise OpenAIEmailAlreadyRegistered(
                    email,
                    reason="account_deactivated",
                ) from error
            mail_provider.mark_mailbox_result(mailbox, success=False, error=error)
            raise"""
new_ref = """        except OpenAIEmailAlreadyRegistered:
            raise
        except Exception as error:
            if _is_openai_invalid_auth_step(error):
                step(index, f"{email} 授权状态异常（invalid_auth_step），判定为邮箱已使用，自动更换子号", "yellow")
                mail_provider.mark_mailbox_result(mailbox, success=True)
                raise OpenAIEmailAlreadyRegistered(email, reason="invalid_auth_step") from error
            if _is_openai_account_deactivated_error(error):
                mail_provider.mark_mailbox_result(mailbox, success=True)
                raise OpenAIEmailAlreadyRegistered(
                    email,
                    reason="account_deactivated",
                ) from error
            mail_provider.mark_mailbox_result(mailbox, success=False, error=error)
            raise"""
if anchor_ref in src and "invalid_auth_step(error):" in src and src.count("invalid_auth_step(error):") >= 2:
    print("ref except exists")
elif anchor_ref in src:
    src = src.replace(anchor_ref, new_ref, 1)
    print("ref except +")
else:
    print("ref except anchor not found (may differ)")

# 4. keep a concise diagnostic in _create_account on invalid_auth_step (replace verbose DIAG if present)
verbose_diag = """            if isinstance(data, dict) and isinstance(data.get("error"), dict) and data["error"].get("code") == "invalid_auth_step":
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
                )"""
if verbose_diag in src:
    src = src.replace(verbose_diag, "", 1)
    print("removed verbose DIAG")

if src == original:
    print("NO CHANGES")
else:
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    py_compile.compile(PATH, doraise=True)
    print("PATCH OK + syntax valid")
