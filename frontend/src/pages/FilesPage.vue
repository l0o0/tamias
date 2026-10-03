<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { ArrowDownToLine, ChevronRight, File, Folder, FolderPlus, RefreshCw, Search, ShieldCheck, Trash2, Upload, X, Copy, Scissors, Archive, Clock3, Pin, PinOff, ExternalLink, AlertTriangle, RotateCcw } from 'lucide-vue-next'
import { api } from '../api'
import BaseModal from '../components/BaseModal.vue'
import SquirrelMark from '../components/SquirrelMark.vue'
import { notify, store } from '../store'
import type { CacheEntry, DeleteTreePreview, FileEntry, ObjectVersion } from '../types'
const emit = defineEmits<{ settings: [] }>()
const path = ref('')
const entries = ref<FileEntry[]>([])
const loading = ref(false)
const filesError = ref('')
let filesRequest = 0
const uploading = ref(false)
const search = ref('')
const uploadInput = ref<HTMLInputElement>()
const showMkdir = ref(false)
const folderName = ref('')
const creatingFolder = ref(false)
const deleteTarget = ref<FileEntry | null>(null)
const deleting = ref(false)
const tab = ref<'files' | 'cache'>('files')
const showTransfer = ref(false)
const transferBusy = ref(false)
const transferSource = ref<FileEntry | null>(null)
const transferKind = ref<'copy' | 'move'>('copy')
const transferDestination = ref('')
const overwrite = ref(false)
const showTreePreview = ref(false)
const treePreview = ref<DeleteTreePreview | null>(null)
const treeBusy = ref(false)
const cacheEntries = ref<CacheEntry[]>([])
const cacheLoading = ref(false)
const cacheError = ref('')
const cacheBusy = ref('')
const cacheTarget = ref<CacheEntry | null>(null)
const cacheAction = ref<'upload' | 'delete' | 'refresh' | ''>('')
const versionsOpen = ref(false)
const versionsLoading = ref(false)
const versionsError = ref('')
const versions = ref<ObjectVersion[]>([])
const versionTarget = ref<FileEntry | null>(null)
const versionBusy = ref('')
const versionRestorePreview = ref<{ token: string; connectionId: string; path: string; versionId: string; version: FileEntry; current?: FileEntry; currentExists: boolean; expectedCurrentEtag: string } | null>(null)
const versionRestoreError = ref('')
const currentConnection = computed(() => store.data.connections.find((c) => c.id === store.selectedConnectionId))
const currentCacheEntries = computed(() => cacheEntries.value.filter((entry) => entry.connectionId === store.selectedConnectionId))
const canWrite = computed(() => currentConnection.value?.kind === 'demo' || !!(currentConnection.value?.tested && currentConnection.value?.capabilities?.conditionalWrite))
const canDelete = computed(() => currentConnection.value?.kind === 'demo' || !!(currentConnection.value?.tested && currentConnection.value?.capabilities?.conditionalDelete))
const canMove = computed(() => currentConnection.value?.kind === 'demo' || !!(currentConnection.value?.tested && currentConnection.value?.capabilities?.conditionalWrite && currentConnection.value?.capabilities?.conditionalDelete))
const uploadLimit = computed(() => store.data.preferences?.maxFileBytes || 128 * 1024 * 1024)
const filtered = computed(() => entries.value.filter((entry) => entry.name.toLocaleLowerCase().includes(search.value.toLocaleLowerCase())))
const crumbs = computed(() => path.value ? path.value.split('/').filter(Boolean) : [])
const folderPath = computed(() => path.value ? `${path.value.replace(/\/$/, '')}/` : '')
type ListingContext = { connectionId: string; folderPath: string; connectionName: string }
type EntryContext = ListingContext & { targetPath: string }
const mkdirContext = ref<ListingContext | null>(null)
const deleteContext = ref<EntryContext | null>(null)
const transferContext = ref<(EntryContext & { sourcePath: string }) | null>(null)
const treeContext = ref<EntryContext | null>(null)
const versionContext = ref<EntryContext | null>(null)
const breadcrumbStrip = ref<HTMLDivElement>()
let cacheRequest = 0
let treeRequest = 0
let versionsRequest = 0
let disposed = false

function currentListingContext(): ListingContext | null {
  const connectionId = store.selectedConnectionId
  if (!connectionId) return null
  return { connectionId, folderPath: path.value, connectionName: currentConnection.value?.name || '存储连接' }
}
function entryTarget(context: ListingContext, entry: FileEntry): EntryContext {
  return { ...context, targetPath: entry.path || [context.folderPath, entry.name].filter(Boolean).join('/') }
}
function sameListing(context: ListingContext) {
  return !disposed && context.connectionId === store.selectedConnectionId && context.folderPath === path.value
}
function listingLabel(context: ListingContext) {
  return `${context.connectionName} / ${context.folderPath || '根目录'}`
}
function clearTreePreview() {
  treeRequest++
  treeContext.value = null
  treePreview.value = null
  showTreePreview.value = false
  treeBusy.value = false
}
function closeVersions() {
  versionsRequest++
  versionsOpen.value = false
  versionsLoading.value = false
  versionTarget.value = null
  versionContext.value = null
  versionRestorePreview.value = null
  versionRestoreError.value = ''
}
watch(() => store.selectedConnectionId, () => {
  path.value = ''; search.value = ''; tab.value = 'files'; entries.value = []; filesError.value = ''
  cacheEntries.value = []; cacheError.value = ''; cacheLoading.value = false; cacheRequest++
  showMkdir.value = false; mkdirContext.value = null; folderName.value = ''
  deleteTarget.value = null; deleteContext.value = null
  showTransfer.value = false; transferSource.value = null; transferContext.value = null
  clearTreePreview()
  closeVersions()
  if (cacheTarget.value?.connectionId !== store.selectedConnectionId) { cacheTarget.value = null; cacheAction.value = '' }
  void loadFiles()
}, { immediate: true })
watch([() => store.selectedConnectionId, path], async () => {
  await nextTick()
  const strip = breadcrumbStrip.value
  if (strip) strip.scrollLeft = strip.scrollWidth
}, { flush: 'post' })
onBeforeUnmount(() => {
  disposed = true
  filesRequest++
  cacheRequest++
  treeRequest++
  versionsRequest++
})
async function loadFiles() {
  const request = ++filesRequest
  if (disposed) return
  const connectionId = store.selectedConnectionId
  if (!connectionId) { entries.value = []; filesError.value = ''; loading.value = false; return }
  loading.value = true; filesError.value = ''
  try {
    const requestedPath = path.value
    const params = new URLSearchParams({ connectionId, path: requestedPath })
    const result = await api.get<{ entries: FileEntry[] }>(`/api/files?${params.toString()}`)
    if (request === filesRequest && connectionId === store.selectedConnectionId && requestedPath === path.value) entries.value = result.entries || []
  } catch (error) { if (request === filesRequest) { entries.value = []; filesError.value = error instanceof Error ? error.message : '读取文件列表失败' } }
  finally { if (request === filesRequest) loading.value = false }
}
async function loadCache() {
  const request = ++cacheRequest
  if (disposed) return
  const connectionId = store.selectedConnectionId
  if (!connectionId) { cacheEntries.value = []; cacheError.value = ''; cacheLoading.value = false; return }
  cacheLoading.value = true; cacheError.value = ''
  try {
    const result = await api.get<CacheEntry[]>('/api/cache')
    if (request === cacheRequest && connectionId === store.selectedConnectionId) cacheEntries.value = result || []
  } catch (error) {
    if (request === cacheRequest && connectionId === store.selectedConnectionId) cacheError.value = error instanceof Error ? error.message : '读取本机缓存失败'
  } finally { if (request === cacheRequest && connectionId === store.selectedConnectionId) cacheLoading.value = false }
}
function selectTab(value: 'files' | 'cache') { tab.value = value; if (value === 'cache') void loadCache() }
function goPath(next: string) { path.value = next.replace(/^\/+|\/+$/g, ''); search.value = ''; entries.value = []; filesError.value = ''; void loadFiles() }
function enterFolder(entry: FileEntry) { goPath(entry.path || [path.value, entry.name].filter(Boolean).join('/')) }
function clickUpload() { uploadInput.value?.click() }
async function uploadFiles(event: Event) {
  const input = event.target as HTMLInputElement
  const files = [...(input.files || [])]
  input.value = ''
  if (uploading.value) return
  const context = currentListingContext()
  if (!files.length || !context) return
  if (!canWrite.value) { notify('该连接尚未通过读写验证，请先到设置中验证读写能力。'); return }
  const connection = currentConnection.value
  const maxFileBytes = uploadLimit.value
  const tooLarge = files.find((f) => f.size > maxFileBytes)
  if (tooLarge) { notify(`${tooLarge.name} 超过设置的单文件上限 ${formatSize(maxFileBytes)}`); return }
  const largeWithoutSafeMultipart = files.find((f) => f.size >= 16 * 1024 * 1024 && connection?.kind !== 'demo' && !connection?.capabilities?.multipartConditional)
  if (largeWithoutSafeMultipart) { notify(`${largeWithoutSafeMultipart.name} 需要安全分片提交能力；请先在设置中验证连接或改用已验证的连接。普通小文件上传不受影响。`); return }
  uploading.value = true
  let succeeded = 0
  const failures: string[] = []
  for (const file of files) {
    try {
      const params = new URLSearchParams({ connectionId: context.connectionId, path: [context.folderPath, file.name].filter(Boolean).join('/') })
      await api.upload(`/api/files/upload?${params.toString()}`, file)
      succeeded++
    } catch (error) { failures.push(`${file.name}：${error instanceof Error ? error.message : '上传失败'}`) }
  }
  uploading.value = false
  if (failures.length) notify(`上传到 ${listingLabel(context)}：成功 ${succeeded} 个，失败 ${failures.length} 个\n${failures.join('\n')}`, 'error')
  else if (succeeded) notify(`已向 ${listingLabel(context)} 上传 ${succeeded} 个文件`, 'success')
  if (sameListing(context)) await loadFiles()
}
async function download(entry: FileEntry) {
  const context = currentListingContext()
  if (!context) return
  const { connectionId, targetPath } = entryTarget(context, entry)
  try { await api.post('/api/files/download', { connectionId, path: targetPath }); notify(`已将 ${targetPath} 交给桌面应用保存`, 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '下载失败') }
}
function openMkdir() {
  const context = currentListingContext()
  if (!context) return
  mkdirContext.value = context
  folderName.value = ''
  showMkdir.value = true
}
function closeMkdir() { showMkdir.value = false; mkdirContext.value = null }
function closeDelete() { deleteTarget.value = null; deleteContext.value = null }
function closeTransfer() { showTransfer.value = false; transferContext.value = null; transferSource.value = null }
async function makeDirectory() {
  const context = mkdirContext.value
  const name = folderName.value.trim()
  if (!context || !name) return
  if (!canWrite.value) { notify('该连接尚未通过读写验证，请先到设置中验证读写能力。'); return }
  const connectionId = context.connectionId
  const targetPath = [context.folderPath, name].filter(Boolean).join('/')
  creatingFolder.value = true
  try {
    await api.post('/api/files/mkdir', { connectionId, path: targetPath })
    if (mkdirContext.value === context) { showMkdir.value = false; mkdirContext.value = null; folderName.value = '' }
    notify(`已在 ${listingLabel(context)} 创建 ${name}`, 'success')
    if (sameListing(context)) await loadFiles()
  } catch (error) { notify(`${targetPath}：${error instanceof Error ? error.message : '新建目录失败'}`) }
  finally { creatingFolder.value = false }
}
function confirmDelete(entry: FileEntry) {
  const context = currentListingContext()
  if (!context) return
  deleteTarget.value = entry
  deleteContext.value = entryTarget(context, entry)
}
async function deleteEntry() {
  const target = deleteTarget.value
  const context = deleteContext.value
  if (!target || !context) return
  if (!canDelete.value) { notify('该连接尚未通过删除能力验证，请先到设置中验证读写能力。'); return }
  if (target.isDir) {
    deleteTarget.value = null
    deleteContext.value = null
    await previewTreeDelete(context)
    return
  }
  const connectionId = context.connectionId
  const targetPath = context.targetPath
  const etag = target.etag
  deleting.value = true
  try {
    await api.post('/api/files/delete', { connectionId, path: targetPath, etag })
    if (deleteContext.value === context) { deleteTarget.value = null; deleteContext.value = null }
    notify(`已从 ${context.connectionName} 删除 ${targetPath}`, 'success')
    if (sameListing(context)) await loadFiles()
  } catch (error) { notify(`${targetPath}：${error instanceof Error ? error.message : '删除失败'}`) }
  finally { deleting.value = false }
}
async function previewTreeDelete(context: EntryContext) {
  const request = ++treeRequest
  treeBusy.value = true; treePreview.value = null; treeContext.value = context
  try {
    const preview = await api.post<DeleteTreePreview>('/api/files/delete-tree-preview', { connectionId: context.connectionId, path: context.targetPath })
    if (request === treeRequest && sameListing(context)) { treePreview.value = preview; showTreePreview.value = true }
  } catch (error) {
    if (request === treeRequest && sameListing(context)) notify(`${context.targetPath}：${error instanceof Error ? error.message : '生成目录删除预览失败'}`)
  } finally { if (request === treeRequest) treeBusy.value = false }
}
function closeTreePreview() { clearTreePreview() }
async function deleteTree() {
  const preview = treePreview.value
  const context = treeContext.value
  if (!preview || !context || !canDelete.value || treeBusy.value || context.connectionId !== store.selectedConnectionId) return
  const connectionId = context.connectionId
  const targetPath = preview.path
  const token = preview.token
  const request = ++treeRequest
  treeBusy.value = true
  try {
    await api.post('/api/files/delete-tree', { connectionId, path: targetPath, token })
    if (request === treeRequest && treeContext.value === context && treePreview.value?.token === token) clearTreePreview()
    notify(`已从 ${context.connectionName} 删除目录 ${targetPath}`, 'success')
    if (sameListing(context)) await loadFiles()
  } catch (error) { notify(`${targetPath}：${error instanceof Error ? error.message : '目录删除失败'}`) }
  finally { if (request === treeRequest) treeBusy.value = false }
}
function openTransfer(entry: FileEntry, kind: 'copy' | 'move') {
  const listing = currentListingContext()
  if (!listing) return
  const context = entryTarget(listing, entry)
  transferContext.value = { ...context, sourcePath: context.targetPath }
  transferSource.value = entry; transferKind.value = kind; overwrite.value = false; transferDestination.value = ''
  showTransfer.value = true
}
async function executeTransfer() {
  const context = transferContext.value
  const kind = transferKind.value
  const destination = transferDestination.value.trim().replace(/^\/+|\/+$/g, '')
  const allowOverwrite = overwrite.value
  if (!context || !destination || transferBusy.value) return
  if (kind === 'copy' ? !canWrite.value : !canMove.value) { notify('当前连接未验证所需的写入和删除能力。'); return }
  const connectionId = context.connectionId
  const sourcePath = context.sourcePath
  transferBusy.value = true
  try {
    await api.post(`/api/files/${kind}`, { connectionId, path: sourcePath, destination, overwrite: allowOverwrite })
    if (transferContext.value === context) { showTransfer.value = false; transferContext.value = null; transferSource.value = null }
    notify(`已在 ${context.connectionName} ${kind === 'copy' ? '复制' : '移动'} ${sourcePath} 到 ${destination}`, 'success')
    if (sameListing(context)) await loadFiles()
  } catch (error) { notify(`${sourcePath}：${error instanceof Error ? error.message : `${kind === 'copy' ? '复制' : '移动'}失败；若服务提示部分完成，请核对源和目标后再重试。`}`) }
  finally { transferBusy.value = false }
}
async function openVersions(entry: FileEntry) {
  const listing = currentListingContext()
  if (!listing) return
  const context = entryTarget(listing, entry)
  const request = ++versionsRequest
  versionTarget.value = entry; versionContext.value = context; versions.value = []; versionsError.value = ''; versionsOpen.value = true; versionsLoading.value = true
  try {
    const params = new URLSearchParams({ connectionId: context.connectionId, path: context.targetPath })
    const result = await api.get<ObjectVersion[]>(`/api/files/versions?${params}`)
    if (request === versionsRequest && sameListing(context)) versions.value = result || []
  } catch (error) {
    if (request === versionsRequest && sameListing(context)) versionsError.value = error instanceof Error ? error.message : '读取版本历史失败'
  } finally {
    if (request === versionsRequest) {
      if (sameListing(context)) versionsLoading.value = false
      else closeVersions()
    }
  }
}
async function saveVersion(version: ObjectVersion) {
  const context = versionContext.value
  if (!versionTarget.value || !context || versionBusy.value) return
  const connectionId = context.connectionId
  const targetPath = context.targetPath
  const versionId = version.versionId
  versionBusy.value = version.versionId
  try { await api.post('/api/files/version-save', { connectionId, path: targetPath, versionId }); notify(`已将 ${targetPath} 的版本交给桌面应用保存`, 'success') }
  catch (error) { notify(`${targetPath}：${error instanceof Error ? error.message : '另存版本失败'}`) }
  finally { versionBusy.value = '' }
}
async function previewVersionRestore(version: ObjectVersion) {
  const context = versionContext.value
  if (!versionTarget.value || !context || versionBusy.value || version.deleteMarker) return
  versionBusy.value = `preview:${version.versionId}`
  versionRestoreError.value = ''
  versionRestorePreview.value = null
  try {
    const result = await api.post<{ token: string; connectionId: string; path: string; versionId: string; version: FileEntry; current?: FileEntry; currentExists: boolean; expectedCurrentEtag: string }>('/api/files/version-restore-preview', { connectionId: context.connectionId, path: context.targetPath, versionId: version.versionId })
    if (sameListing(context) && versionContext.value === context) versionRestorePreview.value = result
  } catch (error) { versionRestoreError.value = error instanceof Error ? error.message : '无法预览版本恢复' }
  finally { versionBusy.value = '' }
}
async function restoreVersion() {
  const preview = versionRestorePreview.value
  const context = versionContext.value
  if (!preview?.token || !context || versionBusy.value) return
  versionBusy.value = `restore:${preview.versionId}`
  try {
    await api.post('/api/files/version-restore', { token: preview.token })
    closeVersions()
    notify(`已将 ${preview.versionId} 恢复到 ${preview.path}，被替换内容已保留到恢复区`, 'success')
    if (sameListing(context)) await loadFiles()
  } catch (error) {
    versionRestoreError.value = error instanceof Error ? error.message : '恢复历史版本失败，请重新预览'
    versionRestorePreview.value = null
  } finally { versionBusy.value = '' }
}
async function fetchCache(entry: FileEntry, pin = false) {
  const listing = currentListingContext()
  if (!listing) return
  const context = entryTarget(listing, entry)
  const { connectionId, targetPath } = context
  cacheBusy.value = targetPath
  try { await api.post('/api/cache/fetch', { connectionId, path: targetPath, pinned: pin, allowOffline: false }); notify(`${listingLabel(context)}：${pin ? '文件已缓存并固定' : '文件已加入本机缓存'}（${targetPath}）`, 'success'); if (!disposed && connectionId === store.selectedConnectionId) await loadCache() }
  catch (error) { notify(`${targetPath}：${error instanceof Error ? error.message : '缓存文件失败'}`) }
  finally { cacheBusy.value = '' }
}
async function pinCache(entry: CacheEntry) {
  const id = entry.id
  const pinned = !entry.pinned
  const connectionId = entry.connectionId
  cacheBusy.value = id
  try { await api.post('/api/cache/pin', { id, pinned }); if (!disposed && connectionId === store.selectedConnectionId) await loadCache() }
  catch (error) { notify(error instanceof Error ? error.message : '更新固定状态失败') }
  finally { cacheBusy.value = '' }
}
function confirmCache(entry: CacheEntry, action: 'upload' | 'delete' | 'refresh') { cacheTarget.value = { ...entry }; cacheAction.value = action }
async function checkCached(entry: CacheEntry) {
  const target = { ...entry }
  cacheBusy.value = target.id
  try {
    const result = await api.post<CacheEntry>('/api/cache/fetch', { connectionId: target.connectionId, path: target.path, pinned: target.pinned, allowOffline: true })
    if (!disposed && target.connectionId === store.selectedConnectionId) await loadCache()
    notify(`${target.path}：${result.offline ? '远端暂不可达；已保留本机副本并记录离线状态' : result.remoteChanged ? '远端版本已变化；本机副本已保留' : result.etag !== target.etag ? '本机缓存已更新到远端当前版本' : '远端版本与本机缓存一致'}`, result.offline || result.remoteChanged ? 'info' : 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '检查远端版本失败') }
  finally { cacheBusy.value = '' }
}
async function runCacheAction() {
  if (!cacheTarget.value || !cacheAction.value || cacheBusy.value) return
  const target = cacheTarget.value; const action = cacheAction.value; cacheBusy.value = target.id
  try {
    if (action === 'upload') { await api.post('/api/cache/upload', { id: target.id }); notify('编辑内容已通过版本检查写回远端', 'success') }
    else if (action === 'refresh') { const result = await api.post<CacheEntry>('/api/cache/fetch', { connectionId: target.connectionId, path: target.path, pinned: target.pinned, allowOffline: false, refresh: true }); notify(result.etag === target.etag ? '远端版本未变化，本机缓存保持不变' : '已拉取远端版本；替换前的干净缓存已保留到恢复区', 'success') }
    else { await api.post('/api/cache/delete', { id: target.id }); notify('本机缓存已移除', 'success') }
    if (cacheTarget.value?.id === target.id && cacheAction.value === action) { cacheTarget.value = null; cacheAction.value = '' }
    if (!disposed && target.connectionId === store.selectedConnectionId) await loadCache()
  } catch (error) { notify(error instanceof Error ? error.message : action === 'upload' ? '写回失败' : '清理缓存失败') }
  finally { cacheBusy.value = '' }
}
async function openCached(entry: CacheEntry) {
  cacheBusy.value = entry.id
  try { await api.post('/api/cache/open', { id: entry.id, connectionId: entry.connectionId, path: entry.path, pinned: entry.pinned }); notify('已交给桌面应用打开本机缓存', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '打开缓存失败') }
  finally { cacheBusy.value = '' }
}
async function refreshCached(entry: CacheEntry) {
  confirmCache(entry, 'refresh')
}
function formatSize(size: number) {
  if (size < 1024) return `${size} B`
  const units = ['KB', 'MB', 'GB', 'TB']; let value = size / 1024; let index = 0
  while (value >= 1024 && index < units.length - 1) { value /= 1024; index++ }
  return `${value.toFixed(value >= 10 ? 0 : 1)} ${units[index]}`
}
function formatDate(value: string) { if (!value) return '—'; const d = new Date(value); return Number.isNaN(d.valueOf()) ? value : new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(d) }
</script>
<template>
  <div class="page-view files-page">
    <header class="page-heading"><div><div class="eyebrow">REMOTE FILES</div><h1>{{ tab === 'files' ? '文件' : '本机缓存' }}</h1><p>{{ tab === 'files' ? '浏览、传输和安全管理已连接存储中的文件。' : '查看固定、编辑和离线状态，缓存文件保留在本机。' }}</p></div><div v-if="tab === 'files'" class="heading-actions"><button class="button secondary" :disabled="!currentConnection || !canWrite || uploading" :title="!canWrite ? '先在设置中验证读写能力' : ''" @click="clickUpload"><Upload :size="16" />{{ uploading ? '正在上传…' : '上传文件' }}</button><button class="button primary" :disabled="!currentConnection || !canWrite" :title="!canWrite ? '先在设置中验证读写能力' : ''" @click="openMkdir"><FolderPlus :size="16" />新建文件夹</button></div><button v-else class="button secondary" :disabled="cacheLoading" @click="loadCache"><RefreshCw :size="15" :class="{ spin: cacheLoading }" />刷新缓存</button><input ref="uploadInput" type="file" multiple hidden @change="uploadFiles" /></header>
    <div v-if="currentConnection" class="file-subtabs"><button :class="{ active: tab === 'files' }" :aria-pressed="tab === 'files'" @click="selectTab('files')"><Folder :size="15" />远端文件</button><button :class="{ active: tab === 'cache' }" :aria-pressed="tab === 'cache'" @click="selectTab('cache')"><Archive :size="15" />本机缓存</button></div>
    <div v-if="tab === 'files' && currentConnection" class="file-toolbar"><div ref="breadcrumbStrip" class="breadcrumbs"><button class="crumb-root" :title="currentConnection.name" @click="goPath('')">{{ currentConnection.name }}</button><template v-for="(crumb, i) in crumbs" :key="`${crumb}-${i}`"><ChevronRight :size="14" /><button :class="{ current: i === crumbs.length - 1 }" :title="crumbs.slice(0, i + 1).join('/')" @click="goPath(crumbs.slice(0, i + 1).join('/'))">{{ crumb }}</button></template></div><div class="file-tools"><span class="file-count">{{ entries.length }} 项</span><button class="icon-button" :disabled="loading" title="刷新" @click="loadFiles"><RefreshCw :size="16" :class="{ spin: loading }" /></button><label class="search-box"><Search :size="15" /><input v-model="search" aria-label="搜索当前列表" placeholder="搜索当前列表" /><button v-if="search" class="search-clear" aria-label="清除搜索" @click="search = ''"><X :size="13" /></button></label></div></div>
    <div v-if="tab === 'files' && currentConnection && currentConnection.kind !== 'demo' && (!canWrite || !canDelete)" class="file-permission-note"><span><ShieldCheck :size="14" />为保护远端内容，写入和删除操作仅在能力验证通过后启用。</span><button @click="emit('settings')">前往设置 <ChevronRight :size="13" /></button></div>
    <div v-if="!currentConnection" class="empty-card small-empty"><div class="empty-illustration small"><Folder :size="26" /></div><h2>先选择一个存储连接</h2><p>通过顶部的连接选择器添加 WebDAV 或 S3 存储。</p></div>
    <div v-else-if="tab === 'files'" class="file-table-card">
      <div class="file-table-head"><span>名称</span><span>修改时间</span><span>大小</span><span></span></div>
      <div v-if="loading && !entries.length" class="table-loading squirrel-loading-state" role="status"><SquirrelMark variant="eat" :size="56" /><span>正在读取远端文件列表…</span></div>
      <div v-else-if="filesError" class="table-empty feature-error"><AlertTriangle :size="19" /><b>无法读取此目录</b><span>{{ filesError }}</span><button class="button secondary small" @click="loadFiles"><RefreshCw :size="14"/>重试</button></div>
      <div v-else-if="filtered.length" class="file-table-body">
        <div v-for="entry in filtered" :key="entry.path || entry.name" class="file-row">
          <button class="file-name-cell" @click="entry.isDir ? enterFolder(entry) : download(entry)"><span class="file-type-icon" :class="{ directory: entry.isDir }"><Folder v-if="entry.isDir" :size="17" /><File v-else :size="16" /></span><span class="file-name" :title="entry.name">{{ entry.name }}</span><ChevronRight v-if="entry.isDir" :size="14" class="row-chevron" /></button>
          <span class="file-modified">{{ formatDate(entry.modified) }}</span><span class="file-size">{{ entry.isDir ? '—' : formatSize(entry.size) }}</span>
          <div class="file-row-actions"><button v-if="!entry.isDir" class="icon-button" title="下载到本机" @click="download(entry)"><ArrowDownToLine :size="15" /></button><button v-if="!entry.isDir" class="icon-button" :disabled="cacheBusy !== ''" title="加入本机缓存" @click="fetchCache(entry)"><Archive :size="15" /></button><button v-if="!entry.isDir && currentConnection?.kind === 's3'" class="icon-button" title="版本历史" @click="openVersions(entry)"><Clock3 :size="15" /></button><button class="icon-button" :disabled="!canWrite" title="复制" @click="openTransfer(entry, 'copy')"><Copy :size="14" /></button><button class="icon-button" :disabled="!canMove" :title="canMove ? '移动' : '移动需要验证条件写入和删除能力'" @click="openTransfer(entry, 'move')"><Scissors :size="14" /></button><button class="icon-button danger-hover" :disabled="!canDelete" :title="canDelete ? (entry.isDir ? '预览并删除目录' : '删除') : '先在设置中验证删除能力'" @click="confirmDelete(entry)"><Trash2 :size="15" /></button></div>
        </div>
      </div>
      <div v-else-if="search" class="table-empty"><div class="table-empty-icon"><Search :size="19" /></div><b>没有找到“{{ search }}”</b><span>搜索范围仅包括当前加载的目录。</span><button class="text-button" @click="search = ''">清除搜索</button></div>
      <div v-else class="table-empty"><div class="table-empty-icon"><Folder :size="20" /></div><b>这个目录还是空的</b><span>上传文件或新建一个文件夹开始整理。</span><button class="button secondary small" :disabled="!canWrite || uploading" :title="!canWrite ? '先在设置中验证读写能力' : ''" @click="clickUpload"><Upload :size="14" />上传文件</button></div>
      <footer class="file-table-foot"><span><span class="status-dot green"></span>{{ currentConnection.kind === 'demo' ? '隔离演示空间' : currentConnection.kind === 's3' ? 'S3 兼容存储' : 'WebDAV' }}</span><span>搜索当前列表 · {{ currentConnection.kind === 's3' ? '支持版本历史' : '当前连接不提供版本历史' }}</span></footer>
    </div>
    <section v-if="tab === 'cache' && currentConnection" class="cache-section">
      <div class="cache-intro"><Archive :size="18" /><span><b>本机副本不会自动覆盖远端</b><small>“检查远端”只记录最近一次版本状态，不代表持续实时监控；拉取新版本前会请求保留旧干净副本，未提交编辑会拒绝刷新。</small></span><button class="icon-button" :disabled="cacheLoading" title="刷新缓存列表" @click="loadCache"><RefreshCw :size="15" :class="{ spin: cacheLoading }" /></button></div>
      <div v-if="cacheLoading && !currentCacheEntries.length" class="file-table-card table-loading squirrel-loading-state" role="status"><SquirrelMark variant="eat" :size="36" /><span>正在读取本机缓存…</span></div>
      <div v-else-if="cacheError" class="cache-empty"><AlertTriangle :size="17" /><span>{{ cacheError }}</span><button class="button secondary small" @click="loadCache">重试</button></div>
      <div v-else-if="currentCacheEntries.length" class="cache-list">
        <article v-for="entry in currentCacheEntries" :key="entry.id" class="cache-card">
          <div class="cache-file-icon"><File :size="17" /></div><div class="cache-copy"><b :title="entry.path">{{ entry.path }}</b><small>{{ formatSize(entry.size) }} · 更新 {{ formatDate(entry.updated) }} · 最近检查 {{ entry.checked ? formatDate(entry.checked) : '尚未检查' }}</small><div class="cache-badges"><span :class="entry.pinned ? 'is-pinned' : ''">{{ entry.pinned ? '已固定' : '可自动清理' }}</span><span v-if="entry.dirty" class="cache-dirty">本地已编辑</span><span v-if="entry.remoteChanged" class="cache-changed">远端已变化</span><span v-if="entry.offline" class="cache-offline">离线可用</span></div><small v-if="entry.dirty && entry.remoteChanged" class="cache-warning">本地和远端都已变化，写回按钮已停用；先另存本地版本并人工合并。</small></div>
          <div class="cache-actions"><button class="button subtle small" :disabled="cacheBusy !== ''" @click="openCached(entry)"><ExternalLink :size="14" />打开</button><button class="button subtle small" :disabled="cacheBusy !== ''" @click="checkCached(entry)"><RefreshCw :size="14"/>检查远端</button><button class="button subtle small" :disabled="cacheBusy !== ''" @click="pinCache(entry)"><Pin v-if="!entry.pinned" :size="14" /><PinOff v-else :size="14" />{{ entry.pinned ? '取消固定' : '固定' }}</button><button v-if="entry.dirty" class="button subtle small" :disabled="cacheBusy !== '' || entry.remoteChanged || !canWrite" :title="entry.remoteChanged ? '远端版本发生变化，安全保护已阻止直接写回' : !canWrite ? '请先验证条件写入能力' : ''" @click="confirmCache(entry, 'upload')"><Upload :size="14" />写回远端</button><button v-else class="button subtle small" :disabled="cacheBusy !== ''" title="先显示恢复副本保护说明，再确认刷新" @click="refreshCached(entry)"><RotateCcw :size="14" />拉取远端最新版本</button><button class="button subtle small danger-text" :disabled="cacheBusy !== ''" @click="confirmCache(entry, 'delete')"><Trash2 :size="14" />清除</button></div>
        </article>
      </div>
      <div v-else class="cache-empty"><Archive :size="20" /><span>尚无缓存文件。可在远端文件列表中选择文件加入缓存。</span></div>
    </section>
    <BaseModal :model-value="showMkdir" title="新建文件夹" :subtitle="`将在 ${mkdirContext ? listingLabel(mkdirContext) : '当前目录'} 中创建文件夹。`" @update:model-value="(v) => { if (!v) closeMkdir() }">
      <form class="form-stack" @submit.prevent="makeDirectory"><label class="field"><span>文件夹名称</span><input v-model="folderName" autofocus required maxlength="255" placeholder="新文件夹" /></label><footer class="modal-actions"><button class="button secondary" type="button" @click="closeMkdir">取消</button><button class="button primary" type="submit" :disabled="creatingFolder">{{ creatingFolder ? '正在创建…' : '创建文件夹' }}</button></footer></form>
    </BaseModal>
    <BaseModal :model-value="!!deleteTarget" title="删除远端项目" :subtitle="deleteContext ? listingLabel(deleteContext) : '此操作会修改远端存储。'" @update:model-value="(v) => { if (!v) closeDelete() }">
      <div class="confirm-panel"><div class="confirm-warning"><Trash2 :size="18" /></div><p>确定删除 <b>{{ deleteContext?.targetPath }}</b> 吗？{{ deleteTarget?.isDir ? '下一步会先列出目录内所有文件和子目录。' : '系统会尝试保留本机恢复副本。' }}</p><footer class="modal-actions"><button class="button secondary" @click="closeDelete">取消</button><button class="button danger" :disabled="deleting || treeBusy" @click="deleteEntry">{{ treeBusy ? '正在生成预览…' : deleteTarget?.isDir ? '查看删除清单' : deleting ? '正在删除…' : '确认删除文件' }}</button></footer></div>
    </BaseModal>
    <BaseModal :model-value="showTransfer" :title="transferKind === 'copy' ? '复制文件或目录' : '移动文件或目录'" :subtitle="transferContext ? `${transferContext.connectionName} / ${transferContext.folderPath || '根目录'}` : ''" width="560px" @update:model-value="(v) => { if (!v) closeTransfer() }">
      <form class="form-stack" @submit.prevent="executeTransfer"><div class="transfer-preview"><span>源路径</span><code>{{ transferContext?.sourcePath }}</code><ChevronRight :size="15" /><span>目标路径</span><code>{{ transferDestination || '等待填写' }}</code></div><label class="field"><span>目标路径 <small>从连接根目录起</small></span><input v-model="transferDestination" required placeholder="例如：归档/文件名.ext" /></label><label class="check-row overwrite-option"><input v-model="overwrite" type="checkbox" />目标存在时允许替换（替换前会尝试保留恢复副本）</label><div class="file-action-note"><ShieldCheck :size="15" /><span>覆盖和移动使用版本检查。若移动中断，源和目标状态需要分别核对后再重试。</span></div><footer class="modal-actions"><button class="button secondary" type="button" @click="closeTransfer">取消</button><button class="button primary" type="submit" :disabled="transferBusy || (transferKind === 'copy' ? !canWrite : !canMove) || !transferDestination.trim()">{{ transferBusy ? '正在处理…' : transferKind === 'copy' ? '确认复制' : '确认移动' }}</button></footer></form>
    </BaseModal>
    <BaseModal :model-value="showTreePreview" title="删除目录预览" :subtitle="treePreview ? `将删除 ${treePreview.path} 下的全部项目` : ''" width="620px" :dismissible="!treeBusy" @update:model-value="(v) => { if (!v) closeTreePreview() }">
      <div v-if="treePreview" class="tree-delete-review"><div class="tree-delete-summary"><AlertTriangle :size="17" /><span><b>{{ treePreview.files.length }} 个文件 · {{ treePreview.directories.length }} 个目录</b><small>请逐项检查。此操作不能一键撤销；远端文件会尝试先保留本机恢复副本。</small></span></div><div class="tree-delete-paths"><div v-for="dir in treePreview.directories" :key="`d:${dir}`"><Folder :size="14" /><code>{{ dir }}/</code><small>目录</small></div><div v-for="item in treePreview.files" :key="`f:${item.path}`"><File :size="14" /><code>{{ item.path }}</code><small>{{ formatSize(item.size) }}</small></div></div><footer class="modal-actions"><button class="button secondary" :disabled="treeBusy" @click="closeTreePreview">取消</button><button class="button danger" :disabled="treeBusy || !canDelete" @click="deleteTree">{{ treeBusy ? '正在删除…' : `确认删除 ${treePreview.files.length + treePreview.directories.length} 项` }}</button></footer></div>
    </BaseModal>
    <BaseModal :model-value="versionsOpen" title="S3 版本历史" :subtitle="versionContext?.targetPath" width="620px" @update:model-value="(v) => { if (!v) closeVersions() }">
      <div v-if="versionsLoading" class="feature-loading squirrel-inline-loading" role="status"><SquirrelMark variant="eat" :size="34" /><span>正在读取版本列表…</span></div><div v-else-if="versionsError" class="feature-error"><AlertTriangle :size="16" /><span>{{ versionsError }}</span><button class="button secondary small" @click="versionTarget && openVersions(versionTarget)">重试</button></div><div v-else-if="versions.length" class="versions-list"><article v-for="version in versions" :key="version.versionId" class="version-row"><div class="version-marker"><span :class="version.deleteMarker ? 'marker-delete' : version.isLatest ? 'marker-latest' : ''"></span></div><div class="version-copy"><b>{{ version.deleteMarker ? '删除标记' : version.isLatest ? '当前版本' : '历史版本' }}</b><small>{{ formatDate(version.entry.modified) }} · {{ version.deleteMarker ? '无文件内容' : formatSize(version.entry.size) }}</small><code>{{ version.versionId }}</code><small v-if="version.entry.etag">ETag {{ version.entry.etag }}</small></div><button v-if="!version.deleteMarker" class="button secondary small" :disabled="versionBusy !== ''" @click="saveVersion(version)"><ArrowDownToLine :size="14" />{{ versionBusy === version.versionId ? '正在另存…' : '另存到本机' }}</button><button v-if="!version.deleteMarker" class="button primary small" :disabled="versionBusy !== '' || !canWrite" :title="canWrite ? '先预览目标当前版本，再条件恢复到原路径' : '请先验证条件写入能力'" @click="previewVersionRestore(version)"><RotateCcw :size="14" />{{ versionBusy === `preview:${version.versionId}` ? '正在预览…' : '恢复到远端' }}</button></article></div><div v-else class="cache-empty">此对象没有可读取的历史版本，或 Bucket 未启用版本控制。</div>
      <div v-if="versionRestoreError" class="feature-error"><AlertTriangle :size="15"/><span>{{ versionRestoreError }}</span><button class="button secondary small" @click="versionRestoreError = ''">关闭</button></div>
    </BaseModal>
    <BaseModal :model-value="!!versionRestorePreview" title="确认恢复历史版本" :subtitle="versionRestorePreview?.path" width="560px" :dismissible="versionBusy === ''" @update:model-value="v => { if (!v) versionRestorePreview = null }">
      <div v-if="versionRestorePreview" class="confirm-panel"><div class="confirm-warning"><RotateCcw :size="18"/></div><p>将版本 <code>{{ versionRestorePreview.versionId }}</code> 写回远端原路径 <b>{{ versionRestorePreview.path }}</b>。目标会按预览时的 ETag 条件更新；若预览时目标不存在，则只在路径仍不存在时创建。</p><p v-if="versionRestorePreview.currentExists">当前内容（{{ formatSize(versionRestorePreview.current?.size || 0) }}）会先保存到本机恢复区，再提交所选版本（{{ formatSize(versionRestorePreview.version.size) }}）。</p><p v-else>目标当前不存在；恢复会以“路径仍不存在”为条件创建对象。</p><p class="field-hint">如果文件在预览后发生变化，恢复会停止，请重新预览。</p><footer class="modal-actions"><button class="button secondary" :disabled="versionBusy !== ''" @click="versionRestorePreview = null">取消</button><button class="button primary" :disabled="versionBusy !== '' || !canWrite" @click="restoreVersion"><RefreshCw v-if="versionBusy.startsWith('restore:')" :size="14" class="spin"/><RotateCcw v-else :size="14"/>{{ versionBusy.startsWith('restore:') ? '正在恢复…' : '确认恢复到原路径' }}</button></footer></div>
    </BaseModal>
    <BaseModal :model-value="!!cacheTarget && !!cacheAction" :title="cacheAction === 'upload' ? '写回编辑后的缓存' : cacheAction === 'refresh' ? '拉取远端最新版本' : '清除本机缓存'" :subtitle="cacheTarget?.path || ''" @update:model-value="(v) => { if (!v) { cacheTarget = null; cacheAction = '' } }">
      <div v-if="cacheTarget" class="confirm-panel"><div class="confirm-warning"><AlertTriangle :size="18" /></div><p v-if="cacheAction === 'upload'">将把本机编辑内容写回远端路径 <b>{{ cacheTarget.path }}</b>。当前远端 ETag 必须仍与缓存版本一致；写回前会保留恢复副本。</p><p v-else-if="cacheAction === 'refresh'">将拉取远端路径 <b>{{cacheTarget.path}}</b> 的当前版本。若版本变化，刷新前会先把现有干净缓存保存到恢复区；有未提交编辑时服务会拒绝刷新。此处展示的是最近一次远端检查状态，不代表持续实时监控。</p><p v-else>将从本机移除缓存文件 <b>{{ cacheTarget.localPath }}</b>。远端对象不会改变；本地编辑内容请先另存。</p><p v-if="cacheTarget.remoteChanged && cacheAction === 'upload'" class="cache-warning">检测到远端版本已变化，不能安全写回此缓存。</p><footer class="modal-actions"><button class="button secondary" :disabled="cacheBusy !== ''" @click="cacheTarget = null; cacheAction = ''">返回</button><button class="button danger" :disabled="cacheBusy !== '' || cacheAction === 'upload' && cacheTarget.remoteChanged" @click="runCacheAction">{{ cacheBusy === cacheTarget.id ? '处理中…' : cacheAction === 'upload' ? '确认写回' : cacheAction === 'refresh' ? '确认刷新' : '确认清除' }}</button></footer></div>
    </BaseModal>
  </div>
</template>

<style scoped>
.file-subtabs{display:flex;gap:8px;margin:0 0 18px;padding:4px;width:max-content;border-radius:12px;background:#f2eff8}.file-subtabs button{display:inline-flex;align-items:center;gap:7px;padding:8px 13px;border:0;border-radius:9px;background:transparent;color:#716d82;font:inherit;cursor:pointer}.file-subtabs button.active{background:#fff;color:#59478e;box-shadow:0 2px 8px #45336714}.file-row-actions{display:flex;justify-content:flex-end;gap:2px}.file-row-actions .icon-button{width:30px;height:30px}.cache-section{display:grid;gap:14px}.cache-intro,.file-action-note,.tree-delete-summary{display:flex;align-items:center;gap:11px;padding:14px 16px;border:1px solid #eae6f2;border-radius:13px;background:#fbfaff;color:#5b5670}.cache-intro>svg,.file-action-note>svg{flex:none;color:#8168ca}.cache-intro span,.tree-delete-summary span{display:grid;gap:3px;flex:1}.cache-intro b,.tree-delete-summary b{font-size:13px;color:#3d3852}.cache-intro small,.tree-delete-summary small{font-size:12px;color:#817d91}.cache-list{display:grid;gap:10px}.cache-card{display:flex;align-items:flex-start;gap:13px;padding:16px;border:1px solid #eae6f2;border-radius:14px;background:#fff;box-shadow:0 3px 12px #38275f08}.cache-file-icon{width:36px;height:36px;display:grid;place-items:center;flex:none;border-radius:11px;background:#f0edfa;color:#806bc4}.cache-copy{flex:1;min-width:0;display:grid;gap:5px}.cache-copy>b{overflow-wrap:anywhere;color:#3b364b;font-size:14px}.cache-copy>small{font-size:12px;color:#888497}.cache-badges{display:flex;flex-wrap:wrap;gap:6px;margin-top:3px}.cache-badges span{padding:3px 7px;border-radius:20px;background:#f2f0f5;color:#726e7d;font-size:11px}.cache-badges .is-pinned{background:#f0edff;color:#6853b2}.cache-badges .cache-dirty{background:#fff1dc;color:#9a651c}.cache-badges .cache-changed{background:#fff0ed;color:#a64c42}.cache-badges .cache-offline{background:#eaf3ff;color:#476d9a}.cache-warning{color:#a34f45!important}.cache-actions{display:flex;flex-wrap:wrap;justify-content:flex-end;gap:6px}.cache-empty,.feature-loading,.feature-error{display:flex;align-items:center;justify-content:center;gap:10px;min-height:100px;padding:18px;border:1px dashed #ddd7ea;border-radius:14px;background:#fcfbfe;color:#7a7689;font-size:13px}.feature-error{justify-content:space-between;color:#a34f45}.transfer-preview{display:grid;grid-template-columns:auto 1fr;align-items:center;gap:8px 12px;padding:13px;border:1px solid #e9e5f1;border-radius:12px;background:#faf9fd}.transfer-preview span{color:#898598;font-size:12px}.transfer-preview code{overflow-wrap:anywhere;color:#413b55;font-size:13px}.transfer-preview svg{display:none}.overwrite-option{padding:9px 0}.file-action-note{align-items:flex-start;font-size:12px;line-height:1.5}.tree-delete-review{display:grid;gap:12px}.tree-delete-summary{border-color:#f0d9d4;background:#fff8f6}.tree-delete-summary>svg{color:#b65952;flex:none}.tree-delete-paths{display:grid;gap:5px;max-height:350px;overflow:auto;padding:10px;border:1px solid #eeeaf4;border-radius:12px;background:#fcfbfd}.tree-delete-paths>div{display:grid;grid-template-columns:18px minmax(0,1fr) auto;align-items:center;gap:8px;padding:6px 2px;border-bottom:1px solid #f1eef5}.tree-delete-paths>div:last-child{border-bottom:0}.tree-delete-paths svg{color:#8979bd}.tree-delete-paths code{overflow-wrap:anywhere;color:#48425c;font-size:12px}.tree-delete-paths small{color:#8b8798;font-size:11px}.versions-list{display:grid;gap:8px;max-height:500px;overflow:auto}.version-row{display:flex;align-items:center;gap:12px;padding:12px;border:1px solid #ebe7f2;border-radius:12px;background:#fff}.version-marker{width:10px;flex:none}.version-marker span{display:block;width:8px;height:8px;border-radius:50%;background:#bcb7ca}.version-marker .marker-latest{background:#65a78a}.version-marker .marker-delete{background:#cf746b}.version-copy{flex:1;min-width:0;display:grid;gap:3px}.version-copy b{color:#3d3850;font-size:13px}.version-copy small{color:#827e90;font-size:11px}.version-copy code{overflow-wrap:anywhere;color:#645a7c;font-size:11px}@media(max-width:900px){.cache-card{flex-wrap:wrap}.cache-actions{width:100%;justify-content:flex-start}.file-row-actions{width:112px;max-width:112px;flex-wrap:wrap;justify-content:flex-end}.file-row-actions .icon-button{flex:0 0 30px;width:30px;height:32px}.file-table-head,.file-row{grid-template-columns:minmax(0,1fr) 90px 55px 112px;column-gap:8px}.file-subtabs{max-width:100%}}
.file-toolbar{min-width:0}.breadcrumbs{flex:1 1 auto;width:0;min-width:0;overflow-x:auto;overflow-y:hidden;scrollbar-width:thin;scrollbar-color:#c9c3d7 transparent;overscroll-behavior-x:contain}.breadcrumbs>*{flex:0 0 auto}.breadcrumbs button{max-width:clamp(100px,20vw,190px)}.breadcrumbs::-webkit-scrollbar{height:4px}.breadcrumbs::-webkit-scrollbar-thumb{border-radius:4px;background:#c9c3d7}
</style>
