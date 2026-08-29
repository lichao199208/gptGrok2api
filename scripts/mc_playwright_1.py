import asyncio
import json
import os

os.environ.setdefault("XAI_HEADLESS", "1")

LOGIN_URL = "https://login.mail.com/"
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
        print("new_context error:", type(e).__name__, str(e)[:300])
        page = await browser.new_page()

    async def on_request(req):
        if "mail.com" in req.url:
            body = ""
            try:
                if req.method in ("POST", "PUT", "PATCH"):
                    body = (req.post_data or "")[:800]
            except Exception:
                pass
            events.append({"type": "req", "method": req.method, "url": req.url[:300], "body": body})

    async def on_response(resp):
        if "mail.com" in resp.url:
            events.append({
                "type": "resp", "status": resp.status,
                "url": resp.url[:300],
                "location": (resp.headers.get("location") or "")[:300],
                "ctype": resp.headers.get("content-type", "")[:60],
            })

    page.on("request", on_request)
    page.on("response", on_response)
    try:
        resp = await page.goto(LOGIN_URL, timeout=45000, wait_until="domcontentloaded")
        print("NAV status:", resp.status if resp else None, "final:", page.url)
    except Exception as e:
        print("NAV error:", type(e).__name__, str(e)[:300])
    await page.wait_for_timeout(6000)
    print("TITLE:", await page.title())
    html = await page.content()
    print("HTML len:", len(html))
    fields = await page.eval_on_selector_all("input", "els => els.map(e => ({type: e.type, name: e.name, id: e.id, placeholder: e.placeholder}))")
    print("INPUTS:", json.dumps(fields, ensure_ascii=False)[:1500])
    forms = await page.eval_on_selector_all("form", "els => els.map(e => ({action: e.action, id: e.id}))")
    print("FORMS:", json.dumps(forms, ensure_ascii=False)[:800])
    with open("/tmp/mailcom_login.html", "w", encoding="utf-8") as f:
        f.write(html)
    await page.screenshot(path="/tmp/mailcom_login.png", full_page=True)
    print("screenshot saved")
    print("=== EVENTS ===")
    for ev in events[:60]:
        print(json.dumps(ev, ensure_ascii=False)[:400])
    await browser.close()


asyncio.run(main())
