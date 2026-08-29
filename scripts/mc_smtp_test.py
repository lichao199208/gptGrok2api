import sys
import smtplib
import time
from email.mime.text import MIMEText
from email.utils import formatdate

MOTHER = "ChunkyButtotm@mail.com"
PASSWORD = "WMNukRC0Zng"
TARGET = sys.argv[1] if len(sys.argv) > 1 else ""

msg = MIMEText("Your temporary ChatGPT verification code is 482913. It expires in 10 minutes.", "plain", "utf-8")
msg["Subject"] = "Your temporary ChatGPT verification code"
msg["From"] = MOTHER
msg["To"] = TARGET
msg["Date"] = formatdate(localtime=True)

try:
    s = smtplib.SMTP_SSL("smtp.mail.com", 465, timeout=30)
    s.login(MOTHER, PASSWORD)
    s.sendmail(MOTHER, [TARGET], msg.as_string())
    s.quit()
    print("SMTP SENT to", TARGET)
except Exception as e:
    print("SMTP ERROR:", type(e).__name__, str(e)[:300])
