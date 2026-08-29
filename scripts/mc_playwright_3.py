import asyncio
import json
import os

os.environ.setdefault("XAI_HEADLESS", "1")

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
        if "mail.com" in req.url:
            body = ""
            try:
                if req.method in ("POST", "PUT", "PATCH"):
                    body = (req.post_data or "")[:600]
            except Exception:
                pass
            events.append({"type": "req", "method": req.method, "url": req.url[:400], "body": body})

    async def on_response(resp):
        if "mail.com" in resp.url:
            events.append({
                "type": "resp", "status": resp.status,
                "url": resp.url[:400],
                "location": (resp.headers.get("location") or "")[:400],
            })

    page.on("request", on_request)
    page.on("response", on_response)

    try:
        resp = await page.goto("https://www.mail.com/", timeout=45000, wait_until="domcontentloaded")
        print("NAV status:", resp.status if resp else None, "final:", page.url)
    except Exception as e:
        print("NAV error:", type(e).__name__, str(e)[:200])
    await page.wait_for_timeout(4000)
    print("TITLE:", await page.title())

    # Look for consent buttons and click accept if present
    buttons = await page.eval_on_selector_all("button", "els => els.map(e => ({text: (e.innerText||'').trim().slice(0,60), id: e.id, cls: (e.className||'').slice(0,60)}))")
    print("BUTTONS:", json.dumps(buttons, ensure_ascii=False)[:2000])

    for sel in ["#accept-button", "#acceptAllButton", "button:has-text('Accept')", "button:has-text('ACCEPT')", "button:has-text('Agree')", "button:has-text('I agree')", "button:has-text('Allow all')", "button:has-text('OK')", "#tcf-consent-accept", "[data-testid*=accept]"]:
        try:
            loc = page.locator(sel)
            if await loc.count() > 0 and await loc.first.is_visible():
                await loc.first.click(timeout=5000)
                print("CLICKED consent:", sel)
                await page.wait_for_timeout(3000)
                break
        except Exception:
            continue

    print("AFTER CONSENT URL:", page.url)
    # now find login link/button
    links = await page.eval_on_selector_all("a", "els => els.map(e => e.href)")
    import re
    login_hrefs = [l for l in links if l and re.search(r"login|authorize|signin|sso", l, re.I)]
    print("LOGIN HREFS:", json.dumps(list(dict.fromkeys(login_hrefs)), ensure_ascii=False)[:2000])

    # click any login-ish button
    for sel in ["a:has-text('Log in')", "a:has-text('Login')", "a:has-text('Sign in')", "button:has-text('Log in')", "button:has-text('Login')", "[data-testid*=login]"]:
        try:
            loc = page.locator(sel)
            if await loc.count() > 0 and await loc.first.is_visible():
                await loc.first.click(timeout=5000)
                print("CLICKED login:", sel)
                break
        except Exception:
            continue

    await page.wait_for_timeout(5000)
    print("AFTER LOGIN URL:", page.url[:300])
    print("TITLE2:", await page.title())
    fields = await page.eval_on_selector_all("input", "els => els.map(e => ({type: e.type, name: e.name, id: e.id, placeholder: e.placeholder}))")
    print("INPUTS:", json.dumps(fields, ensure_ascii=False)[:1500])

    print("=== EVENTS ===")
    seen = set()
    for ev in events:
        key = (ev["type"], ev["url"])
        if key in seen:
            continue
        seen.add(key)
        print(json.dumps(ev, ensure_ascii=False)[:500])
    await page.screenshot(path="/tmp/mailcom_after_login.png", full_page=True)
    await browser.close()


asyncio.run(main())
