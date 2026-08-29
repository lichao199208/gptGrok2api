import sys
import imaplib
import email
from email.header import decode_header

EMAIL = sys.argv[1] if len(sys.argv) > 1 else ""
PASSWORD = sys.argv[2] if len(sys.argv) > 2 else ""
SUB_ADDRESS = sys.argv[3] if len(sys.argv) > 3 else ""

for host, port in [("imap.mail.com", 993)]:
    try:
        m = imaplib.IMAP4_SSL(host, port)
        m.socket().settimeout(30)
        r = m.login(EMAIL, PASSWORD)
        print(f"{host}: login -> {r}")
        r = m.select("INBOX")
        print(f"{host}: select -> {r}")
        # search messages to the sub address
        for test_addr in [SUB_ADDRESS, "q6q3fg33nu@umpire.com"]:
            try:
                status, data = m.search(None, 'TO "%s"' % test_addr)
                print(f"{host}: search TO {test_addr} -> {status} {[x.decode() for x in data]}")
            except Exception as e:
                print(f"{host}: search TO {test_addr} ERR {e}")
            try:
                status, data = m.search("UTF-8", 'TO "%s"' % test_addr)
                print(f"{host}: search UTF8 TO {test_addr} -> {status} {[x.decode() for x in data]}")
            except Exception as e:
                print(f"{host}: search UTF8 TO {test_addr} ERR {e}")
        status, data = m.search(None, "ALL")
        ids = data[0].split() if status == "OK" else []
        print(f"{host}: total messages = {len(ids)}")
        if ids:
            # latest 5
            for msgid in ids[-5:]:
                status2, msgdata = m.fetch(msgid, "(BODY.PEEK[HEADER.FIELDS (FROM TO SUBJECT DATE)])")
                if status2 == "OK":
                    raw = msgdata[0][1]
                    msg = email.message_from_bytes(raw)
                    def dh(v):
                        if not v:
                            return ""
                        parts = decode_header(v)
                        out = []
                        for txt, enc in parts:
                            try:
                                out.append(txt.decode(enc or "utf-8", errors="replace") if isinstance(txt, bytes) else str(txt))
                            except Exception:
                                out.append(str(txt))
                        return "".join(out)
                    print(f"  msg {msgid.decode()}: from={dh(msg.get('From'))} to={dh(msg.get('To'))} subj={dh(msg.get('Subject'))[:60]} date={msg.get('Date')}")
        m.logout()
    except Exception as e:
        print(f"{host}: ERROR {type(e).__name__}: {e}")
