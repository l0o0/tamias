<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { AlertTriangle, Check, RefreshCw, ShieldCheck } from 'lucide-vue-next'
import { api } from '../api'
import { notify, refreshState } from '../store'
import type { Connection, ConnectionKind } from '../types'
import BaseModal from './BaseModal.vue'

interface Impact {
  scopeChanged: boolean
  syncJobs: { id: string; name: string }[]
  gateways: { id: string; name: string }[]
  backups: number
  backupSnapshots: number
  migrations: number
  cacheEntries: number
  downloads: number
  pendingOperations: number
  davLocks: number
  blockers: string[]
}
interface EditPreview {
  token: string
  expiresAt: string
  candidate: Connection
  impact: Impact
  capabilities: { conditionalWrite: boolean; conditionalDelete: boolean; rangeRead: boolean; multipartConditional: boolean }
}

const props = defineProps<{ modelValue: boolean; connection: Connection | null }>()
const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>()
const form = reactive({ name: '', kind: 'webdav' as 'webdav' | 's3', endpoint: '', region: 'us-east-1', bucket: '', prefix: '', username: '', password: '', accessKey: '', secretKey: '', sessionToken: '', pathStyle: false })
const busy = ref(false)
const preview = ref<EditPreview | null>(null)
const issue = ref('')

function close() { preview.value = null; issue.value = ''; emit('update:modelValue', false) }
function payload() {
  if (!props.connection) throw new Error('连接已失效，请刷新后重试')
  return {
    id: props.connection.id, name: form.name.trim(), kind: form.kind as ConnectionKind,
    endpoint: form.endpoint.trim(), region: form.region.trim(), bucket: form.bucket.trim(),
    prefix: form.prefix.trim().replace(/^\/+|\/+$/g, ''), username: form.username.trim(),
    password: form.password, accessKey: form.accessKey, secretKey: form.secretKey,
    sessionToken: form.sessionToken, pathStyle: form.pathStyle,
  }
}
watch(() => [props.modelValue, props.connection] as const, ([open, connection]) => {
  if (!open || !connection) return
  Object.assign(form, { name: connection.name, kind: connection.kind as 'webdav' | 's3', endpoint: connection.endpoint, region: connection.region || (connection.kind === 's3' ? 'us-east-1' : ''), bucket: connection.bucket, prefix: connection.prefix, username: connection.username || '', password: '', accessKey: '', secretKey: '', sessionToken: '', pathStyle: connection.pathStyle })
  preview.value = null
  issue.value = ''
}, { immediate: true })
watch(form, () => { preview.value = null; issue.value = '' }, { deep: true })

async function validateChanges() {
  if (busy.value) return
  busy.value = true; preview.value = null; issue.value = ''
  try {
    preview.value = await api.post<EditPreview>('/api/connections/edit-preview', payload())
    if (preview.value.impact.blockers?.length) issue.value = preview.value.impact.blockers.join('；')
  } catch (error) { issue.value = error instanceof Error ? error.message : '连接验证失败' }
  finally { busy.value = false }
}
async function save() {
  if (!preview.value?.token || busy.value || preview.value.impact.blockers?.length) return
  busy.value = true; issue.value = ''
  try {
    await api.post('/api/connections/edit', { ...payload(), previewToken: preview.value.token })
    await refreshState(true)
    notify('连接配置已验证并保存', 'success')
    close()
  } catch (error) { issue.value = error instanceof Error ? error.message : '保存连接配置失败'; preview.value = null }
  finally { busy.value = false }
}
</script>

<template>
  <BaseModal :model-value="modelValue" title="编辑存储连接" :subtitle="connection?.name" width="640px" :dismissible="!busy" @update:model-value="v => { if (!v) close() }">
    <form class="form-stack" :aria-busy="busy" @submit.prevent="validateChanges">
      <p class="field-hint">连接编号会保留。留空的密码和密钥继续使用已保存值；保存前会重新测试连接并验证条件写入能力。</p>
      <label class="field"><span>连接名称</span><input v-model="form.name" :disabled="busy" required maxlength="64" /></label>
      <label class="field"><span>{{ form.kind === 'webdav' ? '服务地址' : 'S3 Endpoint' }}</span><input v-model="form.endpoint" :disabled="busy" required type="url" /></label>
      <template v-if="form.kind === 'webdav'">
        <div class="form-row">
          <label class="field"><span>用户名</span><input v-model="form.username" :disabled="busy" autocomplete="username" placeholder="留空以保留当前用户名" /></label>
          <label class="field"><span>密码</span><input v-model="form.password" :disabled="busy" type="password" autocomplete="new-password" placeholder="留空以保留现有密码" /></label>
        </div>
      </template>
      <template v-else>
        <div class="form-row">
          <label class="field"><span>Bucket</span><input v-model="form.bucket" :disabled="busy" required /></label>
          <label class="field"><span>Region</span><input v-model="form.region" :disabled="busy" required /></label>
        </div>
        <div class="form-row">
          <label class="field"><span>访问密钥 ID</span><input v-model="form.accessKey" :disabled="busy" type="password" autocomplete="off" placeholder="留空以保留现有密钥" /></label>
          <label class="field"><span>秘密访问密钥</span><input v-model="form.secretKey" :disabled="busy" type="password" autocomplete="new-password" placeholder="留空以保留现有密钥" /></label>
        </div>
        <label class="field"><span>会话令牌</span><input v-model="form.sessionToken" :disabled="busy" type="password" autocomplete="off" placeholder="留空以保留现有令牌" /></label>
        <label class="check-row"><input v-model="form.pathStyle" :disabled="busy" type="checkbox"/><span>使用 Path-style 请求</span></label>
      </template>
      <label class="field"><span>远端前缀</span><input v-model="form.prefix" :disabled="busy" :placeholder="form.kind === 's3' ? '留空表示 Bucket 根目录' : '留空表示连接根目录'" /></label>
      <div v-if="issue" class="edit-error" role="alert"><AlertTriangle :size="15"/><span>{{ issue }}</span></div>
      <div v-if="preview" class="edit-review">
        <b>验证结果</b>
        <div class="edit-capabilities"><span :class="preview.capabilities.conditionalWrite ? 'ok' : 'missing'">{{ preview.capabilities.conditionalWrite ? '✓' : '×' }} 条件写入</span><span :class="preview.capabilities.conditionalDelete ? 'ok' : 'missing'">{{ preview.capabilities.conditionalDelete ? '✓' : '×' }} 条件删除</span><span :class="preview.capabilities.rangeRead ? 'ok' : 'missing'">{{ preview.capabilities.rangeRead ? '✓' : '×' }} 范围读取</span><span :class="preview.capabilities.multipartConditional ? 'ok' : 'missing'">{{ preview.capabilities.multipartConditional ? '✓' : '×' }} 安全分片</span></div>
        <template v-if="preview.impact.scopeChanged">
          <div class="edit-warning"><ShieldCheck :size="16"/><span>Endpoint、Bucket、Region、前缀、请求方式或 WebDAV 用户名改变了存储身份。保存后同步任务保持暂停，旧基线、待执行队列和预览计划会清空，恢复任务前必须重新预览。</span></div>
          <p v-if="preview.impact.syncJobs.length" class="field-hint">受影响同步任务：{{ preview.impact.syncJobs.map(job => job.name).join('、') }}</p>
          <p v-if="preview.impact.gateways.length" class="field-hint">引用此连接的网关：{{ preview.impact.gateways.map(gateway => gateway.name).join('、') }}</p>
          <p v-if="preview.impact.backups || preview.impact.backupSnapshots" class="field-hint">引用此连接的备份任务：{{ preview.impact.backups }}；保留中的历史快照：{{ preview.impact.backupSnapshots }}。请保留原连接，另建连接使用新范围。</p>
        </template>
        <p v-else class="field-hint">存储范围未改变；同步基线继续有效。</p>
      </div>
      <footer class="modal-actions">
        <button class="button secondary" type="button" :disabled="busy" @click="close">取消</button>
        <button v-if="!preview?.token || preview.impact.blockers?.length" class="button secondary" type="submit" :disabled="busy"><RefreshCw v-if="busy" :size="14" class="spin"/><ShieldCheck v-else :size="14"/>{{ busy ? '正在验证…' : '验证更改' }}</button>
        <button v-else class="button primary" type="button" :disabled="busy" @click="save"><RefreshCw v-if="busy" :size="14" class="spin"/><Check v-else :size="14"/>{{ busy ? '正在保存…' : '确认保存' }}</button>
      </footer>
    </form>
  </BaseModal>
</template>

<style scoped>
.edit-review{display:grid;gap:9px;padding:13px;border:1px solid #e8e3f0;border-radius:12px;background:#fbfaff;color:#57516a;font-size:12px}.edit-capabilities{display:flex;flex-wrap:wrap;gap:6px}.edit-capabilities span{padding:4px 8px;border-radius:20px;background:#f1eff5}.edit-capabilities .ok{color:#347759;background:#e9f5ee}.edit-capabilities .missing{color:#a2534b;background:#fff0ed}.edit-warning,.edit-error{display:flex;align-items:flex-start;gap:8px;padding:10px;border:1px solid #f0d7bd;border-radius:10px;background:#fff9f0;color:#8a5c29;line-height:1.5}.edit-warning svg,.edit-error svg{flex:none;margin-top:1px}.edit-error{border-color:#efd4d0;background:#fff7f6;color:#a34f45}.edit-error span{overflow-wrap:anywhere}
</style>
