import json
d = json.load(open('/opt/gptGrok2api/data/register.json'))
print('enabled:', d.get('enabled'))
print('total:', d.get('total'), 'mode:', d.get('mode'), 'target:', d.get('target'))
mail = d.get('mail') or {}
provs = mail.get('providers') or []
for p in provs:
    print(f"  {p.get('type')} enable={p.get('enable')}")
mc = [p for p in provs if p.get('type') == 'mailcom_mother']
if mc:
    accounts = mc[0].get('accounts') or ''
    lines = [ln for ln in accounts.splitlines() if ln.strip()]
    print('mailcom_mother accounts:', len(lines), 'first:', lines[0][:45] if lines else '')
# check the created account persisted
acc = json.load(open('/opt/gptGrok2api/data/accounts.json'))
print('accounts.json total:', len(acc))
for a in acc[-3:]:
    print('  ', a.get('email'), a.get('status'), a.get('access_token', '')[:20])
