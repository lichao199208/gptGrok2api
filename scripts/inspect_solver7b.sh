#!/bin/bash
docker exec chatgpt2api-captcha-solver sh -c 'env | grep -iE "headless|xai|display|xvfb|proxy" ; which xvfb-run Xvfb 2>/dev/null; echo "DISPLAY=$DISPLAY"'
