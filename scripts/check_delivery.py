import sys
import imaplib
import email
from email.header import decode_header

MOTHER = "ChunkyButtotm@mail.com"
PASSWORD = "WMNukRC0Zng"
TARGET = "myyr7cng@email.com"

m = imaplib.IMAP4_SSL("imap.mail.com")
m.socket().settimeout(30)
r = m.login(MOTHER, PASSWORD)
print("login:", r)
r = m.select("INBOX")
print("select:", r)
status, data = m.search(None, 'TO "%s"' % TARGET)
print("search TO", TARGET, "->", status, [x.decode() for x in data])
if status == "OK" and data and data[0]:
    ids = data[0].split()
    print("count:", len(ids))
    for msg_id in ids[-5:]:
        status2, fetched = m.fetch(msg_id, "(BODY.PEEK[HEADER.FIELDS (FROM TO SUBJECT DATE)])")
        if status2 == "OK":
            msg = email.message_from_bytes(fetched[0][1])
            print("  from=%s to=%s subj=%s date=%s" % (msg.get('From'), msg.get('To'), str(msg.get('Subject'))[:60], msg.get('Date')))
else:
    # maybe it went to another address on this mother; show last few messages
    status, data = m.search(None, "ALL")
    ids = data[0].split()
    print("no TO match; total msgs:", len(ids))
    for msg_id in ids[-5:]:
        status2, fetched = m.fetch(msg_id, "(BODY.PEEK[HEADER.FIELDS (FROM TO SUBJECT DATE)])")
        if status2 == "OK":
            msg = email.message_from_bytes(fetched[0][1])
            print("  from=%s to=%s subj=%s date=%s" % (msg.get('From'), msg.get('To'), str(msg.get('Subject'))[:60], msg.get('Date')))
m.logout()
