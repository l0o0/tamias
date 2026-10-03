<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { Activity, AlertTriangle, Check, ChevronRight, Clock3, Database, HardDrive, KeyRound, Pencil, Plus, RefreshCw, Server, Settings2, ShieldCheck, Trash2, Upload, X } from 'lucide-vue-next'
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
const preferencesSaveError = ref('')
const savedToggles = ref<{ autoStart: boolean; notifications: boolean } | null>(null)
const awaitingNotificationAuthorization = computed(() => preferencesSaving.value && savedToggles.value?.notifications === false && preferences.notifications)
const statistics = ref<TransferStatistic[]>([])
const statisticsLoading = ref(false)
const statisticsError = ref('')
const diagnostics = ref<Record<string, unknown> | null>(null)
const diagnosticsLoading = ref(false)
const diagnosticsError = ref('')
const environment = ref<AutomationEnvironment | null>(null)
const environmentLoading = ref(false)
const environmentError = ref('')
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
function formatBytes(bytes: number) { if (!bytes || bytes < 0) return '0 B'; const units = ['B', 'KB', 'MB', 'GB', 'TB']; let n = bytes; let i = 0; while (n >= 1024 && i < units.length - 1) { n /= 1024; i++ } return `${n.toFixed(i === 0 || n >= 10 ? 0 : 1)} ${units[i]}` }
function percent(value: number, limit: number) { return limit > 0 ? Math.min(100, Math.max(0, value / limit * 100)) : 0 }
function kindName(connection: Connection) { return connection.kind === 'demo' ? '隔离演示空间' : connection.kind === 's3' ? 'S3 兼容存储' : 'WebDAV' }
function endpoint(connection: Connection) { return connection.kind === 's3' && connection.bucket ? `${connection.endpoint.replace(/\/$/, '')} · ${connection.bucket}` : connection.endpoint }
async function loadPreferences() {
  preferencesLoading.value = true; preferencesError.value = ''
  try { const loaded = await api.get<Preferences>('/api/preferences'); Object.assign(preferences, loaded); preferences.rules = { ...defaultRules, ...(loaded.rules || {}), weekdays: loaded.rules?.weekdays || [], networks: loaded.rules?.networks || [] }; savedToggles.value = { autoStart: loaded.autoStart, notifications: loaded.notifications }; preferencesSaveError.value = '' }
  catch (error) { preferencesError.value = error instanceof Error ? error.message : '读取设置失败' }
  finally { preferencesLoading.value = false }
}
async function savePreferences() {
  if (preferencesSaving.value) return
  const submitted = { ...preferences, rules: { ...preferences.rules, weekdays: [...preferences.rules.weekdays], networks: [...preferences.rules.networks] } }
  preferencesSaving.value = true
  preferencesSaveError.value = ''
  try {
    await api.post('/api/preferences', submitted)
    savedToggles.value = { autoStart: submitted.autoStart, notifications: submitted.notifications }
    preferences.autoStart = submitted.autoStart
    preferences.notifications = submitted.notifications
    await refreshState(true); preferencesError.value = ''; notify('传输、自动执行与本机保护设置已保存', 'success')
  }
  catch (error) {
    if (savedToggles.value) {
      preferences.autoStart = savedToggles.value.autoStart
      preferences.notifications = savedToggles.value.notifications
    }
    preferencesSaveError.value = `${error instanceof Error ? error.message : '保存设置失败'}。登录启动和系统通知开关已恢复为已保存状态；其他表单输入仍保留。`
  }
  finally { preferencesSaving.value = false }
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
    credentialTarget.value = null; await refreshState(true); notify('凭据已验证并安全更新，请重新验证连接能力', 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '凭据更新失败') }
  finally { credentialSaving.value = false }
}
async function loadStatistics() {
  statisticsLoading.value = true; statisticsError.value = ''
  try { statistics.value = await api.get<TransferStatistic[]>('/api/statistics') || [] }
  catch (error) { statisticsError.value = error instanceof Error ? error.message : '读取统计失败' }
  finally { statisticsLoading.value = false }
}
async function loadDiagnostics() {
  diagnosticsLoading.value = true; diagnosticsError.value = ''
  try { diagnostics.value = await api.get<Record<string, unknown>>('/api/diagnostics') }
  catch (error) { diagnostics.value = null; diagnosticsError.value = error instanceof Error ? error.message : '读取诊断信息失败' }
  finally { diagnosticsLoading.value = false }
}
async function loadAutomationEnvironment() {
  environmentLoading.value = true; environmentError.value = ''
  try { environment.value = await api.get<AutomationEnvironment>('/api/automation/environment') }
  catch (error) { environment.value = null; environmentError.value = error instanceof Error ? error.message : '读取系统网络与电源状态失败' }
  finally { environmentLoading.value = false }
}
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
  try { const result = await api.post<Record<string, number>>('/api/config/import', importPreview.value); importPreview.value = null; await refreshState(true); notify(`配置已导入：${result.connections || 0} 个连接、${result.jobs || 0} 个同步、${result.backupJobs || 0} 个备份、${result.migrationJobs || 0} 个迁移、${result.gateways || 0} 个网关`, 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '导入配置失败') }
  finally { importBusy.value = false }
}
function scopeLabel(scope: string) { return scope === 'connection' ? '连接' : scope === 'job' ? '同步任务' : scope === 'backup' ? '备份任务' : scope === 'migration' ? '迁移任务' : scope === 'gateway' ? '网关' : scope }
function statName(item: TransferStatistic) { return item.scope === 'connection' ? store.data.connections.find(c => c.id === item.id)?.name || item.id : item.scope === 'job' ? store.data.jobs.find(j => j.id === item.id)?.name || item.id : item.scope === 'gateway' ? store.data.gateways.find(g => g.id === item.id)?.name || item.id : item.id }
async function testConnection(connection: Connection, writeTest: boolean) {
  testingId.value = `${connection.id}:${writeTest ? 'write' : 'read'}`
  try {
    await api.post('/api/connections/test', { id: connection.id, writeTest })
    await refreshState(true)
    notify(writeTest ? '读写能力验证完成' : '连接测试完成', 'success')
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
onMounted(() => { void loadPreferences(); void loadStatistics(); void loadDiagnostics(); void loadAutomationEnvironment() })
</script>
<template>
  <div class="page-view settings-page">
    <header class="page-heading"><div><div class="eyebrow">PREFERENCES</div><h1>设置</h1><p>管理存储连接、读写能力和本机数据空间。</p></div></header>
    <section class="settings-section">
      <div class="section-heading"><div><h2><Database :size="17" />存储连接</h2><p>连接凭据保存在本机安全凭据库中。</p></div><button class="button secondary small" @click="emit('addConnection')"><Plus :size="15" />添加连接</button></div>
      <div v-if="store.loading" class="settings-connection-list settings-loading-state" role="status"><SquirrelMark variant="eat" :size="36" /><span>正在读取存储连接…</span></div>
      <div v-else-if="store.data.connections.length" class="settings-connection-list">
        <article v-for="connection in store.data.connections" :key="connection.id" class="settings-connection-card">
          <div class="settings-connection-main"><div class="settings-connection-icon" :class="connection.kind"><HardDrive v-if="connection.kind === 's3' || connection.kind === 'demo'" :size="17" /><Server v-else :size="17" /></div><div class="settings-connection-copy"><div class="settings-connection-name"><b>{{ connection.name }}</b><span class="connection-kind-tag">{{ kindName(connection) }}</span><span v-if="connection.kind === 'demo'" class="demo-mark">隔离</span></div><span class="settings-endpoint" :title="endpoint(connection)">{{ endpoint(connection) || '无需远端地址' }}</span></div><span v-if="connection.kind !== 'demo'" class="connection-health" :class="connection.error ? 'red' : connection.tested ? 'green' : 'gray'"><i></i>{{ connection.error ? '连接异常' : connection.tested ? '已测试' : '待测试' }}</span><button class="icon-button danger-hover" title="删除连接" @click="deleteTarget = connection"><Trash2 :size="15" /></button></div>
          <div v-if="connection.error" class="connection-error"><AlertTriangle :size="14" />{{ connection.error }}</div>
          <div class="connection-capabilities"><span class="capability-label">能力状态</span><span class="capability-chip" :class="connection.kind === 'demo' || connection.capabilities?.conditionalWrite ? 'available' : 'unverified'"><Check v-if="connection.kind === 'demo' || connection.capabilities?.conditionalWrite" :size="12" /><X v-else :size="12" />{{ connection.kind === 'demo' ? '演示读写' : connection.capabilities?.conditionalWrite ? '条件写入' : '写入未验证' }}</span><span class="capability-chip" :class="connection.kind === 'demo' || connection.capabilities?.conditionalDelete ? 'available' : 'unverified'"><Check v-if="connection.kind === 'demo' || connection.capabilities?.conditionalDelete" :size="12" /><X v-else :size="12" />{{ connection.kind === 'demo' ? '演示删除' : connection.capabilities?.conditionalDelete ? '条件删除' : '删除未验证' }}</span><span class="capability-chip" :class="connection.capabilities?.rangeRead ? 'available' : 'unverified'"><Check v-if="connection.capabilities?.rangeRead" :size="12"/><X v-else :size="12"/>{{connection.capabilities?.rangeRead?'范围读取':'范围读取未验证'}}</span><span class="capability-chip" :class="connection.capabilities?.multipartConditional ? 'available' : 'unverified'"><Check v-if="connection.capabilities?.multipartConditional" :size="12"/><X v-else :size="12"/>{{connection.capabilities?.multipartConditional?'安全分片提交':'安全分片未验证'}}</span><span class="capability-help">只读测试不会修改远端；读写验证会创建并清理独立临时对象。安全分片能力单独验证，不影响普通小文件写入。</span></div>
          <footer class="connection-actions"><button class="button subtle small" :disabled="connection.kind === 'demo'" @click="editConnection(connection)"><Pencil :size="14"/>编辑配置</button><button class="button subtle small" :disabled="connection.kind === 'demo'" @click="openCredentials(connection)"><KeyRound :size="14"/>补充凭据</button><button class="button subtle small" :disabled="testingId !== '' || connection.kind === 'demo'" @click="testConnection(connection, false)"><RefreshCw :size="14" :class="{ spin: testingId === `${connection.id}:read` }" />{{ testingId === `${connection.id}:read` ? '正在测试…' : '连接测试' }}</button><button class="button subtle small" :disabled="testingId !== '' || connection.kind === 'demo'" @click="testConnection(connection, true)"><ShieldCheck :size="14" :class="{ spin: testingId === `${connection.id}:write` }" />{{ testingId === `${connection.id}:write` ? '验证中…' : '验证读写' }}</button><span class="connection-test-note"><KeyRound :size="13" />需用户主动点击授权</span></footer>
        </article>
      </div>
      <div v-else class="settings-empty"><span class="settings-empty-icon"><Server :size="18" /></span><div><b>还没有存储连接</b><small>添加 WebDAV 或 S3 存储，或从任务页建立隔离演示空间。</small></div><button class="text-button" @click="emit('addConnection')">添加连接 <ChevronRight :size="14" /></button></div>
    </section>
    <section class="settings-section disk-section">
      <div class="section-heading"><div><h2><HardDrive :size="17" />磁盘空间</h2><p>暂存与恢复副本都由本机管理，并受容量上限保护。</p></div><span class="disk-free"><span class="status-dot green"></span>可用空间 {{ formatBytes(disk.freeBytes) }}</span></div>
      <div class="disk-card"><div class="disk-card-top"><div><span class="disk-location-label">应用数据目录</span><code>{{ store.data.dataDir || '等待本地服务报告数据目录' }}</code></div><div class="data-dir-icon"><HardDrive :size="18" /></div></div><div class="disk-divider"></div><div class="disk-usage-grid"><div v-for="bar in diskBars" :key="bar.label" class="disk-usage-item"><div class="disk-usage-head"><span><component :is="bar.icon" :size="15" />{{ bar.label }}</span><b>{{ formatBytes(bar.used) }} <small>/ {{ formatBytes(bar.limit) }}</small></b></div><div class="meter-track"><div class="meter-fill" :class="bar.tone" :style="{ width: `${percent(bar.used, bar.limit)}%` }"></div></div><span class="meter-caption">{{ bar.limit ? `${percent(bar.used, bar.limit).toFixed(0)}% 已使用` : '容量限制由本地服务提供' }}</span></div></div></div>
      <div class="settings-footnote"><Check :size="14" />未决操作引用的内容会保留，不会被当作缓存自动清除。</div>
    </section>
    <section class="settings-section preference-section">
      <div class="section-heading"><div><h2><Settings2 :size="17"/>传输与本机保护</h2><p>设置会应用到后续传输；额度不能低于现有受保护数据。</p></div><button class="button primary small" :disabled="preferencesSaving || preferencesLoading || !!preferencesError" @click="savePreferences"><RefreshCw v-if="preferencesSaving" :size="14" class="spin"/><Check v-else :size="14"/>{{preferencesSaving?'保存中…':'保存设置'}}</button></div>
      <div v-if="awaitingNotificationAuthorization" class="settings-inline-state preference-auth-wait" role="status" aria-live="polite"><RefreshCw :size="15" class="spin"/>正在等待操作系统通知授权。请完成系统提示；授权请求最长等待 15 秒。</div>
      <div v-if="preferencesSaveError" class="settings-inline-error preference-save-error" role="alert"><AlertTriangle :size="15"/><span>{{preferencesSaveError}}</span></div>
      <div v-if="preferencesLoading" class="settings-inline-state">正在读取设置…</div>
      <div v-else-if="preferencesError" class="settings-inline-error">{{preferencesError}} <button class="button secondary small" @click="loadPreferences">重试</button></div>
      <template v-else>
        <div class="preference-grid">
          <label class="field"><span>总带宽上限（MiB/s，0 为不限）</span><input v-model.number="bandwidthMiB" type="number" min="0" max="1048576" step="0.5"/><small class="field-hint">当前 {{formatBytes(preferences.bandwidthBytes)}}/s</small></label>
          <label class="field"><span>最大并发读取</span><input v-model.number="preferences.maxReaders" type="number" min="1" max="16"/><small class="field-hint">范围 1–16</small></label>
          <label class="field"><span>传输暂存额度（MiB）</span><input v-model.number="stagingMiB" type="number" min="16" max="32768" step="16"/><small class="field-hint">当前 {{formatBytes(preferences.stagingBytes)}}</small></label>
          <label class="field"><span>恢复副本额度（MiB）</span><input v-model.number="recoveryMiB" type="number" min="16" max="32768" step="16"/><small class="field-hint">当前 {{formatBytes(preferences.recoveryBytes)}}</small></label>
          <label class="field"><span>单文件传输上限（MiB）</span><input v-model.number="maxFileMiB" type="number" min="1" max="32768" step="1"/><small class="field-hint">不能大于传输暂存额度</small></label>
          <label class="field"><span>本机缓存额度（MiB）</span><input v-model.number="cacheMiB" type="number" min="16" max="32768" step="16"/><small class="field-hint">当前 {{formatBytes(preferences.cacheBytes)}}</small></label><label class="field"><span>恢复副本保留天数</span><input v-model.number="preferences.recoveryDays" type="number" min="1" max="3650"/></label>
        </div>
        <div class="preference-toggles"><label class="check-row"><input v-model="preferences.notifications" type="checkbox" :disabled="preferencesSaving"/><span>启用系统通知</span><small>首次启用时会请求操作系统授权。</small></label><label class="check-row"><input v-model="preferences.autoStart" type="checkbox" :disabled="preferencesSaving"/><span>登录时启动 tamiops</span><small>保存时会更新本机启动配置。</small></label></div>
      </template>
    </section>
    <section class="settings-section automation-rules-section">
      <div class="section-heading"><div><h2><Clock3 :size="17"/>自动执行条件</h2><p>仅限制同步和备份的计划任务；手动立即同步、手动备份仍可运行。</p></div></div>
      <div v-if="preferencesLoading" class="settings-inline-state">正在读取自动执行规则…</div>
      <div v-else-if="preferencesError" class="settings-inline-error">{{preferencesError}} <button class="button secondary small" @click="loadPreferences">重试</button></div>
      <div v-else class="automation-rule-card">
        <label class="check-row automation-enable"><input v-model="preferences.rules.enabled" type="checkbox"/><span>启用自动执行限制</span><small>关闭时，下面的时间、日期、网卡和电源条件均不限制计划任务。</small></label>
        <div class="automation-rule-fields" :class="{disabled:!preferences.rules.enabled}">
          <div class="automation-time-fields"><label class="field"><span>允许开始时间</span><input v-model="preferences.rules.start" type="time" :disabled="!preferences.rules.enabled"/></label><label class="field"><span>允许结束时间</span><input v-model="preferences.rules.end" type="time" :disabled="!preferences.rules.enabled"/></label></div><small class="automation-hint">开始和结束都留空表示全天；设置时段时必须同时填写。</small>
          <fieldset class="weekday-field" :disabled="!preferences.rules.enabled"><legend>允许的星期 <small>不选择表示每天</small></legend><label v-for="day in weekdays" :key="day.value" class="weekday-chip" :class="{selected:preferences.rules.weekdays.includes(day.value)}"><input type="checkbox" :value="day.value" v-model="preferences.rules.weekdays"/><span>{{day.label}}</span></label></fieldset>
          <div class="field network-field"><span>允许的活动网卡 <small>不选择表示不限</small></span><div v-if="environmentLoading" class="automation-environment-state">正在读取活动网卡与电源状态…</div><div v-else-if="environmentError" class="automation-environment-error"><span>{{environmentError}}</span><button type="button" class="button secondary small" @click="loadAutomationEnvironment">重试</button></div><div v-else-if="availableNetworks.length" class="automation-network-list"><label v-for="name in availableNetworks" :key="name" class="network-choice"><input v-model="preferences.rules.networks" type="checkbox" :value="name" :disabled="!preferences.rules.enabled"/><span>{{name}}</span><small v-if="environment?.networkInterfaces?.includes(name)">当前活动</small><small v-else>目前未检测到</small></label></div><div v-else class="automation-environment-state">没有活动的非回环网卡。留空时不限制网络；若选择网卡，自动任务会等到该网卡活动。</div></div>
          <label class="check-row"><input v-model="preferences.rules.onlyOnAC" type="checkbox" :disabled="!preferences.rules.enabled"/><span>仅接通电源时运行自动任务</span><small>电源状态：{{environmentLoading?'读取中…':environmentError?'未知':!environment?.powerKnown?'未知':environment.onAC?'已接通':'未接通'}}。启用后，状态未知或未接电源时计划任务会暂停。</small></label>
        </div>
        <p class="automation-rule-note">规则与传输设置一并保存。自动执行条件不会取消或中断已开始的手动操作。</p><footer class="modal-actions"><button class="button primary small" :disabled="preferencesSaving || preferencesLoading || !!preferencesError" @click="savePreferences"><RefreshCw v-if="preferencesSaving" :size="14" class="spin"/><Check v-else :size="14"/>{{preferencesSaving?'保存中…':'保存自动执行与传输设置'}}</button></footer>
      </div>
    </section>
    <section class="settings-section configuration-section">
      <div class="section-heading"><div><h2><ShieldCheck :size="17"/>配置迁移</h2><p>导出文件不含系统凭据库中的密码或密钥；导入连接后需要逐个补充凭据并测试。</p></div></div>
      <div class="config-actions"><button class="button secondary" @click="exportConfiguration"><Upload :size="15"/>导出配置</button><button class="button secondary" @click="chooseImport"><Database :size="15"/>选择配置文件导入</button><input ref="configFile" type="file" accept="application/json,.json" hidden @change="readImport"/><span class="field-hint">导入会新增连接、暂停的任务和停止状态的网关，不会覆盖现有项目。</span></div>
    </section>
    <section class="settings-section telemetry-section">
      <div class="section-heading"><div><h2><Activity :size="17"/>传输统计</h2><p>统计应用记录的传输调用和网关请求，包含读取核验流量；不代表协议底层全部 HTTP 请求或服务商账单。</p></div><button class="icon-button" :disabled="statisticsLoading" title="刷新统计" @click="loadStatistics"><RefreshCw :size="15" :class="{spin:statisticsLoading}"/></button></div>
      <div v-if="statisticsLoading" class="settings-inline-state">正在读取统计…</div><div v-else-if="statisticsError" class="settings-inline-error">{{statisticsError}} <button class="button secondary small" @click="loadStatistics">重试</button></div><div v-else-if="statistics.length" class="statistics-table"><div class="statistics-head"><span>范围</span><span>对象</span><span>请求</span><span>错误</span><span>上传</span><span>下载</span><span>最近访问</span></div><div v-for="item in statistics" :key="item.scope+item.id" class="statistics-row"><span>{{scopeLabel(item.scope)}}</span><b :title="item.id">{{statName(item)}}</b><span>{{item.requests}}</span><span>{{item.errors}}</span><span>{{formatBytes(item.uploaded)}}</span><span>{{formatBytes(item.downloaded)}}</span><span>{{item.lastAccess||'—'}}</span></div></div><div v-else class="settings-inline-state">暂无统计记录。</div>
    </section>
    <section class="settings-section diagnostics-section">
      <div class="section-heading"><div><h2><AlertTriangle :size="17"/>诊断信息</h2><p>只包含应用版本、平台、连接类型与功能状态，不包含秘密凭据。</p></div><button class="button secondary small" :disabled="diagnosticsLoading" @click="exportJSON('diagnostics')"><Upload :size="14"/>导出诊断</button><button class="icon-button" :disabled="diagnosticsLoading" title="刷新诊断" @click="loadDiagnostics"><RefreshCw :size="15" :class="{spin:diagnosticsLoading}"/></button></div>
      <pre v-if="diagnostics && !diagnosticsLoading" class="diagnostics-json">{{JSON.stringify(diagnostics,null,2)}}</pre><div v-else-if="diagnosticsError" class="settings-inline-error">{{diagnosticsError}} <button class="button secondary small" @click="loadDiagnostics">重试</button></div><div v-else class="settings-inline-state">{{diagnosticsLoading?'正在读取诊断信息…':'尚无诊断信息'}}</div>
    </section>
    <section class="settings-section about-section"><div class="section-heading"><div><h2><KeyRound :size="17" />关于 tamiops</h2><p>私有存储工作台</p></div></div><div class="about-card"><span class="about-logo"><SquirrelMark variant="logo" :size="24" /></span><div><b>tamiops</b><small>本机桌面版 · {{ store.data.version || '开发预览' }}</small></div><span class="about-private"><ShieldCheck :size="14" />配置保存在本机</span></div></section>
    <EditConnectionModal v-model="editOpen" :connection="editTarget" />
    <BaseModal :model-value="!!credentialTarget" :dismissible="!credentialSaving" title="补充连接凭据" :subtitle="credentialTarget?.name" @update:model-value="v=>{if(!v)credentialTarget=null}"><form class="form-stack" @submit.prevent="saveCredentials"><template v-if="credentialTarget?.kind==='s3'"><label class="field"><span>访问密钥 ID</span><input v-model="credentialForm.accessKey" type="password" autocomplete="off" required/></label><label class="field"><span>秘密访问密钥</span><input v-model="credentialForm.secretKey" type="password" autocomplete="new-password" required/></label><label class="field"><span>会话令牌（可选）</span><input v-model="credentialForm.sessionToken" type="password" autocomplete="off"/></label></template><template v-else><label class="field"><span>用户名</span><input v-model="credentialForm.username" autocomplete="username" required/></label><label class="field"><span>密码</span><input v-model="credentialForm.password" type="password" autocomplete="new-password" required/></label></template><p class="field-hint">提交前会用新凭据读取远端列表；验证失败时不会替换已保存凭据。</p><footer class="modal-actions"><button class="button secondary" type="button" :disabled="credentialSaving" @click="credentialTarget=null">取消</button><button class="button primary" type="submit" :disabled="credentialSaving"><RefreshCw v-if="credentialSaving" :size="14" class="spin"/>验证并保存</button></footer></form></BaseModal>
    <BaseModal :model-value="!!importPreview" :dismissible="!importBusy" title="确认导入配置" subtitle="配置将追加到当前工作区。" @update:model-value="v=>{if(!v)importPreview=null}"><div v-if="importPreview" class="confirm-panel"><div class="confirm-warning"><ShieldCheck :size="18"/></div><p>即将新增 {{importPreview.connections.length}} 个连接、{{importPreview.jobs.length}} 个暂停同步、{{importPreview.backupJobs?.length||0}} 个备份、{{importPreview.migrationJobs?.length||0}} 个迁移及 {{importPreview.gateways.length}} 个停止网关。导入任务会要求重新选择本机目录，并应用导入文件中的传输额度与自动执行规则。</p><p>配置文件版本：{{importPreview.version}}；任何凭据都需要通过“补充凭据”单独重新输入。</p><footer class="modal-actions"><button class="button secondary" :disabled="importBusy" @click="importPreview=null">取消</button><button class="button primary" :disabled="importBusy" @click="importConfiguration">{{importBusy?'正在导入…':'确认导入'}}</button></footer></div></BaseModal>
    <BaseModal :model-value="!!deleteTarget" :dismissible="!deleting" title="删除存储连接" subtitle="请先移除依赖任务、网关及本机缓存，处理未决操作。" @update:model-value="(v) => { if (!v) deleteTarget = null }"><div class="confirm-panel"><div class="confirm-warning"><Trash2 :size="18" /></div><p>确定删除“{{ deleteTarget?.name }}”吗？远端文件不会被删除。</p><footer class="modal-actions"><button class="button secondary" :disabled="deleting" @click="deleteTarget = null">取消</button><button class="button danger" :disabled="deleting" @click="removeConnection">{{ deleting ? '正在删除…' : '删除连接' }}</button></footer></div></BaseModal>
  </div>
</template>
<style scoped>
.preference-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:13px;padding:15px;border:1px solid #eae6f2;border-radius:13px;background:#fff}.preference-toggles{display:grid;gap:10px;margin-top:12px;padding:14px;border:1px solid #eae6f2;border-radius:13px;background:#fff}.preference-toggles .check-row{display:flex;flex-wrap:wrap}.preference-toggles .check-row small{flex-basis:100%;margin-left:24px;color:#898598}.automation-rule-card{display:grid;gap:12px;padding:14px;border:1px solid #eae6f2;border-radius:13px;background:#fff}.automation-enable{padding-bottom:10px;border-bottom:1px solid #f0edf5}.automation-enable small,.automation-rule-fields .check-row small{flex-basis:100%;margin-left:24px;color:#898598}.automation-rule-fields{display:grid;gap:12px}.automation-time-fields{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px}.automation-rule-fields.disabled{opacity:.55}.automation-hint{margin-top:-8px;color:#898598;font-size:11px}.weekday-field{display:flex;align-items:center;gap:7px;min-width:0;margin:0;padding:0;border:0}.weekday-field legend{float:left;margin-right:8px;color:#5e586f;font-size:12px}.weekday-field legend small,.automation-time-fields .field small{color:#898598;font-weight:400}.weekday-chip{display:grid;place-items:center;width:31px;height:31px;border:1px solid #e6e1ee;border-radius:9px;color:#756f83;cursor:pointer}.weekday-chip.selected{border-color:#baa9e6;background:#f5f1ff;color:#65519d}.weekday-chip input{position:absolute;opacity:0;pointer-events:none}.network-field{display:grid;gap:7px}.network-field>span{color:#5e586f;font-size:12px}.network-field>span small{color:#898598;font-weight:400}.automation-network-list{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:6px}.network-choice{display:flex;align-items:center;gap:8px;min-width:0;padding:8px 10px;border:1px solid #eeeaf5;border-radius:9px;background:#fcfbfe;color:#625b73;font-size:11px}.network-choice span{overflow-wrap:anywhere;flex:1}.network-choice small{color:#898598;font-size:10px}.automation-environment-state,.automation-environment-error{display:flex;align-items:center;justify-content:space-between;gap:10px;padding:10px;border:1px dashed #ddd7ea;border-radius:9px;color:#898598;font-size:11px}.automation-environment-error{color:#a34f45}.automation-rule-note{margin:0;color:#898598;font-size:11px}.config-actions{display:flex;align-items:center;gap:9px;flex-wrap:wrap;padding:15px;border:1px solid #eae6f2;border-radius:13px;background:#fff}.config-actions .field-hint{flex:1;min-width:220px}.statistics-table{display:grid;overflow:auto;border:1px solid #eae6f2;border-radius:12px;background:#fff}.statistics-head,.statistics-row{display:grid;grid-template-columns:90px minmax(130px,1.4fr) 65px 65px 90px 90px minmax(130px,1fr);align-items:center;gap:8px;min-width:740px;padding:10px 12px;border-bottom:1px solid #f0edf5;font-size:11px}.statistics-head{background:#faf9fc;color:#858194}.statistics-row{color:#716d82}.statistics-row:last-child{border-bottom:0}.statistics-row b{overflow:hidden;text-overflow:ellipsis;color:#4b445f;font-weight:600;white-space:nowrap}.diagnostics-json{max-height:420px;overflow:auto;margin:0;padding:14px;border:1px solid #eae6f2;border-radius:12px;background:#faf9fc;color:#5d5571;font:11px/1.55 ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;overflow-wrap:anywhere}.settings-inline-state,.settings-inline-error{display:flex;align-items:center;justify-content:center;gap:8px;min-height:65px;padding:12px;border:1px dashed #ddd7ea;border-radius:12px;color:#7a7689;font-size:12px}.settings-inline-error{justify-content:space-between;color:#a34f45}@media(max-width:740px){.preference-grid,.automation-time-fields{grid-template-columns:1fr}.config-actions{align-items:stretch}.config-actions .button{justify-content:center}.weekday-field{flex-wrap:wrap}.weekday-field legend{width:100%}}
.preference-auth-wait{justify-content:flex-start;min-height:0;margin-top:10px;border-style:solid;border-color:#dcd5ee;background:#f8f6fd;color:#665887}.preference-save-error{justify-content:flex-start;min-height:0;margin-top:10px;border-style:solid;align-items:flex-start}.preference-save-error span{overflow-wrap:anywhere}
.weekday-chip:focus-within{outline:2px solid #7662b2;outline-offset:2px}
</style>
