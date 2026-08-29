import asyncio
import json
import os
import sys

os.environ.setdefault("XAI_HEADLESS", "1")

EMAIL = sys.argv[1] if len(sys.argv) > 1 else ""
PASSWORD = sys.argv[2] if len(sys.argv) > 2 else ""

events = []


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
                body = (req.post_data or "")[:900]
        except Exception:
            pass
        events.append({"type": "req", "method": req.method, "url": req.url[:450], "body": body})

    async def on_response(resp):
        events.append({
            "type": "resp", "status": resp.status,
            "url": resp.url[:450],
            "location": (resp.headers.get("location") or "")[:600],
            "setcookie": (resp.headers.get("set-cookie") or "")[:150],
        })

    page.on("request", on_request)
    page.on("response", on_response)

    # 1. consent flow
    try:
        await page.goto("https://www.mail.com/", timeout=45000, wait_until="domcontentloaded")
    except Exception as e:
        print("NAV0 error:", type(e).__name__, str(e)[:150])
    await page.wait_for_timeout(2500)
    for sel in ["button:has-text('Continue to Mail.com')"]:
        try:
            loc = page.locator(sel)
            if await loc.count() > 0 and await loc.first.is_visible():
                await loc.first.click(timeout=5000)
                await page.wait_for_timeout(2500)
                break
        except Exception:
            continue

    # 2. go to the login page directly
    try:
        resp = await page.goto("https://login.mail.com/login", timeout=45000, wait_until="domcontentloaded")
        print("LOGIN PAGE status:", resp.status if resp else None, "url:", page.url[:300])
    except Exception as e:
        print("LOGIN PAGE error:", type(e).__name__, str(e)[:200])
    await page.wait_for_timeout(4000)
    print("TITLE:", (await page.title())[:120])
    html = await page.content()
    print("HTML len:", len(html), "head:", html[:300].replace("\n", " ")[:300])

    # dump inputs of the first form
    form_info = await page.eval_on_selector_all("form", "forms => forms.slice(0,3).map(f => ({action: f.action, id: f.id, inputs: Array.from(f.querySelectorAll('input')).map(i => ({name: i.name, type: i.type, value: (i.value||'').slice(0,150)}))}))")
    print("FORMS:", json.dumps(form_info, ensure_ascii=False)[:3000])

    # try to fill
    filled = False
    for name, value in [("username", EMAIL), ("password", PASSWORD)]:
        try:
            await page.fill(f"input[name={name}]", value, timeout=4000)
            filled = True
        except Exception as e:
            print(f"fill {name} err:", type(e).__name__, str(e)[:120])
    if filled:
        for sel in ["button[type=submit]", "input[type=submit]", "button:has-text('Log in')", "button:has-text('Login')"]:
            try:
                loc = page.locator(sel)
                if await loc.count() > 0 and await loc.first.is_visible():
                    await loc.first.click(timeout=5000)
                    print("CLICKED:", sel)
                    break
            except Exception:
                continue
        else:
            await page.keyboard.press("Enter")
            print("pressed Enter")

    for _ in range(15):
        await page.wait_for_timeout(3000)
        print("TICK url:", page.url[:300], "title:", (await page.title())[:70])
        if any(k in page.url for k in ("webmail", "mail.mail.com", "inbox", "authorize", "oauth", "bridge")):
            break

    print("=== EVENTS (relevant hosts) ===")
    seen = set()
    for ev in events:
        url = ev["url"]
        if not any(h in url for h in ("login.mail.com", "oauth", "bridge", "webmail", "mail.mail.com", "sso", "token", "authorize")):
            continue
        key = (ev["type"], ev.get("method") or "", ev.get("status") or "", url)
        if key in seen:
            continue
        seen.add(key)
        print(json.dumps(ev, ensure_ascii=False)[:700])
    await page.screenshot(path="/tmp/mailcom_login2.png", full_page=True)
    await browser.close()


asyncio.run(main())
