<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Activity, AlertCircle, ArrowLeft, Check, ChevronRight, CircleHelp, Files, LayoutDashboard, RefreshCw, Settings2, Wrench, X } from 'lucide-vue-next'
import { api } from './api'
import AddConnectionModal from './components/AddConnectionModal.vue'
import ConnectionPicker from './components/ConnectionPicker.vue'
import AppHelp from './components/AppHelp.vue'
import SquirrelMark from './components/SquirrelMark.vue'
import ActivityPage from './pages/ActivityPage.vue'
import BackupsMigrationsPage from './pages/BackupsMigrationsPage.vue'
import FilesPage from './pages/FilesPage.vue'
import GatewaysPage from './pages/GatewaysPage.vue'
import SettingsPage from './pages/SettingsPage.vue'
import TasksPage from './pages/TasksPage.vue'
import ToolsPage from './pages/ToolsPage.vue'
import { notify, refreshState, setSelectedConnection, store } from './store'
const currentPage = ref('tasks')
const activityInitialTab = ref<'activity' | 'operations'>('activity')
const showAddConnection = ref(false)
const showHelp = ref(false)
const usesMacTitlebar = window.location.protocol === 'wails:' && navigator.platform.startsWith('Mac')
const navItems = [
  { id: 'tasks', label: '同步', icon: LayoutDashboard, hint: '同步任务' },
  { id: 'files', label: '文件', icon: Files, hint: '远端浏览' },
  { id: 'activity', label: '活动', icon: Activity, hint: '操作记录' },
]
const secondaryNavItems = [
  { id: 'tools', label: '工具', icon: Wrench },
  { id: 'settings', label: '设置', icon: Settings2, hint: '连接与空间' },
]
const toolPage = computed(() => ['gateways', 'backups'].includes(currentPage.value))
const pageTitle = computed(() => toolPage.value ? '工具' : [...navItems, ...secondaryNavItems].find((item) => item.id === currentPage.value)?.label || '同步')
const attentionCount = computed(() => store.data.jobs.filter((job) => ['failed', 'error', 'attention', 'needs_attention'].includes(job.status)).length)
let timer: number | undefined
async function makeDemo() {
  try {
    const result = await api.post<{ id: string }>('/api/demo')
    await refreshState(true)
    setSelectedConnection(result.id)
    currentPage.value = 'tasks'
    notify('演示空间已建立，内容在退出后清空。', 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '建立演示空间失败') }
}
function syncPolling() {
  if (timer) window.clearInterval(timer)
  timer = undefined
  if (document.visibilityState === 'visible') timer = window.setInterval(() => { void refreshState(true) }, 3000)
}
function onVisibility() { syncPolling() }
function closeToast() { store.toast.message = '' }
function navigateTo(page: string) {
  if (page === 'activity') activityInitialTab.value = 'activity'
  currentPage.value = page
}
function openPendingOperations() {
  activityInitialTab.value = 'operations'
  currentPage.value = 'activity'
}
onMounted(() => {
  void refreshState(true)
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
      <div class="brand-lockup"><span class="brand-icon"><SquirrelMark variant="logo" :size="30" label="小花鼠 Tamias" /></span><div><strong aria-hidden="true">小花鼠</strong></div></div>
      <nav class="main-nav" aria-label="主导航">
        <button v-for="item in navItems" :key="item.id" class="nav-item" :class="{ active: currentPage === item.id }" :title="item.label" :aria-label="item.label" :aria-current="currentPage === item.id ? 'page' : undefined" @click="navigateTo(item.id)"><span class="nav-icon"><component :is="item.icon" :size="18" :stroke-width="currentPage === item.id ? 2.2 : 1.8" /></span><span class="nav-label">{{ item.label }}</span><span v-if="item.id === 'tasks' && attentionCount" class="nav-count">{{ attentionCount }}</span><ChevronRight v-if="currentPage === item.id" :size="14" class="nav-current-chevron" /></button>
      </nav>
      <div class="sidebar-spacer"></div>
      <nav class="secondary-nav" aria-label="工具与设置">
        <button v-for="item in secondaryNavItems" :key="item.id" class="nav-item" :class="{ active: currentPage === item.id || item.id === 'tools' && toolPage }" :aria-label="item.label" :aria-current="currentPage === item.id || item.id === 'tools' && toolPage ? 'page' : undefined" @click="navigateTo(item.id)"><span class="nav-icon"><component :is="item.icon" :size="18" /></span><span class="nav-label">{{ item.label }}</span></button>
      </nav>
      <div class="sidebar-footer"><button class="icon-button sidebar-help" title="帮助与关于" aria-label="打开帮助与关于" @click="showHelp = true"><CircleHelp :size="16" /></button></div>
    </aside>
    <main class="main-area">
      <header class="topbar"><div class="topbar-location"><SquirrelMark v-if="usesMacTitlebar" variant="logo" :size="28" label="小花鼠 Tamias" /><span>工作空间</span><ChevronRight :size="14" /><b>{{ pageTitle }}</b></div><div class="topbar-actions"><ConnectionPicker @add="showAddConnection = true" @demo="makeDemo" /></div></header>
      <div v-if="store.error" class="service-banner" role="alert" aria-live="assertive">
        <span class="service-banner-icon"><AlertCircle :size="16" /></span>
        <span class="service-banner-copy"><b>本机服务暂不可用</b><small>{{ store.error }}</small></span>
        <button type="button" :disabled="store.refreshing" title="重试连接本机服务" aria-label="重试连接本机服务" @click="refreshState(true)">重试 <RefreshCw :size="13" :class="{ spin: store.refreshing }" /></button>
      </div>
      <div class="page-scroll">
        <div v-if="toolPage" class="tool-back"><button class="text-button" @click="navigateTo('tools')"><ArrowLeft :size="14" />所有工具</button></div>
        <TasksPage v-if="currentPage === 'tasks'" @demo="makeDemo" @add-connection="showAddConnection = true" @files="currentPage = 'files'" @operations="openPendingOperations" />
        <FilesPage v-else-if="currentPage === 'files'" @settings="currentPage = 'settings'" />
        <ToolsPage v-else-if="currentPage === 'tools'" @navigate="navigateTo" />
        <GatewaysPage v-else-if="currentPage === 'gateways'" @add-connection="showAddConnection = true" />
        <ActivityPage v-else-if="currentPage === 'activity'" :initial-tab="activityInitialTab" />
        <BackupsMigrationsPage v-else-if="currentPage === 'backups'" />
        <SettingsPage v-else @add-connection="showAddConnection = true" />
      </div>
    </main>
    <AddConnectionModal v-model="showAddConnection" @created="setSelectedConnection" />
    <AppHelp v-model="showHelp" :version="store.data.version || '开发预览'" @navigate="(page) => { navigateTo(page); showHelp = false }" />
  </div>
  <Teleport to="body">
    <div id="app-toast-region" data-toast-region>
      <Transition name="toast"><div v-if="store.toast.message" class="toast-notice" :class="store.toast.tone" role="status" aria-live="polite"><span class="toast-mark"><Check v-if="store.toast.tone === 'success'" :size="16" /><AlertCircle v-else :size="16" /></span><span>{{ store.toast.message }}</span><button class="icon-button" aria-label="关闭通知" @click="closeToast"><X :size="15" /></button></div></Transition>
    </div>
  </Teleport>
</template>
