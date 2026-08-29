#!/bin/bash
docker cp chatgpt2api-warp:/app/services/register/openai_register.py /opt/gptGrok2api/services/register/openai_register.py.live
echo "copied, size: $(wc -c < /opt/gptGrok2api/services/register/openai_register.py.live)"
grep -n "def _create_account\|def _validate_otp\|invalid_auth_step" /opt/gptGrok2api/services/register/openai_register.py.live | head
