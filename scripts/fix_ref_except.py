#!/usr/bin/env python3
import py_compile

PATH = "/opt/gptGrok2api/services/register/openai_register.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

anchor = """        except OpenAIEmailAlreadyRegistered:
            # OpenAI has positively identified this address as an existing
            # account.  Keep it out of future GPT signup claims even though
            # this worker did not create a new account with it.
            mail_provider.mark_mailbox_result(mailbox, success=True)
            raise
        except Exception as error:
            mail_provider.mark_mailbox_result(mailbox, success=False, error=error)
            raise"""
new = """        except OpenAIEmailAlreadyRegistered:
            # OpenAI has positively identified this address as an existing
            # account.  Keep it out of future GPT signup claims even though
            # this worker did not create a new account with it.
            mail_provider.mark_mailbox_result(mailbox, success=True)
            raise
        except Exception as error:
            if _is_openai_invalid_auth_step(error):
                step(index, f"{email} 授权状态异常（invalid_auth_step），判定为邮箱已使用，自动更换子号", "yellow")
                mail_provider.mark_mailbox_result(mailbox, success=True)
                raise OpenAIEmailAlreadyRegistered(email, reason="invalid_auth_step") from error
            mail_provider.mark_mailbox_result(mailbox, success=False, error=error)
            raise"""

if anchor in src:
    src = src.replace(anchor, new, 1)
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    py_compile.compile(PATH, doraise=True)
    print("ref except patched")
else:
    print("anchor not found")
