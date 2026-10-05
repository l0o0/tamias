<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { AlertTriangle } from 'lucide-vue-next'
import { api } from '../api'
import { notify, refreshState } from '../store'
import type { ConnectionKind, WriteMode } from '../types'
import BaseModal from './BaseModal.vue'

const props = defineProps<{ modelValue: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: boolean]; created: [id: string] }>()
const form = reactive({ name: '', kind: 'webdav' as 'webdav' | 's3', endpoint: '', region: 'us-east-1', bucket: '', prefix: '', username: '', password: '', accessKey: '', secretKey: '', sessionToken: '', pathStyle: false, writeMode: 'standard' as WriteMode, keepRecovery: false })
const saving = ref(false)
const showAdvanced = ref(false)
const errorMessage = ref('')
function syncAdvanced(event: Event) { showAdvanced.value = (event.currentTarget as HTMLDetailsElement).open }
watch(() => props.modelValue, (open) => { if (open) { Object.assign(form, { name: '', kind: 'webdav', endpoint: '', region: 'us-east-1', bucket: '', prefix: '', username: '', password: '', accessKey: '', secretKey: '', sessionToken: '', pathStyle: false, writeMode: 'standard', keepRecovery: false }); showAdvanced.value = false; errorMessage.value = '' } })
async function submit() {
  if (saving.value) return
  errorMessage.value = ''
  saving.value = true
  try {
    const result = await api.post<{ id: string }>('/api/connections', {
      name: form.name.trim(), kind: form.kind as ConnectionKind, endpoint: form.endpoint.trim(), region: form.kind === 's3' ? form.region.trim() : '',
      bucket: form.kind === 's3' ? form.bucket.trim() : '', prefix: form.kind === 's3' ? form.prefix.trim().replace(/^\/+|\/+$/g, '') : '', username: form.kind === 'webdav' ? form.username.trim() : '', writeMode: form.writeMode, keepRecovery: form.keepRecovery,
      password: form.kind === 'webdav' ? form.password : '', accessKey: form.kind === 's3' ? form.accessKey : '', secretKey: form.kind === 's3' ? form.secretKey : '', sessionToken: form.kind === 's3' ? form.sessionToken : '', pathStyle: form.kind === 's3' && form.pathStyle,
    })
    await refreshState(true)
    emit('created', result.id)
    emit('update:modelValue', false)
    notify('连接已添加', 'success')
  } catch (error) { errorMessage.value = error instanceof Error ? error.message : '添加连接失败' }
  finally { saving.value = false }
}
</script>
<template>
  <BaseModal :model-value="modelValue" title="添加存储连接" subtitle="凭据保存在本机安全凭据库中。" :dismissible="!saving" @update:model-value="emit('update:modelValue', $event)">
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
        <label class="field"><span>Bucket</span><input v-model="form.bucket" :disabled="saving" required /></label>
        <div class="form-row">
          <label class="field"><span>Access Key <small>可留空用于匿名访问</small></span><input v-model="form.accessKey" :disabled="saving" autocomplete="off" /></label>
          <label class="field"><span>Secret Key</span><input v-model="form.secretKey" :disabled="saving" type="password" autocomplete="new-password" /></label>
        </div>
      </template>
      <details class="connection-options" :open="showAdvanced" @toggle="syncAdvanced">
        <summary>高级设置</summary>
        <template v-if="form.kind === 's3'">
          <label class="field"><span>Region <small>可选，默认 us-east-1</small></span><input v-model="form.region" :disabled="saving" placeholder="us-east-1" /></label>
          <label class="field"><span>访问前缀 <small>可选</small></span><input v-model="form.prefix" :disabled="saving" placeholder="例如：documents" /></label>
          <label class="field"><span>Session Token <small>可选</small></span><input v-model="form.sessionToken" :disabled="saving" type="password" autocomplete="off" /></label>
          <label class="check-row"><input v-model="form.pathStyle" :disabled="saving" type="checkbox" /> 使用 Path-style 寻址</label>
        </template>
        <label class="field"><span>同步方式</span><select v-model="form.writeMode" :disabled="saving"><option value="standard">常规同步（默认）</option><option value="strict">严格防覆盖</option><option value="copy">仅上传为新副本</option></select></label>
        <p class="field-hint">{{ form.writeMode === 'copy' ? '上传文件时使用新名称，保留远端同名文件；同步下载可用，远端删除、移动和覆盖不可用。' : form.writeMode === 'strict' ? '仅在服务端支持条件写入时开放覆盖；删除、移动等操作也需服务端支持。' : '按原路径直接上传和更新同名文件。删除、移动等操作仍取决于服务端能力。' }}</p>
        <label class="check-row"><input v-model="form.keepRecovery" :disabled="saving" type="checkbox"/><span>覆盖或删除前保留恢复副本</span></label>
        <p v-if="form.keepRecovery" class="field-hint">恢复副本会占用本机空间。</p>
      </details>
      <div v-if="errorMessage" class="connection-form-error" role="alert"><AlertTriangle :size="15"/><span>{{errorMessage}}</span></div>
      <footer class="modal-actions"><button class="button secondary" type="button" :disabled="saving" @click="emit('update:modelValue', false)">取消</button><button class="button primary" type="submit" :disabled="saving">{{ saving ? '正在连接并自动检测…' : '添加连接' }}</button></footer>
    </form>
  </BaseModal>
</template>

<style scoped>
.connection-options{display:grid;gap:12px;padding:10px 12px;border:1px solid #e9e8ef;border-radius:10px;background:#fff}
.connection-options>summary{color:#5e5b6b;font-size:12px;font-weight:600;cursor:pointer}
.connection-options[open]>summary{margin-bottom:2px}
.connection-options .field-hint{margin:0}
.connection-form-error{display:flex;align-items:flex-start;gap:8px;padding:10px;border:1px solid #efd4d0;border-radius:10px;background:#fff7f6;color:#a34f45;font-size:12px;line-height:1.5}
.connection-form-error svg{flex:none;margin-top:1px}
.connection-form-error span{overflow-wrap:anywhere}
</style>
