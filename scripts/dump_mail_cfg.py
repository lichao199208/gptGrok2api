import json

d = json.load(open('/opt/gptGrok2api/data/register.json'))
m = d.get('mail', {})
print('MAIL KEYS:', list(m.keys()))
print('WAIT:', m.get('wait_timeout'), m.get('wait_interval'))
provs = m.get('providers', [])
for p in provs:
    print('---', p.get('type'), 'id=', p.get('id'), 'enable=', p.get('enable'))
    keys = list(p.keys())
    print('    keys:', keys)
    # print non-secret values only
    for k in keys:
        v = p.get(k)
        if isinstance(v, list):
            print('    %s: <list len=%d>' % (k, len(v)))
        elif isinstance(v, dict):
            print('    %s: <dict keys=%s>' % (k, list(v.keys())))
        else:
            s = str(v)
            if len(s) > 120:
                s = s[:120] + '...'
            print('    %s: %s' % (k, s))
