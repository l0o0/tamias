<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { api } from '../api'
import { notify, refreshState } from '../store'
import type { ConnectionKind } from '../types'
import BaseModal from './BaseModal.vue'

const props = defineProps<{ modelValue: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: boolean]; created: [id: string] }>()
const form = reactive({ name: '', kind: 'webdav' as 'webdav' | 's3', endpoint: '', region: 'us-east-1', bucket: '', prefix: '', username: '', password: '', accessKey: '', secretKey: '', sessionToken: '', pathStyle: false })
const saving = ref(false)
const showAdvanced = ref(false)
watch(() => props.modelValue, (open) => { if (open) { Object.assign(form, { name: '', kind: 'webdav', endpoint: '', region: 'us-east-1', bucket: '', prefix: '', username: '', password: '', accessKey: '', secretKey: '', sessionToken: '', pathStyle: false }); showAdvanced.value = false } })
async function submit() {
  if (saving.value) return
  saving.value = true
  try {
    const result = await api.post<{ id: string }>('/api/connections', {
      name: form.name.trim(), kind: form.kind as ConnectionKind, endpoint: form.endpoint.trim(), region: form.region.trim(),
      bucket: form.bucket.trim(), prefix: form.prefix.trim().replace(/^\/+|\/+$/g, ''), username: form.username.trim(),
      password: form.password, accessKey: form.accessKey, secretKey: form.secretKey, sessionToken: form.sessionToken, pathStyle: form.pathStyle,
    })
    await refreshState(true)
    emit('created', result.id)
    emit('update:modelValue', false)
    notify('连接已添加', 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '添加连接失败') }
  finally { saving.value = false }
}
</script>
<template>
  <BaseModal :model-value="modelValue" title="添加存储连接" subtitle="凭据由本机安全保存，不会显示在活动记录中。" :dismissible="!saving" @update:model-value="emit('update:modelValue', $event)">
    <form class="form-stack" :aria-busy="saving" @submit.prevent="submit">
      <div class="segmented-choice" aria-label="存储类型">
        <button type="button" :disabled="saving" :class="{ active: form.kind === 'webdav' }" @click="form.kind = 'webdav'">WebDAV</button>
        <button type="button" :disabled="saving" :class="{ active: form.kind === 's3' }" @click="form.kind = 's3'">S3 兼容存储</button>
      </div>
      <label class="field"><span>连接名称</span><input v-model="form.name" :disabled="saving" required maxlength="64" placeholder="例如：我的 NAS" /></label>
      <label class="field"><span>{{ form.kind === 'webdav' ? '服务地址' : 'S3 Endpoint' }}</span><input v-model="form.endpoint" :disabled="saving" required type="url" placeholder="https://storage.example.com" /></label>
      <template v-if="form.kind === 'webdav'">
        <div class="form-row">
          <label class="field"><span>用户名</span><input v-model="form.username" :disabled="saving" autocomplete="username" /></label>
          <label class="field"><span>密码 / 应用密码</span><input v-model="form.password" :disabled="saving" type="password" autocomplete="new-password" /></label>
        </div>
      </template>
      <template v-else>
        <div class="form-row">
          <label class="field"><span>Bucket</span><input v-model="form.bucket" :disabled="saving" required /></label>
          <label class="field"><span>Region</span><input v-model="form.region" :disabled="saving" placeholder="us-east-1" /></label>
        </div>
        <label class="field"><span>访问前缀 <small>可选</small></span><input v-model="form.prefix" :disabled="saving" placeholder="例如：documents" /></label>
        <label class="field"><span>Access Key</span><input v-model="form.accessKey" :disabled="saving" autocomplete="off" /></label>
        <div class="form-row">
          <label class="field"><span>Secret Key</span><input v-model="form.secretKey" :disabled="saving" type="password" autocomplete="new-password" /></label>
          <label class="field"><span>Session Token <small>可选</small></span><input v-model="form.sessionToken" :disabled="saving" type="password" /></label>
        </div>
        <button class="text-button" type="button" :disabled="saving" @click="showAdvanced = !showAdvanced">{{ showAdvanced ? '收起高级设置' : '高级设置' }}</button>
        <label v-if="showAdvanced" class="check-row"><input v-model="form.pathStyle" :disabled="saving" type="checkbox" /> 使用 Path-style 寻址</label>
      </template>
      <footer class="modal-actions"><button class="button secondary" type="button" :disabled="saving" @click="emit('update:modelValue', false)">取消</button><button class="button primary" type="submit" :disabled="saving">{{ saving ? '正在保存…' : '添加连接' }}</button></footer>
    </form>
  </BaseModal>
</template>
