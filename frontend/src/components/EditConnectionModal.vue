<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { AlertTriangle, Check, RefreshCw, ShieldCheck } from 'lucide-vue-next'
import { api } from '../api'
import { notify, refreshState } from '../store'
import type { Connection, ConnectionKind, WriteMode } from '../types'
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
  writeModeChanged: boolean
  recoveryChanged: boolean
  policyChanged: boolean
  impact: Impact
  capabilities: { conditionalWrite: boolean; conditionalDelete: boolean; rangeRead: boolean; multipartConditional: boolean }
  writeRestriction?: string
}

const props = defineProps<{ modelValue: boolean; connection: Connection | null }>()
const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>()
const form = reactive({ name: '', kind: 'webdav' as 'webdav' | 's3', endpoint: '', region: 'us-east-1', bucket: '', prefix: '', username: '', password: '', accessKey: '', secretKey: '', sessionToken: '', pathStyle: false, writeMode: 'standard' as WriteMode, keepRecovery: false })
const busy = ref(false)
const preview = ref<EditPreview | null>(null)
const issue = ref('')
const needsConfirmation = computed(() => !!preview.value && (preview.value.impact.scopeChanged || preview.value.policyChanged))
const blocked = computed(() => !!preview.value?.impact.blockers?.length)

function close() { preview.value = null; issue.value = ''; emit('update:modelValue', false) }
function payload() {
  if (!props.connection) throw new Error('连接已失效，请刷新后重试')
  return {
    id: props.connection.id, name: form.name.trim(), kind: form.kind as ConnectionKind,
    endpoint: form.endpoint.trim(), region: form.region.trim(), bucket: form.bucket.trim(),
    prefix: form.prefix.trim().replace(/^\/+|\/+$/g, ''), username: form.username.trim(),
    password: form.password, accessKey: form.accessKey, secretKey: form.secretKey,
    sessionToken: form.sessionToken, pathStyle: form.pathStyle, writeMode: form.writeMode, keepRecovery: form.keepRecovery,
  }
}
watch(() => [props.modelValue, props.connection] as const, ([open, connection]) => {
  if (!open || !connection) return
  Object.assign(form, { name: connection.name, kind: connection.kind as 'webdav' | 's3', endpoint: connection.endpoint, region: connection.region || (connection.kind === 's3' ? 'us-east-1' : ''), bucket: connection.bucket, prefix: connection.prefix, username: connection.username || '', password: '', accessKey: '', secretKey: '', sessionToken: '', pathStyle: connection.pathStyle, writeMode: connection.writeMode === 'copy' || connection.writeMode === 'strict' || connection.writeMode === 'compatible' ? connection.writeMode : 'standard', keepRecovery: connection.keepRecovery ?? false })
  preview.value = null
  issue.value = ''
}, { immediate: true })
watch(form, () => { preview.value = null; issue.value = '' }, { deep: true })

async function commitChanges(candidate: EditPreview) {
  if (!candidate.token || candidate.impact.blockers?.length) return
  try {
    await api.post('/api/connections/edit', { ...payload(), previewToken: candidate.token })
    await refreshState(true)
    notify('连接配置已自动检测并保存', 'success')
    close()
  } catch (error) {
    issue.value = error instanceof Error ? error.message : '保存连接配置失败'
    preview.value = null
  }
}

async function saveChanges() {
  if (busy.value) return
  busy.value = true; preview.value = null; issue.value = ''
  try {
    const candidate = await api.post<EditPreview>('/api/connections/edit-preview', payload())
    preview.value = candidate
    if (candidate.impact.blockers?.length) {
      issue.value = candidate.impact.blockers.join('；')
      return
    }
    if (!candidate.token) {
      issue.value = '连接检测未完成，请重试。'
      return
    }
    if (candidate.impact.scopeChanged || candidate.policyChanged) return
    await commitChanges(candidate)
  } catch (error) { issue.value = error instanceof Error ? error.message : '连接检测失败' }
  finally { busy.value = false }
}
async function confirmSave() {
  if (!preview.value?.token || busy.value || blocked.value) return
  busy.value = true; issue.value = ''
  try { await commitChanges(preview.value) }
  finally { busy.value = false }
}
</script>

<template>
  <BaseModal :model-value="modelValue" title="编辑存储连接" :subtitle="connection?.name" width="640px" :dismissible="!busy" @update:model-value="v => { if (!v) close() }">
    <form class="form-stack" :aria-busy="busy" @submit.prevent="saveChanges">
      <p class="field-hint">保存时自动检测。留空的凭据沿用现值。</p>
      <label class="field"><span>连接名称</span><input v-model="form.name" :disabled="busy" required maxlength="64" /></label>
      <label class="field"><span>{{ form.kind === 'webdav' ? '服务地址' : 'S3 Endpoint' }}</span><input v-model="form.endpoint" :disabled="busy" required type="url" /></label>
      <template v-if="form.kind === 'webdav'">
        <label class="field"><span>用户名</span><input v-model="form.username" :disabled="busy" autocomplete="username" placeholder="留空以保留当前用户名" /></label>
      </template>
      <details class="edit-options">
        <summary>存储位置与请求设置</summary>
        <template v-if="form.kind === 's3'">
          <label class="field"><span>Bucket</span><input v-model="form.bucket" :disabled="busy" required /></label>
          <div class="form-row"><label class="field"><span>Region</span><input v-model="form.region" :disabled="busy" required /></label><label class="field"><span>访问前缀</span><input v-model="form.prefix" :disabled="busy" placeholder="留空表示 Bucket 根目录" /></label></div>
          <label class="check-row"><input v-model="form.pathStyle" :disabled="busy" type="checkbox"/><span>使用 Path-style 请求</span></label>
        </template>
        <label v-else class="field"><span>远端前缀</span><input v-model="form.prefix" :disabled="busy" placeholder="留空表示连接根目录" /></label>
      </details>
      <details class="edit-options">
        <summary>替换连接凭据</summary>
        <template v-if="form.kind === 's3'">
          <div class="form-row"><label class="field"><span>访问密钥 ID</span><input v-model="form.accessKey" :disabled="busy" type="password" autocomplete="off" placeholder="留空以保留现有密钥" /></label><label class="field"><span>秘密访问密钥</span><input v-model="form.secretKey" :disabled="busy" type="password" autocomplete="new-password" placeholder="留空以保留现有密钥" /></label></div>
          <label class="field"><span>会话令牌 <small>可选</small></span><input v-model="form.sessionToken" :disabled="busy" type="password" autocomplete="off" placeholder="留空以保留现有令牌" /></label>
        </template>
        <template v-else>
          <label class="field"><span>密码</span><input v-model="form.password" :disabled="busy" type="password" autocomplete="new-password" placeholder="留空以保留现有密码" /></label>
        </template>
        <p class="field-hint">留空会保留已保存的凭据。新凭据会在保存时自动检测。</p>
      </details>
      <details class="edit-options">
        <summary>同步安全选项</summary>
        <label class="field"><span>同步方式</span><select v-model="form.writeMode" :disabled="busy"><option value="standard">常规同步（默认）</option><option v-if="connection?.writeMode === 'compatible' || form.writeMode === 'compatible'" value="compatible">常规同步（兼容设置）</option><option value="strict">严格防覆盖</option><option value="copy">仅上传为新副本</option></select></label>
        <p class="field-hint">{{ form.writeMode === 'copy' ? '上传文件时使用新名称，保留远端同名文件；同步下载可用，远端删除、移动和覆盖不可用。' : form.writeMode === 'strict' ? '仅在服务端支持条件写入时开放覆盖；删除、移动等操作也需服务端支持。' : '按原路径直接上传和更新同名文件。删除、移动等操作仍取决于服务端能力。' }}</p>
        <label class="check-row"><input v-model="form.keepRecovery" :disabled="busy" type="checkbox"/><span>覆盖或删除前保留恢复副本</span></label>
        <p v-if="form.keepRecovery" class="field-hint">恢复副本会占用本机空间。</p>
      </details>
      <div v-if="issue" class="edit-error" role="alert"><AlertTriangle :size="15"/><span>{{ issue }}</span></div>
      <div v-if="preview" class="edit-review">
        <b>自动检测结果</b>
        <p v-if="form.writeMode === 'strict' && preview.writeRestriction" class="edit-warning" role="status"><AlertTriangle :size="16"/><span>严格防覆盖受限：{{preview.writeRestriction}}</span></p>
        <details class="edit-capability-details"><summary>能力检测详情</summary><div class="edit-capabilities"><span :class="preview.capabilities.conditionalWrite ? 'ok' : 'missing'">{{ preview.capabilities.conditionalWrite ? '✓' : '×' }} 条件写入</span><span :class="preview.capabilities.conditionalDelete ? 'ok' : 'missing'">{{ preview.capabilities.conditionalDelete ? '✓' : '×' }} 条件删除</span><span :class="preview.capabilities.rangeRead ? 'ok' : 'missing'">{{ preview.capabilities.rangeRead ? '✓' : '×' }} 范围读取</span><span :class="preview.capabilities.multipartConditional ? 'ok' : 'missing'">{{ preview.capabilities.multipartConditional ? '✓' : '×' }} 安全分片</span></div><p v-if="preview.writeRestriction && form.writeMode !== 'strict'" class="field-hint">高级能力受限：{{preview.writeRestriction}}</p></details>
        <div v-if="preview.policyChanged" class="edit-warning"><ShieldCheck :size="16"/><span>同步方式或恢复副本设置变化后，此连接的同步任务会暂停，旧预览队列会清理，现有基线保留。重新预览后再启用。</span></div>
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
        <button v-if="needsConfirmation && !blocked" class="button primary" type="button" :disabled="busy" @click="confirmSave"><RefreshCw v-if="busy" :size="14" class="spin"/><Check v-else :size="14"/>{{ busy ? '正在保存…' : '确认影响并保存' }}</button>
        <button v-else class="button primary" type="submit" :disabled="busy"><RefreshCw v-if="busy" :size="14" class="spin"/><ShieldCheck v-else :size="14"/>{{ busy ? '正在检测并保存…' : blocked ? '重新检测' : '保存更改' }}</button>
      </footer>
    </form>
  </BaseModal>
</template>

<style scoped>
.edit-options{display:grid;gap:11px;padding:10px 12px;border:1px solid #e9e8ef;border-radius:10px;background:#fff}
.edit-options>summary,.edit-capability-details>summary{color:#5e5b6b;font-size:12px;font-weight:600;cursor:pointer}
.edit-options[open]>summary{margin-bottom:2px}
.edit-options .field-hint,.edit-capability-details .field-hint{margin:0}
.edit-capability-details{padding:9px 10px;border:1px solid #e8e3f0;border-radius:9px;background:#fff}
.edit-capability-details[open]>summary{margin-bottom:8px;color:#5e5b6b;font-size:12px;font-weight:600;cursor:pointer}
.edit-review{display:grid;gap:9px;padding:13px;border:1px solid #e8e3f0;border-radius:12px;background:#fbfaff;color:#57516a;font-size:12px}.edit-capabilities{display:flex;flex-wrap:wrap;gap:6px}.edit-capabilities span{padding:4px 8px;border-radius:20px;background:#f1eff5}.edit-capabilities .ok{color:#347759;background:#e9f5ee}.edit-capabilities .missing{color:#a2534b;background:#fff0ed}.edit-warning,.edit-error{display:flex;align-items:flex-start;gap:8px;padding:10px;border:1px solid #f0d7bd;border-radius:10px;background:#fff9f0;color:#8a5c29;line-height:1.5}.edit-warning svg,.edit-error svg{flex:none;margin-top:1px}.edit-error{border-color:#efd4d0;background:#fff7f6;color:#a34f45}.edit-error span{overflow-wrap:anywhere}
</style>
