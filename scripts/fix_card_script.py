#!/usr/bin/env python3
"""Add mailcomDomainsText computed to RegisterProviderCard.vue script."""
PATH = "/opt/gptGrok2api/web-vue/src/views/register/RegisterProviderCard.vue"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

script_anchor = "const outlookSummary = computed(() => outlookPoolSummary(props.provider))"
script_new = script_anchor + '''
const mailcomDomainsText = computed({
  get: () => arrayText(props.provider.domains),
  set: (value: string) => {
    const list = value.split(/[\\n,]/).map((item: string) => item.trim()).filter(Boolean)
    emit('update-field', props.index, 'domains', list)
  },
})'''

if "const mailcomDomainsText" in src:
    print("already present")
elif script_anchor in src:
    src = src.replace(script_anchor, script_new, 1)
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    print("script computed added")
else:
    print("anchor not found")
