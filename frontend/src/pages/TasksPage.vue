<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { Activity, AlertCircle, ArrowDownToLine, ArrowUpFromLine, ArrowLeftRight, Check, ChevronRight, Clock3, Folder, Pause, Play, Plus, RefreshCw, ScanSearch, Trash2, Pencil, ListChecks, RotateCcw, XCircle } from 'lucide-vue-next'
import { api } from '../api'
import BaseModal from '../components/BaseModal.vue'
import SquirrelMark from '../components/SquirrelMark.vue'
import { notify, refreshState, setSelectedConnection, store } from '../store'
import type { Direction, Job, JobPreview, QueueItem, SyncScanHistory } from '../types'
const emit = defineEmits<{ demo: []; addConnection: []; files: [] }>()
const showCreate = ref(false)
const submitting = ref(false)
const picking = ref(false)
const runningJob = ref('')
const previewBusy = ref('')
const showPreview = ref(false)
const showEdit = ref(false)
const selectedJob = ref<Job | null>(null)
const preview = ref<JobPreview | null>(null)
const selectedJobPaused = computed(() => !!selectedJob.value && store.data.jobs.some(j => j.id === selectedJob.value?.id && !j.enabled))
const deleteTarget = ref<Job | null>(null)
const editTarget = ref<Job | null>(null)
const deleteConfirmed = ref(false)
const queueFor = ref('')
const queueLoading = ref(false)
const queueItems = ref<QueueItem[]>([])
const queueError = ref('')
const scanHistoryFor = ref('')
const scanHistoryLoading = ref(false)
const scanHistoryItems = ref<SyncScanHistory[]>([])
const scanHistoryError = ref('')
const resolving = ref(false)
const resolution = ref<{ action: string; choice: 'local' | 'remote' | 'keep-both' } | null>(null)
const form = reactive({ name: '', connectionId: '', localPath: '', remotePath: '', direction: 'both' as Direction, excludeText: '', scheduleMinutes: 0, watch: false, deleteThreshold: 20 })
const selectedConnection = computed(() => store.data.connections.find((c) => c.id === form.connectionId))
const previewCounts = computed(() => {
  const actions = preview.value?.actions || []
  return {
    transfers: actions.filter((action) => action.kind === 'upload' || action.kind === 'download').length,
    conflicts: actions.filter((action) => action.kind === 'conflict').length,
    skipped: actions.filter((action) => action.kind === 'skip').length,
    deletes: actions.filter((action) => action.kind === 'delete-local' || action.kind === 'delete-remote').length,
  }
})
const statusText: Record<string, string> = { synced: '已同步', running: '正在同步', retrying: '正在重试', scanning: '正在扫描', paused: '已暂停', waiting: '等待网络', attention: '需要处理', needs_attention: '需要处理', failed: '失败', error: '失败', idle: '待同步' }
const statusTone: Record<string, string> = { synced: 'green', running: 'blue', retrying: 'blue', scanning: 'blue', paused: 'gray', waiting: 'amber', attention: 'amber', needs_attention: 'amber', failed: 'red', error: 'red', idle: 'gray' }
watch(showCreate, (open) => { if (open) resetForm() })
function resetForm() { Object.assign(form, { name: '', connectionId: store.selectedConnectionId, localPath: '', remotePath: '', direction: 'both', excludeText: '', scheduleMinutes: 0, watch: false, deleteThreshold: 20 }) }
function openCreate() { showCreate.value = true }
async function pickFolder() {
  picking.value = true
  try { const result = await api.post<{ path: string }>('/api/pick-folder'); if (result.path) form.localPath = result.path }
  catch (error) { notify(error instanceof Error ? error.message : '选择文件夹失败') }
  finally { picking.value = false }
}
async function createJob() {
  if (submitting.value) return
  submitting.value = true
  try {
    const job = await api.post<Job>('/api/jobs', jobPayload())
    if (form.connectionId !== store.selectedConnectionId) setSelectedConnection(form.connectionId)
    await refreshState(true)
    showCreate.value = false
    notify(`“${job.name || form.name}”已创建`, 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '创建任务失败') }
  finally { submitting.value = false }
}
function jobPayload() {
  return {
    name: form.name.trim(), connectionId: form.connectionId, localPath: form.localPath.trim(), remotePath: form.remotePath.trim(), direction: form.direction,
    exclude: form.excludeText.split(/\r?\n/).map((item) => item.trim()).filter(Boolean), scheduleMinutes: Number(form.scheduleMinutes), watch: form.watch, deleteThreshold: Number(form.deleteThreshold),
  }
}
function openEdit(job: Job) {
  editTarget.value = job
  Object.assign(form, { name: job.name, connectionId: job.connectionId, localPath: job.localPath, remotePath: job.remotePath, direction: job.direction, excludeText: (job.exclude || []).join('\n'), scheduleMinutes: job.scheduleMinutes || 0, watch: !!job.watch, deleteThreshold: job.deleteThreshold || 20 })
  showEdit.value = true
}
async function updateJob() {
  if (!editTarget.value || submitting.value) return
  submitting.value = true
  try {
    await api.post<Job>('/api/jobs/update', { ...editTarget.value, ...jobPayload() })
    showEdit.value = false
    editTarget.value = null
    await refreshState(true)
    notify('任务配置已更新', 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '更新任务失败') }
  finally { submitting.value = false }
}
async function previewJob(job: Job) {
  if (previewBusy.value || runningJob.value) return
  previewBusy.value = job.id
  preview.value = null
  deleteConfirmed.value = false
  selectedJob.value = job
  showPreview.value = true
  try {
    preview.value = await api.post<JobPreview>('/api/jobs/preview', { id: job.id })
    deleteConfirmed.value = false
  } catch (error) { showPreview.value = false; notify(error instanceof Error ? error.message : '生成预览失败') }
  finally { previewBusy.value = '' }
}
async function runJob() {
  if (!selectedJob.value || !preview.value?.token || runningJob.value) return
  if (preview.value.actions.some((action) => action.kind === 'conflict')) {
    notify('请在下方逐项选择冲突处理方式，然后重新核对。')
    return
  }
  if ((preview.value.deletePaths?.length || 0) > 0 && !deleteConfirmed.value) {
    notify('请先查看待删除路径并勾选确认。')
    return
  }
  const needsConditionalWrite = preview.value.actions.some((action) => action.kind === 'upload')
  const needsConditionalDelete = preview.value.actions.some((action) => action.kind === 'delete-remote')
  const connection = store.data.connections.find((item) => item.id === selectedJob.value?.connectionId)
  if (needsConditionalWrite && connection?.kind !== 'demo' && !(connection?.tested && connection.capabilities?.conditionalWrite)) {
    notify('该连接尚未通过读写验证，请先到设置中验证读写能力。')
    return
  }
  if (needsConditionalDelete && connection?.kind !== 'demo' && !(connection?.tested && connection.capabilities?.conditionalDelete)) {
    notify('该连接尚未通过条件删除验证，请先到设置中验证读写能力。')
    return
  }
  runningJob.value = selectedJob.value.id
  try {
    await api.post<Job>('/api/jobs/run', { id: selectedJob.value.id, token: preview.value.token, confirmDeletes: deleteConfirmed.value })
    showPreview.value = false
    notify('同步执行完成', 'success')
    await refreshState(true)
  } catch (error) { notify(error instanceof Error ? error.message : '启动同步失败') }
  finally { runningJob.value = '' }
}
async function pauseJob(job: Job) {
  if (runningJob.value) return
  runningJob.value = job.id
  try { await api.post('/api/jobs/pause', { id: job.id }); await refreshState(true); notify('任务已暂停', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '暂停任务失败') }
  finally { runningJob.value = '' }
}
async function resumeJob(job: Job) {
  if (runningJob.value) return
  runningJob.value = job.id
  try { await api.post('/api/jobs/resume', { id: job.id }); await refreshState(true); notify('任务已恢复', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '恢复任务失败') }
  finally { runningJob.value = '' }
}
async function retryJob(job: Job) {
  runningJob.value = job.id
  try { await api.post<Job>('/api/jobs/retry', { id: job.id }); await refreshState(true); notify('失败项已重新核对', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '重试失败') }
  finally { runningJob.value = '' }
}
async function loadQueue(job: Job) {
  if (queueFor.value === job.id) { queueFor.value = ''; queueItems.value = []; return }
  queueFor.value = job.id
  queueLoading.value = true
  queueError.value = ''
  try { queueItems.value = await api.get<QueueItem[]>(`/api/jobs/queue?id=${encodeURIComponent(job.id)}`) || [] }
  catch (error) { queueError.value = error instanceof Error ? error.message : '读取重试队列失败'; queueItems.value = [] }
  finally { queueLoading.value = false }
}
async function loadScanHistory(job: Job, refresh = false) {
  if (!refresh && scanHistoryFor.value === job.id) { scanHistoryFor.value = ''; scanHistoryItems.value = []; return }
  scanHistoryFor.value = job.id
  scanHistoryLoading.value = true
  scanHistoryError.value = ''
  try { scanHistoryItems.value = await api.get<SyncScanHistory[]>(`/api/jobs/scan-history?id=${encodeURIComponent(job.id)}`) || [] }
  catch (error) { scanHistoryError.value = error instanceof Error ? error.message : '读取核对记录失败'; scanHistoryItems.value = [] }
  finally { scanHistoryLoading.value = false }
}
function scanStatusText(status: string) { return ({ scanning: '扫描中', success: '完成', needs_attention: '需要处理', error: '失败', cancelled: '已取消', interrupted: '中断' } as Record<string, string>)[status] || status }
function requestResolution(path: string, choice: 'local' | 'remote' | 'keep-both') { resolution.value = { action: path, choice } }
async function confirmResolution() {
  if (!selectedJob.value || !resolution.value || resolving.value) return
  resolving.value = true
  try {
    await api.post('/api/jobs/resolve', { id: selectedJob.value.id, path: resolution.value.action, choice: resolution.value.choice })
    resolution.value = null
    await previewJob(selectedJob.value)
    await refreshState(true)
    notify('冲突已处理，已生成新的核对预览', 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '冲突处理失败') }
  finally { resolving.value = false }
}
async function removeJob() {
  if (!deleteTarget.value || runningJob.value) return
  runningJob.value = deleteTarget.value.id
  try { await api.post('/api/jobs/delete', { id: deleteTarget.value.id }); notify('任务已删除', 'success'); deleteTarget.value = null; await refreshState(true) }
  catch (error) { notify(error instanceof Error ? error.message : '删除任务失败') }
  finally { runningJob.value = '' }
}
function directionLabel(direction: Direction) { return direction === 'both' ? '双向同步' : direction === 'upload' ? '仅上传' : direction === 'download' ? '仅下载' : direction === 'mirror-upload' ? '镜像上传' : '镜像下载' }
function directionIcon(direction: Direction) { return direction === 'both' ? ArrowLeftRight : direction === 'upload' || direction === 'mirror-upload' ? ArrowUpFromLine : ArrowDownToLine }
function connectionName(id: string) { return store.data.connections.find((c) => c.id === id)?.name || '已移除的连接' }
function displayTime(value: string) { if (!value) return '尚未运行'; const d = new Date(value); return Number.isNaN(d.valueOf()) ? value : new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(d) }
function formatBytes(bytes: number) { if (bytes < 1024) return `${bytes} B`; const units = ['KB', 'MB', 'GB', 'TB']; let value = bytes / 1024; let i = 0; while (value >= 1024 && i < units.length - 1) { value /= 1024; i++ } return `${value.toFixed(value >= 10 ? 0 : 1)} ${units[i]}` }
</script>
<template>
  <div class="page-view tasks-page">
    <header class="page-heading"><div><div class="eyebrow">SYNC WORKSPACE</div><h1>同步任务</h1><p>让本地文件夹与远端保持有序同步。</p></div><button class="button primary" :disabled="!store.data.connections.length" @click="openCreate"><Plus :size="16" />新建任务</button></header>
    <div class="safety-note"><span class="safety-dot"></span><span>每次执行前先预览。覆盖和删除会尝试保留恢复副本；冲突可逐项选择处理方式，批量删除需要单独确认。</span></div>
    <div v-if="store.loading" class="task-loading-state" role="status"><SquirrelMark variant="eat" :size="64" /><span>正在读取同步任务…</span></div>
    <div v-else-if="store.data.jobs.length" class="job-list">
      <article v-for="job in store.data.jobs" :key="job.id" class="job-card">
        <div class="job-card-main">
          <div class="job-icon" :class="{ 'job-icon-working': job.status === 'running' || job.status === 'scanning' }"><SquirrelMark v-if="job.status === 'running' || job.status === 'scanning'" variant="eat" :size="36" /><component v-else :is="directionIcon(job.direction)" :size="18" /></div>
          <div class="job-info"><div class="job-title-line"><h2>{{ job.name }}</h2><span class="status-pill" :class="statusTone[job.status] || 'gray'"><i></i>{{ statusText[job.status] || job.status }}</span><span class="direction-pill">{{ directionLabel(job.direction) }}</span></div><div class="job-connection"><span>{{ connectionName(job.connectionId) }}</span><span class="dot-separator">·</span><span>{{ displayTime(job.lastRun) }}</span></div><div v-if="job.lastScanSummary" class="job-scan-result"><Clock3 :size="12" />最近核对 · {{ displayTime(job.lastScanAt || '') }} · {{ job.lastScanSummary }}</div></div>
          <div class="job-quick-actions"><button class="icon-button compact-action" title="预览同步" :disabled="!!previewBusy || !!runningJob" @click="previewJob(job)"><RefreshCw v-if="previewBusy === job.id" :size="16" class="spin" /><ScanSearch v-else :size="16" /></button><button class="icon-button compact-action" title="编辑任务" :disabled="job.status === 'running' || job.status === 'scanning' || job.status === 'retrying'" @click="openEdit(job)"><Pencil :size="15" /></button><button class="icon-button compact-action" title="查看队列" @click="loadQueue(job)"><ListChecks :size="16" /></button><button class="icon-button compact-action" title="查看近期核对记录" @click="loadScanHistory(job)"><Clock3 :size="15" /></button></div>
        </div>
        <div class="job-paths"><div><span class="path-label">本地文件夹</span><span class="path-value" :title="job.localPath"><Folder :size="14" />{{ job.localPath || '尚未选择' }}</span></div><ChevronRight :size="15" class="muted-icon path-arrow" /><div><span class="path-label">远端目录</span><span class="path-value" :title="job.remotePath">{{ job.remotePath || '/' }}</span></div></div>
        <div v-if="job.queueTotal" class="job-progress"><span>{{ job.queueDone || 0 }} / {{ job.queueTotal }} 项</span><div><i :style="{ width: `${Math.min(100, Math.max(0, job.progress || 0))}%` }"></i></div><b>{{ Math.min(100, Math.max(0, job.progress || 0)) }}%</b></div>
          <div class="job-card-footer"><span class="job-detail"><Activity :size="14" />{{ job.detail || (job.status === 'running' ? '正在同步文件…' : job.status === 'scanning' ? '正在扫描本地与远端文件…' : '首次运行会先扫描并生成操作预览。') }}</span><div class="inline-actions"><button v-if="job.status === 'running'" class="button subtle small" :disabled="runningJob!==''" @click="pauseJob(job)"><XCircle :size="14" />{{runningJob===job.id?'正在取消…':'取消'}}</button><button v-else-if="job.status === 'paused' || !job.enabled" class="button subtle small" :disabled="runningJob!==''" @click="resumeJob(job)"><Play :size="14" />{{runningJob===job.id?'正在恢复…':'恢复'}}</button><button v-else-if="['error','failed','attention','needs_attention'].includes(job.status)" class="button subtle small" :disabled="runningJob !== ''" @click="retryJob(job)"><RotateCcw :size="14" />{{ runningJob === job.id ? '核对中…' : '重试' }}</button><button v-else class="button subtle small" :disabled="runningJob!==''" @click="pauseJob(job)"><Pause :size="14" />{{runningJob===job.id?'正在暂停…':'暂停'}}</button><button class="button subtle small danger-text" :disabled="runningJob!==''" @click="deleteTarget = job"><Trash2 :size="14" />删除</button></div></div>
        <section v-if="queueFor === job.id" class="job-queue-panel"><div class="queue-head"><div><b>最近操作队列</b><small>按任务暂停、取消与重试；重试时会重新核对未完成项。</small></div><button class="text-button" :disabled="queueLoading" @click="loadQueue(job)"><RefreshCw :size="13" :class="{ spin: queueLoading }" />刷新</button></div><div v-if="queueLoading" class="queue-empty queue-loading-state" role="status"><SquirrelMark variant="eat" :size="28" /><span>正在读取队列…</span></div><div v-else-if="queueError" class="queue-error"><span>{{ queueError }}</span><button class="button secondary small" @click="loadQueue(job)">重试</button></div><div v-else-if="queueItems.length" class="queue-list"><div v-for="item in queueItems" :key="`${item.path}-${item.kind}`" class="queue-row"><div><b :title="item.path">{{ item.path }}</b><small>{{ item.kind }} · {{ item.state }}<template v-if="item.bytesDone"> · {{ formatBytes(item.bytesDone) }}</template></small><small v-if="item.error" class="queue-error-text">{{ item.error }}</small></div><time>{{ displayTime(item.updated) }}</time></div></div><div v-else class="queue-empty">队列为空。失败或中断的项目会在此显示。</div></section>
        <section v-if="scanHistoryFor === job.id" class="job-queue-panel scan-history-panel"><div class="queue-head"><div><b>近期核对记录</b><small>每次完整扫描的范围、结果和预计文件传输量。</small></div><button class="text-button" :disabled="scanHistoryLoading" @click="loadScanHistory(job, true)"><RefreshCw :size="13" :class="{ spin: scanHistoryLoading }" />刷新</button></div><div v-if="scanHistoryLoading" class="queue-empty queue-loading-state" role="status"><SquirrelMark variant="eat" :size="28" /><span>正在读取核对记录…</span></div><div v-else-if="scanHistoryError" class="queue-error"><span>{{ scanHistoryError }}</span><button class="button secondary small" @click="loadScanHistory(job, true)">重试</button></div><div v-else-if="scanHistoryItems.length" class="queue-list"><div v-for="item in scanHistoryItems" :key="item.id" class="queue-row scan-history-row"><div><b>{{ displayTime(item.started) }} · {{ scanStatusText(item.status) }}</b><small>本地 {{ item.localFiles }} 个文件 / {{ item.localDirectories }} 个目录；远端 {{ item.remoteFiles }} 个文件 / {{ item.remoteDirectories }} 个目录</small><small>上传 {{ item.uploads }} 项 · {{ formatBytes(item.uploadBytes) }}；下载 {{ item.downloads }} 项 · {{ formatBytes(item.downloadBytes) }}；删除 {{ item.deletes }} 项 · 冲突 {{ item.conflicts }} 项</small><small>范围：{{ item.scope.localPath }} ↔ {{ item.scope.remotePath || '/' }} · {{ directionLabel(item.scope.direction) }}</small><small v-if="item.error" class="queue-error-text">{{ item.error }}</small></div><time>{{ item.completed ? displayTime(item.completed) : '进行中' }}</time></div></div><div v-else class="queue-empty">还没有核对记录。</div></section>
      </article>
    </div>
    <div v-else class="empty-card task-empty">
      <div class="empty-illustration"><div class="empty-orbit orbit-one"></div><div class="empty-orbit orbit-two"></div><div class="empty-folder"><Folder :size="30" /></div><span class="empty-spark spark-a">✦</span><span class="empty-spark spark-b">✦</span></div>
      <div class="eyebrow">YOUR FILES, IN SYNC</div><h2>{{ store.data.connections.length ? '创建第一个同步任务' : '从一个安全的演示空间开始' }}</h2><p>{{ store.data.connections.length ? '选择本地文件夹和远端目录，先检查预览，再决定要执行哪些操作。' : '演示空间完全隔离，不会访问你的真实存储。添加连接后，也可以在这里创建同步任务。' }}</p>
      <div class="empty-actions"><button v-if="!store.data.connections.length" class="button primary" @click="emit('demo')"><Plus :size="16" />建立演示空间</button><button v-else class="button primary" @click="openCreate"><Plus :size="16" />新建同步任务</button><button v-if="!store.data.connections.length" class="button secondary" @click="emit('addConnection')">添加存储连接</button></div>
      <div class="empty-caption"><Check :size="14" />预览后执行 · 冲突逐项决议 · 批量删除单独确认</div>
    </div>
    <div v-if="store.data.jobs.length" class="page-footnote"><Clock3 :size="14" />任务状态每 3 秒更新一次</div>

    <BaseModal v-model="showCreate" :dismissible="!submitting && !picking" title="新建同步任务" subtitle="先选目录和方向，首次执行前会展示完整操作预览。">
      <form class="form-stack" @submit.prevent="createJob">
        <label class="field"><span>任务名称</span><input v-model="form.name" required maxlength="80" placeholder="例如：工作资料" /></label>
        <label class="field"><span>存储连接</span><select v-model="form.connectionId" required><option value="" disabled>选择一个连接</option><option v-for="c in store.data.connections" :key="c.id" :value="c.id">{{ c.name }} · {{ c.kind === 'demo' ? '演示空间' : c.kind === 's3' ? 'S3' : 'WebDAV' }}</option></select></label>
        <div class="field"><span>本地文件夹</span><div class="input-button-row"><input v-model="form.localPath" required placeholder="选择本机文件夹" /><button class="button secondary" type="button" :disabled="picking" @click="pickFolder">{{ picking ? '选择中…' : '浏览' }}</button></div></div>
        <label class="field"><span>远端目录</span><input v-model="form.remotePath" placeholder="/，留空表示连接根目录" /></label>
        <label class="field"><span>同步方向</span><select v-model="form.direction"><option value="both">双向同步</option><option value="upload">仅上传</option><option value="download">仅下载</option><option value="mirror-upload">镜像上传（会删除远端多余文件）</option><option value="mirror-download">镜像下载（会删除本地多余文件）</option></select></label>
        <div v-if="form.direction.startsWith('mirror')" class="task-danger-note"><AlertCircle :size="16" /><span>镜像会让目标侧匹配源侧。执行前会显示待删除路径；达到阈值后必须额外确认，删除前会尝试保留恢复副本。</span></div>
        <label v-if="form.direction.startsWith('mirror')" class="field"><span>删除确认阈值</span><input v-model.number="form.deleteThreshold" type="number" min="1" max="10000" required /><small class="field-hint">待删除数达到此值时，执行预览必须显式确认。</small></label>
        <label class="field"><span>排除规则 <small>每行一条</small></span><textarea v-model="form.excludeText" rows="3" placeholder="例如：cache/**&#10;*.tmp"></textarea><small class="field-hint">支持 glob 规则；排除路径不会产生删除操作。</small></label>
        <div class="form-row"><label class="field"><span>定时核对</span><select v-model.number="form.scheduleMinutes"><option :value="0">关闭，仅手动</option><option :value="5">每 5 分钟</option><option :value="15">每 15 分钟</option><option :value="30">每 30 分钟</option><option :value="60">每小时</option><option :value="360">每 6 小时</option></select></label><label class="field"><span>自动观察</span><span class="check-row"><input v-model="form.watch" type="checkbox" />文件变化后自动核对</span><small class="field-hint">自动任务遇到冲突或大批删除会暂停等待处理。</small></label></div>
        <footer class="modal-actions"><button class="button secondary" type="button" :disabled="submitting || picking" @click="showCreate = false">取消</button><button class="button primary" type="submit" :disabled="submitting">{{ submitting ? '正在创建…' : '创建任务' }}</button></footer>
      </form>
    </BaseModal>
    <BaseModal v-model="showEdit" :dismissible="!submitting && !picking" title="编辑同步任务" :subtitle="editTarget ? `修改 ${editTarget.name} 的同步范围与调度设置。范围变更会重置同步基线。` : ''" width="620px">
      <form class="form-stack" @submit.prevent="updateJob">
        <label class="field"><span>任务名称</span><input v-model="form.name" required maxlength="80" /></label>
        <label class="field"><span>存储连接</span><select v-model="form.connectionId" required><option v-for="c in store.data.connections" :key="c.id" :value="c.id">{{ c.name }} · {{ c.kind === 'demo' ? '演示空间' : c.kind === 's3' ? 'S3' : 'WebDAV' }}</option></select></label>
        <div class="field"><span>本地文件夹</span><div class="input-button-row"><input v-model="form.localPath" required /><button class="button secondary" type="button" :disabled="picking" @click="pickFolder">{{ picking ? '选择中…' : '浏览' }}</button></div></div>
        <label class="field"><span>远端目录</span><input v-model="form.remotePath" placeholder="留空表示连接根目录" /></label>
        <label class="field"><span>同步方向</span><select v-model="form.direction"><option value="both">双向同步</option><option value="upload">仅上传</option><option value="download">仅下载</option><option value="mirror-upload">镜像上传（会删除远端多余文件）</option><option value="mirror-download">镜像下载（会删除本地多余文件）</option></select></label>
        <div v-if="form.direction.startsWith('mirror')" class="task-danger-note"><AlertCircle :size="16" /><span>镜像会删除目标侧多余文件。每次执行都必须检查路径预览并确认；远端删除前会尝试保留本机恢复副本。</span></div>
        <label v-if="form.direction.startsWith('mirror')" class="field"><span>删除确认阈值</span><input v-model.number="form.deleteThreshold" type="number" min="1" max="10000" required /></label>
        <label class="field"><span>排除规则 <small>每行一条</small></span><textarea v-model="form.excludeText" rows="3" placeholder="cache/**&#10;*.tmp"></textarea><small class="field-hint">排除路径不参与传输或删除。</small></label>
        <div class="form-row"><label class="field"><span>定时核对</span><select v-model.number="form.scheduleMinutes"><option :value="0">关闭，仅手动</option><option :value="5">每 5 分钟</option><option :value="15">每 15 分钟</option><option :value="30">每 30 分钟</option><option :value="60">每小时</option><option :value="360">每 6 小时</option></select></label><label class="field"><span>自动观察</span><span class="check-row"><input v-model="form.watch" type="checkbox" />文件变化后自动核对</span></label></div>
        <footer class="modal-actions"><button class="button secondary" type="button" :disabled="submitting || picking" @click="showEdit = false">取消</button><button class="button primary" type="submit" :disabled="submitting">{{ submitting ? '正在保存…' : '保存任务' }}</button></footer>
      </form>
    </BaseModal>
    <BaseModal v-model="showPreview" title="同步预览" :subtitle="selectedJob ? `${selectedJob.name} · 检查后再执行` : ''" width="620px">
      <div v-if="previewBusy && !preview" class="preview-loading-state" role="status"><SquirrelMark variant="eat" :size="64" /><span>正在生成同步预览…</span></div>
      <div v-else-if="preview" class="preview-content">
        <div v-if="runningJob === selectedJob?.id" class="sync-executing-state" role="status"><SquirrelMark variant="eat" :size="44" /><div><b>正在同步文件…</b><span>可以关闭此窗口，在任务卡片中查看进度。</span></div></div>
        <div class="preview-summary"><div><strong>{{ previewCounts.transfers }}</strong><span>项待传输</span></div><div class="preview-summary-details"><span v-if="previewCounts.conflicts" class="conflict-count">{{ previewCounts.conflicts }} 项冲突</span><span v-if="previewCounts.skipped" class="skip-count">{{ previewCounts.skipped }} 项跳过</span><span v-if="previewCounts.deletes" class="conflict-count">{{ previewCounts.deletes }} 项删除</span><span v-if="!previewCounts.deletes">本次不删除文件</span></div></div>
        <div class="preview-transfer-estimates"><div><ArrowUpFromLine :size="14" /><span>预计上传</span><b>{{ formatBytes(preview.uploadBytes || 0) }}</b></div><div><ArrowDownToLine :size="14" /><span>预计下载</span><b>{{ formatBytes(preview.downloadBytes || 0) }}</b></div><small>按当前预览中的文件版本统计；删除、跳过和目录操作不计入传输量。</small></div>
        <div v-if="preview.actions.length" class="preview-list"><div v-for="(action, i) in preview.actions" :key="`${action.path}-${i}`" class="preview-row"><span class="action-mark" :class="action.kind">{{ action.kind === 'upload' ? '↑' : action.kind === 'download' ? '↓' : action.kind === 'conflict' ? '!' : action.kind.startsWith('delete') ? '−' : '·' }}</span><span class="preview-path"><b>{{ action.path }}</b><small>{{ action.reason }}<template v-if="action.kind === 'upload' || action.kind === 'download'"> · {{ formatBytes(action.size || 0) }}</template></small></span><span class="action-label" :class="action.kind">{{ action.kind === 'upload' ? '上传' : action.kind === 'download' ? '下载' : action.kind === 'conflict' ? '冲突' : action.kind.startsWith('delete') ? '删除' : '跳过' }}</span></div></div>
        <div v-if="preview.deletePaths?.length" class="destructive-preview"><div class="destructive-preview-heading"><AlertCircle :size="17" /><span><b>待删除 {{ preview.deleteCount || preview.deletePaths.length }} 项</b><small>以下是这次预览中的具体路径；重新核对后清单会变化。</small></span></div><div class="delete-path-list"><code v-for="item in preview.deletePaths" :key="item">{{ item }}</code></div><label class="check-row destructive-confirm"><input v-model="deleteConfirmed" type="checkbox" />我已检查上述路径，确认这些删除操作<span v-if="preview.requiresDeleteConfirmation">（超过任务阈值）</span></label></div>
        <div v-else-if="!preview.actions.length" class="preview-clear"><Check :size="18" /><span>当前没有需要同步的改动。</span></div>
        <div v-if="preview.actions.some((a) => a.kind === 'conflict')" class="preview-conflict-warning"><AlertCircle :size="17" /><div><b>发现冲突，请逐项选择</b><span>选择会覆盖的方向前会再次展示具体路径；覆盖操作仍受版本检查和恢复副本保护。</span></div></div>
        <div v-if="preview.actions.some((a) => a.kind === 'conflict')" class="conflict-actions-list"><div v-for="action in preview.actions.filter((a) => a.kind === 'conflict')" :key="action.path" class="conflict-action-row"><div><b>{{ action.path }}</b><small>{{ action.reason }}</small></div><div class="conflict-buttons"><button class="button subtle small" @click="requestResolution(action.path, 'local')">保留本地</button><button class="button subtle small" @click="requestResolution(action.path, 'remote')">保留远端</button><button class="button subtle small" @click="requestResolution(action.path, 'keep-both')">保留两份</button></div></div></div>
        <p v-if="!preview.actions.some((a) => a.kind === 'conflict') && !preview.deletePaths?.length" class="preview-hint">本次核对没有删除路径；有变化的文件仍会按预览和版本检查处理。</p>
        <p v-if="selectedJobPaused" class="safety-note">任务已暂停。请关闭预览并先恢复任务，再重新预览执行。</p><footer class="modal-actions"><button class="button secondary" @click="showPreview = false">关闭</button><button class="button primary" :disabled="selectedJobPaused || runningJob !== '' || preview.actions.some((a) => a.kind === 'conflict') || (!preview.actions.length && !preview.deletePaths?.length) || (!!preview.deletePaths?.length && !deleteConfirmed)" @click="runJob"><Play :size="15" />{{ runningJob ? '同步中…' : '执行同步' }}</button></footer>
      </div>
    </BaseModal>
    <BaseModal :model-value="!!resolution" title="确认冲突处理" :subtitle="resolution?.action || ''" @update:model-value="(v) => { if (!v) resolution = null }">
      <div v-if="resolution" class="confirm-panel"><div class="confirm-warning"><AlertCircle :size="18" /></div><p><b>{{ resolution.action }}</b></p><p>{{ resolution.choice === 'local' ? '将以本地版本替换远端版本。' : resolution.choice === 'remote' ? '将以远端版本替换本地文件。' : '将保留当前本地文件，并把远端版本另存为副本。' }}覆盖前会按本机保护策略处理。</p><footer class="modal-actions"><button class="button secondary" :disabled="resolving" @click="resolution = null">返回</button><button class="button primary" :disabled="resolving" @click="confirmResolution">{{ resolving ? '正在处理…' : '确认此项选择' }}</button></footer></div>
    </BaseModal>
    <BaseModal :model-value="!!deleteTarget" title="删除同步任务" subtitle="删除任务配置不会删除本地或远端文件。" @update:model-value="(v) => { if (!v) deleteTarget = null }">
      <div class="confirm-panel"><div class="confirm-warning"><Trash2 :size="18" /></div><p>确定删除“{{ deleteTarget?.name }}”吗？此操作只移除任务配置，不会删除本地或远端文件。</p><footer class="modal-actions"><button class="button secondary" :disabled="runningJob!==''" @click="deleteTarget = null">取消</button><button class="button danger" :disabled="runningJob!==''" @click="removeJob">{{runningJob===deleteTarget?.id?'正在删除…':'删除任务'}}</button></footer></div>
    </BaseModal>
  </div>
</template>

<style scoped>
.job-quick-actions,.conflict-buttons{display:flex;align-items:center;gap:6px}.job-progress{display:flex;align-items:center;gap:10px;margin:0 24px 12px 74px;color:#77748a;font-size:12px}.job-progress>div{flex:1;height:6px;border-radius:8px;background:#efedf5;overflow:hidden}.job-progress i{display:block;height:100%;background:#8971db;border-radius:8px;transition:width .2s}.job-progress b{min-width:34px;text-align:right;color:#5e547e}.job-queue-panel{margin:0 20px 16px 74px;padding:14px 16px;border:1px solid #eeebf6;border-radius:14px;background:#fbfaff}.queue-head{display:flex;align-items:center;justify-content:space-between;margin-bottom:8px;color:#3e3855}.queue-head .text-button{display:inline-flex;align-items:center;gap:5px}.queue-empty,.queue-error{display:flex;align-items:center;justify-content:space-between;padding:12px;color:#77748a;font-size:13px}.queue-list{max-height:260px;overflow:auto}.queue-row{display:flex;justify-content:space-between;gap:12px;padding:10px 2px;border-top:1px solid #eeeaf4}.queue-row>div{display:grid;gap:3px;min-width:0}.queue-row b,.queue-row small{overflow-wrap:anywhere}.queue-row b{font-size:13px;color:#3b354e}.queue-row small,.queue-row time{font-size:11px;color:#89869a}.queue-row .queue-error-text{color:#b45151}.queue-error{color:#a74a4a}.scan-history-row>div{gap:5px}.preview-transfer-estimates{display:grid;grid-template-columns:1fr 1fr;gap:9px 14px;padding:11px 12px;border:1px solid #eeebf6;border-radius:9px;background:#fff;color:#77748a;font-size:11px}.preview-transfer-estimates>div{display:flex;align-items:center;gap:7px}.preview-transfer-estimates>div:first-child svg{color:#348d68}.preview-transfer-estimates>div:nth-child(2) svg{color:#5878cf}.preview-transfer-estimates b{margin-left:auto;color:#454153;font-variant-numeric:tabular-nums}.preview-transfer-estimates small{grid-column:1/-1;color:#92909f;font-size:10px}.task-danger-note,.destructive-preview{display:flex;gap:10px;padding:12px 14px;border:1px solid #f1d9d5;border-radius:12px;background:#fff8f6;color:#744743;font-size:13px;line-height:1.55}.task-danger-note{align-items:flex-start}.task-danger-note svg{flex:none;color:#c16b63;margin-top:1px}.field-hint{display:block;margin-top:5px;color:#89869a;font-size:12px;line-height:1.45}.form-stack textarea{width:100%;resize:vertical;border:1px solid #e7e3ee;border-radius:10px;padding:10px 12px;color:#39344a;background:#fff;font:inherit;outline:none}.form-stack textarea:focus{border-color:#a595df;box-shadow:0 0 0 3px #9c8add22}.preview-content{max-height:min(76vh,760px);overflow-y:auto;padding-right:3px}.destructive-preview{display:block;margin-top:14px}.destructive-preview-heading{display:flex;gap:9px;align-items:flex-start}.destructive-preview-heading svg{flex:none;color:#bf655e;margin-top:2px}.destructive-preview-heading span{display:grid;gap:2px}.destructive-preview-heading small{color:#92716d}.delete-path-list{display:grid;gap:5px;max-height:220px;overflow:auto;margin:12px 0;padding:10px;border:1px solid #f2e1de;border-radius:9px;background:#fff}.delete-path-list code{font-size:12px;color:#714642;overflow-wrap:anywhere}.destructive-confirm{color:#633f3c}.preview-conflict-warning{margin:14px 0 8px}.conflict-actions-list{display:grid;gap:8px}.conflict-action-row{display:flex;justify-content:space-between;align-items:center;gap:12px;padding:12px;border:1px solid #f0e3d6;border-radius:11px;background:#fffaf4}.conflict-action-row>div:first-child{min-width:0;display:grid;gap:4px}.conflict-action-row b,.conflict-action-row small{overflow-wrap:anywhere}.conflict-action-row small{color:#858092;font-size:12px}.conflict-buttons{flex-wrap:wrap;justify-content:flex-end}.action-mark.delete-local,.action-mark.delete-remote,.action-label.delete-local,.action-label.delete-remote{color:#bd554f}@media(max-width:720px){.job-progress,.job-queue-panel{margin-left:16px;margin-right:16px}.conflict-action-row{align-items:flex-start;flex-direction:column}.conflict-buttons{justify-content:flex-start}.job-card-footer{align-items:flex-start;flex-direction:column}.inline-actions{flex-wrap:wrap}}
.job-scan-result{display:flex;align-items:center;gap:4px;margin-top:4px;color:#77748a;font-size:10px;line-height:1.4;overflow-wrap:anywhere}.job-scan-result svg{flex:0 0 auto;color:#8981c9}
</style>
