<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { Activity as ActivityIcon, AlertCircle, Archive, ArrowDownToLine, ArrowUpFromLine, Check, Clock3, File, Filter, FolderOpen, LoaderCircle, RefreshCw, Save, ShieldAlert, Trash2, RotateCcw, XCircle } from 'lucide-vue-next'
import { api } from '../api'
import BaseModal from '../components/BaseModal.vue'
import type { Activity, DownloadTransfer, MultipartUpload, OperationStatus, RecoveryEntry, RecoveryPreview } from '../types'
import { notify, store } from '../store'

type Tab = 'activity' | 'operations' | 'transfers' | 'recovery'
interface DirectoryAdoptPreview { id: string; token: string; exists: boolean; directory: boolean }
const tab = ref<Tab>('activity')
const filter = ref('全部')
const options = ['全部', '同步', '上传', '下载', '删除', '网关']
const recoveryEntries = ref<RecoveryEntry[]>([])
const recoveryLoading = ref(false)
const recoveryError = ref('')
const recoveryPreview = ref<RecoveryPreview | null>(null)
const adoptPreview = ref<DirectoryAdoptPreview | null>(null)
const adoptTarget = ref<OperationStatus | null>(null)
const adoptBusy = ref(false)
const previewLoading = ref('')
const operations = ref<OperationStatus[]>([])
const operationsLoading = ref(false)
const operationsError = ref('')
const uploads = ref<MultipartUpload[]>([])
const downloads = ref<DownloadTransfer[]>([])
const transfersLoading = ref(false)
const transfersError = ref('')
const busyId = ref('')
const savingId = ref('')
const confirmTarget = ref<{ kind: 'recovery' | 'upload' | 'download'; id: string; path: string; detail: string } | null>(null)
const filtered = computed(() => store.data.activities.filter((item) => filter.value === '全部' || kindLabel(item.kind) === filter.value))
function kindLabel(kind: string) {
  const k = kind.toLowerCase()
  if (k.includes('upload') || k.includes('上传')) return '上传'
  if (k.includes('download') || k.includes('下载')) return '下载'
  if (k.includes('delete') || k.includes('删除')) return '删除'
  if (k.includes('gateway') || k.includes('网关')) return '网关'
  return '同步'
}
function statusLabel(status: string) {
  const s = status.toLowerCase()
  if (['success', 'done', 'completed', 'committed', '成功'].includes(s)) return '已完成'
  if (['running', 'active', '进行中'].includes(s)) return '进行中'
  if (['failed', 'error', '失败'].includes(s)) return '失败'
  if (['conflict', 'attention', 'needs_attention', 'uncertain', 'committing', '需要处理'].includes(s)) return '需要核对'
  return status || '记录'
}
function statusTone(status: string) { const s = status.toLowerCase(); return ['success', 'done', 'completed', 'committed', '成功'].includes(s) ? 'green' : ['failed', 'error', '失败'].includes(s) ? 'red' : ['conflict', 'attention', 'needs_attention', 'uncertain', 'committing', '需要处理'].includes(s) ? 'amber' : 'gray' }
function iconFor(item: Activity) { const kind = kindLabel(item.kind); return kind === '上传' ? ArrowUpFromLine : kind === '下载' ? ArrowDownToLine : kind === '删除' ? Trash2 : kind === '网关' ? ShieldAlert : File }
function timeLabel(value: string) { if (!value) return '时间未知'; const d = new Date(value); return Number.isNaN(d.valueOf()) ? value : new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(d) }
function sizeLabel(value: number) { if (value < 1024) return value + ' B'; const units = ['KB', 'MB', 'GB', 'TB']; let size = value / 1024; let i = 0; while (size >= 1024 && i < units.length - 1) { size /= 1024; i++ } return size.toFixed(size >= 10 ? 0 : 1) + ' ' + units[i] }
function recoveryState(state: string) { const value = state.toLowerCase(); if (value==='cache-retired') return '缓存保留副本'; if (['ready', 'available', 'retained', 'untracked', 'committed', 'failed'].includes(value)) return '可另存'; if (value.includes('unknown') || value.includes('pending') || value === 'uncertain' || value === 'committing') return '操作待核对'; return state || '已保留' }
function recoveryTone(state: string) { return recoveryState(state) === '可另存' ? 'green' : 'amber' }
function canReconcile(state: string) { return state === 'uncertain' || state === 'committing' }
function canAdoptDirectory(item: OperationStatus) { return item.kind.toLowerCase().includes('mkdir') && item.state === 'uncertain' }
async function loadRecovery() {
  recoveryLoading.value = true; recoveryError.value = ''
  try { const result = await api.get<{ entries: RecoveryEntry[] }>('/api/recovery'); recoveryEntries.value = result.entries || [] }
  catch (error) { recoveryError.value = error instanceof Error ? error.message : '读取恢复副本失败'; recoveryEntries.value = [] }
  finally { recoveryLoading.value = false }
}
async function loadOperations() {
  operationsLoading.value = true; operationsError.value = ''
  try { operations.value = await api.get<OperationStatus[]>('/api/operations') || [] }
  catch (error) { operationsError.value = error instanceof Error ? error.message : '读取未决操作失败'; operations.value = [] }
  finally { operationsLoading.value = false }
}
async function loadTransfers() {
  transfersLoading.value = true; transfersError.value = ''
  try {
    const [uploadRows, downloadRows] = await Promise.all([api.get<MultipartUpload[]>('/api/transfers'), api.get<DownloadTransfer[]>('/api/downloads')])
    uploads.value = uploadRows || []; downloads.value = downloadRows || []
  } catch (error) { transfersError.value = error instanceof Error ? error.message : '读取传输列表失败' }
  finally { transfersLoading.value = false }
}
async function loadAll() { await Promise.all([loadRecovery(), loadOperations(), loadTransfers()]) }
async function saveRecovery(entry: RecoveryEntry) {
  if (savingId.value) return
  savingId.value = entry.id
  try { const result = await api.post<{ path: string }>('/api/recovery/save', { id: entry.id }); if (result.path) notify('副本已另存到：' + result.path, 'success'); else notify('已取消另存', 'info') }
  catch (error) { notify(error instanceof Error ? error.message : '另存副本失败') }
  finally { savingId.value = '' }
}
async function previewRestore(entry: RecoveryEntry) {
  if (!entry.canRestoreToRemote) { notify('此副本没有关联的远端路径，只能另存到本机。'); return }
  previewLoading.value = entry.id; recoveryPreview.value = null
  try { recoveryPreview.value = await api.post<RecoveryPreview>('/api/recovery/preview', { id: entry.id }) }
  catch (error) { notify(error instanceof Error ? error.message : '生成恢复预览失败') }
  finally { previewLoading.value = '' }
}
async function restoreRemote() {
  if (!recoveryPreview.value || busyId.value) return
  const preview = recoveryPreview.value
  busyId.value = preview.id
  try { await api.post('/api/recovery/restore', { id: preview.id, etag: preview.expectedCurrentEtag }); recoveryPreview.value = null; notify('恢复副本已按预览写回远端', 'success'); await loadRecovery(); await loadOperations() }
  catch (error) { notify(error instanceof Error ? error.message : '恢复写回失败；远端版本可能已变化，请重新预览。') }
  finally { busyId.value = '' }
}
async function reconcile(item: OperationStatus) {
  busyId.value = item.id
  try { await api.post('/api/operations/reconcile', { id: item.id }); notify('操作核对完成', 'success'); await loadOperations(); await loadRecovery() }
  catch (error) { notify(error instanceof Error ? error.message : '操作仍待核对') }
  finally { busyId.value = '' }
}
async function previewDirectoryAdoption(item: OperationStatus) {
  previewLoading.value = item.id; adoptPreview.value = null; adoptTarget.value = item
  try { adoptPreview.value = await api.post<DirectoryAdoptPreview>('/api/operations/adopt-preview', { id: item.id }) }
  catch (error) { notify(error instanceof Error ? error.message : '生成目录接管预览失败') }
  finally { previewLoading.value = '' }
}
async function adoptDirectory() {
  const preview = adoptPreview.value
  if (!preview || !preview.exists || !preview.directory || adoptBusy.value) return
  adoptBusy.value = true
  try { await api.post('/api/operations/adopt', { id: preview.id, token: preview.token }); adoptPreview.value = null; adoptTarget.value = null; notify('已按预览接管远端目录状态', 'success'); await loadOperations() }
  catch (error) { notify(error instanceof Error ? error.message : '目录接管失败；请重新预览当前状态') }
  finally { adoptBusy.value = false }
}
async function resumeUpload(item: MultipartUpload) {
  busyId.value = item.operationId
  try { await api.post('/api/transfers/resume', { id: item.operationId }); notify('分片上传已完成', 'success'); await loadTransfers(); await loadRecovery() }
  catch (error) { notify(error instanceof Error ? error.message : '续传失败；提交状态未知时请先核对操作。') }
  finally { busyId.value = '' }
}
async function resumeDownload(item: DownloadTransfer) {
  busyId.value = item.id
  try { await api.post('/api/downloads/resume', { id: item.id }); notify('续传下载已完成', 'success'); await loadTransfers() }
  catch (error) { notify(error instanceof Error ? error.message : '下载续传失败') }
  finally { busyId.value = '' }
}
function askConfirm(kind: 'recovery' | 'upload' | 'download', id: string, path: string, detail: string) { confirmTarget.value = { kind, id, path, detail } }
async function runConfirmedAction() {
  const target = confirmTarget.value
  if (!target || busyId.value) return
  busyId.value = target.id
  try {
    if (target.kind === 'recovery') await api.post('/api/recovery/delete', { id: target.id })
    else if (target.kind === 'upload') await api.post('/api/transfers/abort', { id: target.id })
    else await api.post('/api/downloads/cancel', { id: target.id })
    confirmTarget.value = null; notify(target.kind === 'recovery' ? '恢复副本已清理' : '传输已取消', 'success'); await loadAll()
  } catch (error) { notify(error instanceof Error ? error.message : '操作失败') }
  finally { busyId.value = '' }
}
function confirmTitle() { return confirmTarget.value?.kind === 'recovery' ? '清理恢复副本' : confirmTarget.value?.kind === 'upload' ? '中止分片上传' : '取消续传下载' }
onMounted(() => { void loadAll() })
</script>
<template>
  <div class="page-view activity-page">
    <header class="page-heading"><div><div class="eyebrow">RECENT EVENTS</div><h1>活动与恢复</h1><p>查看运行记录、核对未知结果，并管理本机保护副本和续传任务。</p></div><button class="button secondary" :disabled="recoveryLoading || operationsLoading || transfersLoading" @click="loadAll"><RefreshCw :size="15" :class="{ spin: recoveryLoading || operationsLoading || transfersLoading }" />刷新</button></header>
    <nav class="operation-tabs"><button v-for="item in [{id:'activity',label:'活动记录'},{id:'operations',label:'未决操作'},{id:'transfers',label:'续传队列'},{id:'recovery',label:'恢复副本'}]" :key="item.id" :class="{ active: tab === item.id }" :aria-pressed="tab === item.id" @click="tab = item.id as Tab">{{ item.label }}</button></nav>
    <template v-if="tab === 'activity'">
      <div class="activity-toolbar"><div class="filter-label"><Filter :size="15" />筛选</div><div class="filter-chips"><button v-for="option in options" :key="option" :class="{ active: filter === option }" :aria-pressed="filter === option" @click="filter = option">{{ option }}</button></div><span class="activity-count">{{ filtered.length }} 条记录</span></div>
      <div v-if="store.loading" class="activity-card"><div v-for="n in 5" :key="n" class="skeleton-row"></div></div>
      <div v-else-if="filtered.length" class="activity-card"><div v-for="(item, index) in filtered" :key="item.id || item.time + index" class="activity-row"><div class="activity-rail"><div class="activity-icon" :class="statusTone(item.status)"><component :is="iconFor(item)" :size="15" /></div><div v-if="index < filtered.length - 1" class="activity-line"></div></div><div class="activity-main"><div class="activity-title-row"><b>{{ item.message || kindLabel(item.kind) }}</b><span class="status-pill" :class="statusTone(item.status)"><i></i>{{ statusLabel(item.status) }}</span></div><div class="activity-subline"><span>{{ timeLabel(item.time) }}</span><span v-if="item.path" class="dot-separator">·</span><code v-if="item.path">{{ item.path }}</code></div></div><span class="activity-kind">{{ kindLabel(item.kind) }}</span></div></div>
      <div v-else class="empty-card activity-empty"><div class="activity-empty-icon"><Clock3 :size="24" /></div><h2>{{ filter === '全部' ? '还没有活动记录' : '没有' + filter + '记录' }}</h2><p>{{ filter === '全部' ? '这里会显示文件操作、同步进度和需要处理的事项。' : '试试其他筛选条件。' }}</p><button v-if="filter !== '全部'" class="button secondary small" @click="filter = '全部'">查看全部活动</button><div v-else class="empty-caption"><Check :size="14" />同步冲突和失败会在这里清楚标记</div></div>
      <div class="activity-note"><AlertCircle :size="14" />活动信息不包含密码或访问密钥。</div>
    </template>
    <section v-else-if="tab === 'operations'" class="workflow-panel">
      <div class="workflow-intro"><ShieldAlert :size="17" /><span><b>未知结果先核对，不自动重放</b><small>核对只比较操作回执与当前远端版本，不会再次写入。仍无法判断时，副本和暂存会继续保留。</small></span><button class="icon-button" title="刷新未决操作" aria-label="刷新未决操作" :disabled="operationsLoading" @click="loadOperations"><RefreshCw :size="15" :class="{ spin: operationsLoading }" /></button></div>
      <div v-if="operationsLoading" class="workflow-empty">正在读取操作记录…</div><div v-else-if="operationsError" class="workflow-error"><AlertCircle :size="16" /><span>{{ operationsError }}</span><button class="button secondary small" @click="loadOperations">重试</button></div><div v-else-if="operations.length" class="workflow-list"><article v-for="item in operations" :key="item.id" class="workflow-row"><div class="workflow-mark" :class="statusTone(item.state)"><AlertCircle :size="16" /></div><div class="workflow-copy"><div><b>{{ item.kind }} · {{ statusLabel(item.state) }}</b><span class="status-pill" :class="statusTone(item.state)"><i></i>{{ item.state }}</span></div><small v-if="item.sourcePath">源：{{ item.sourcePath }}</small><small v-if="item.destinationPath">目标：{{ item.destinationPath }}</small><small>{{ timeLabel(item.created) }}</small><small v-if="item.error" class="workflow-error-text">{{ item.error }}</small><code>{{ item.id }}</code></div><div class="workflow-actions"><button v-if="canReconcile(item.state)" class="button primary small" :disabled="busyId !== ''" @click="reconcile(item)"><RotateCcw :size="14" />{{ busyId === item.id ? '核对中…' : '核对远端状态' }}</button><button v-if="canAdoptDirectory(item)" class="button secondary small" :disabled="previewLoading !== ''" @click="previewDirectoryAdoption(item)"><FolderOpen :size="14"/>{{previewLoading===item.id?'正在预览…':'预览并接管目录'}}</button></div></article></div><div v-else class="workflow-empty">没有需要核对的操作。</div>
    </section>
    <section v-else-if="tab === 'transfers'" class="workflow-panel">
      <div class="workflow-intro"><ArrowDownToLine :size="17" /><span><b>中断传输</b><small>续传前会重新检查本地暂存和远端版本。提交结果未知的分片上传必须先在“未决操作”中核对。</small></span><button class="icon-button" title="刷新续传队列" aria-label="刷新续传队列" :disabled="transfersLoading" @click="loadTransfers"><RefreshCw :size="15" :class="{ spin: transfersLoading }" /></button></div>
      <div v-if="transfersLoading" class="workflow-empty">正在读取传输队列…</div><div v-else-if="transfersError" class="workflow-error"><AlertCircle :size="16" /><span>{{ transfersError }}</span><button class="button secondary small" @click="loadTransfers">重试</button></div>
      <template v-else>
        <h2 class="workflow-subhead"><ArrowUpFromLine :size="15" />分片上传 <span>{{ uploads.length }}</span></h2><div v-if="uploads.length" class="workflow-list"><article v-for="item in uploads" :key="item.operationId" class="workflow-row"><div class="workflow-mark"><ArrowUpFromLine :size="16" /></div><div class="workflow-copy"><div><b>{{ item.path }}</b><span class="status-pill" :class="statusTone(item.state)"><i></i>{{ item.state }}</span></div><small>{{ sizeLabel(item.size) }} · {{ item.parts?.length || 0 }} 个已登记分片 · {{ item.connectionId }}</small><code>{{ item.operationId }}</code></div><div class="workflow-actions"><button class="button secondary small" :disabled="busyId !== '' || item.state === 'completing'" @click="resumeUpload(item)"><RotateCcw :size="14" />{{ busyId === item.operationId ? '续传中…' : '续传' }}</button><button class="button subtle small danger-text" :disabled="busyId !== '' || item.state === 'completing'" @click="askConfirm('upload', item.operationId, item.path, '中止会清理远端分片和本机暂存；如果正在提交，系统会要求先核对。')"><XCircle :size="14" />中止</button></div></article></div><div v-else class="workflow-empty compact">没有待续传的分片上传。</div>
        <h2 class="workflow-subhead"><ArrowDownToLine :size="15" />续传下载 <span>{{ downloads.length }}</span></h2><div v-if="downloads.length" class="workflow-list"><article v-for="item in downloads" :key="item.id" class="workflow-row"><div class="workflow-mark"><ArrowDownToLine :size="16" /></div><div class="workflow-copy"><div><b>{{ item.path }}</b><span class="status-pill" :class="statusTone(item.state)"><i></i>{{ item.state }}</span></div><small>{{ sizeLabel(item.received) }} / {{ sizeLabel(item.size) }} · 保存到 {{ item.destination }}</small><small>版本 {{ item.etag }} · {{ timeLabel(item.updated) }}</small></div><div class="workflow-actions"><button class="button secondary small" :disabled="busyId !== ''" @click="resumeDownload(item)"><RotateCcw :size="14" />{{ busyId === item.id ? '续传中…' : '继续' }}</button><button class="button subtle small danger-text" :disabled="busyId !== ''" @click="askConfirm('download', item.id, item.destination, '取消会删除本机未完成暂存，不会修改远端文件。')"><XCircle :size="14" />取消</button></div></article></div><div v-else class="workflow-empty compact">没有中断的续传下载。</div>
      </template>
    </section>
    <section v-else class="settings-section recovery-section">
      <div class="section-heading"><div><h2><Archive :size="17" />恢复副本</h2><p>覆盖或删除前在本机保留的副本，可另存、预览后写回或逐项清理。</p></div><button class="icon-button" :disabled="recoveryLoading" title="刷新恢复副本" @click="loadRecovery"><RefreshCw :size="15" :class="{ spin: recoveryLoading }" /></button></div>
      <div class="recovery-scope-note"><ShieldAlert :size="14" /><span>操作结果未知时，关联恢复副本不会自动清理。每次写回都先对比当前远端版本；若版本变化，请重新预览。</span></div>
      <div v-if="recoveryLoading" class="activity-card recovery-list"><div v-for="n in 2" :key="n" class="skeleton-row"></div></div>
      <div v-else-if="recoveryError" class="recovery-error"><AlertCircle :size="16" /><span><b>无法读取恢复副本</b><small>{{ recoveryError }}</small></span><button class="button secondary small" @click="loadRecovery">重试</button></div>
      <div v-else-if="recoveryEntries.length" class="activity-card recovery-list"><div v-for="entry in recoveryEntries" :key="entry.id" class="recovery-row"><div class="recovery-file-icon"><File :size="16" /></div><div class="recovery-main"><div class="recovery-title-line"><b :title="entry.path">{{ entry.path }}</b><span class="status-pill" :class="recoveryTone(entry.state)"><i></i>{{ recoveryState(entry.state) }}</span></div><div class="recovery-subline"><span>{{ sizeLabel(entry.size) }}</span><span>·</span><span>{{ timeLabel(entry.created) }}</span><span>·</span><span>{{ entry.connectionId || '仅本地副本' }}</span><span>·</span><span :class="entry.integrity==='sha256'?'integrity-verified':'integrity-unverified'">{{entry.integrity==='sha256'?'SHA-256 恢复校验':entry.state==='cache-retired'?'可能仍被编辑 · 需手动清理':'未预先校验'}}</span></div></div><div class="recovery-actions"><button class="button secondary small" :disabled="savingId !== ''" @click="saveRecovery(entry)"><Save :size="14" />{{ savingId === entry.id ? '正在另存…' : '另存本机' }}</button><button v-if="entry.canRestoreToRemote" class="button subtle small" :disabled="previewLoading !== '' || !['ready','untracked','committed','failed'].includes(entry.state)" @click="previewRestore(entry)"><RefreshCw :size="14" />{{ previewLoading === entry.id ? '生成预览…' : '预览写回' }}</button><button class="button subtle small danger-text" :disabled="busyId !== '' || ['uncertain','committing'].includes(entry.state)" @click="askConfirm('recovery', entry.id, entry.path, '将永久删除这份本机恢复副本；关联操作待核对时系统会拒绝清理。')"><Trash2 :size="14" />清理</button></div></div></div>
      <div v-else class="recovery-empty"><Archive :size="17" /><span>当前没有可管理的恢复副本。</span></div>
    </section>
    <BaseModal :model-value="!!recoveryPreview" :dismissible="busyId===''" title="写回远端前核对" :subtitle="recoveryPreview?.path || ''" @update:model-value="(v) => { if (!v) recoveryPreview = null }" width="600px"><div v-if="recoveryPreview" class="restore-preview"><div class="restore-compare"><div><small>恢复副本</small><b>{{ sizeLabel(recoveryPreview.saved.size) }}</b><code>{{ recoveryPreview.saved.etag }}</code><code>SHA-256 {{ recoveryPreview.savedHash }}</code></div><div><small>当前远端</small><b>{{ recoveryPreview.currentExists ? sizeLabel(recoveryPreview.current?.size || 0) : '路径当前不存在' }}</b><code>{{ recoveryPreview.current?.etag || '—' }}</code></div></div><p>确认后会将恢复副本写回此远端路径。若当前版本再次变化，写回会失败并要求重新预览；替换前当前文件会再次保留为恢复副本。</p><footer class="modal-actions"><button class="button secondary" :disabled="busyId !== ''" @click="recoveryPreview = null">取消</button><button class="button danger" :disabled="busyId !== ''" @click="restoreRemote">{{ busyId === recoveryPreview.id ? '正在写回…' : '确认写回远端' }}</button></footer></div></BaseModal>
    <BaseModal :model-value="!!adoptPreview" :dismissible="busyId===''" title="接管远端目录状态" :subtitle="adoptTarget?.destinationPath || adoptTarget?.sourcePath || adoptTarget?.id" @update:model-value="v=>{if(!v){adoptPreview=null;adoptTarget=null}}"><div v-if="adoptPreview" class="restore-preview"><div class="adopt-state-card"><span>远端路径存在</span><b :class="adoptPreview.exists?'green-text':'red-text'">{{adoptPreview.exists?'是':'否'}}</b><span>当前对象为目录</span><b :class="adoptPreview.directory?'green-text':'red-text'">{{adoptPreview.directory?'是':'否'}}</b><code>{{adoptTarget?.destinationPath||adoptTarget?.sourcePath||'路径由操作记录提供'}}</code></div><p>接管只适用于已存在的目录：这会将本次 mkdir 操作记为已完成并解除阻断，不会创建、覆盖或删除远端内容。请确认目录确为预期目标。</p><footer class="modal-actions"><button class="button secondary" :disabled="adoptBusy" @click="adoptPreview=null;adoptTarget=null">取消</button><button class="button primary" :disabled="adoptBusy||!adoptPreview.exists||!adoptPreview.directory" @click="adoptDirectory"><LoaderCircle v-if="adoptBusy" :size="14" class="spin"/>确认接管目录</button></footer></div></BaseModal>
    <BaseModal :model-value="!!confirmTarget" :dismissible="busyId===''" :title="confirmTitle()" :subtitle="confirmTarget?.path || ''" @update:model-value="(v) => { if (!v) confirmTarget = null }"><div v-if="confirmTarget" class="confirm-panel"><div class="confirm-warning"><AlertCircle :size="18" /></div><p>{{ confirmTarget.detail }}</p><p><b>{{ confirmTarget.path }}</b></p><footer class="modal-actions"><button class="button secondary" :disabled="busyId !== ''" @click="confirmTarget = null">返回</button><button class="button danger" :disabled="busyId !== ''" @click="runConfirmedAction">{{ busyId === confirmTarget.id ? '处理中…' : '确认操作' }}</button></footer></div></BaseModal>
  </div>
</template>
<style scoped>
.operation-tabs{display:flex;gap:7px;overflow:auto;margin-bottom:18px;padding:4px;border-radius:12px;background:#f1eef7}.operation-tabs button{flex:none;padding:9px 14px;border:0;border-radius:9px;background:transparent;color:#716d82;font:inherit;cursor:pointer}.operation-tabs button.active{background:#fff;color:#59478e;box-shadow:0 2px 8px #45336714}.workflow-panel{display:grid;gap:14px}.workflow-intro{display:flex;align-items:center;gap:11px;padding:14px 16px;border:1px solid #eae6f2;border-radius:13px;background:#fbfaff;color:#5b5670}.workflow-intro>svg{flex:none;color:#8168ca}.workflow-intro span{display:grid;gap:3px;flex:1}.workflow-intro b{font-size:13px;color:#3d3852}.workflow-intro small{font-size:12px;color:#817d91;line-height:1.5}.workflow-list{display:grid;gap:9px}.workflow-row{display:flex;align-items:flex-start;gap:12px;padding:15px;border:1px solid #eae6f2;border-radius:14px;background:#fff}.workflow-mark{width:34px;height:34px;display:grid;place-items:center;flex:none;border-radius:10px;background:#f0edfa;color:#806bc4}.workflow-copy{display:grid;gap:4px;flex:1;min-width:0}.workflow-copy>div{display:flex;align-items:center;gap:8px;flex-wrap:wrap}.workflow-copy b{overflow-wrap:anywhere;color:#3d3852;font-size:13px}.workflow-copy small,.workflow-copy code{overflow-wrap:anywhere;color:#827e90;font-size:11px}.workflow-copy .workflow-error-text{color:#a34f45}.workflow-row>.button{flex:none}.workflow-actions{display:flex;flex-wrap:wrap;justify-content:flex-end;gap:6px}.workflow-empty,.workflow-error{display:flex;align-items:center;justify-content:center;gap:10px;min-height:86px;padding:15px;border:1px dashed #ddd7ea;border-radius:14px;background:#fcfbfe;color:#7a7689;font-size:13px}.workflow-empty.compact{min-height:52px}.workflow-error{justify-content:space-between;color:#a34f45}.workflow-subhead{display:flex;align-items:center;gap:7px;margin:8px 0 0;color:#51496a;font-size:14px}.workflow-subhead span{display:grid;place-items:center;min-width:22px;height:21px;margin-left:auto;border-radius:20px;background:#eeeaf7;color:#665890;font-size:11px}.restore-preview{display:grid;gap:14px}.restore-preview>p{margin:0;color:#6f6a7d;font-size:13px;line-height:1.6}.restore-compare{display:grid;grid-template-columns:1fr 1fr;gap:10px}.restore-compare>div{display:grid;gap:6px;padding:13px;border:1px solid #eae6f2;border-radius:12px;background:#fbfaff;min-width:0}.restore-compare small{color:#878397}.restore-compare b{color:#403951}.restore-compare code{overflow-wrap:anywhere;color:#6f6685;font-size:11px}.recovery-actions{display:flex;gap:5px;flex-wrap:wrap;justify-content:flex-end}@media(max-width:740px){.workflow-row{flex-wrap:wrap}.workflow-actions,.recovery-actions{width:100%;justify-content:flex-start}.restore-compare{grid-template-columns:1fr}}
.adopt-state-card{display:grid;grid-template-columns:1fr auto;gap:7px 14px;padding:14px;border:1px solid #eae6f2;border-radius:12px;background:#faf9fd;color:#797487;font-size:12px}.adopt-state-card b{color:#554d6a}.adopt-state-card code{grid-column:1/-1;overflow-wrap:anywhere;color:#625978;font-size:11px}.green-text{color:#4d9877!important}.red-text{color:#a34f45!important}.integrity-verified{color:#4d9877}.integrity-unverified{color:#99743d}
</style>
