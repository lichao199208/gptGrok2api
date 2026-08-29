import asyncio
import json
import os
import sys

os.environ.setdefault("XAI_HEADLESS", "1")

EMAIL = sys.argv[1] if len(sys.argv) > 1 else ""
PASSWORD = sys.argv[2] if len(sys.argv) > 2 else ""

events = []
cookies_log = []


async def main():
    import cloakbrowser
    browser = await cloakbrowser.launch_async(humanize=True, headless=True)
    try:
        context = await browser.new_context(
            user_agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36",
            viewport={"width": 1366, "height": 900},
            locale="en-US",
        )
        page = await context.new_page()
    except Exception as e:
        print("new_context error:", type(e).__name__, str(e)[:200])
        page = await browser.new_page()

    async def on_request(req):
        body = ""
        try:
            if req.method in ("POST", "PUT", "PATCH"):
                body = (req.post_data or "")[:800]
        except Exception:
            pass
        events.append({"type": "req", "method": req.method, "url": req.url[:400], "body": body})

    async def on_response(resp):
        events.append({
            "type": "resp", "status": resp.status,
            "url": resp.url[:400],
            "location": (resp.headers.get("location") or "")[:500],
            "setcookie": (resp.headers.get("set-cookie") or "")[:120],
        })

    page.on("request", on_request)
    page.on("response", on_response)

    try:
        resp = await page.goto("https://www.mail.com/", timeout=45000, wait_until="domcontentloaded")
        print("NAV status:", resp.status if resp else None, "final:", page.url)
    except Exception as e:
        print("NAV error:", type(e).__name__, str(e)[:200])
    await page.wait_for_timeout(3000)

    # consent if present
    for sel in ["button:has-text('Continue to Mail.com')", "#accept-button", "button:has-text('Accept')"]:
        try:
            loc = page.locator(sel)
            if await loc.count() > 0 and await loc.first.is_visible():
                await loc.first.click(timeout=5000)
                print("CLICKED consent:", sel)
                await page.wait_for_timeout(3000)
                break
        except Exception:
            continue

    print("URL:", page.url)
    # dump login form
    form_info = await page.eval_on_selector_all("#mailcom-login-form, form[action*='login'], form[action*='halogin'], form",
        "forms => forms.map(f => ({action: f.action, id: f.id, inputs: Array.from(f.querySelectorAll('input')).map(i => ({name: i.name, value: (i.value||'').slice(0,120)}))}))")
    print("FORMS:", json.dumps(form_info, ensure_ascii=False)[:3000])

    # fill credentials
    try:
        await page.fill("#login-email", EMAIL, timeout=5000)
        await page.fill("#login-password", PASSWORD, timeout=5000)
        print("filled credentials")
    except Exception as e:
        print("fill error:", type(e).__name__, str(e)[:200])
        try:
            await page.fill("input[name=username]", EMAIL, timeout=3000)
            await page.fill("input[name=password]", PASSWORD, timeout=3000)
            print("filled via name selectors")
        except Exception as e2:
            print("fill2 error:", type(e2).__name__, str(e2)[:200])

    # find and click the login submit button
    submitted = False
    for sel in ["#login-form-submit", "button[type=submit]", "input[type=submit]", "button:has-text('Log in')", "button:has-text('Login')"]:
        try:
            loc = page.locator(sel)
            if await loc.count() > 0 and await loc.first.is_visible():
                await loc.first.click(timeout=5000)
                print("CLICKED submit:", sel)
                submitted = True
                break
        except Exception:
            continue
    if not submitted:
        try:
            await page.keyboard.press("Enter")
            print("pressed Enter")
        except Exception as e:
            print("enter error:", str(e)[:100])

    # wait and follow the flow
    for _ in range(12):
        await page.wait_for_timeout(3000)
        print("TICK url:", page.url[:300], "title:", (await page.title())[:80])
        if "login" not in page.url and "mail.com" in page.url:
            # maybe logged in / token issued
            pass
        if "webmail" in page.url or "mail.mail.com" in page.url or "inbox" in page.url.lower():
            break

    print("=== EVENTS (mail.com + sso + oauth only) ===")
    seen = set()
    for ev in events:
        url = ev["url"]
        if not any(h in url for h in ("mail.com", "sso", "oauth", "united-internet", "1und1", "gmw")):
            continue
        key = (ev["type"], ev["method"] if ev["type"] == "req" else "", ev["status"] if ev["type"] == "resp" else "", url)
        if key in seen:
            continue
        seen.add(key)
        print(json.dumps(ev, ensure_ascii=False)[:600])
    await page.screenshot(path="/tmp/mailcom_login_result.png", full_page=True)
    await browser.close()


asyncio.run(main())
