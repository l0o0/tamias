<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { AlertCircle, ArrowDownToLine, ArrowUpFromLine, ArrowLeftRight, Check, Clock3, Folder, Pause, Play, Plus, RefreshCw, ScanSearch, Trash2, Pencil, ListChecks, RotateCcw, ShieldAlert } from 'lucide-vue-next'
import { api } from '../api'
import BaseModal from '../components/BaseModal.vue'
import SquirrelMark from '../components/SquirrelMark.vue'
import TaskIcon from '../components/TaskIcon.vue'
import TaskIconPicker from '../components/TaskIconPicker.vue'
import RemoteFolderPicker from '../components/RemoteFolderPicker.vue'
import TaskFlow from '../components/TaskFlow.vue'
import { notify, refreshState, setSelectedConnection, store } from '../store'
import type { Direction, Job, JobPreview, QueueItem, SyncScanHistory } from '../types'
import { normalizeTaskIcon, taskIconLabel, type TaskIconName } from '../taskIcons'
const emit = defineEmits<{ demo: []; addConnection: []; files: []; operations: [] }>()
const showCreate = ref(false)
const submitting = ref(false)
const picking = ref(false)
const runningJob = ref('')
const previewBusy = ref('')
const showPreview = ref(false)
const showEdit = ref(false)
const remoteFolderPickerOpen = ref(false)
const selectedJob = ref<Job | null>(null)
const savingIconId = ref('')
const iconPickerOpen = ref(false)
const iconPickerMode = ref<'form' | 'quick'>('form')
const iconPickerJobId = ref('')
const iconPickerIcon = ref<TaskIconName>('')
const iconPickerError = ref('')
const editSnapshot = ref<string | null>(null)
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
const form = reactive({ name: '', icon: '' as TaskIconName, connectionId: '', localPath: '', remotePath: '', direction: 'both' as Direction, excludeText: '', scheduleMinutes: 0, watch: false, deleteThreshold: 20 })
const selectedConnection = computed(() => store.data.connections.find((c) => c.id === form.connectionId))
const iconPickerDirection = computed(() => iconPickerMode.value === 'quick' ? store.data.jobs.find(job => job.id === iconPickerJobId.value)?.direction || 'both' : form.direction)
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
function resetForm() { Object.assign(form, { name: '', icon: '', connectionId: store.selectedConnectionId, localPath: '', remotePath: '', direction: 'both', excludeText: '', scheduleMinutes: 0, watch: false, deleteThreshold: 20 }) }
function openCreate() { showCreate.value = true }
function openRemoteFolderPicker() { remoteFolderPickerOpen.value = true }
function openFormIconPicker() {
  iconPickerMode.value = 'form'
  iconPickerJobId.value = ''
  iconPickerIcon.value = normalizeTaskIcon(form.icon)
  iconPickerError.value = ''
  iconPickerOpen.value = true
}
function openJobIconPicker(job: Job) {
  iconPickerMode.value = 'quick'
  iconPickerJobId.value = job.id
  iconPickerIcon.value = normalizeTaskIcon(job.icon)
  iconPickerError.value = ''
  iconPickerOpen.value = true
}
async function selectTaskIcon(icon: TaskIconName) {
  iconPickerIcon.value = normalizeTaskIcon(icon)
  iconPickerError.value = ''
  if (iconPickerMode.value === 'form') {
    form.icon = iconPickerIcon.value
    iconPickerOpen.value = false
    return
  }
  if (savingIconId.value) return
  const job = store.data.jobs.find(item => item.id === iconPickerJobId.value)
  if (!job) {
    iconPickerError.value = '任务已不存在，请关闭此窗口。'
    return
  }
  if (normalizeTaskIcon(job.icon) === iconPickerIcon.value) {
    iconPickerOpen.value = false
    return
  }
  savingIconId.value = job.id
  try {
    await api.post('/api/jobs/icon', { id: job.id, icon: iconPickerIcon.value })
    await refreshState(true)
    iconPickerOpen.value = false
  } catch (error) {
    iconPickerError.value = error instanceof Error ? error.message : '保存任务图标失败'
  } finally { savingIconId.value = '' }
}
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
    name: form.name.trim(), icon: normalizeTaskIcon(form.icon), connectionId: form.connectionId, localPath: form.localPath.trim(), remotePath: form.remotePath.trim(), direction: form.direction,
    exclude: form.excludeText.split(/\r?\n/).map((item) => item.trim()).filter(Boolean), scheduleMinutes: Number(form.scheduleMinutes), watch: form.watch, deleteThreshold: Number(form.deleteThreshold),
  }
}
function jobPayloadWithoutIcon() {
  const { icon: _icon, ...fields } = jobPayload()
  return fields
}
function openEdit(job: Job) {
  editTarget.value = job
  Object.assign(form, { name: job.name, icon: normalizeTaskIcon(job.icon), connectionId: job.connectionId, localPath: job.localPath, remotePath: job.remotePath, direction: job.direction, excludeText: (job.exclude || []).join('\n'), scheduleMinutes: job.scheduleMinutes || 0, watch: !!job.watch, deleteThreshold: job.deleteThreshold || 20 })
  editSnapshot.value = JSON.stringify(jobPayloadWithoutIcon())
  showEdit.value = true
}
async function updateJob() {
  if (!editTarget.value || submitting.value) return
  submitting.value = true
  try {
    const payload = jobPayload()
    const onlyIconChanged = editSnapshot.value !== null && JSON.stringify(jobPayloadWithoutIcon()) === editSnapshot.value
    if (onlyIconChanged) {
      await api.post('/api/jobs/icon', { id: editTarget.value.id, icon: payload.icon })
    } else {
      await api.post<Job>('/api/jobs/update', { ...editTarget.value, ...payload })
    }
    showEdit.value = false
    editTarget.value = null
    editSnapshot.value = null
    await refreshState(true)
    if (!onlyIconChanged) notify('任务配置已更新', 'success')
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
  const needsUpload = preview.value.actions.some((action) => action.kind === 'upload')
  const needsConditionalDelete = preview.value.actions.some((action) => action.kind === 'delete-remote')
  const connection = store.data.connections.find((item) => item.id === selectedJob.value?.connectionId)
  if (connection?.detecting) {
    notify('正在自动检测连接，完成后即可同步。')
    return
  }
  const ready = !!connection?.tested && !connection.error
  const normalMode = !connection?.writeMode || connection.writeMode === 'standard' || connection.writeMode === 'compatible'
  const canUpload = ready && connection?.writeMode !== 'copy' && (normalMode || connection?.capabilities?.conditionalWrite)
  if (needsUpload && connection?.kind !== 'demo' && !canUpload) {
    notify(!ready ? '连接暂不可用，请在设置中检查地址、登录信息或网络。' : connection?.writeMode === 'copy' ? '仅上传为新副本模式不支持同步上传，可在文件页上传新副本。' : '此服务暂不支持严格防覆盖，可在连接设置中选择常规同步。')
    return
  }
  if (needsConditionalDelete && connection?.kind !== 'demo' && !(ready && connection?.writeMode !== 'copy' && connection?.capabilities?.conditionalWrite && connection?.capabilities?.conditionalDelete)) {
    notify('此连接暂不支持安全删除远端文件，请保留远端文件或调整同步方向。')
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
async function loadQueue(job: Job, refresh = false) {
  if (!refresh && queueFor.value === job.id) { queueFor.value = ''; queueItems.value = []; return }
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
function queueKindText(kind: string) { return ({ upload: '上传', download: '下载', conflict: '冲突', skip: '跳过', baseline: '更新基线', 'mkdir-local': '创建本地目录', 'mkdir-remote': '创建远端目录', 'delete-local': '删除本地文件', 'delete-remote': '删除远端文件', 'delete-local-dir': '删除本地目录' } as Record<string, string>)[kind] || '其他操作' }
function queueStateText(state: string) { return ({ pending: '待处理', running: '处理中', done: '已完成', error: '失败', cancelled: '已取消', canceled: '已取消' } as Record<string, string>)[state] || '未知状态' }
function connectionName(id: string) { return store.data.connections.find((c) => c.id === id)?.name || '已移除的连接' }
function displayTime(value: string) { if (!value) return '尚未运行'; const d = new Date(value); return Number.isNaN(d.valueOf()) ? value : new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(d) }
function formatBytes(bytes: number) { if (bytes < 1024) return `${bytes} B`; const units = ['KB', 'MB', 'GB', 'TB']; let value = bytes / 1024; let i = 0; while (value >= 1024 && i < units.length - 1) { value /= 1024; i++ } return `${value.toFixed(value >= 10 ? 0 : 1)} ${units[i]}` }
function shouldShowJobDetail(job: Job) {
  return !!job.detail && (['error', 'failed', 'attention', 'needs_attention', 'waiting'].includes(job.status) || (!job.enabled && job.status !== 'paused'))
}
function needsOperationReconciliation(job: Job) {
  return job.status === 'needs_attention' && job.detail.includes('待核对')
}
function closeTaskMenu(event: Event) {
  const menu = (event.currentTarget as HTMLElement).closest('details') as HTMLDetailsElement | null
  if (menu) {
    menu.open = false
    menu.querySelector('summary')?.focus()
  }
}
function closeTaskMenuOutside(event: MouseEvent) {
  const menu = document.querySelector<HTMLDetailsElement>('.tasks-page .job-more-menu[open]')
  if (menu && event.target instanceof Node && !menu.contains(event.target)) menu.open = false
}
function keepOneTaskMenuOpen(event: Event) {
  const menu = event.currentTarget as HTMLDetailsElement
  if (!menu.open) return
  document.querySelectorAll<HTMLDetailsElement>('.tasks-page .job-more-menu[open]').forEach((other) => {
    if (other !== menu) other.open = false
  })
}
function closeTaskMenuOnEscape(event: KeyboardEvent) {
  const menu = event.currentTarget as HTMLDetailsElement
  menu.open = false
  menu.querySelector('summary')?.focus()
}
onMounted(() => document.addEventListener('click', closeTaskMenuOutside))
onBeforeUnmount(() => document.removeEventListener('click', closeTaskMenuOutside))
</script>
<template>
  <div class="page-view tasks-page">
    <header class="page-heading"><div><h1>同步任务</h1></div><button class="button primary" :disabled="!store.data.connections.length" @click="openCreate"><Plus :size="16" />新建任务</button></header>
    <div v-if="store.loading" class="task-loading-state" role="status"><SquirrelMark variant="eat" :size="64" /><span>正在读取同步任务…</span></div>
    <div v-else-if="store.data.jobs.length" class="job-list">
      <article v-for="job in store.data.jobs" :key="job.id" class="job-card">
          <div class="job-card-main">
          <button type="button" class="job-icon job-icon-button" :class="{ 'job-icon-working': (job.status === 'running' || job.status === 'scanning') && !normalizeTaskIcon(job.icon) }" :disabled="!!savingIconId" :aria-label="`选择任务“${job.name}”的图标，当前${taskIconLabel(job.icon)}`" aria-haspopup="dialog" :aria-expanded="iconPickerOpen && iconPickerMode === 'quick' && iconPickerJobId === job.id" title="更改任务图标" @click="openJobIconPicker(job)"><TaskIcon :icon="job.icon" :direction="job.direction" :running="job.status === 'running' || job.status === 'scanning'" :size="!normalizeTaskIcon(job.icon) && ['running','scanning'].includes(job.status) ? 36 : normalizeTaskIcon(job.icon) === 'squirrel' ? 26 : 18" /></button>
          <div class="job-info"><div class="job-title-line"><h2>{{ job.name }}</h2><span class="status-pill" :class="statusTone[job.status] || 'gray'"><i></i>{{ statusText[job.status] || job.status }}</span><span class="direction-pill">{{ directionLabel(job.direction) }}</span></div><div class="job-connection"><span>{{ connectionName(job.connectionId) }}</span><span class="dot-separator">·</span><span>{{ displayTime(job.lastRun) }}</span></div></div>
        </div>
        <div class="job-paths">
          <div class="job-path-end"><span class="path-label">本地文件夹</span><span class="path-value" :title="job.localPath"><Folder :size="14" /><span class="path-text">{{ job.localPath || '尚未选择' }}</span></span></div>
          <TaskFlow class="job-flow" :job="job" />
          <div class="job-path-end"><span class="path-label">远端目录</span><span class="path-value" :title="job.remotePath"><span class="path-text">{{ job.remotePath || '/' }}</span></span></div>
        </div>
        <div class="job-card-footer">
          <span v-if="shouldShowJobDetail(job)" class="job-detail"><AlertCircle :size="14" />{{ job.detail }}</span>
          <div class="inline-actions">
            <button v-if="['running','scanning','retrying'].includes(job.status)" class="button primary small" :disabled="runningJob!==''" @click="pauseJob(job)"><Pause :size="14" />{{runningJob===job.id?'正在暂停…':'暂停'}}</button>
            <button v-else-if="job.status === 'paused' || !job.enabled" class="button primary small" :disabled="runningJob!==''" @click="resumeJob(job)"><Play :size="14" />{{runningJob===job.id?'正在恢复…':'恢复'}}</button>
            <button v-else-if="needsOperationReconciliation(job)" class="button primary small" @click="emit('operations')"><ShieldAlert :size="14" />核对操作</button>
            <button v-else class="button primary small" :disabled="!!previewBusy || !!runningJob" @click="previewJob(job)"><RefreshCw v-if="previewBusy === job.id" :size="14" class="spin" /><ScanSearch v-else :size="14" />{{ previewBusy === job.id ? '正在预览…' : '预览同步' }}</button>
            <details class="job-more-menu" @toggle="keepOneTaskMenuOpen" @keydown.esc.prevent="closeTaskMenuOnEscape">
              <summary>更多</summary>
              <div class="job-more-actions">
                <button v-if="needsOperationReconciliation(job)" type="button" :disabled="!!previewBusy || !!runningJob" @click="closeTaskMenu($event); previewJob(job)"><ScanSearch :size="14" />预览同步</button>
                <button v-if="['error','failed','attention','needs_attention'].includes(job.status) && !needsOperationReconciliation(job)" type="button" :disabled="runningJob!==''" @click="closeTaskMenu($event); retryJob(job)"><RotateCcw :size="14" />重试失败项</button>
                <button type="button" :disabled="['running','scanning','retrying'].includes(job.status)" @click="closeTaskMenu($event); openEdit(job)"><Pencil :size="14" />编辑任务</button>
                <button type="button" @click="closeTaskMenu($event); loadQueue(job)"><ListChecks :size="14" />文件操作队列</button>
                <button type="button" @click="closeTaskMenu($event); loadScanHistory(job)"><Clock3 :size="14" />近期核对记录</button>
                <button type="button" class="danger-action" :disabled="runningJob!==''" @click="closeTaskMenu($event); deleteTarget = job"><Trash2 :size="14" />删除任务</button>
              </div>
            </details>
          </div>
        </div>
        <section v-if="queueFor === job.id" class="job-queue-panel"><div class="queue-head"><div><b>文件操作队列</b><small>显示操作状态；重试失败项前会重新核对。</small></div><button class="text-button" :disabled="queueLoading" @click="loadQueue(job, true)"><RefreshCw :size="13" :class="{ spin: queueLoading }" />刷新</button></div><div v-if="queueLoading" class="queue-empty queue-loading-state" role="status"><SquirrelMark variant="eat" :size="28" /><span>正在读取队列…</span></div><div v-else-if="queueError" class="queue-error"><span>{{ queueError }}</span><button class="button secondary small" @click="loadQueue(job, true)">重试</button></div><div v-else-if="queueItems.length" class="queue-list"><div v-for="item in queueItems" :key="`${item.path}-${item.kind}`" class="queue-row"><div><b :title="item.path">{{ item.path }}</b><small>{{ queueKindText(item.kind) }} · {{ queueStateText(item.state) }}<template v-if="item.bytesDone"> · {{ formatBytes(item.bytesDone) }}</template></small><small v-if="item.error" class="queue-error-text">{{ item.error }}</small></div><time>{{ displayTime(item.updated) }}</time></div></div><div v-else class="queue-empty">尚无文件操作记录。失败或中断的操作会保留在此。</div></section>
        <section v-if="scanHistoryFor === job.id" class="job-queue-panel scan-history-panel"><div class="queue-head"><div><b>近期核对记录</b><small>每次完整扫描的范围、结果和预计文件传输量。</small></div><button class="text-button" :disabled="scanHistoryLoading" @click="loadScanHistory(job, true)"><RefreshCw :size="13" :class="{ spin: scanHistoryLoading }" />刷新</button></div><div v-if="scanHistoryLoading" class="queue-empty queue-loading-state" role="status"><SquirrelMark variant="eat" :size="28" /><span>正在读取核对记录…</span></div><div v-else-if="scanHistoryError" class="queue-error"><span>{{ scanHistoryError }}</span><button class="button secondary small" @click="loadScanHistory(job, true)">重试</button></div><div v-else-if="scanHistoryItems.length" class="queue-list"><div v-for="item in scanHistoryItems" :key="item.id" class="queue-row scan-history-row"><div><b>{{ displayTime(item.started) }} · {{ scanStatusText(item.status) }}</b><small>本地 {{ item.localFiles }} 个文件 / {{ item.localDirectories }} 个目录；远端 {{ item.remoteFiles }} 个文件 / {{ item.remoteDirectories }} 个目录</small><small>上传 {{ item.uploads }} 项 · {{ formatBytes(item.uploadBytes) }}；下载 {{ item.downloads }} 项 · {{ formatBytes(item.downloadBytes) }}；删除 {{ item.deletes }} 项 · 冲突 {{ item.conflicts }} 项</small><small>范围：{{ item.scope.localPath }} ↔ {{ item.scope.remotePath || '/' }} · {{ directionLabel(item.scope.direction) }}</small><small v-if="item.error" class="queue-error-text">{{ item.error }}</small></div><time>{{ item.completed ? displayTime(item.completed) : '进行中' }}</time></div></div><div v-else class="queue-empty">还没有核对记录。</div></section>
      </article>
    </div>
    <div v-else class="empty-card task-empty">
      <div class="empty-illustration"><div class="empty-orbit orbit-one"></div><div class="empty-orbit orbit-two"></div><div class="empty-folder"><Folder :size="30" /></div><span class="empty-spark spark-a">✦</span><span class="empty-spark spark-b">✦</span></div>
      <h2>{{ store.data.connections.length ? '创建第一个同步任务' : '连接存储，开始同步' }}</h2><p>{{ store.data.connections.length ? '选择本机文件夹和远端目录。每次同步前都可以先查看预览。' : '添加远端连接后，选择本机文件夹并查看同步预览。' }}</p>
      <div class="empty-actions"><button v-if="!store.data.connections.length" class="button primary" @click="emit('addConnection')"><Plus :size="16" />添加存储连接</button><button v-else class="button primary" @click="openCreate"><Plus :size="16" />新建同步任务</button><button v-if="!store.data.connections.length" class="button secondary" @click="emit('demo')">试用演示空间</button></div>
    </div>

    <BaseModal v-model="showCreate" :dismissible="!submitting && !picking" title="新建同步任务">
      <form class="form-stack" @submit.prevent="createJob">
        <div class="task-name-field"><label class="field"><span>任务名称</span><input v-model="form.name" required maxlength="80" placeholder="例如：工作资料" /></label><button type="button" class="task-form-icon-trigger" :disabled="submitting || picking || !!savingIconId" :aria-label="`选择任务图标，当前${taskIconLabel(form.icon)}`" aria-haspopup="dialog" :aria-expanded="iconPickerOpen && iconPickerMode === 'form'" @click="openFormIconPicker"><TaskIcon :icon="form.icon" :direction="form.direction" :size="20" /><span>{{taskIconLabel(form.icon)}}</span></button></div>
        <label class="field"><span>远端连接</span><select v-model="form.connectionId" required><option value="" disabled>选择一个连接</option><option v-for="c in store.data.connections" :key="c.id" :value="c.id">{{ c.name }}</option></select></label>
        <div class="field"><span>本地文件夹</span><div class="input-button-row"><input v-model="form.localPath" aria-label="本地文件夹" required placeholder="选择本机文件夹" /><button class="button secondary" type="button" :disabled="picking" @click="pickFolder">{{ picking ? '选择中…' : '浏览' }}</button></div></div>
        <div class="field"><span>远端文件夹</span><div class="input-button-row"><input v-model="form.remotePath" aria-label="远端文件夹" placeholder="留空表示连接根目录" /><button class="button secondary" type="button" :disabled="submitting || picking || !form.connectionId" @click="openRemoteFolderPicker">浏览</button></div></div>
        <label class="field"><span>同步方式</span><select v-model="form.direction"><option value="both">双向同步</option><option value="upload">只上传</option><option value="download">只下载</option><option v-if="form.direction === 'mirror-upload'" value="mirror-upload">上传镜像（高级设置）</option><option v-if="form.direction === 'mirror-download'" value="mirror-download">下载镜像（高级设置）</option></select></label>
        <details class="task-options"><summary>高级设置</summary><div class="form-stack">
          <div class="field"><span>镜像同步</span><small class="field-hint">镜像会删除多余文件；执行前会列出具体路径并要求确认。</small><div class="mirror-options"><button type="button" :class="{ selected: form.direction === 'mirror-upload' }" :aria-pressed="form.direction === 'mirror-upload'" @click="form.direction = 'mirror-upload'"><b>上传镜像</b><small>会删除远端多余文件</small></button><button type="button" :class="{ selected: form.direction === 'mirror-download' }" :aria-pressed="form.direction === 'mirror-download'" @click="form.direction = 'mirror-download'"><b>下载镜像</b><small>会删除本地多余文件</small></button></div></div>
          <div v-if="form.direction.startsWith('mirror')" class="task-danger-note"><AlertCircle :size="16" /><span>待删除路径会列在执行预览中；数量达到确认阈值时还需额外确认。</span></div>
          <label class="field"><span>批量删除确认数量</span><input v-model.number="form.deleteThreshold" type="number" min="1" max="10000" required /><small class="field-hint">达到这个数量时，需要在预览中额外勾选确认。</small></label>
          <label class="field"><span>排除规则 <small>每行一条</small></span><textarea v-model="form.excludeText" rows="3" placeholder="例如：cache/**&#10;*.tmp"></textarea><small class="field-hint">符合规则的文件不会传输或触发删除。</small></label>
          <label class="field"><span>远端核对间隔</span><select v-model.number="form.scheduleMinutes"><option :value="0">不开启定时核对</option><option :value="5">每 5 分钟</option><option :value="15">每 15 分钟</option><option :value="30">每 30 分钟</option><option :value="60">每小时</option><option :value="360">每 6 小时</option></select></label>
          <label class="watch-toggle check-row"><input v-model="form.watch" type="checkbox" /><span><b>自动同步本地变化</b><small v-if="form.watch">本地变化会自动同步；{{ form.scheduleMinutes ? `远端每 ${form.scheduleMinutes >= 60 ? `${form.scheduleMinutes / 60} 小时` : `${form.scheduleMinutes} 分钟`}核对` : '远端约每分钟核对' }}。冲突、达到删除确认数量或有待核对操作时会暂停。</small><small v-else-if="form.scheduleMinutes">远端每 {{ form.scheduleMinutes >= 60 ? `${form.scheduleMinutes / 60} 小时` : `${form.scheduleMinutes} 分钟` }}核对并自动同步；冲突、达到删除确认数量或有待核对操作时会暂停。</small><small v-else>关闭自动运行；从任务卡片预览并启动同步。</small></span></label>
        </div></details>
        <footer class="modal-actions"><button class="button secondary" type="button" :disabled="submitting || picking" @click="showCreate = false">取消</button><button class="button primary" type="submit" :disabled="submitting">{{ submitting ? '正在创建…' : '创建任务' }}</button></footer>
      </form>
    </BaseModal>
    <BaseModal v-model="showEdit" :dismissible="!submitting && !picking" title="编辑同步任务" subtitle="修改同步目录会重新建立同步记录。" width="620px">
      <form class="form-stack" @submit.prevent="updateJob">
        <div class="task-name-field"><label class="field"><span>任务名称</span><input v-model="form.name" required maxlength="80" /></label><button type="button" class="task-form-icon-trigger" :disabled="submitting || picking || !!savingIconId" :aria-label="`选择任务图标，当前${taskIconLabel(form.icon)}`" aria-haspopup="dialog" :aria-expanded="iconPickerOpen && iconPickerMode === 'form'" @click="openFormIconPicker"><TaskIcon :icon="form.icon" :direction="form.direction" :size="20" /><span>{{taskIconLabel(form.icon)}}</span></button></div>
        <label class="field"><span>远端连接</span><select v-model="form.connectionId" required><option v-for="c in store.data.connections" :key="c.id" :value="c.id">{{ c.name }}</option></select></label>
        <div class="field"><span>本地文件夹</span><div class="input-button-row"><input v-model="form.localPath" aria-label="本地文件夹" required /><button class="button secondary" type="button" :disabled="picking" @click="pickFolder">{{ picking ? '选择中…' : '浏览' }}</button></div></div>
        <div class="field"><span>远端文件夹</span><div class="input-button-row"><input v-model="form.remotePath" aria-label="远端文件夹" placeholder="留空表示连接根目录" /><button class="button secondary" type="button" :disabled="submitting || picking || !form.connectionId" @click="openRemoteFolderPicker">浏览</button></div></div>
        <label class="field"><span>同步方式</span><select v-model="form.direction"><option value="both">双向同步</option><option value="upload">只上传</option><option value="download">只下载</option><option v-if="form.direction === 'mirror-upload'" value="mirror-upload">上传镜像（高级设置）</option><option v-if="form.direction === 'mirror-download'" value="mirror-download">下载镜像（高级设置）</option></select></label>
        <details class="task-options" :open="!!(editTarget?.exclude?.length || editTarget?.watch || editTarget?.scheduleMinutes || editTarget?.direction.startsWith('mirror'))"><summary>高级设置</summary><div class="form-stack">
          <div class="field"><span>镜像同步</span><small class="field-hint">镜像会删除多余文件；执行前会列出具体路径并要求确认。</small><div class="mirror-options"><button type="button" :class="{ selected: form.direction === 'mirror-upload' }" :aria-pressed="form.direction === 'mirror-upload'" @click="form.direction = 'mirror-upload'"><b>上传镜像</b><small>会删除远端多余文件</small></button><button type="button" :class="{ selected: form.direction === 'mirror-download' }" :aria-pressed="form.direction === 'mirror-download'" @click="form.direction = 'mirror-download'"><b>下载镜像</b><small>会删除本地多余文件</small></button></div></div>
          <div v-if="form.direction.startsWith('mirror')" class="task-danger-note"><AlertCircle :size="16" /><span>待删除路径会列在执行预览中；数量达到确认阈值时还需额外确认。</span></div>
          <label class="field"><span>批量删除确认数量</span><input v-model.number="form.deleteThreshold" type="number" min="1" max="10000" required /><small class="field-hint">达到这个数量时，需要在预览中额外勾选确认。</small></label>
          <label class="field"><span>排除规则 <small>每行一条</small></span><textarea v-model="form.excludeText" rows="3" placeholder="例如：cache/**&#10;*.tmp"></textarea><small class="field-hint">符合规则的文件不会传输或触发删除。</small></label>
          <label class="field"><span>远端核对间隔</span><select v-model.number="form.scheduleMinutes"><option :value="0">不开启定时核对</option><option :value="5">每 5 分钟</option><option :value="15">每 15 分钟</option><option :value="30">每 30 分钟</option><option :value="60">每小时</option><option :value="360">每 6 小时</option></select></label>
          <label class="watch-toggle check-row"><input v-model="form.watch" type="checkbox" /><span><b>自动同步本地变化</b><small v-if="form.watch">本地变化会自动同步；{{ form.scheduleMinutes ? `远端每 ${form.scheduleMinutes >= 60 ? `${form.scheduleMinutes / 60} 小时` : `${form.scheduleMinutes} 分钟`}核对` : '远端约每分钟核对' }}。冲突、达到删除确认数量或有待核对操作时会暂停。</small><small v-else-if="form.scheduleMinutes">远端每 {{ form.scheduleMinutes >= 60 ? `${form.scheduleMinutes / 60} 小时` : `${form.scheduleMinutes} 分钟` }}核对并自动同步；冲突、达到删除确认数量或有待核对操作时会暂停。</small><small v-else>关闭自动运行；从任务卡片预览并启动同步。</small></span></label>
        </div></details>
        <footer class="modal-actions"><button class="button secondary" type="button" :disabled="submitting || picking" @click="showEdit = false">取消</button><button class="button primary" type="submit" :disabled="submitting">{{ submitting ? '正在保存…' : '保存任务' }}</button></footer>
      </form>
    </BaseModal>
    <BaseModal v-model="showPreview" title="同步预览" :subtitle="selectedJob ? `${selectedJob.name} · 检查后再执行` : ''" width="620px">
      <div v-if="previewBusy && !preview" class="preview-loading-state" role="status"><SquirrelMark variant="eat" :size="64" /><span>正在生成同步预览…</span></div>
      <div v-else-if="preview" class="preview-content">
        <div v-if="runningJob === selectedJob?.id" class="sync-executing-state" role="status"><SquirrelMark variant="eat" :size="44" /><div><b>正在同步文件…</b><span>可以关闭此窗口，在任务卡片中查看进度。</span></div></div>
        <div class="preview-summary"><div><strong>{{ previewCounts.transfers }}</strong><span>项待传输</span></div><div class="preview-summary-details"><span v-if="previewCounts.conflicts" class="conflict-count">{{ previewCounts.conflicts }} 项冲突</span><span v-if="previewCounts.skipped" class="skip-count">{{ previewCounts.skipped }} 项跳过</span><span v-if="previewCounts.deletes" class="conflict-count">{{ previewCounts.deletes }} 项删除</span><span v-if="!previewCounts.deletes">本次不删除文件</span></div></div>
        <div class="preview-transfer-estimates"><div><ArrowUpFromLine :size="14" /><span>预计上传</span><b>{{ formatBytes(preview.uploadBytes || 0) }}</b></div><div><ArrowDownToLine :size="14" /><span>预计下载</span><b>{{ formatBytes(preview.downloadBytes || 0) }}</b></div></div>
        <div v-if="preview.actions.length" class="preview-list"><div v-for="(action, i) in preview.actions" :key="`${action.path}-${i}`" class="preview-row"><span class="action-mark" :class="action.kind">{{ action.kind === 'upload' ? '↑' : action.kind === 'download' ? '↓' : action.kind === 'conflict' ? '!' : action.kind.startsWith('delete') ? '−' : '·' }}</span><span class="preview-path"><b>{{ action.path }}</b><small>{{ action.reason }}<template v-if="action.kind === 'upload' || action.kind === 'download'"> · {{ formatBytes(action.size || 0) }}</template></small></span><span class="action-label" :class="action.kind">{{ action.kind === 'upload' ? '上传' : action.kind === 'download' ? '下载' : action.kind === 'conflict' ? '冲突' : action.kind.startsWith('delete') ? '删除' : '跳过' }}</span></div></div>
        <div v-if="preview.deletePaths?.length" class="destructive-preview"><div class="destructive-preview-heading"><AlertCircle :size="17" /><span><b>待删除 {{ preview.deleteCount || preview.deletePaths.length }} 项</b><small>以下是这次预览中的具体路径；重新核对后清单会变化。</small></span></div><div class="delete-path-list"><code v-for="item in preview.deletePaths" :key="item">{{ item }}</code></div><label class="check-row destructive-confirm"><input v-model="deleteConfirmed" type="checkbox" />我已检查上述路径，确认这些删除操作<span v-if="preview.requiresDeleteConfirmation">（超过任务阈值）</span></label></div>
        <div v-else-if="!preview.actions.length" class="preview-clear"><Check :size="18" /><span>当前没有需要同步的改动。</span></div>
        <div v-if="preview.actions.some((a) => a.kind === 'conflict')" class="preview-conflict-warning"><AlertCircle :size="17" /><div><b>发现冲突，请逐项选择</b><span>选择会覆盖的方向前会再次展示具体路径并检查版本；是否创建恢复副本取决于连接设置。</span></div></div>
        <div v-if="preview.actions.some((a) => a.kind === 'conflict')" class="conflict-actions-list"><div v-for="action in preview.actions.filter((a) => a.kind === 'conflict')" :key="action.path" class="conflict-action-row"><div><b>{{ action.path }}</b><small>{{ action.reason }}</small></div><div class="conflict-buttons"><button class="button subtle small" @click="requestResolution(action.path, 'local')">保留本地</button><button class="button subtle small" @click="requestResolution(action.path, 'remote')">保留远端</button><button class="button subtle small" @click="requestResolution(action.path, 'keep-both')">保留两份</button></div></div></div>

        <p v-if="selectedJobPaused" class="safety-note">任务已暂停。请关闭预览并先恢复任务，再重新预览执行。</p><footer class="modal-actions"><button class="button secondary" @click="showPreview = false">关闭</button><button class="button primary" :disabled="selectedJobPaused || runningJob !== '' || preview.actions.some((a) => a.kind === 'conflict') || (!preview.actions.length && !preview.deletePaths?.length) || (!!preview.deletePaths?.length && !deleteConfirmed)" @click="runJob"><Play :size="15" />{{ runningJob ? '同步中…' : '执行同步' }}</button></footer>
      </div>
    </BaseModal>
    <BaseModal :model-value="!!resolution" title="确认冲突处理" :subtitle="resolution?.action || ''" @update:model-value="(v) => { if (!v) resolution = null }">
      <div v-if="resolution" class="confirm-panel"><div class="confirm-warning"><AlertCircle :size="18" /></div><p><b>{{ resolution.action }}</b></p><p>{{ resolution.choice === 'local' ? '将以本地版本替换远端版本。' : resolution.choice === 'remote' ? '将以远端版本替换本地文件。' : '将保留当前本地文件，并把远端版本另存为副本。' }}远端版本会在操作前重新检查；是否创建恢复副本取决于连接设置。</p><footer class="modal-actions"><button class="button secondary" :disabled="resolving" @click="resolution = null">返回</button><button class="button primary" :disabled="resolving" @click="confirmResolution">{{ resolving ? '正在处理…' : '确认此项选择' }}</button></footer></div>
    </BaseModal>
    <BaseModal :model-value="!!deleteTarget" title="删除同步任务" subtitle="仅删除任务配置，文件会保留。" @update:model-value="(v) => { if (!v) deleteTarget = null }">
      <div class="confirm-panel"><div class="confirm-warning"><Trash2 :size="18" /></div><p>确定删除“{{ deleteTarget?.name }}”吗？</p><footer class="modal-actions"><button class="button secondary" :disabled="runningJob!==''" @click="deleteTarget = null">取消</button><button class="button danger" :disabled="runningJob!==''" @click="removeJob">{{runningJob===deleteTarget?.id?'正在删除…':'删除任务'}}</button></footer></div>
    </BaseModal>
    <RemoteFolderPicker v-model="remoteFolderPickerOpen" :connection-id="form.connectionId" :initial-path="form.remotePath" @select="form.remotePath = $event" />
    <TaskIconPicker v-model="iconPickerOpen" :icon="iconPickerIcon" :direction="iconPickerDirection" :busy="!!savingIconId" :error="iconPickerError" @select="selectTaskIcon" />
  </div>
</template>

<style scoped>
.task-options { border-top: 1px solid var(--line); padding-top: 12px; }
.task-options summary { width: fit-content; cursor: pointer; color: var(--muted); font-size: 12px; }
.task-options[open] summary { margin-bottom: 14px; color: var(--ink); }

.job-quick-actions,.conflict-buttons{display:flex;align-items:center;gap:6px}.job-queue-panel{margin:12px 0 0;padding:14px 16px;border:1px solid #eeebf6;border-radius:14px;background:#fbfaff}.queue-head{display:flex;align-items:flex-start;justify-content:space-between;gap:12px;margin-bottom:8px;color:#3e3855}.queue-head>div{display:grid;gap:3px;min-width:0}.queue-head small{display:block;white-space:normal;line-height:1.45;color:#89869a;font-size:11px}.queue-head .text-button{display:inline-flex;align-items:center;gap:5px;flex:0 0 auto}.queue-empty,.queue-error{display:flex;align-items:center;justify-content:space-between;padding:12px;color:#77748a;font-size:13px}.queue-list{max-height:260px;overflow:auto}.queue-row{display:flex;justify-content:space-between;gap:12px;padding:10px 2px;border-top:1px solid #eeeaf4}.queue-row>div{display:grid;gap:3px;min-width:0}.queue-row b,.queue-row small{overflow-wrap:anywhere}.queue-row b{font-size:13px;color:#3b354e}.queue-row small,.queue-row time{font-size:11px;color:#89869a}.queue-row .queue-error-text{color:#b45151}.queue-error{color:#a74a4a}.scan-history-row>div{gap:5px}.preview-transfer-estimates{display:grid;grid-template-columns:1fr 1fr;gap:9px 14px;padding:11px 12px;border:1px solid #eeebf6;border-radius:9px;background:#fff;color:#77748a;font-size:11px}.preview-transfer-estimates>div{display:flex;align-items:center;gap:7px}.preview-transfer-estimates>div:first-child svg{color:#348d68}.preview-transfer-estimates>div:nth-child(2) svg{color:#5878cf}.preview-transfer-estimates b{margin-left:auto;color:#454153;font-variant-numeric:tabular-nums}.preview-transfer-estimates small{grid-column:1/-1;color:#92909f;font-size:10px}.task-danger-note,.destructive-preview{display:flex;gap:10px;padding:12px 14px;border:1px solid #f1d9d5;border-radius:12px;background:#fff8f6;color:#744743;font-size:13px;line-height:1.55}.task-danger-note{align-items:flex-start}.task-danger-note svg{flex:none;color:#c16b63;margin-top:1px}.field-hint{display:block;margin-top:5px;color:#89869a;font-size:12px;line-height:1.45}.watch-toggle{display:flex;align-items:flex-start;gap:9px;min-width:0;padding:4px 0;color:#62616f;cursor:pointer}.watch-toggle>input{flex:0 0 auto;margin:2px 0 0}.watch-toggle>span{display:grid;gap:3px;min-width:0}.watch-toggle b{font-size:12px;font-weight:600}.watch-toggle small{margin:0;color:#89869a;font-size:11px;line-height:1.4}.preview-content{max-height:min(76vh,760px);overflow-y:auto;padding-right:3px}.destructive-preview{display:block;margin-top:14px}.destructive-preview-heading{display:flex;gap:9px;align-items:flex-start}.destructive-preview-heading svg{flex:none;color:#bf655e;margin-top:2px}.destructive-preview-heading span{display:grid;gap:2px}.destructive-preview-heading small{color:#92716d}.delete-path-list{display:grid;gap:5px;max-height:220px;overflow:auto;margin:12px 0;padding:10px;border:1px solid #f2e1de;border-radius:9px;background:#fff}.delete-path-list code{font-size:12px;color:#714642;overflow-wrap:anywhere}.destructive-confirm{color:#633f3c}.preview-conflict-warning{margin:14px 0 8px}.conflict-actions-list{display:grid;gap:8px}.conflict-action-row{display:flex;justify-content:space-between;align-items:center;gap:12px;padding:12px;border:1px solid #f0e3d6;border-radius:11px;background:#fffaf4}.conflict-action-row>div:first-child{min-width:0;display:grid;gap:4px}.conflict-action-row b,.conflict-action-row small{overflow-wrap:anywhere}.conflict-action-row small{color:#858092;font-size:12px}.conflict-buttons{flex-wrap:wrap;justify-content:flex-end}.action-mark.delete-local,.action-mark.delete-remote,.action-label.delete-local,.action-label.delete-remote{color:#bd554f}@media(max-width:720px){.conflict-action-row{align-items:flex-start;flex-direction:column}.conflict-buttons{justify-content:flex-start}.job-card-footer{align-items:flex-start;flex-direction:column}.job-card-footer .job-detail{flex:0 1 auto}.inline-actions{flex-wrap:wrap}}
.job-scan-result{display:flex;align-items:center;gap:4px;margin-top:4px;color:#77748a;font-size:10px;line-height:1.4;overflow-wrap:anywhere}.job-scan-result svg{flex:0 0 auto;color:#8981c9}
.tasks-page .job-paths{grid-template-columns:minmax(0,1fr) minmax(94px,1.2fr) minmax(0,1fr);gap:10px}
.tasks-page .job-path-end{min-width:0}
.tasks-page .path-value{display:flex;align-items:center;gap:5px;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;overflow-wrap:normal}
.tasks-page .path-text{min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.tasks-page .job-flow{min-width:0;align-self:center}
.tasks-page .job-card-footer{align-items:center}
.tasks-page .job-card-footer .inline-actions{margin-left:auto}
.tasks-page .job-detail{display:flex;align-items:flex-start;gap:7px;flex:1 1 220px;min-width:0;white-space:normal;overflow:visible;text-overflow:clip;overflow-wrap:anywhere;line-height:1.45}
.tasks-page .job-detail svg{flex:0 0 auto;color:#bd625f;margin-top:1px}
.tasks-page .job-icon-button{display:grid;place-items:center;flex:0 0 42px;width:42px;height:42px;padding:0;border:1px solid transparent;border-radius:12px;background:transparent;color:var(--accent);cursor:pointer;transition:background .15s,border-color .15s,transform .15s}
.tasks-page .job-icon-button:hover:not(:disabled){border-color:#e9e6f5;background:#f8f7fc;transform:translateY(-1px)}
.tasks-page .job-icon-button:focus-visible,.task-form-icon-trigger:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.tasks-page .job-icon-button:disabled{opacity:.55;cursor:wait}
.task-name-field{display:grid;grid-template-columns:minmax(0,1fr) 86px;align-items:end;gap:9px}
.task-name-field>.field{min-width:0}
.task-form-icon-trigger{display:grid;grid-template-columns:24px minmax(0,1fr);align-items:center;justify-items:center;gap:5px;min-width:0;height:39px;padding:0 7px;border:1px solid var(--line);border-radius:8px;background:#fff;color:var(--muted);cursor:pointer}
.task-form-icon-trigger:hover:not(:disabled){border-color:#c9c2f5;background:#faf9ff;color:var(--accent)}
.task-form-icon-trigger>span{max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:11px}
.task-form-icon-trigger:disabled{opacity:.55;cursor:wait}
@media(max-width:480px){.task-name-field{grid-template-columns:minmax(0,1fr) 78px;gap:7px}.task-form-icon-trigger{grid-template-columns:20px minmax(0,1fr);padding:0 5px}}
@media(max-width:720px){.tasks-page .job-card-footer{align-items:flex-start;flex-direction:column}.tasks-page .job-card-footer .job-detail{flex:0 1 auto}}
.mirror-options{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px;margin-top:9px}
.mirror-options button{display:grid;gap:4px;min-width:0;padding:10px 11px;border:1px solid var(--line);border-radius:9px;background:#fff;text-align:left;color:var(--ink);cursor:pointer}
.mirror-options button.selected{border-color:var(--accent);background:var(--accent-soft)}
.mirror-options button b{font-size:12px;font-weight:600}
.mirror-options button small{color:var(--muted);font-size:11px;line-height:1.4}
.mirror-options button:focus-visible,.job-more-menu summary:focus-visible,.job-more-actions button:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.tasks-page .inline-actions{flex:1 1 auto;flex-wrap:wrap;justify-content:flex-end;min-width:0;max-width:100%}
.job-more-menu{position:static;flex:0 0 auto}
.job-more-menu>summary{display:flex;align-items:center;justify-content:center;min-height:34px;padding:0 10px;border:1px solid var(--line);border-radius:8px;background:#fff;color:var(--muted);font-size:12px;cursor:pointer;list-style:none}
.job-more-menu>summary::-webkit-details-marker{display:none}
.job-more-menu[open]>summary{border-color:var(--accent);color:var(--accent)}
.job-more-menu[open]{flex:1 0 100%}
.job-more-menu[open]>summary{margin-left:auto}
.job-more-actions{position:static;display:grid;width:min(240px,100%);margin:6px 0 0 auto;padding:5px;border:1px solid var(--line);border-radius:10px;background:#fff;box-shadow:0 4px 14px rgba(39,36,68,.08)}
.job-more-actions button{display:flex;align-items:center;gap:8px;width:100%;min-height:34px;padding:0 9px;border:0;border-radius:6px;background:transparent;color:var(--ink);font-size:12px;text-align:left;cursor:pointer}
.job-more-actions button:hover:not(:disabled){background:var(--accent-soft)}
.job-more-actions button:disabled{opacity:.5;cursor:not-allowed}
.job-more-actions .danger-action{color:#b64f55}
@media(max-width:520px){.mirror-options{grid-template-columns:1fr}.job-more-menu[open]>summary{margin-left:0}.job-more-actions{margin-left:0}}
</style>
