import { ref } from 'vue'

import { userKeysApi, type UserKey } from '@/api/userKeys'
import { useConfirmDialog } from '@/composables/useConfirmDialog'
import type { usePageRuntime } from '@/composables/usePageRuntime'
import { usePageQuery } from '@/composables/usePageQuery'
import { useToast } from '@/composables/useToast'
import { errorMessage } from '@/lib/errorMessage'

export type UserKeyForm = {
  name: string
  key: string
}

type SettingsUserKeysRuntimeOptions = {
  runtime: ReturnType<typeof usePageRuntime>
  requestKey: string
}

function createUserKeyForm(): UserKeyForm {
  return { name: '', key: '' }
}

export function useSettingsUserKeysRuntime(options: SettingsUserKeysRuntimeOptions) {
  const userKeys = ref<UserKey[]>([])
  const userKeysLoaded = ref(false)
  const userKeysLoading = ref(false)
  const userKeyBusy = ref('')
  const userKeyModal = ref<'create' | 'edit' | ''>('')
  const editingUserKey = ref<UserKey | null>(null)
  const newUserKey = ref('')
  const userKeyForm = ref<UserKeyForm>(createUserKeyForm())
  const toast = useToast()
  const confirmDialog = useConfirmDialog()

  const userKeysQuery = usePageQuery({
    runtime: options.runtime,
    key: options.requestKey,
    loading: userKeysLoading,
    errorMessage: '加载用户密钥失败',
  })

  async function copyUserKey(value: string) {
    if (!value) return
    try {
      await navigator.clipboard.writeText(value)
      toast.success('已复制密钥')
    } catch {
      toast.error('复制失败，请手动复制')
    }
  }

  function resetUserKeyForm() {
    userKeyForm.value = createUserKeyForm()
    editingUserKey.value = null
  }

  function openUserKeyCreateModal() {
    resetUserKeyForm()
    userKeyModal.value = 'create'
  }

  function openUserKeyEditModal(item: UserKey) {
    editingUserKey.value = item
    userKeyForm.value = {
      name: item.name || '',
      key: '',
    }
    userKeyModal.value = 'edit'
  }

  function closeUserKeyModal() {
    if (userKeyBusy.value === 'create') return
    if (editingUserKey.value && userKeyBusy.value === editingUserKey.value.id) return
    userKeyModal.value = ''
    resetUserKeyForm()
  }

  async function loadUserKeys() {
    await userKeysQuery.run(
      () => userKeysApi.list(),
      {
        apply: (response) => {
          userKeys.value = Array.isArray(response.items) ? response.items : []
          userKeysLoaded.value = true
        },
        onError: (message) => {
          userKeys.value = []
          toast.error(message)
        },
      },
    )
  }

  async function createUserKey() {
    userKeyBusy.value = 'create'
    newUserKey.value = ''
    try {
      const response = await userKeysApi.create(userKeyForm.value.name.trim())
      const items = Array.isArray(response.items) ? response.items : null
      const key = typeof response.key === 'string' ? response.key.trim() : ''

      if (items) {
        userKeys.value = items
      } else {
        // Preserve the visible list if the create response is malformed, then
        // reconcile it from the authoritative list endpoint.
        await loadUserKeys()
      }

      if (!key) {
        // The server may have persisted a key even though the one-time raw key
        // was lost in transit. Close the dialog so a retry cannot create a
        // duplicate before the user reviews the refreshed list.
        toast.error('服务器未返回新密钥；密钥可能已创建，请先刷新列表确认后再操作')
        userKeyModal.value = ''
        resetUserKeyForm()
        return
      }

      newUserKey.value = key
      toast.success('用户密钥已创建')
      userKeyModal.value = ''
      resetUserKeyForm()
    } catch (error) {
      toast.error(errorMessage(error, '创建用户密钥失败'))
    } finally {
      userKeyBusy.value = ''
    }
  }

  async function updateUserKey() {
    const item = editingUserKey.value
    if (!item) return
    const nextName = userKeyForm.value.name.trim()
    const nextKey = userKeyForm.value.key.trim()
    const updates: { name?: string; key?: string } = {}
    if (nextName !== item.name) updates.name = nextName
    if (nextKey) updates.key = nextKey
    if (!Object.keys(updates).length) {
      closeUserKeyModal()
      return
    }

    userKeyBusy.value = item.id
    try {
      const response = await userKeysApi.update(item.id, updates)
      userKeys.value = response.items || []
      toast.success(nextKey ? '用户密钥已更新' : '用户名称已更新')
      userKeyModal.value = ''
      resetUserKeyForm()
    } catch (error) {
      toast.error(errorMessage(error, '更新用户密钥失败'))
    } finally {
      userKeyBusy.value = ''
    }
  }

  async function toggleUserKey(item: UserKey) {
    userKeyBusy.value = item.id
    try {
      const response = await userKeysApi.update(item.id, { enabled: !item.enabled })
      userKeys.value = response.items || []
      toast.success(item.enabled ? '用户密钥已禁用' : '用户密钥已启用')
    } catch (error) {
      toast.error(errorMessage(error, '更新用户密钥失败'))
    } finally {
      userKeyBusy.value = ''
    }
  }

  async function deleteUserKey(item: UserKey) {
    const confirmed = await confirmDialog.ask({
      title: '删除用户密钥',
      message: `确定删除用户密钥「${item.name || item.id}」吗？删除后这条密钥将无法继续调用接口。`,
      confirmText: '删除',
      cancelText: '取消',
    })
    if (!confirmed) return

    userKeyBusy.value = item.id
    try {
      const response = await userKeysApi.delete(item.id)
      userKeys.value = response.items || []
      if (editingUserKey.value?.id === item.id) {
        userKeyModal.value = ''
        resetUserKeyForm()
      }
      toast.success('用户密钥已删除')
    } catch (error) {
      toast.error(errorMessage(error, '删除用户密钥失败'))
    } finally {
      userKeyBusy.value = ''
    }
  }

  function invalidate() {
    userKeysQuery.invalidate()
    userKeysLoaded.value = false
  }

  return {
    userKeys,
    userKeysLoaded,
    userKeysLoading,
    userKeyBusy,
    userKeyModal,
    editingUserKey,
    newUserKey,
    userKeyForm,
    copyUserKey,
    resetUserKeyForm,
    openUserKeyCreateModal,
    openUserKeyEditModal,
    closeUserKeyModal,
    loadUserKeys,
    createUserKey,
    updateUserKey,
    toggleUserKey,
    deleteUserKey,
    invalidate,
  }
}
