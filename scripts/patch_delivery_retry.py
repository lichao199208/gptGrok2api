#!/usr/bin/env python3
"""openai_register.py: allow per-provider delivery-timeout retries with fresh emails."""
import py_compile

PATH = "/opt/gptGrok2api/services/register/openai_register.py"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

# 1. constant
old_const = "OPENAI_EXISTING_EMAIL_RETRY_LIMIT = 20"
new_const = "OPENAI_EXISTING_EMAIL_RETRY_LIMIT = 20\nMAIL_DELIVERY_RETRY_LIMIT = 3"
if "MAIL_DELIVERY_RETRY_LIMIT" not in src:
    src = src.replace(old_const, new_const, 1)
    print("const +")
else:
    print("const exists")

# 2. _register_with_fresh_email: per-provider delivery budget
old_loop = """def _register_with_fresh_email(index: int) -> tuple[PlatformRegistrar, dict]:
    skipped = 0
    delivery_failures = 0
    excluded_provider_refs: set[str] = set()
    provider_count = max(1, _enabled_mail_provider_count())
    while True:
        registrar = PlatformRegistrar(config["proxy"])
        registrar.excluded_mail_provider_refs = set(excluded_provider_refs)
        try:
            return registrar, registrar.register(index)
        except OpenAIMailboxDeliveryTimeout as error:
            registrar.close()
            delivery_failures += 1
            if error.provider_ref:
                excluded_provider_refs.add(error.provider_ref)
            remaining = provider_count - max(delivery_failures, len(excluded_provider_refs))
            if remaining <= 0:
                raise RuntimeError(
                    f"所有启用邮箱来源均未收到 ChatGPT 验证码，最后失败来源：{error.label}"
                ) from error
            step(
                index,
                f"{error.label} 未收到验证码，正在切换下一个邮箱来源（剩余 {remaining} 个）",
                "yellow",
            )"""
new_loop = """def _register_with_fresh_email(index: int) -> tuple[PlatformRegistrar, dict]:
    skipped = 0
    delivery_failures = 0
    delivery_failures_by_provider: dict[str, int] = {}
    excluded_provider_refs: set[str] = set()
    provider_count = max(1, _enabled_mail_provider_count())
    while True:
        registrar = PlatformRegistrar(config["proxy"])
        registrar.excluded_mail_provider_refs = set(excluded_provider_refs)
        try:
            return registrar, registrar.register(index)
        except OpenAIMailboxDeliveryTimeout as error:
            registrar.close()
            delivery_failures += 1
            provider_ref = str(error.provider_ref or "").strip()
            if provider_ref:
                delivery_failures_by_provider[provider_ref] = delivery_failures_by_provider.get(provider_ref, 0) + 1
                if delivery_failures_by_provider[provider_ref] >= MAIL_DELIVERY_RETRY_LIMIT:
                    excluded_provider_refs.add(provider_ref)
            remaining = provider_count - len(excluded_provider_refs)
            if remaining <= 0:
                raise RuntimeError(
                    f"所有启用邮箱来源均多次未收到 ChatGPT 验证码（同一来源最多重试 {MAIL_DELIVERY_RETRY_LIMIT} 次），任务停止；最后失败来源：{error.label}"
                ) from error
            same_provider_attempts = delivery_failures_by_provider.get(provider_ref, 0)
            step(
                index,
                f"{error.label} 未收到验证码，自动更换邮箱重试（同来源第 {same_provider_attempts} 次，剩余来源 {remaining} 个）",
                "yellow",
            )"""
if "delivery_failures_by_provider" not in src:
    if old_loop in src:
        src = src.replace(old_loop, new_loop, 1)
        print("retry loop patched")
    else:
        print("retry loop anchor not found")
else:
    print("retry loop exists")

with open(PATH, "w", encoding="utf-8") as f:
    f.write(src)
py_compile.compile(PATH, doraise=True)
print("OK")
