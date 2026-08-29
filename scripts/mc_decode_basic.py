import base64
for b in [
    "bWFpbGNvbV9scHNfbGl2ZToqKioqKioq",
    "bWFpbGNvbV93ZWJtYWlsZXJtYWlscm9vdF9saXZlOioqKioqKio=",
    "bWFpbGNvbV9tYWlsc2lkZWJhcl9wYXNzcG9ydF9saXZlOioqKioqKio=",
]:
    dec = base64.b64decode(b).decode()
    print(repr(dec), "| secret repr:", repr(dec.split(":", 1)[1]))
