#!/usr/bin/env python3
"""Add proxies textarea + pool_batch input to mailcom_mother card section."""
PATH = "/opt/gptGrok2api/web-vue/src/views/register/RegisterProviderCard.vue"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

# 1. add pool_batch + proxies fields after the proxy input
anchor = """        <label class="register-field">
          <span class="register-label">请求代理（建议 WARP）</span>
          <Input
            :model-value="provider.proxy"
            block
            root-class="font-mono"
            :disabled="disabled"
            placeholder="http://privoxy:8118"
            @update:model-value="value => emit('update-field', index, 'proxy', String(value || '').trim())"
          />
        </label>
      </div>
"""
new = """        <label class="register-field">
          <span class="register-label">主代理（建议 WARP）</span>
          <Input
            :model-value="provider.proxy"
            block
            root-class="font-mono"
            :disabled="disabled"
            placeholder="http://privoxy:8118"
            @update:model-value="value => emit('update-field', index, 'proxy', String(value || '').trim())"
          />
        </label>

        <label class="register-field">
          <span class="register-label">每次登录预建子号数</span>
          <Input
            :model-value="provider.pool_batch"
            type="number"
            min="1"
            max="6"
            block
            :disabled="disabled"
            @update:model-value="value => emit('update-field', index, 'pool_batch', numberModelValue(value))"
          />
        </label>
      </div>

      <label class="register-field">
        <span class="register-label">备用代理（每行一个，自动轮换）</span>
        <textarea
          class="register-textarea"
          :disabled="disabled"
          :value="mailcomProxiesText"
          placeholder="http://privoxy:8118&#10;http://user:pass@host:port"
          @input="mailcomProxiesText = ($event.target as HTMLTextAreaElement).value"
        ></textarea>
      </label>
"""
if "备用代理（每行一个" in src:
    print("card fields exist")
elif anchor in src:
    src = src.replace(anchor, new, 1)
    print("card fields added")
else:
    print("card anchor not found")

# 2. add computed mailcomProxiesText
script_anchor = """const mailcomDomainsText = computed({
  get: () => arrayText(props.provider.domains),
  set: (value: string) => {
    const list = value.split(/[\\n,]/).map((item: string) => item.trim()).filter(Boolean)
    emit('update-field', props.index, 'domains', list)
  },
})"""
script_new = script_anchor + """
const mailcomProxiesText = computed({
  get: () => arrayText(props.provider.proxies),
  set: (value: string) => {
    const list = value.split(/[\\n,]/).map((item: string) => item.trim()).filter(Boolean)
    emit('update-field', props.index, 'proxies', list)
  },
})"""
if "mailcomProxiesText" in src:
    print("script exists")
elif script_anchor in src:
    src = src.replace(script_anchor, script_new, 1)
    print("script added")
else:
    print("script anchor not found")

with open(PATH, "w", encoding="utf-8") as f:
    f.write(src)
print("done")
