import asyncio
import json
import os
import sys

os.environ.setdefault("XAI_HEADLESS", "1")

EMAIL = sys.argv[1] if len(sys.argv) > 1 else ""
PASSWORD = sys.argv[2] if len(sys.argv) > 2 else ""

captured = []


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
        if "oauth2/token" in req.url:
            hdrs = req.headers
            captured.append({
                "url": req.url[:500],
                "method": req.method,
                "body": (req.post_data or "")[:800],
                "headers": {k: v for k, v in hdrs.items() if k.lower() in ("authorization", "content-type", "origin", "referer", "user-agent", "x-requested-with", "x-ui-app", "accept")},
            })

    async def on_response(resp):
        if "oauth2/token" in resp.url:
            body = ""
            try:
                body = await resp.text()
            except Exception:
                pass
            captured.append({"response_status": resp.status, "response_body": body[:1200], "response_headers": dict(resp.headers)})

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

    await page.evaluate("""async (args) => {
        const {email, password} = args;
        const form = document.querySelector("form[action*='login']");
        if (!form) return {ok: false, reason: "no form"};
        const inputs = form.querySelectorAll("input");
        for (const i of inputs) {
            if (i.name === "username") i.value = email;
            if (i.name === "password") i.value = password;
        }
        form.submit();
        return {ok: true};
    }""", {"email": EMAIL, "password": PASSWORD})

    for _ in range(20):
        await page.wait_for_timeout(3000)
        if captured:
            break

    print("=== CAPTURED ===")
    for c in captured:
        print(json.dumps(c, ensure_ascii=False)[:1500])
    print("=== URL:", page.url[:250])
    await browser.close()


asyncio.run(main())
