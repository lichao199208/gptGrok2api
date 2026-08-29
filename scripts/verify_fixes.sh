#!/bin/bash
docker exec chatgpt2api-warp sh -c 'grep -c "invalid_auth_step" /app/services/register/openai_register.py'
docker exec chatgpt2api-warp sh -c 'grep -c "MAIL_DELIVERY_RETRY_LIMIT" /app/services/register/openai_register.py'
docker exec chatgpt2api-warp sh -c 'grep -c "delivery_failures_by_provider" /app/services/register/openai_register.py'
