#!/bin/bash
docker exec chatgpt2api-warp sh -c 'cd /app && grep -c "class MailComMotherProvider" services/register/mail_provider.py && grep -c "mailcom_mother" services/register_service.py'
echo "=== settings route ==="
grep -rn "settings" /opt/gptGrok2api/api/register.py | head -8
