<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Activity, AlertCircle, Check, ChevronRight, CircleHelp, Cloud, Database, Files, LayoutDashboard, LoaderCircle, Plus, RefreshCw, Settings2, ShieldCheck, Sparkles, X, Archive } from 'lucide-vue-next'
import { api } from './api'
import AddConnectionModal from './components/AddConnectionModal.vue'
import ConnectionPicker from './components/ConnectionPicker.vue'
import SquirrelMark from './components/SquirrelMark.vue'
import ActivityPage from './pages/ActivityPage.vue'
import BackupsMigrationsPage from './pages/BackupsMigrationsPage.vue'
import FilesPage from './pages/FilesPage.vue'
import GatewaysPage from './pages/GatewaysPage.vue'
import SettingsPage from './pages/SettingsPage.vue'
import TasksPage from './pages/TasksPage.vue'
import { notify, refreshState, setSelectedConnection, store } from './store'
const currentPage = ref('tasks')
const showAddConnection = ref(false)
const ticking = ref(false)
const usesMacTitlebar = window.location.protocol === 'wails:' && navigator.platform.startsWith('Mac')
const navItems = [
  { id: 'tasks', label: '任务', icon: LayoutDashboard, hint: '同步任务' },
  { id: 'files', label: '文件', icon: Files, hint: '远端浏览' },
  { id: 'gateways', label: '网关', icon: Cloud, hint: 'WebDAV 入口' },
  { id: 'activity', label: '活动', icon: Activity, hint: '操作记录' },
  { id: 'backups', label: '备份迁移', icon: Archive, hint: '保护与迁移' },
  { id: 'settings', label: '设置', icon: Settings2, hint: '连接与空间' },
]
const pageTitle = computed(() => navItems.find((item) => item.id === currentPage.value)?.label || '任务')
const attentionCount = computed(() => store.data.jobs.filter((job) => ['failed', 'error', 'attention', 'needs_attention'].includes(job.status)).length)
let timer: number | undefined
async function makeDemo() {
  try {
    const result = await api.post<{ id: string }>('/api/demo')
    await refreshState(true)
    setSelectedConnection(result.id)
    currentPage.value = 'tasks'
    notify('隔离演示空间已建立，可以放心体验文件操作。', 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '建立演示空间失败') }
}
async function doRefresh() {
  if (ticking.value) return
  ticking.value = true
  await refreshState()
  ticking.value = false
}
function syncPolling() {
  if (timer) window.clearInterval(timer)
  timer = undefined
  if (document.visibilityState === 'visible') timer = window.setInterval(() => { void refreshState(true) }, 3000)
}
function onVisibility() { syncPolling() }
function closeToast() { store.toast.message = '' }
onMounted(() => {
  void refreshState()
  syncPolling()
  document.addEventListener('visibilitychange', onVisibility)
})
onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
  document.removeEventListener('visibilitychange', onVisibility)
})
</script>
<template>
  <div class="app-shell" :class="{ 'mac-titlebar': usesMacTitlebar }">
    <aside class="sidebar">
      <div class="brand-lockup"><span class="brand-icon"><SquirrelMark variant="logo" :size="30" label="tamiops" /></span><div><strong aria-hidden="true">tamiops</strong><span>私有存储工作台</span></div></div>
      <div class="side-section-label">工作空间</div>
      <nav class="main-nav" aria-label="主导航">
        <button v-for="item in navItems" :key="item.id" class="nav-item" :class="{ active: currentPage === item.id }" :title="item.label" :aria-label="item.label" :aria-current="currentPage === item.id ? 'page' : undefined" @click="currentPage = item.id"><span class="nav-icon"><component :is="item.icon" :size="18" :stroke-width="currentPage === item.id ? 2.2 : 1.8" /></span><span class="nav-label">{{ item.label }}</span><span v-if="item.id === 'tasks' && attentionCount" class="nav-count">{{ attentionCount }}</span><ChevronRight v-if="currentPage === item.id" :size="14" class="nav-current-chevron" /></button>
      </nav>
      <div class="sidebar-spacer"></div>
      <div class="sidebar-card">
        <div class="sidebar-card-icon"><ShieldCheck :size="17" /></div><div><b>连接由你掌控</b><span>凭据由系统保管</span></div><Check :size="14" class="sidebar-card-check" />
      </div>
      <div class="sidebar-footer"><div class="local-avatar">T</div><div class="local-user"><b>本机工作区</b><span>个人存储空间</span></div><button class="icon-button sidebar-help" title="关于私有存储" @click="currentPage = 'settings'"><CircleHelp :size="16" /></button></div>
    </aside>
    <main class="main-area">
      <header class="topbar"><div class="topbar-location"><SquirrelMark v-if="usesMacTitlebar" variant="logo" :size="28" label="tamiops" /><span>工作空间</span><ChevronRight :size="14" /><b>{{ pageTitle }}</b></div><div class="topbar-actions"><ConnectionPicker @add="showAddConnection = true" @demo="makeDemo" /><button class="icon-button top-refresh" :disabled="ticking" title="刷新状态" @click="doRefresh"><RefreshCw :size="16" :class="{ spin: ticking }" /></button><div class="top-divider"></div><button class="top-profile" title="本机设置" aria-label="本机设置" @click="currentPage = 'settings'"><Settings2 :size="16" /></button></div></header>
      <div v-if="store.error" class="service-banner"><span class="service-banner-icon"><AlertCircle :size="16" /></span><span><b>本地服务暂不可用</b> {{ store.error }}</span><button @click="doRefresh">重试 <RefreshCw :size="13" /></button></div>
      <div class="page-scroll"><TasksPage v-if="currentPage === 'tasks'" @demo="makeDemo" @add-connection="showAddConnection = true" @files="currentPage = 'files'" /><FilesPage v-else-if="currentPage === 'files'" @settings="currentPage = 'settings'" /><GatewaysPage v-else-if="currentPage === 'gateways'" @add-connection="showAddConnection = true" /><ActivityPage v-else-if="currentPage === 'activity'" /><BackupsMigrationsPage v-else-if="currentPage === 'backups'" /><SettingsPage v-else @add-connection="showAddConnection = true" /></div>
      <footer class="app-statusbar"><span class="statusbar-left"><span class="status-dot" :class="store.error ? 'red' : 'green'"></span>{{ store.error ? '本地服务离线' : store.loading ? '正在连接本地服务' : '本机服务已连接' }}<span class="statusbar-separator">·</span>{{ store.data.version ? `版本 ${store.data.version}` : '开发预览' }}</span><span class="statusbar-right"><Sparkles :size="13" />你的存储，由你管理</span></footer>
    </main>
    <AddConnectionModal v-model="showAddConnection" @created="setSelectedConnection" />
  </div>
  <Teleport to="body">
    <div id="app-toast-region" data-toast-region>
      <Transition name="toast"><div v-if="store.toast.message" class="toast-notice" :class="store.toast.tone" role="status" aria-live="polite"><span class="toast-mark"><Check v-if="store.toast.tone === 'success'" :size="16" /><AlertCircle v-else :size="16" /></span><span>{{ store.toast.message }}</span><button class="icon-button" aria-label="关闭通知" @click="closeToast"><X :size="15" /></button></div></Transition>
    </div>
  </Teleport>
</template>
