#!/bin/bash
docker exec chatgpt2api-captcha-solver sh -c 'python3 --version; pip list 2>/dev/null | grep -iE "playwright|selenium|requests|curl|httpx" ; ls /app 2>/dev/null | head'
