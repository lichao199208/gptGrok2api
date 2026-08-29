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
                body = (req.post_data or "")[:1000]
        except Exception:
            pass
        events.append({"type": "req", "method": req.method, "url": req.url[:450], "body": body})

    async def on_response(resp):
        events.append({
            "type": "resp", "status": resp.status,
            "url": resp.url[:450],
            "location": (resp.headers.get("location") or "")[:700],
            "setcookie": (resp.headers.get("set-cookie") or "")[:250],
        })

    page.on("request", on_request)
    page.on("response", on_response)

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

    # fill + submit via native form submission
    result = await page.evaluate("""async (args) => {
        const {email, password} = args;
        const form = document.querySelector("form[action*='login']");
        if (!form) return {ok: false, reason: "no form"};
        const inputs = form.querySelectorAll("input");
        let foundU = null, foundP = null;
        for (const i of inputs) {
            if (i.name === "username") { i.value = email; foundU = i; }
            if (i.name === "password") { i.value = password; foundP = i; }
        }
        if (!foundU || !foundP) return {ok: false, reason: "no fields"};
        form.submit();
        return {ok: true};
    }""", {"email": EMAIL, "password": PASSWORD})
    print("SUBMIT:", json.dumps(result, ensure_ascii=False)[:400])

    for i in range(20):
        await page.wait_for_timeout(3000)
        print(f"TICK{i} url:", page.url[:350], "title:", (await page.title())[:70])
        if "webmail" in page.url or "mail.mail.com" in page.url or "inbox" in page.url.lower() or "oauth" in page.url:
            break

    print("=== EVENTS (relevant) ===")
    seen = set()
    for ev in events:
        url = ev["url"]
        if not any(h in url for h in ("login.mail.com", "oauth", "bridge", "webmail", "mail.mail.com", "sso", "token", "authorize", "halogin", "ott")):
            continue
        key = (ev["type"], ev.get("method") or "", ev.get("status") or "", url)
        if key in seen:
            continue
        seen.add(key)
        print(json.dumps(ev, ensure_ascii=False)[:800])
    await page.screenshot(path="/tmp/mailcom_login4.png", full_page=True)
    await browser.close()


asyncio.run(main())
