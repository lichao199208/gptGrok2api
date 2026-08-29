import asyncio
import json
import os
import re

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
            events.append({"type": "req", "method": req.method, "url": req.url[:400]})

    page.on("request", on_request)

    # 1. load www.mail.com, find login links
    try:
        resp = await page.goto("https://www.mail.com/", timeout=45000, wait_until="domcontentloaded")
        print("NAV1 status:", resp.status if resp else None, "final:", page.url)
    except Exception as e:
        print("NAV1 error:", type(e).__name__, str(e)[:200])
    await page.wait_for_timeout(5000)
    links = await page.eval_on_selector_all("a", "els => els.map(e => ({href: e.href, text: (e.innerText||'').trim().slice(0,40)}))")
    login_links = [l for l in links if re.search(r"login|sign|anmelden|einloggen|connexion|acceder", l["href"], re.I)]
    print("LOGIN LINKS:", json.dumps(login_links, ensure_ascii=False)[:2000])
    all_hosts = set()
    for l in links:
        try:
            from urllib.parse import urlsplit
            h = urlsplit(l["href"]).netloc
            if h:
                all_hosts.add(h)
        except Exception:
            pass
    print("LINK HOSTS:", sorted(all_hosts)[:40])

    # 2. directly try likely OAuth authorize URL shapes
    for name, url in [
        ("oauth-authorize", "https://login.mail.com/authorize"),
        ("oauth-authorize-q", "https://login.mail.com/authorize?client_id=mailcom.mailset-compose&state=probe&redirect_uri=https%3A%2F%2Fmail.com%2F&response_type=code&scope=mail_account_r"),
        ("sso", "https://sso.mail.com/"),
        ("login-param", "https://login.mail.com/?client_id=webmail&state=probe&redirect_uri=https%3A%2F%2Fmail.com%2F"),
    ]:
        try:
            r = await page.goto(url, timeout=25000, wait_until="domcontentloaded")
            print(f"### {name} -> status={r.status if r else None} url={page.url[:200]}")
            await page.wait_for_timeout(2000)
            html = await page.content()
            print(f"    title={await page.title()} html_len={len(html)} head={html[:200]!r}")
        except Exception as e:
            print(f"### {name} ERROR {type(e).__name__}: {str(e)[:150]}")

    print("=== EVENTS ===")
    seen = set()
    for ev in events:
        key = ev["url"]
        if key in seen:
            continue
        seen.add(key)
        print(json.dumps(ev, ensure_ascii=False)[:400])
    await browser.close()


asyncio.run(main())
