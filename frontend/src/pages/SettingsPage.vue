<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { Activity, AlertTriangle, Check, ChevronRight, Clock3, Database, HardDrive, KeyRound, Plus, RefreshCw, Server, Settings2, ShieldCheck, Trash2, Upload, X } from 'lucide-vue-next'
import { api } from '../api'
import BaseModal from '../components/BaseModal.vue'
import EditConnectionModal from '../components/EditConnectionModal.vue'
import SquirrelMark from '../components/SquirrelMark.vue'
import { notify, refreshState, store } from '../store'
import type { AutomationEnvironment, ConfigurationExport, Connection, ExecutionRules, Preferences, TransferStatistic } from '../types'
const emit = defineEmits<{ addConnection: [] }>()
const testingId = ref('')
const deleting = ref(false)
const deleteTarget = ref<Connection | null>(null)
const editTarget = ref<Connection | null>(null)
const editOpen = ref(false)
const credentialTarget = ref<Connection | null>(null)
const credentialSaving = ref(false)
const credentialForm = reactive({ username: '', password: '', accessKey: '', secretKey: '', sessionToken: '' })
const defaultRules: ExecutionRules = { enabled: false, start: '', end: '', weekdays: [], networks: [], onlyOnAC: false }
const preferences = reactive<Preferences>({ rules: { ...defaultRules }, bandwidthBytes: 0, maxReaders: 4, stagingBytes: 512 << 20, recoveryBytes: 512 << 20, cacheBytes: 512 << 20, maxFileBytes: 128 << 20, recoveryDays: 30, autoStart: false, notifications: false })
const quotaMiB = (key:'stagingBytes'|'recoveryBytes'|'cacheBytes'|'maxFileBytes') => computed({get:()=>preferences[key]/1048576,set:(value:number)=>{preferences[key]=Math.round(value*1048576)}})
const stagingMiB=quotaMiB('stagingBytes'), recoveryMiB=quotaMiB('recoveryBytes'), cacheMiB=quotaMiB('cacheBytes'), maxFileMiB=quotaMiB('maxFileBytes')
const preferencesLoading = ref(false)
const preferencesSaving = ref(false)
const preferencesError = ref('')
type PreferenceSection = 'common' | 'resources' | 'rules'
const savingSection = ref<PreferenceSection | null>(null)
const preferenceErrors = reactive<Record<PreferenceSection, string>>({ common: '', resources: '', rules: '' })
const savedPreferences = ref<Preferences | null>(null)
const resourceFields = ref<HTMLElement>()
const ruleFields = ref<HTMLElement>()
const resourceKeys = ['bandwidthBytes', 'maxReaders', 'stagingBytes', 'recoveryBytes', 'cacheBytes', 'maxFileBytes', 'recoveryDays'] as const
function sectionValues(value: Preferences, section: PreferenceSection) {
  if (section === 'common') return { autoStart: value.autoStart, notifications: value.notifications }
  if (section === 'rules') return { rules: value.rules }
  return Object.fromEntries(resourceKeys.map(key => [key, value[key]]))
}
function sectionDirty(section: PreferenceSection) { return !!savedPreferences.value && JSON.stringify(sectionValues(preferences, section)) !== JSON.stringify(sectionValues(savedPreferences.value, section)) }
function copyPreferences(value: Preferences): Preferences { return JSON.parse(JSON.stringify(value)) }

const savedToggles = ref<{ autoStart: boolean; notifications: boolean } | null>(null)
const awaitingNotificationAuthorization = computed(() => savingSection.value === 'common' && preferencesSaving.value && savedToggles.value?.notifications === false && preferences.notifications)
const statistics = ref<TransferStatistic[]>([])
const statisticsLoading = ref(false)
const statisticsError = ref('')
const statisticsLoaded = ref(false)
const diagnostics = ref<Record<string, unknown> | null>(null)
const diagnosticsLoading = ref(false)
const diagnosticsError = ref('')
const diagnosticsLoaded = ref(false)
const environment = ref<AutomationEnvironment | null>(null)
const environmentLoading = ref(false)
const environmentError = ref('')
const environmentLoaded = ref(false)
const configFile = ref<HTMLInputElement>()
const importPreview = ref<ConfigurationExport | null>(null)
const importBusy = ref(false)
const disk = computed(() => store.data.disk)
const bandwidthMiB = computed({ get: () => preferences.bandwidthBytes / (1024 * 1024), set: (value: number) => { preferences.bandwidthBytes = Math.max(0, Math.round(value * 1024 * 1024)) } })
const availableNetworks = computed(() => [...new Set([...(environment.value?.networkInterfaces || []), ...preferences.rules.networks])].sort((a, b) => a.localeCompare(b)))
const weekdays = [{ value: 1, label: '一' }, { value: 2, label: '二' }, { value: 3, label: '三' }, { value: 4, label: '四' }, { value: 5, label: '五' }, { value: 6, label: '六' }, { value: 0, label: '日' }]
const diskBars = computed(() => [
  { label: '传输暂存', used: disk.value.stagingBytes, limit: disk.value.stagingLimit, icon: Upload, tone: 'purple' },
  { label: '恢复副本', used: disk.value.recoveryBytes, limit: disk.value.recoveryLimit, icon: ShieldCheck, tone: 'green' },
  { label: '本机缓存', used: disk.value.cacheBytes || 0, limit: disk.value.cacheLimit || 0, icon: HardDrive, tone: 'purple' },
])
const diskUsage = computed(() => diskBars.value.reduce((total, bar) => total + bar.used, 0))
function formatBytes(bytes: number) { if (!bytes || bytes < 0) return '0 B'; const units = ['B', 'KB', 'MB', 'GB', 'TB']; let n = bytes; let i = 0; while (n >= 1024 && i < units.length - 1) { n /= 1024; i++ } return `${n.toFixed(i === 0 || n >= 10 ? 0 : 1)} ${units[i]}` }
function percent(value: number, limit: number) { return limit > 0 ? Math.min(100, Math.max(0, value / limit * 100)) : 0 }
function kindName(connection: Connection) { return connection.kind === 'demo' ? '隔离演示空间' : connection.kind === 's3' ? 'S3 兼容存储' : 'WebDAV' }
function endpoint(connection: Connection) { return connection.kind === 's3' && connection.bucket ? `${connection.endpoint.replace(/\/$/, '')} · ${connection.bucket}` : connection.endpoint }
function writeModeName(connection: Connection) { return connection.writeMode === 'copy' ? '仅上传为新副本' : connection.writeMode === 'strict' ? '严格防覆盖' : connection.writeMode === 'compatible' ? '常规同步（兼容设置）' : '常规同步（默认）' }
function writeModeDescription(connection: Connection) { return connection.writeMode === 'copy' ? '上传使用新名称，不改写同名文件；同步下载可用，删除和移动等操作不可用。' : connection.writeMode === 'strict' ? '覆盖仅在服务端支持条件写入时开放；删除、移动等操作也需要服务端支持。' : '按原路径直接上传和更新同名文件。删除、移动等操作仍取决于服务端能力。' }
function formatConnectionCheck(value: string) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.valueOf()) ? value : new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }).format(date)
}
type CapabilityKey = 'conditionalWrite' | 'conditionalDelete' | 'rangeRead' | 'multipartConditional'
const capabilityLabels: { key: CapabilityKey; label: string }[] = [
  { key: 'conditionalWrite', label: '条件写入' },
  { key: 'conditionalDelete', label: '条件删除' },
  { key: 'rangeRead', label: '范围读取' },
  { key: 'multipartConditional', label: '安全分片' },
]
function capabilityResult(connection: Connection, key: CapabilityKey) {
  if (connection.capabilities?.[key]) return { state: 'available', text: '可用' }
  const profile = connection.compatibilityProfile
  const isWriteCapability = key !== 'rangeRead'
  if (key === 'rangeRead') return { state: 'unverified', text: '未确认' }
  if (profile === 'read-checked' && isWriteCapability) return { state: 'unverified', text: '未检测' }
  if (profile === 'limited' && isWriteCapability) return { state: 'limited', text: '检测未完成' }
  if (!connection.capabilitiesCheckedAt && !profile) return { state: 'unverified', text: '未检测' }
  if (profile === 'failed') return { state: 'unverified', text: '未确认' }
  return { state: 'unsupported', text: '不支持' }
}
function connectionCapabilityRows(connection: Connection) {
  return capabilityLabels.filter(item => item.key !== 'multipartConditional' || connection.kind === 's3')
    .map(item => ({ ...item, ...capabilityResult(connection, item.key) }))
}
function strictModeRestricted(connection: Connection) {
  return connection.writeMode === 'strict' && !connection.capabilities?.conditionalWrite && !!connection.compatibilityProfile && connection.compatibilityProfile !== 'conditional' && connection.compatibilityProfile !== 'failed'
}
async function loadPreferences() {
  preferencesLoading.value = true; preferencesError.value = ''
  try { const loaded = await api.get<Preferences>('/api/preferences'); Object.assign(preferences, loaded); preferences.rules = { ...defaultRules, ...(loaded.rules || {}), weekdays: loaded.rules?.weekdays || [], networks: loaded.rules?.networks || [] }; savedToggles.value = { autoStart: loaded.autoStart, notifications: loaded.notifications }; Object.assign(preferenceErrors, { common: '', resources: '', rules: '' }); savedPreferences.value = copyPreferences(preferences) }
  catch (error) { preferencesError.value = error instanceof Error ? error.message : '读取设置失败' }
  finally { preferencesLoading.value = false }
}
async function savePreferences(section: PreferenceSection) {
  if (preferencesSaving.value || preferencesLoading.value || !savedPreferences.value) return
  preferenceErrors[section] = ''
  const fields = section === 'resources' ? resourceFields.value : section === 'rules' ? ruleFields.value : undefined
  const inputs = [...(fields?.querySelectorAll<HTMLInputElement>('input') || [])]
  for (const input of inputs) input.setCustomValidity('')
  if (section === 'resources' && preferences.maxFileBytes > preferences.stagingBytes) {
    fields?.querySelector<HTMLInputElement>('[data-preference="maxFileBytes"]')?.setCustomValidity('单文件上限不能超过传输暂存额度')
  }
  if (section === 'rules') {
    const { start, end } = preferences.rules
    const message = !!start !== !!end ? '开始和结束时间必须同时填写' : start && start === end ? '开始和结束时间不能相同' : ''
    if (message) {
      preferenceErrors.rules = message
      const input = fields?.querySelector<HTMLInputElement>(`[data-preference="${start ? 'end' : 'start'}"]`)
      if (input && !input.disabled) { input.setCustomValidity(message); input.reportValidity() }
      return
    }
  }
  const invalid = inputs.find(input => !input.checkValidity())
  if (invalid) { preferenceErrors[section] = invalid.validationMessage; invalid.reportValidity(); return }
  // Each save submits only its own section; edits in other sections stay pending.
  const submitted = Object.assign(copyPreferences(savedPreferences.value), sectionValues(copyPreferences(preferences), section))
  preferencesSaving.value = true; savingSection.value = section
  try {
    await api.post('/api/preferences', submitted)
    savedPreferences.value = copyPreferences(submitted)
    savedToggles.value = { autoStart: submitted.autoStart, notifications: submitted.notifications }
    await refreshState(true); preferencesError.value = ''; notify({ common: '常用偏好已保存', resources: '传输速度和空间额度已保存', rules: '计划任务限制已保存' }[section], 'success')
  } catch (error) {
    if (section === 'common' && savedToggles.value) {
      preferences.autoStart = savedToggles.value.autoStart
      preferences.notifications = savedToggles.value.notifications
    }
    preferenceErrors[section] = error instanceof Error ? error.message : '保存设置失败'
  } finally { preferencesSaving.value = false; savingSection.value = null }
}
function openCredentials(connection: Connection) {
  credentialTarget.value = connection
  Object.assign(credentialForm, { username: connection.username || '', password: '', accessKey: '', secretKey: '', sessionToken: '' })
}
function editConnection(connection: Connection) { editTarget.value = connection; editOpen.value = true }
async function saveCredentials() {
  if (!credentialTarget.value || credentialSaving.value) return
  credentialSaving.value = true
  try {
    await api.post('/api/connections/credentials', { id: credentialTarget.value.id, ...credentialForm })
    credentialTarget.value = null; await refreshState(true); notify('凭据已安全更新并自动检测', 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '凭据更新失败') }
  finally { credentialSaving.value = false }
}
async function loadStatistics() {
  statisticsLoading.value = true; statisticsError.value = ''
  try { statistics.value = await api.get<TransferStatistic[]>('/api/statistics') || []; statisticsLoaded.value = true }
  catch (error) { statisticsError.value = error instanceof Error ? error.message : '读取统计失败' }
  finally { statisticsLoading.value = false }
}
async function loadDiagnostics() {
  diagnosticsLoading.value = true; diagnosticsError.value = ''
  try { diagnostics.value = await api.get<Record<string, unknown>>('/api/diagnostics'); diagnosticsLoaded.value = true }
  catch (error) { diagnostics.value = null; diagnosticsError.value = error instanceof Error ? error.message : '读取诊断信息失败' }
  finally { diagnosticsLoading.value = false }
}
async function loadAutomationEnvironment() {
  environmentLoading.value = true; environmentError.value = ''
  try { environment.value = await api.get<AutomationEnvironment>('/api/automation/environment'); environmentLoaded.value = true }
  catch (error) { environment.value = null; environmentError.value = error instanceof Error ? error.message : '读取系统网络与电源状态失败' }
  finally { environmentLoading.value = false }
}
function onStatisticsToggle(event: Event) { if ((event.currentTarget as HTMLDetailsElement).open && !statisticsLoaded.value && !statisticsLoading.value) void loadStatistics() }
function onDiagnosticsToggle(event: Event) { if ((event.currentTarget as HTMLDetailsElement).open && !diagnosticsLoaded.value && !diagnosticsLoading.value) void loadDiagnostics() }
function onAutomationToggle(event: Event) { if ((event.currentTarget as HTMLDetailsElement).open && !environmentLoaded.value && !environmentLoading.value) void loadAutomationEnvironment() }
async function exportJSON(kind: 'config' | 'diagnostics') {
  try {
    const result = await api.post<{saved:boolean; path?:string; name:string; value?:unknown}>(`/api/${kind}/save`, {})
    if (!result.saved) {
      const blob = new Blob([JSON.stringify(result.value, null, 2)], {type:'application/json'})
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url; link.download = result.name; link.click()
      window.setTimeout(() => URL.revokeObjectURL(url), 1000)
    }
    notify(kind === 'config' ? '配置已导出，不含密码和密钥' : '脱敏诊断已导出', 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '导出失败') }
}
function exportConfiguration() { return exportJSON('config') }
function chooseImport() { configFile.value?.click() }
async function readImport(event: Event) {
  const input = event.target as HTMLInputElement; const file = input.files?.[0]; input.value = ''
  if (!file) return
  try {
    const parsed = JSON.parse(await file.text()) as ConfigurationExport
    if (parsed.version !== 1 || !Array.isArray(parsed.connections) || !Array.isArray(parsed.jobs) || !Array.isArray(parsed.gateways) || (parsed.backupJobs != null && !Array.isArray(parsed.backupJobs)) || (parsed.migrationJobs != null && !Array.isArray(parsed.migrationJobs)) || !parsed.preferences) throw new Error('配置文件格式或版本不受支持')
    parsed.preferences.rules ||= { ...defaultRules }
    importPreview.value = parsed
  } catch (error) { notify(error instanceof Error ? error.message : '无法读取配置文件') }
}
async function importConfiguration() {
  if (!importPreview.value || importBusy.value) return
  importBusy.value = true
  try { const result = await api.post<Record<string, number>>('/api/config/import', importPreview.value); importPreview.value = null; await refreshState(true); await loadPreferences(); notify(`配置已导入：${result.connections || 0} 个连接、${result.jobs || 0} 个同步、${result.backupJobs || 0} 个备份、${result.migrationJobs || 0} 个迁移、${result.gateways || 0} 个网关`, 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '导入配置失败') }
  finally { importBusy.value = false }
}
function scopeLabel(scope: string) { return scope === 'connection' ? '连接' : scope === 'job' ? '同步任务' : scope === 'backup' ? '备份任务' : scope === 'migration' ? '迁移任务' : scope === 'gateway' ? '网关' : scope }
function statName(item: TransferStatistic) { return item.scope === 'connection' ? store.data.connections.find(c => c.id === item.id)?.name || item.id : item.scope === 'job' ? store.data.jobs.find(j => j.id === item.id)?.name || item.id : item.scope === 'gateway' ? store.data.gateways.find(g => g.id === item.id)?.name || item.id : item.id }
async function testConnection(connection: Connection) {
  testingId.value = connection.id
  try {
    await api.post('/api/connections/test', { id: connection.id, writeTest: true })
    await refreshState(true)
    notify('连接能力已重新检测', 'success')
  } catch (error) { await refreshState(true); notify(error instanceof Error ? error.message : '连接测试失败') }
  finally { testingId.value = '' }
}
async function removeConnection() {
  if (!deleteTarget.value || deleting.value) return
  deleting.value = true
  try { await api.post('/api/connections/delete', { id: deleteTarget.value.id }); deleteTarget.value = null; await refreshState(true); notify('连接已删除', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '删除连接失败') }
  finally { deleting.value = false }
}
onMounted(() => { void loadPreferences() })
</script>
<template>
  <div class="page-view settings-page">
    <header class="page-heading"><div><h1>设置</h1></div></header>
    <section class="settings-section">
      <div class="section-heading"><div><h2><Database :size="17" />存储连接</h2></div><button class="button secondary small" @click="emit('addConnection')"><Plus :size="15" />添加连接</button></div>
      <div v-if="store.loading" class="settings-connection-list settings-loading-state" role="status"><SquirrelMark variant="eat" :size="36" /><span>正在读取存储连接…</span></div>
      <div v-else-if="store.data.connections.length" class="settings-connection-list">
        <article v-for="connection in store.data.connections" :key="connection.id" class="settings-connection-card">
          <div class="settings-connection-main">
            <div class="settings-connection-icon" :class="connection.kind"><HardDrive v-if="connection.kind === 's3' || connection.kind === 'demo'" :size="17" /><Server v-else :size="17" /></div>
            <div class="settings-connection-copy"><div class="settings-connection-name"><b>{{ connection.name }}</b><span class="connection-kind-tag">{{ kindName(connection) }}</span></div><span class="settings-endpoint" :title="endpoint(connection)">{{ endpoint(connection) || '无需远端地址' }}</span></div>
            <span v-if="connection.kind !== 'demo' && (connection.detecting || connection.error || connection.compatibilityProfile === 'limited' || connection.compatibilityProfile === 'failed')" class="connection-health" :class="connection.detecting ? 'gray' : connection.error || connection.compatibilityProfile === 'failed' ? 'red' : 'amber'"><i></i>{{ connection.detecting ? '正在自动检测' : connection.error || connection.compatibilityProfile === 'failed' ? '连接异常' : '写入能力检测未完成' }}</span>
            <button v-if="connection.kind !== 'demo'" class="button primary small connection-configure" @click="editConnection(connection)"><Settings2 :size="14"/>编辑连接</button>
          </div>
          <div v-if="connection.error" class="connection-error"><AlertTriangle :size="14" />{{ connection.error }}</div>
          <div v-else-if="connection.kind !== 'demo' && connection.compatibilityProfile === 'failed'" class="connection-error"><AlertTriangle :size="14" />连接检测失败，请检查地址和凭据后重新检测。</div>
          <div v-if="connection.kind !== 'demo' && connection.compatibilityProfile === 'limited' && !connection.error" class="connection-detection-note" role="status"><AlertTriangle :size="14"/><span>连接可读取，但写入能力检测未完成。请检查存储权限或重新检测；严格防覆盖会限制相关写入。</span></div>
          <div v-if="connection.kind !== 'demo' && strictModeRestricted(connection)" class="connection-write-restriction"><AlertTriangle :size="14"/><span><b>严格防覆盖受限</b><small>此连接当前无法执行受保护的远端写入；详细检测结果见连接详情。</small></span></div>
          <details class="connection-more">
            <summary>连接详情</summary>
            <div v-if="connection.kind !== 'demo'" class="connection-detail-body">
              <p class="field-hint">同步方式：{{ writeModeName(connection) }}。{{ writeModeDescription(connection) }} 自动恢复副本：{{ connection.keepRecovery ? '开启' : '关闭' }}。</p>
              <details class="connection-capability-details"><summary>能力检测详情</summary><div class="connection-capabilities"><span v-for="item in connectionCapabilityRows(connection)" :key="item.key" class="capability-chip" :class="item.state"><Check v-if="item.state === 'available'" :size="12"/><X v-else-if="item.state === 'unsupported'" :size="12"/><span v-else aria-hidden="true">…</span>{{item.label}}{{item.text}}</span><span class="capability-help">{{ connection.compatibilityProfile === 'read-checked' ? '已确认可以读取存储；写入能力未检测。' : connection.compatibilityProfile === 'limited' ? '读取可用，但写入能力检测没有完成；严格防覆盖下相关操作会受限。' : connection.writeMode === 'copy' ? '仅上传为新副本，不会执行写入能力检测。' : '' }}</span><p v-if="connection.writeRestriction" class="connection-capability-note">检测说明：{{connection.writeRestriction}}</p><p v-if="connection.capabilitiesCheckedAt" class="connection-capability-note">最近检测：{{formatConnectionCheck(connection.capabilitiesCheckedAt)}}</p></div></details>
              <footer class="connection-actions"><button class="button subtle small" @click="openCredentials(connection)"><KeyRound :size="14"/>补充凭据</button><button class="button subtle small" :disabled="testingId !== ''" @click="testConnection(connection)"><RefreshCw :size="14" :class="{ spin: testingId === connection.id }" />{{ testingId === connection.id ? '正在检测…' : '重新检测' }}</button><button class="button subtle small danger-hover" @click="deleteTarget = connection"><Trash2 :size="14"/>删除连接</button></footer>
            </div>
            <p v-else class="field-hint connection-detail-body">隔离演示空间仅使用本机测试数据，不会访问远端存储。</p>
          </details>
        </article>
      </div>
      <div v-else class="settings-empty"><span class="settings-empty-icon"><Server :size="18" /></span><div><b>还没有存储连接</b><small>添加 WebDAV 或 S3 存储，或从任务页建立隔离演示空间。</small></div><button class="text-button" @click="emit('addConnection')">添加连接 <ChevronRight :size="14" /></button></div>
    </section>
    <section class="settings-section preference-section">
      <div class="section-heading"><div><h2><Settings2 :size="17"/>常用偏好 <small v-if="sectionDirty('common')" class="unsaved-label">未保存</small></h2></div><button class="button primary small" :disabled="preferencesSaving || preferencesLoading || !!preferencesError" @click="savePreferences('common')"><RefreshCw v-if="preferencesSaving" :size="14" class="spin"/><Check v-else :size="14"/>{{preferencesSaving?'保存中…':'保存设置'}}</button></div>
      <div v-if="awaitingNotificationAuthorization" class="settings-inline-state preference-auth-wait" role="status" aria-live="polite"><RefreshCw :size="15" class="spin"/>正在等待操作系统通知授权。请完成系统提示；授权请求最长等待 15 秒。</div>
      <div v-if="preferenceErrors.common" class="settings-inline-error preference-save-error" role="alert"><AlertTriangle :size="15"/><span>{{preferenceErrors.common}}</span></div>
      <div v-if="preferencesLoading" class="settings-inline-state">正在读取设置…</div>
      <div v-else-if="preferencesError" class="settings-inline-error">{{preferencesError}} <button class="button secondary small" @click="loadPreferences">重试</button></div>
      <template v-else>
        <div class="common-preferences"><label class="check-row"><input v-model="preferences.notifications" type="checkbox" :disabled="preferencesSaving"/><span>启用系统通知</span><small>首次启用时会请求系统授权。</small></label><label class="check-row"><input v-model="preferences.autoStart" type="checkbox" :disabled="preferencesSaving"/><span>登录时启动小花鼠</span></label></div>
        <details class="setting-disclosure preference-advanced"><summary><span>传输速度和空间额度</span><small v-if="sectionDirty('resources')">未保存</small></summary><div class="setting-disclosure-body"><p class="field-hint">仅在需要限制资源占用时调整。额度不能低于现有受保护数据。</p><div ref="resourceFields" class="preference-grid">
          <label class="field"><span>总带宽上限（MiB/s，0 为不限）</span><input v-model.number="bandwidthMiB" type="number" required :disabled="preferencesSaving" min="0" max="1048576" step="any"/><small class="field-hint">当前 {{formatBytes(preferences.bandwidthBytes)}}/s</small></label>
          <label class="field"><span>最大并发读取</span><input v-model.number="preferences.maxReaders" type="number" required :disabled="preferencesSaving" min="1" max="16"/><small class="field-hint">范围 1–16</small></label>
          <label class="field"><span>传输暂存额度（MiB）</span><input v-model.number="stagingMiB" type="number" required :disabled="preferencesSaving" min="16" max="32768" step="any"/><small class="field-hint">当前 {{formatBytes(preferences.stagingBytes)}}</small></label>
          <label class="field"><span>恢复副本额度（MiB）</span><input v-model.number="recoveryMiB" type="number" required :disabled="preferencesSaving" min="16" max="32768" step="any"/><small class="field-hint">当前 {{formatBytes(preferences.recoveryBytes)}}</small></label>
          <label class="field"><span>单文件传输上限（MiB）</span><input data-preference="maxFileBytes" v-model.number="maxFileMiB" type="number" required :disabled="preferencesSaving" min="1" max="32768" step="any"/><small class="field-hint">不能大于传输暂存额度</small></label>
          <label class="field"><span>本机缓存额度（MiB）</span><input v-model.number="cacheMiB" type="number" required :disabled="preferencesSaving" min="16" max="32768" step="any"/><small class="field-hint">当前 {{formatBytes(preferences.cacheBytes)}}</small></label><label class="field"><span>恢复副本保留天数</span><input v-model.number="preferences.recoveryDays" type="number" required :disabled="preferencesSaving" min="1" max="3650"/></label>
        </div><p v-if="preferenceErrors.resources" class="section-save-error" role="alert">{{preferenceErrors.resources}}</p><footer class="modal-actions"><button class="button secondary small" :disabled="preferencesSaving || preferencesLoading || !!preferencesError" @click="savePreferences('resources')"><RefreshCw v-if="preferencesSaving" :size="14" class="spin"/><Check v-else :size="14"/>{{preferencesSaving?'保存中…':'保存设置'}}</button></footer></div></details>
      </template>
    </section>
    <section class="settings-section disk-section">
      <div class="section-heading"><div><h2><HardDrive :size="17" />磁盘空间</h2></div></div>
      <div class="disk-summary"><span>缓存与恢复副本 <b>{{formatBytes(diskUsage)}}</b></span><span class="disk-summary-divider">·</span><span>可用空间 <b>{{formatBytes(disk.freeBytes)}}</b></span></div>
      <details class="setting-disclosure disk-details"><summary>查看数据目录和空间详情</summary><div class="disk-card"><div class="disk-card-top"><div><span class="disk-location-label">应用数据目录</span><code>{{ store.data.dataDir || '等待本地服务报告数据目录' }}</code></div><div class="data-dir-icon"><HardDrive :size="18" /></div></div><div class="disk-divider"></div><div class="disk-usage-grid"><div v-for="bar in diskBars" :key="bar.label" class="disk-usage-item"><div class="disk-usage-head"><span><component :is="bar.icon" :size="15" />{{ bar.label }}</span><b>{{ formatBytes(bar.used) }} <small>/ {{ formatBytes(bar.limit) }}</small></b></div><div class="meter-track"><div class="meter-fill" :class="bar.tone" :style="{ width: `${percent(bar.used, bar.limit)}%` }"></div></div><span class="meter-caption">{{ bar.limit ? `${percent(bar.used, bar.limit).toFixed(0)}% 已使用` : '容量限制由本地服务提供' }}</span></div></div></div><div class="settings-footnote"><Check :size="14" />未决操作引用的内容会保留，不会被当作缓存自动清除。</div></details>
    </section>
    <section class="settings-section automation-rules-section">
      <details class="setting-disclosure" @toggle="onAutomationToggle"><summary><span><Clock3 :size="16"/>计划任务限制</span><small>{{sectionDirty('rules')?'未保存':preferences.rules.enabled?'已启用':'未启用'}}</small></summary><div class="setting-disclosure-body"><p class="field-hint">限制同步和备份的计划任务；手动运行仍可继续。</p>
      <div v-if="preferencesLoading" class="settings-inline-state">正在读取自动执行规则…</div>
      <div v-else-if="preferencesError" class="settings-inline-error">{{preferencesError}} <button class="button secondary small" @click="loadPreferences">重试</button></div>
      <div v-else class="automation-rule-card">
        <label class="check-row automation-enable"><input v-model="preferences.rules.enabled" :disabled="preferencesSaving" type="checkbox"/><span>启用自动执行限制</span><small>关闭时，下面的时间、日期、网卡和电源条件均不限制计划任务。</small></label>
        <div ref="ruleFields" class="automation-rule-fields">
          <div class="automation-time-fields"><label class="field"><span>允许开始时间</span><input data-preference="start" v-model="preferences.rules.start" type="time" :disabled="preferencesSaving"/></label><label class="field"><span>允许结束时间</span><input data-preference="end" v-model="preferences.rules.end" type="time" :disabled="preferencesSaving"/></label></div><small class="automation-hint">开始和结束都留空表示全天；设置时段时必须同时填写。</small>
          <fieldset class="weekday-field" :disabled="preferencesSaving"><legend>允许的星期 <small>不选择表示每天</small></legend><label v-for="day in weekdays" :key="day.value" class="weekday-chip" :class="{selected:preferences.rules.weekdays.includes(day.value)}"><input type="checkbox" :value="day.value" v-model="preferences.rules.weekdays"/><span>{{day.label}}</span></label></fieldset>
          <div class="field network-field"><span>允许的活动网卡 <small>不选择表示不限</small></span><div v-if="environmentLoading" class="automation-environment-state">正在读取活动网卡与电源状态…</div><div v-else-if="environmentError" class="automation-environment-error"><span>{{environmentError}}</span><button type="button" class="button secondary small" @click="loadAutomationEnvironment">重试</button></div><div v-else-if="availableNetworks.length" class="automation-network-list"><label v-for="name in availableNetworks" :key="name" class="network-choice"><input v-model="preferences.rules.networks" type="checkbox" :value="name" :disabled="preferencesSaving"/><span>{{name}}</span><small v-if="environment?.networkInterfaces?.includes(name)">当前活动</small><small v-else>目前未检测到</small></label></div><div v-else class="automation-environment-state">没有活动的非回环网卡。留空时不限制网络；若选择网卡，自动任务会等到该网卡活动。</div></div>
          <label class="check-row"><input v-model="preferences.rules.onlyOnAC" type="checkbox" :disabled="preferencesSaving"/><span>仅接通电源时运行自动任务</span><small>电源状态：{{environmentLoading?'读取中…':environmentError?'未知':!environment?.powerKnown?'未知':environment.onAC?'已接通':'未接通'}}。启用后，状态未知或未接电源时计划任务会暂停。</small></label>
        </div>
        <p v-if="preferenceErrors.rules" class="section-save-error" role="alert">{{preferenceErrors.rules}}</p><footer class="modal-actions"><button class="button primary small" :disabled="preferencesSaving || preferencesLoading || !!preferencesError" @click="savePreferences('rules')"><RefreshCw v-if="preferencesSaving" :size="14" class="spin"/><Check v-else :size="14"/>{{preferencesSaving?'保存中…':'保存设置'}}</button></footer>
      </div>
      </div></details>
    </section>
    <section class="settings-section configuration-section">
      <details class="setting-disclosure"><summary><span><ShieldCheck :size="16"/>配置导入与导出</span></summary><div class="setting-disclosure-body"><div class="config-actions"><button class="button secondary" @click="exportConfiguration"><Upload :size="15"/>导出配置</button><button class="button secondary" @click="chooseImport"><Database :size="15"/>选择配置文件导入</button><input ref="configFile" type="file" accept="application/json,.json" hidden @change="readImport"/><span class="field-hint">导出不含密码。导入会新增连接与任务，并替换传输额度和计划任务限制。</span></div></div></details>
    </section>
    <section class="settings-section telemetry-section">
      <details class="setting-disclosure" @toggle="onStatisticsToggle"><summary><span><Activity :size="16"/>传输统计</span></summary><div class="setting-disclosure-body"><div class="section-heading"><p>应用记录的传输与网关请求；不代表服务商账单。</p><button class="icon-button" :disabled="statisticsLoading" title="刷新统计" @click="loadStatistics"><RefreshCw :size="15" :class="{spin:statisticsLoading}"/></button></div>
      <div v-if="statisticsLoading" class="settings-inline-state">正在读取统计…</div><div v-else-if="statisticsError" class="settings-inline-error">{{statisticsError}} <button class="button secondary small" @click="loadStatistics">重试</button></div><div v-else-if="statistics.length" class="statistics-table"><div class="statistics-head"><span>范围</span><span>对象</span><span>请求</span><span>错误</span><span>上传</span><span>下载</span><span>最近访问</span></div><div v-for="item in statistics" :key="item.scope+item.id" class="statistics-row"><span>{{scopeLabel(item.scope)}}</span><b :title="item.id">{{statName(item)}}</b><span>{{item.requests}}</span><span>{{item.errors}}</span><span>{{formatBytes(item.uploaded)}}</span><span>{{formatBytes(item.downloaded)}}</span><span>{{item.lastAccess||'—'}}</span></div></div><div v-else class="settings-inline-state">暂无统计记录。</div></div></details>
    </section>
    <section class="settings-section diagnostics-section">
      <details class="setting-disclosure" @toggle="onDiagnosticsToggle"><summary><span><AlertTriangle :size="16"/>诊断信息</span></summary><div class="setting-disclosure-body"><div class="section-heading"><p>包含应用版本和功能状态，不含秘密凭据。</p><div class="diagnostic-actions"><button class="button secondary small" :disabled="diagnosticsLoading" @click="exportJSON('diagnostics')"><Upload :size="14"/>导出诊断</button><button class="icon-button" :disabled="diagnosticsLoading" title="刷新诊断" @click="loadDiagnostics"><RefreshCw :size="15" :class="{spin:diagnosticsLoading}"/></button></div></div>
      <details v-if="diagnostics && !diagnosticsLoading" class="diagnostics-details"><summary>查看诊断详情</summary><pre class="diagnostics-json">{{JSON.stringify(diagnostics,null,2)}}</pre></details><div v-else-if="diagnosticsError" class="settings-inline-error">{{diagnosticsError}} <button class="button secondary small" @click="loadDiagnostics">重试</button></div><div v-else class="settings-inline-state">{{diagnosticsLoading?'正在读取诊断信息…':'尚无诊断信息'}}</div></div></details>
    </section>
    <section class="settings-section about-section"><details class="setting-disclosure"><summary><span><KeyRound :size="16"/>关于小花鼠 Tamias</span></summary><div class="setting-disclosure-body"><div class="about-card"><span class="about-logo"><SquirrelMark variant="logo" :size="24" /></span><div><b>小花鼠 Tamias</b><small>本机桌面版 · {{ store.data.version || '开发预览' }}</small></div><span class="about-private"><ShieldCheck :size="14" />配置保存在本机</span></div></div></details></section>
    <EditConnectionModal v-model="editOpen" :connection="editTarget" />
    <BaseModal :model-value="!!credentialTarget" :dismissible="!credentialSaving" title="补充连接凭据" :subtitle="credentialTarget?.name" @update:model-value="v=>{if(!v)credentialTarget=null}"><form class="form-stack" :aria-busy="credentialSaving" @submit.prevent="saveCredentials"><template v-if="credentialTarget?.kind==='s3'"><label class="field"><span>访问密钥 ID</span><input v-model="credentialForm.accessKey" :disabled="credentialSaving" type="password" autocomplete="off" required/></label><label class="field"><span>秘密访问密钥</span><input v-model="credentialForm.secretKey" :disabled="credentialSaving" type="password" autocomplete="new-password" required/></label><label class="field"><span>会话令牌（可选）</span><input v-model="credentialForm.sessionToken" :disabled="credentialSaving" type="password" autocomplete="off"/></label></template><template v-else><label class="field"><span>用户名</span><input v-model="credentialForm.username" :disabled="credentialSaving" autocomplete="username" readonly/></label><label class="field"><span>密码</span><input v-model="credentialForm.password" :disabled="credentialSaving" type="password" autocomplete="new-password" required/></label></template><p class="field-hint">新凭据会在保存时自动检测；如果检测失败，原凭据会保留。更改用户名请使用“编辑连接”。</p><footer class="modal-actions"><button class="button secondary" type="button" :disabled="credentialSaving" @click="credentialTarget=null">取消</button><button class="button primary" type="submit" :disabled="credentialSaving"><RefreshCw v-if="credentialSaving" :size="14" class="spin"/>{{credentialSaving?'正在检测并保存…':'保存凭据'}}</button></footer></form></BaseModal>
    <BaseModal :model-value="!!importPreview" :dismissible="!importBusy" title="确认导入配置" subtitle="配置将追加到当前工作区。" @update:model-value="v=>{if(!v)importPreview=null}"><div v-if="importPreview" class="confirm-panel"><div class="confirm-warning"><ShieldCheck :size="18"/></div><p>即将新增 {{importPreview.connections.length}} 个连接、{{importPreview.jobs.length}} 个暂停同步、{{importPreview.backupJobs?.length||0}} 个备份、{{importPreview.migrationJobs?.length||0}} 个迁移及 {{importPreview.gateways.length}} 个停止网关。导入任务会要求重新选择本机目录，并应用导入文件中的传输额度与自动执行规则。</p><p>配置文件版本：{{importPreview.version}}；任何凭据都需要通过“补充凭据”单独重新输入。</p><footer class="modal-actions"><button class="button secondary" :disabled="importBusy" @click="importPreview=null">取消</button><button class="button primary" :disabled="importBusy" @click="importConfiguration">{{importBusy?'正在导入…':'确认导入'}}</button></footer></div></BaseModal>
    <BaseModal :model-value="!!deleteTarget" :dismissible="!deleting" title="删除存储连接" subtitle="请先移除依赖任务、网关及本机缓存，处理未决操作。" @update:model-value="(v) => { if (!v) deleteTarget = null }"><div class="confirm-panel"><div class="confirm-warning"><Trash2 :size="18" /></div><p>确定删除“{{ deleteTarget?.name }}”吗？远端文件不会被删除。</p><footer class="modal-actions"><button class="button secondary" :disabled="deleting" @click="deleteTarget = null">取消</button><button class="button danger" :disabled="deleting" @click="removeConnection">{{ deleting ? '正在删除…' : '删除连接' }}</button></footer></div></BaseModal>
  </div>
</template>
<style scoped>
.unsaved-label{font-size:11px;font-weight:400;color:var(--accent)}.section-save-error{margin:10px 0 0;color:#a34f45;font-size:12px;overflow-wrap:anywhere}
.preference-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:13px;padding:15px;border:1px solid #eae6f2;border-radius:13px;background:#fff}.preference-toggles{display:grid;gap:10px;margin-top:12px;padding:14px;border:1px solid #eae6f2;border-radius:13px;background:#fff}.preference-toggles .check-row{display:flex;flex-wrap:wrap}.preference-toggles .check-row small{flex-basis:100%;margin-left:24px;color:#898598}.automation-rule-card{display:grid;gap:12px;padding:14px;border:1px solid #eae6f2;border-radius:13px;background:#fff}.automation-rule-card .check-row{flex-wrap:wrap}.automation-enable{padding-bottom:10px;border-bottom:1px solid #f0edf5}.automation-enable small,.automation-rule-fields .check-row small{flex-basis:100%;margin-left:24px;color:#898598}.automation-rule-fields{display:grid;gap:12px}.automation-time-fields{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px}.automation-rule-fields.disabled{opacity:.55}.automation-hint{margin-top:-8px;color:#898598;font-size:11px}.weekday-field{display:flex;align-items:center;gap:7px;min-width:0;margin:0;padding:0;border:0}.weekday-field legend{float:left;margin-right:8px;color:#5e586f;font-size:12px}.weekday-field legend small,.automation-time-fields .field small{color:#898598;font-weight:400}.weekday-chip{position:relative;display:grid;place-items:center;width:31px;height:31px;border:1px solid #e6e1ee;border-radius:9px;color:#756f83;cursor:pointer}.weekday-chip:focus-within{outline:3px solid rgba(99,84,255,.22);outline-offset:2px}.weekday-chip.selected{border-color:#baa9e6;background:#f5f1ff;color:#65519d}.weekday-chip input{position:absolute;opacity:0;pointer-events:none}.network-field{display:grid;gap:7px}.network-field>span{color:#5e586f;font-size:12px}.network-field>span small{color:#898598;font-weight:400}.automation-network-list{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:6px}.network-choice{display:flex;align-items:center;gap:8px;min-width:0;padding:8px 10px;border:1px solid #eeeaf5;border-radius:9px;background:#fcfbfe;color:#625b73;font-size:11px}.network-choice span{overflow-wrap:anywhere;flex:1}.network-choice small{color:#898598;font-size:10px}.automation-environment-state,.automation-environment-error{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:10px;border:1px dashed #ddd7ea;border-radius:9px;color:#898598;font-size:11px}.automation-environment-error{color:#a34f45}.automation-rule-note{margin:0;color:#898598;font-size:11px}.config-actions{display:flex;align-items:center;gap:9px;flex-wrap:wrap;padding:15px;border:1px solid #eae6f2;border-radius:13px;background:#fff}.config-actions .field-hint{flex:1;min-width:220px}.statistics-table{display:grid;overflow:auto;border:1px solid #eae6f2;border-radius:12px;background:#fff}.statistics-head,.statistics-row{display:grid;grid-template-columns:90px minmax(130px,1.4fr) 65px 65px 90px 90px minmax(130px,1fr);align-items:center;gap:8px;min-width:740px;padding:10px 12px;border-bottom:1px solid #f0edf5;font-size:11px}.statistics-head{background:#faf9fc;color:#858194}.statistics-row{color:#716d82}.statistics-row:last-child{border-bottom:0}.statistics-row b{overflow:hidden;text-overflow:ellipsis;color:#4b445f;font-weight:600;white-space:nowrap}.diagnostics-details summary{padding:10px 0;color:var(--text-secondary);font-size:12px;cursor:pointer}.diagnostics-json{max-height:420px;overflow:auto;margin:0;padding:14px;border:1px solid #eae6f2;border-radius:12px;background:#faf9fc;color:#5d5571;font:11px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;overflow-wrap:anywhere}.settings-inline-state,.settings-inline-error{display:flex;align-items:center;justify-content:center;gap:8px;min-height:65px;padding:12px;border:1px dashed #ddd7ea;border-radius:12px;color:#7a7689;font-size:12px}.settings-inline-error{justify-content:space-between;color:#a34f45}@media(max-width:740px){.preference-grid,.automation-time-fields{grid-template-columns:1fr}.config-actions{align-items:stretch}.config-actions .button{justify-content:center}.weekday-field{flex-wrap:wrap}.weekday-field legend{width:100%}}
.preference-auth-wait{justify-content:flex-start;min-height:0;margin-top:10px;border-style:solid;border-color:#dcd5ee;background:#f8f6fd;color:#665887}.preference-save-error{justify-content:flex-start;min-height:0;margin-top:10px;border-style:solid;align-items:flex-start}.preference-save-error span{overflow-wrap:anywhere}
.weekday-chip:focus-within{outline:2px solid #7662b2;outline-offset:2px}
.connection-health.amber{color:#a97827}
.capability-chip.unsupported{background:#fff0ed;color:#a2534b}
.capability-chip.limited{background:#fff5e1;color:#9a6b22}
.connection-detection-note{display:flex;align-items:flex-start;gap:7px;margin:8px 0 0 40px;padding:8px 9px;border:1px solid #f0dfb9;border-radius:7px;background:#fffaf0;color:#916b2b;font-size:11px;line-height:1.45}
.connection-detection-note>svg{flex:0 0 auto;margin-top:1px}
.capability-chip.restricted{background:#fff5e1;color:#9a6b22}
.connection-write-restriction{display:flex;align-items:flex-start;gap:7px;margin:8px 0 0 40px;padding:7px 9px;border:1px solid #f0dfb9;border-radius:7px;background:#fffaf0;color:#99691e;font-size:11px}
.connection-write-restriction>svg{flex:0 0 auto;margin-top:1px}
.connection-write-restriction span,.connection-write-restriction b,.connection-write-restriction small{display:block}
.connection-write-restriction b{font-size:11px;font-weight:650}
.connection-write-restriction small{margin-top:3px;color:#8e764e;font-size:10px;line-height:1.4;overflow-wrap:anywhere}
.setting-disclosure{overflow:hidden;border:1px solid #e9e8ef;border-radius:10px;background:#fff}
.setting-disclosure>summary,.connection-more>summary,.connection-capability-details>summary{display:flex;align-items:center;justify-content:space-between;gap:10px;min-height:39px;padding:9px 12px;color:#555462;font-size:12px;font-weight:600;cursor:pointer;list-style:none}
.setting-disclosure>summary::-webkit-details-marker,.connection-more>summary::-webkit-details-marker,.connection-capability-details>summary::-webkit-details-marker{display:none}
.setting-disclosure>summary::after,.connection-more>summary::after,.connection-capability-details>summary::after{content:'+';color:#8177ce;font-size:16px;font-weight:400}
.setting-disclosure[open]>summary::after,.connection-more[open]>summary::after,.connection-capability-details[open]>summary::after{content:'−'}
.setting-disclosure>summary>span{display:flex;align-items:center;gap:8px;min-width:0}
.setting-disclosure>summary svg{flex:0 0 auto;color:#8378dc}
.setting-disclosure>summary small{margin-left:auto;color:#777684;font-size:11px;font-weight:400}
.setting-disclosure-body{display:grid;gap:12px;padding:12px;border-top:1px solid #e9e8ef}
.setting-disclosure-body>.section-heading{margin:0}
.setting-disclosure-body>.section-heading>p{flex:1}
.disk-summary{display:flex;align-items:center;gap:10px;min-height:40px;padding:8px 12px;border:1px solid #e9e8ef;border-radius:9px;background:#fff;color:#777684;font-size:12px}
.disk-summary b{color:#52515e;font-size:12px;font-weight:600}
.disk-summary-divider{color:#c1bfca}
.disk-details{margin-top:8px}
.disk-card{display:grid;gap:0}
.common-preferences{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:9px;padding:12px;border:1px solid #e9e8ef;border-radius:10px;background:#fff}
.common-preferences .check-row{align-items:flex-start;flex-wrap:wrap;min-width:0}
.common-preferences .check-row small{flex-basis:100%;margin-left:24px;color:#777684;font-size:11px}
.preference-advanced{margin-top:9px}
.preference-advanced .setting-disclosure-body{gap:10px}
.preference-advanced .field-hint{margin:0}
.connection-configure{flex:0 0 auto}
.connection-more{margin-top:9px;border-top:1px solid #f0eff4}
.connection-more>summary{min-height:32px;padding:7px 0 0;border:0;color:#777684;font-size:11px;font-weight:500}
.connection-more>summary::after{font-size:14px}
.connection-detail-body{display:grid;gap:8px;padding:8px 0 2px}
.connection-detail-body>.field-hint{margin:0}
.connection-capability-note{margin:0;color:#777684;font-size:11px;line-height:1.5}
.connection-capability-details{border:1px solid #e9e8ef;border-radius:8px;background:#fff}
.connection-capability-details>summary{min-height:32px;padding:6px 9px;font-size:11px;font-weight:500}
.connection-capability-details>summary::after{font-size:14px}
.connection-capabilities{margin:0;padding:0 9px 8px}
.connection-actions{margin:0;padding-top:8px;flex-wrap:wrap}
.connection-actions .button{height:32px}
.diagnostic-actions{display:flex;align-items:center;gap:6px}
@media(max-width:620px){.common-preferences{grid-template-columns:1fr}.settings-connection-main{flex-wrap:wrap}.settings-connection-copy{min-width:calc(100% - 45px)}.connection-configure{margin-left:40px}.connection-health{margin-left:auto}.disk-summary{flex-wrap:wrap;gap:5px 9px}}
</style>
