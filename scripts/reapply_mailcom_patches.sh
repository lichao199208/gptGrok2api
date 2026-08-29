#!/bin/bash
# Re-apply all mail.com 母号 (mailcom_mother) patches to /opt/gptGrok2api.
# Usage: bash scripts/reapply_mailcom_patches.sh
# Safe to run repeatedly (each patch is idempotent).
set -e
cd /opt/gptGrok2api

echo "== 1. mail_provider.py: MailComMotherProvider =="
cp services/register/mail_provider.py services/register/mail_provider.py.bak_mailcom 2>/dev/null || true
python3 scripts/patch_mailcom_provider.py
python3 scripts/fix_mask_email.py
python3 scripts/patch_mailcom_rotation.py
python3 scripts/fix_global_index.py

echo "== 2. register_service.py: redact + merge accounts =="
cp services/register_service.py services/register_service.py.bak_mailcom 2>/dev/null || true
python3 scripts/patch_register_service_mailcom.py
python3 scripts/fix_redact_mailcom.py

echo "== 2b. openai_register.py: invalid_auth_step -> auto-regen + delivery retry =="
cp services/register/openai_register.py services/register/openai_register.py.bak_invauth 2>/dev/null || true
python3 scripts/patch_invalid_auth_step.py
python3 scripts/fix_ref_except.py
python3 scripts/patch_delivery_retry.py

echo "== 2c. image generation: retry once on no_image_generated =="
cp services/protocol/openai_v1_image_generations.py services/protocol/openai_v1_image_generations.py.bak 2>/dev/null || true
python3 scripts/patch_image_retry.py

echo "== 3. web-vue: provider type + card =="
python3 scripts/patch_register_type.py
python3 scripts/patch_register_view_ts.py
python3 scripts/patch_register_card_vue.py
python3 scripts/fix_card_script.py
python3 scripts/patch_frontend_proxies.py
python3 scripts/patch_card_proxies.py

echo "== 4. re-import mother accounts (BOM-free) + full domain list =="
python3 scripts/fix_bom_reimport.py
python3 scripts/set_mailcom_proxies.py
python3 scripts/patch_frontend_domains.py

echo "== 5. rebuild image + restart =="
docker compose -f docker-compose.warp.yml build app
docker compose -f docker-compose.warp.yml up -d app
sleep 20
echo "== 6. sync full domain list into running config (API) =="
python3 scripts/set_full_domains.py
echo "DONE"
