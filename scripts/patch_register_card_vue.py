#!/usr/bin/env python3
"""Patch RegisterProviderCard.vue: add mailcom_mother config section."""
PATH = "/opt/gptGrok2api/web-vue/src/views/register/RegisterProviderCard.vue"
with open(PATH, "r", encoding="utf-8") as f:
    src = f.read()

original = src

# 1. template section before </FormSection>
section = '''    <div v-if="currentType === 'mailcom_mother'" class="register-provider-section register-provider-section--soft">
      <div class="register-provider-section-title">Mail.com 母号配置</div>
      <div class="register-form-grid register-form-grid--three">
        <label class="register-field">
          <span class="register-label">IMAP Host</span>
          <Input
            :model-value="provider.imap_host"
            block
            root-class="font-mono"
            :disabled="disabled"
            placeholder="imap.mail.com"
            @update:model-value="value => emit('update-field', index, 'imap_host', String(value || '').trim())"
          />
        </label>

        <label class="register-field">
          <span class="register-label">每母号最大激活子号</span>
          <Input
            :model-value="provider.max_active"
            type="number"
            min="1"
            max="50"
            block
            :disabled="disabled"
            @update:model-value="value => emit('update-field', index, 'max_active', numberModelValue(value))"
          />
        </label>

        <label class="register-field">
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

      <label class="register-field">
        <span class="register-label">母号池导入</span>
        <textarea
          class="register-textarea register-textarea--tall"
          :disabled="disabled"
          :value="String(provider.accounts || '')"
          placeholder="每行一个：邮箱----密码"
          @input="emit('update-field', index, 'accounts', ($event.target as HTMLTextAreaElement).value)"
        ></textarea>
      </label>

      <div v-if="Number(provider.accounts_count || 0) > 0" class="register-mailcom-summary">
        <MetaChip size="xs" tone="success">已保存 {{ provider.accounts_count }} 个母号</MetaChip>
        <MetaChip
          v-for="preview in (provider.accounts_preview || [])"
          :key="preview"
          size="xs"
          tone="muted"
        >{{ preview }}</MetaChip>
      </div>

      <label class="register-field">
        <span class="register-label">子号域名白名单</span>
        <textarea
          class="register-textarea"
          :disabled="disabled"
          :value="mailcomDomainsText"
          placeholder="每行一个域名，如 mail.com / humanoid.net / salesperson.net"
          @input="mailcomDomainsText = ($event.target as HTMLTextAreaElement).value"
        ></textarea>
      </label>

      <p class="register-preview-line">
        免费 mail.com 母号每个最多 9 个可删激活子号；配额满时自动删除最旧子号后创建新子号。
        注册遇到“邮箱已使用”会自动重新生成子邮箱；验证码通过母号 IMAP 收取。
      </p>
    </div>
  </FormSection>
</template>'''

anchor = "  </FormSection>\n</template>"
if "Mail.com 母号配置" not in src:
    if anchor in src:
        src = src.replace(anchor, section, 1)
        print("template section +")
    else:
        print("template anchor not found")
else:
    print("template section exists")

# 2. script: add mailcomDomainsText computed after outlookSummary
script_anchor = "const outlookSummary = computed(() => outlookPoolSummary(props.provider))"
script_new = script_anchor + '''
const mailcomDomainsText = computed({
  get: () => arrayText(props.provider.domains),
  set: (value: string) => {
    const list = value.split(/[\\n,]/).map((item: string) => item.trim()).filter(Boolean)
    emit('update-field', props.index, 'domains', list)
  },
})'''
if "mailcomDomainsText" not in src:
    if script_anchor in src:
        src = src.replace(script_anchor, script_new, 1)
        print("script computed +")
    else:
        print("script anchor not found")
else:
    print("script computed exists")

if src == original:
    print("NO CHANGES")
else:
    with open(PATH, "w", encoding="utf-8") as f:
        f.write(src)
    print("PATCH OK")
