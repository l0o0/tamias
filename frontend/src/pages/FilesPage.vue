<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ArrowDownToLine, ChevronDown, ChevronRight, File, Folder, FolderPlus, RefreshCw, Search, ShieldCheck, Trash2, Upload, X, Copy, Scissors, Archive, Clock3, Pin, PinOff, ExternalLink, AlertTriangle, RotateCcw, MoreHorizontal } from 'lucide-vue-next'
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
const writeMode = computed(() => currentConnection.value?.writeMode || 'standard')
const keepRecovery = computed(() => !!currentConnection.value?.keepRecovery)
const currentCacheEntries = computed(() => cacheEntries.value.filter((entry) => entry.connectionId === store.selectedConnectionId))
const connectionReady = computed(() => !!currentConnection.value && !currentConnection.value.error && (currentConnection.value.kind === 'demo' || currentConnection.value.tested))
const canConditionalWrite = computed(() => writeMode.value !== 'copy' && connectionReady.value && (currentConnection.value?.kind === 'demo' || !!currentConnection.value?.capabilities?.conditionalWrite))
const canUploadBrowser = computed(() => connectionReady.value && (writeMode.value === 'standard' || writeMode.value === 'copy' || writeMode.value === 'compatible' || currentConnection.value?.kind === 'demo' || !!currentConnection.value?.capabilities?.conditionalWrite))
const canMkdir = computed(() => connectionReady.value && (writeMode.value === 'standard' || writeMode.value === 'compatible' || currentConnection.value?.kind === 'demo' || writeMode.value !== 'copy' && !!currentConnection.value?.capabilities?.conditionalWrite))
const canCacheWrite = computed(() => connectionReady.value && (writeMode.value === 'standard' || writeMode.value === 'compatible' || currentConnection.value?.kind === 'demo' || writeMode.value !== 'copy' && !!currentConnection.value?.capabilities?.conditionalWrite))
const canRemoteCopy = computed(() => canConditionalWrite.value)
const canDelete = computed(() => writeMode.value !== 'copy' && connectionReady.value && (currentConnection.value?.kind === 'demo' || !!(currentConnection.value?.capabilities?.conditionalWrite && currentConnection.value?.capabilities?.conditionalDelete)))
const canMove = computed(() => canRemoteCopy.value && canDelete.value)
const unavailableActionReason = computed(() => {
  if (!currentConnection.value) return ''
  if (currentConnection.value.detecting) return '正在自动检测连接，完成后即可操作。'
  if (!connectionReady.value) return '连接暂不可用，请在设置中检查地址、登录信息或网络。'
  if (writeMode.value === 'strict' && !canConditionalWrite.value) return '此服务暂不支持严格防覆盖。可在连接设置中选择常规同步。'
  if (writeMode.value === 'copy') return '此连接只允许将上传内容另存为新副本；新建文件夹和远端管理操作不可用。'
  return ''
})
function remoteActionReason(action: 'copy' | 'move' | 'delete') {
  if (currentConnection.value?.detecting) return '检测中'
  if (!connectionReady.value) return '连接不可用'
  if (writeMode.value === 'copy') return '此模式不支持'
  if ((action === 'copy' || action === 'move') && !canRemoteCopy.value) return '服务能力受限'
  if ((action === 'move' || action === 'delete') && !canDelete.value) return '服务能力受限'
  return ''
}
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
function closeActionMenus(except?: HTMLDetailsElement) {
  document.querySelectorAll<HTMLDetailsElement>('.files-page .file-action-disclosure[open]').forEach((details) => {
    if (details !== except) details.open = false
  })
}
function closeAllActionMenus() { closeActionMenus() }
function positionActionMenu(details: HTMLDetailsElement) {
  const summary = details.querySelector('summary')
  const menu = details.querySelector<HTMLElement>('.file-row-menu, .cache-action-menu')
  if (!summary || !menu) return
  const anchor = summary.getBoundingClientRect()
  const size = menu.getBoundingClientRect()
  const gap = 6
  const below = window.innerHeight - anchor.bottom >= size.height + gap
  const top = below ? anchor.bottom + gap : Math.max(8, anchor.top - size.height - gap)
  const left = Math.max(8, Math.min(anchor.right - size.width, window.innerWidth - size.width - 8))
  menu.style.top = `${top}px`
  menu.style.left = `${left}px`
}
function handleActionMenuToggle(event: Event) {
  const details = event.currentTarget as HTMLDetailsElement
  if (!details.open) return
  closeActionMenus(details)
  positionActionMenu(details)
}
function closeActionMenusOnOutside(event: PointerEvent) {
  const openMenu = document.querySelector<HTMLDetailsElement>('.files-page .file-action-disclosure[open]')
  if (openMenu && event.target instanceof Node && !openMenu.contains(event.target)) openMenu.open = false
}
function closeActionMenusOnEscape(event: KeyboardEvent) {
  if (event.key !== 'Escape') return
  const openMenu = document.querySelector<HTMLDetailsElement>('.files-page .file-action-disclosure[open]')
  if (!openMenu) return
  event.preventDefault()
  event.stopPropagation()
  openMenu.open = false
  openMenu.querySelector<HTMLElement>('summary')?.focus()
}
function closeActionMenusOnScroll(event: Event) {
  const openMenu = document.querySelector<HTMLDetailsElement>('.files-page .file-action-disclosure[open]')
  if (openMenu && event.target instanceof Node && !openMenu.contains(event.target)) openMenu.open = false
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
  document.removeEventListener('pointerdown', closeActionMenusOnOutside)
  document.removeEventListener('keydown', closeActionMenusOnEscape)
  document.removeEventListener('scroll', closeActionMenusOnScroll, true)
  window.removeEventListener('resize', closeAllActionMenus)
})
onMounted(() => {
  document.addEventListener('pointerdown', closeActionMenusOnOutside)
  document.addEventListener('keydown', closeActionMenusOnEscape)
  document.addEventListener('scroll', closeActionMenusOnScroll, true)
  window.addEventListener('resize', closeAllActionMenus)
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
    if (request === cacheRequest && connectionId === store.selectedConnectionId) cacheError.value = error instanceof Error ? error.message : '读取离线文件失败'
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
  if (!canUploadBrowser.value) { notify('该连接暂不可用，或严格防覆盖模式缺少条件写入能力。请到设置中检查连接状态。'); return }
  const maxFileBytes = uploadLimit.value
  const tooLarge = files.find((f) => f.size > maxFileBytes)
  if (tooLarge) { notify(`${tooLarge.name} 超过设置的单文件上限 ${formatSize(maxFileBytes)}`); return }
  uploading.value = true
  let succeeded = 0
  const savedAsCopies: string[] = []
  const failures: string[] = []
  for (const file of files) {
    try {
      const params = new URLSearchParams({ connectionId: context.connectionId, path: [context.folderPath, file.name].filter(Boolean).join('/') })
      const saved = await api.upload<FileEntry>(`/api/files/upload?${params.toString()}`, file)
      if (saved?.name && saved.name !== file.name) savedAsCopies.push(`${file.name} → ${saved.name}`)
      succeeded++
    } catch (error) { failures.push(`${file.name}：${error instanceof Error ? error.message : '上传失败'}`) }
  }
  uploading.value = false
  if (failures.length) notify(`上传到 ${listingLabel(context)}：成功 ${succeeded} 个，失败 ${failures.length} 个${savedAsCopies.length ? `\n另存为新副本：${savedAsCopies.join('、')}` : ''}\n${failures.join('\n')}`, 'error')
  else if (succeeded) notify(`已向 ${listingLabel(context)} 上传 ${succeeded} 个文件${savedAsCopies.length ? `\n另存为新副本：${savedAsCopies.join('、')}` : ''}`, 'success')
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
  if (!canMkdir.value) { notify('当前模式不开放新建远端目录，或连接暂不可用。'); return }
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
  if (!canDelete.value) { notify('此连接暂不支持安全删除远端文件。'); return }
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
  if (kind === 'copy' ? !canRemoteCopy.value : !canMove.value) { notify('当前连接未验证所需的条件写入和删除能力。'); return }
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
    notify(`已将 ${preview.versionId} 恢复到 ${preview.path}${keepRecovery.value && preview.currentExists ? '，连接的恢复副本设置已开启' : ''}`, 'success')
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
  try { await api.post('/api/cache/fetch', { connectionId, path: targetPath, pinned: pin, allowOffline: false }); notify(`${listingLabel(context)}：${pin ? '文件已下载并保留在这台设备' : '文件已下载为离线文件'}（${targetPath}）`, 'success'); if (!disposed && connectionId === store.selectedConnectionId) await loadCache() }
  catch (error) { notify(`${targetPath}：${error instanceof Error ? error.message : '下载离线文件失败'}`) }
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
    notify(`${target.path}：${result.offline ? '远端暂不可达；离线文件已保留并记录离线状态' : result.remoteChanged ? '远端版本已变化；离线文件已保留' : result.etag !== target.etag ? '离线文件已更新到远端当前版本' : '远端版本与离线文件一致'}`, result.offline || result.remoteChanged ? 'info' : 'success')
  } catch (error) { notify(error instanceof Error ? error.message : '检查远端版本失败') }
  finally { cacheBusy.value = '' }
}
async function runCacheAction() {
  if (!cacheTarget.value || !cacheAction.value || cacheBusy.value) return
  const target = cacheTarget.value; const action = cacheAction.value; cacheBusy.value = target.id
  try {
    if (action === 'upload') { await api.post('/api/cache/upload', { id: target.id }); notify('编辑内容已通过版本检查写回远端', 'success') }
    else if (action === 'refresh') { const result = await api.post<CacheEntry>('/api/cache/fetch', { connectionId: target.connectionId, path: target.path, pinned: target.pinned, allowOffline: false, refresh: true }); notify(result.etag === target.etag ? '远端版本未变化，离线文件保持不变' : `已下载远端最新版本${keepRecovery.value ? '；恢复副本设置已开启' : ''}`, 'success') }
    else { await api.post('/api/cache/delete', { id: target.id }); notify('离线文件已从这台设备移除', 'success') }
    if (cacheTarget.value?.id === target.id && cacheAction.value === action) { cacheTarget.value = null; cacheAction.value = '' }
    if (!disposed && target.connectionId === store.selectedConnectionId) await loadCache()
  } catch (error) { notify(error instanceof Error ? error.message : action === 'upload' ? '写回失败' : '移除离线文件失败') }
  finally { cacheBusy.value = '' }
}
async function openCached(entry: CacheEntry) {
  cacheBusy.value = entry.id
  try { await api.post('/api/cache/open', { id: entry.id, connectionId: entry.connectionId, path: entry.path, pinned: entry.pinned }); notify('已交给桌面应用打开离线文件', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '打开离线文件失败') }
  finally { cacheBusy.value = '' }
}
async function refreshCached(entry: CacheEntry) {
  confirmCache(entry, 'refresh')
}
function closeRowActions(event: Event) {
  const details = (event.currentTarget as HTMLElement).closest('details')
  if (details instanceof HTMLDetailsElement) {
    details.open = false
    details.querySelector<HTMLElement>('summary')?.focus()
  }
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
    <header class="page-heading"><div><h1>{{ tab === 'files' ? '文件' : '离线文件' }}</h1></div><div v-if="tab === 'files'" class="heading-actions"><button class="button secondary" :disabled="!currentConnection || !canUploadBrowser || uploading" :title="!canUploadBrowser ? '当前模式不允许文件页上传，或连接暂不可用' : ''" @click="clickUpload"><Upload :size="16" />{{ uploading ? '正在上传…' : '上传文件' }}</button><button class="button primary" :disabled="!currentConnection || !canMkdir" :title="!canMkdir ? '当前模式不允许新建目录，或连接暂不可用' : ''" @click="openMkdir"><FolderPlus :size="16" />新建文件夹</button></div><input ref="uploadInput" type="file" multiple hidden @change="uploadFiles" /></header>
    <div v-if="currentConnection" class="file-subtabs"><button :class="{ active: tab === 'files' }" :aria-pressed="tab === 'files'" @click="selectTab('files')"><Folder :size="15" />远端文件</button><button :class="{ active: tab === 'cache' }" :aria-pressed="tab === 'cache'" @click="selectTab('cache')"><Archive :size="15" />离线文件</button></div>
    <div v-if="tab === 'files' && currentConnection" class="file-toolbar"><div ref="breadcrumbStrip" class="breadcrumbs"><button class="crumb-root" :title="currentConnection.name" @click="goPath('')">{{ currentConnection.name }}</button><template v-for="(crumb, i) in crumbs" :key="`${crumb}-${i}`"><ChevronRight :size="14" /><button :class="{ current: i === crumbs.length - 1 }" :title="crumbs.slice(0, i + 1).join('/')" @click="goPath(crumbs.slice(0, i + 1).join('/'))">{{ crumb }}</button></template></div><div class="file-tools"><span class="file-count">{{ entries.length }} 项</span><button class="icon-button" :disabled="loading" title="刷新" @click="loadFiles"><RefreshCw :size="16" :class="{ spin: loading }" /></button><label class="search-box"><Search :size="15" /><input v-model="search" aria-label="搜索当前列表" placeholder="搜索当前列表" /><button v-if="search" class="search-clear" aria-label="清除搜索" @click="search = ''"><X :size="13" /></button></label></div></div>
    <div v-if="tab === 'files' && currentConnection && unavailableActionReason" class="file-limit-note"><AlertTriangle :size="14" /><span>{{ unavailableActionReason }}</span><button @click="emit('settings')">前往设置</button></div>
    <details v-if="tab === 'files' && currentConnection && currentConnection.kind !== 'demo'" class="file-permission-disclosure"><summary><ShieldCheck :size="14" />查看此连接的操作限制<ChevronDown :size="13" class="disclosure-chevron" /></summary><div class="file-permission-note"><span>{{ writeMode === 'copy' ? '上传文件时使用新名称，保留远端同名文件；同步下载可用，远端删除、移动和覆盖不可用。' : writeMode === 'strict' ? '严格防覆盖模式要求服务端支持条件写入；删除、移动等操作也需相应能力。' : '常规同步按原路径直接上传并更新同名文件；删除、移动等操作仍取决于服务端能力。' }}</span><button @click="emit('settings')">前往设置 <ChevronRight :size="13" /></button></div></details>
    <div v-if="!currentConnection" class="empty-card small-empty"><div class="empty-illustration small"><Folder :size="26" /></div><h2>先选择一个存储连接</h2><p>通过顶部的连接选择器添加 WebDAV 或 S3 存储。</p></div>
    <div v-else-if="tab === 'files'" class="file-table-card">
      <div class="file-table-head"><span>名称</span><span>修改时间</span><span>大小</span><span></span></div>
      <div v-if="loading && !entries.length" class="table-loading squirrel-loading-state" role="status"><SquirrelMark variant="eat" :size="56" /><span>正在读取远端文件列表…</span></div>
      <div v-else-if="filesError" class="table-empty feature-error"><AlertTriangle :size="19" /><b>无法读取此目录</b><span>{{ filesError }}</span><button class="button secondary small" @click="loadFiles"><RefreshCw :size="14"/>重试</button></div>
      <div v-else-if="filtered.length" class="file-table-body">
        <div v-for="entry in filtered" :key="entry.path || entry.name" class="file-row">
          <button class="file-name-cell" @click="entry.isDir ? enterFolder(entry) : download(entry)"><span class="file-type-icon" :class="{ directory: entry.isDir }"><Folder v-if="entry.isDir" :size="17" /><File v-else :size="16" /></span><span class="file-name" :title="entry.name">{{ entry.name }}</span><ChevronRight v-if="entry.isDir" :size="14" class="row-chevron" /></button>
          <span class="file-modified">{{ formatDate(entry.modified) }}</span><span class="file-size">{{ entry.isDir ? '—' : formatSize(entry.size) }}</span>
          <div class="file-row-actions">
            <button v-if="!entry.isDir" class="button secondary small row-download" title="下载到本机" @click="download(entry)"><ArrowDownToLine :size="14" />下载</button>
            <details class="file-more-actions file-action-disclosure" @toggle="handleActionMenuToggle">
              <summary class="button subtle small"><MoreHorizontal :size="14" />更多</summary>
              <div class="file-row-menu" role="group" :aria-label="`${entry.name} 的更多操作`">
                <button v-if="!entry.isDir" @click="fetchCache(entry); closeRowActions($event)" :disabled="cacheBusy !== ''"><Archive :size="14" />下载为离线文件</button>
                <button v-if="!entry.isDir && currentConnection?.kind === 's3'" @click="openVersions(entry); closeRowActions($event)"><Clock3 :size="14" />版本历史</button>
                <button @click="openTransfer(entry, 'copy'); closeRowActions($event)" :disabled="!canRemoteCopy" :title="!canRemoteCopy ? remoteActionReason('copy') : ''"><Copy :size="14" />复制<small v-if="!canRemoteCopy">{{ remoteActionReason('copy') }}</small></button>
                <button @click="openTransfer(entry, 'move'); closeRowActions($event)" :disabled="!canMove" :title="!canMove ? remoteActionReason('move') : ''"><Scissors :size="14" />移动<small v-if="!canMove">{{ remoteActionReason('move') }}</small></button>
                <button class="danger-text" @click="confirmDelete(entry); closeRowActions($event)" :disabled="!canDelete" :title="!canDelete ? remoteActionReason('delete') : entry.isDir ? '预览并删除目录' : ''"><Trash2 :size="14" />删除<small v-if="!canDelete">{{ remoteActionReason('delete') }}</small></button>
              </div>
            </details>
          </div>
        </div>
      </div>
      <div v-else-if="search" class="table-empty"><div class="table-empty-icon"><Search :size="19" /></div><b>没有找到“{{ search }}”</b><span>搜索范围仅包括当前加载的目录。</span><button class="text-button" @click="search = ''">清除搜索</button></div>
      <div v-else class="table-empty"><div class="table-empty-icon"><Folder :size="20" /></div><b>这个目录还是空的</b><span>上传文件或新建一个文件夹开始整理。</span><button class="button secondary small" :disabled="!canUploadBrowser || uploading" :title="!canUploadBrowser ? '当前模式不允许文件页上传，或连接暂不可用' : ''" @click="clickUpload"><Upload :size="14" />上传文件</button></div>
      <footer class="file-table-foot"><span><span class="status-dot green"></span>{{ currentConnection.kind === 'demo' ? '隔离演示空间' : currentConnection.kind === 's3' ? 'S3 兼容存储' : 'WebDAV' }}</span><span>{{ currentConnection.kind === 's3' ? '支持版本历史' : '当前连接不提供版本历史' }}</span></footer>
    </div>
    <section v-if="tab === 'cache' && currentConnection" class="cache-section">
      <div class="cache-intro"><Archive :size="18" /><span><b>离线文件不会自动覆盖远端</b></span><button class="button secondary small" :disabled="cacheLoading" @click="loadCache"><RefreshCw :size="14" :class="{ spin: cacheLoading }" />刷新列表</button><details class="cache-guidance"><summary>离线文件说明<ChevronDown :size="13" class="disclosure-chevron" /></summary><div>检查远端只核对当前版本，不会持续监听；有未提交编辑时，拉取新版本会被拒绝，以免丢失本地内容。</div></details></div>
      <div v-if="cacheLoading && !currentCacheEntries.length" class="file-table-card table-loading squirrel-loading-state" role="status"><SquirrelMark variant="eat" :size="36" /><span>正在读取离线文件…</span></div>
      <div v-else-if="cacheError" class="cache-empty"><AlertTriangle :size="17" /><span>{{ cacheError }}</span><button class="button secondary small" @click="loadCache">重试</button></div>
      <div v-else-if="currentCacheEntries.length" class="cache-list">
        <article v-for="entry in currentCacheEntries" :key="entry.id" class="cache-card">
          <div class="cache-file-icon"><File :size="17" /></div><div class="cache-copy"><b :title="entry.path">{{ entry.path }}</b><small>{{ formatSize(entry.size) }} · 更新 {{ formatDate(entry.updated) }} · 最近检查 {{ entry.checked ? formatDate(entry.checked) : '尚未检查' }}</small><div class="cache-badges"><span :class="entry.pinned ? 'is-pinned' : ''">{{ entry.pinned ? '已固定' : '可自动清理' }}</span><span v-if="entry.dirty" class="cache-dirty">本地已编辑</span><span v-if="entry.remoteChanged" class="cache-changed">远端已变化</span><span v-if="entry.offline" class="cache-offline">离线可用</span></div><small v-if="entry.dirty && entry.remoteChanged" class="cache-warning">本地和远端都已变化，写回已停用；先另存本地版本并人工合并。</small><small v-if="entry.dirty && !canCacheWrite" class="cache-warning">连接未通过写回所需能力验证。<button class="text-button" @click="emit('settings')">前往设置查看原因</button></small></div>
          <div class="cache-actions"><button class="button secondary small" :disabled="cacheBusy !== ''" @click="openCached(entry)"><ExternalLink :size="14" />打开</button><details class="cache-manage-actions file-action-disclosure" @toggle="handleActionMenuToggle"><summary class="button subtle small"><MoreHorizontal :size="14" />管理离线文件</summary><div class="cache-action-menu" role="group" :aria-label="`${entry.path} 的离线文件管理操作`"><button @click="checkCached(entry); closeRowActions($event)" :disabled="cacheBusy !== ''"><RefreshCw :size="14"/>检查远端版本</button><button @click="pinCache(entry); closeRowActions($event)" :disabled="cacheBusy !== ''"><Pin v-if="!entry.pinned" :size="14" /><PinOff v-else :size="14" />{{ entry.pinned ? '取消固定' : '固定以免自动清理' }}</button><button v-if="entry.dirty" @click="confirmCache(entry, 'upload'); closeRowActions($event)" :disabled="cacheBusy !== '' || entry.remoteChanged || !canCacheWrite" :title="entry.remoteChanged ? '远端版本发生变化，安全保护已阻止直接写回' : !canCacheWrite ? '当前模式不允许缓存写回，或连接暂不可用' : ''"><Upload :size="14" />写回远端</button><button v-else @click="refreshCached(entry); closeRowActions($event)" :disabled="cacheBusy !== ''"><RotateCcw :size="14" />下载远端最新版本</button><button class="danger-text" @click="confirmCache(entry, 'delete'); closeRowActions($event)" :disabled="cacheBusy !== ''"><Trash2 :size="14" />移除离线文件</button></div></details></div>
        </article>
      </div>
      <div v-else class="cache-empty"><Archive :size="20" /><span>还没有离线文件。可在远端文件列表中选择文件下载到这台设备。</span><button class="button secondary small" @click="selectTab('files')"><Folder :size="14" />浏览远端文件</button></div>
    </section>
    <BaseModal :model-value="showMkdir" title="新建文件夹" :subtitle="`将在 ${mkdirContext ? listingLabel(mkdirContext) : '当前目录'} 中创建文件夹。`" @update:model-value="(v) => { if (!v) closeMkdir() }">
      <form class="form-stack" @submit.prevent="makeDirectory"><label class="field"><span>文件夹名称</span><input v-model="folderName" autofocus required maxlength="255" placeholder="新文件夹" /></label><footer class="modal-actions"><button class="button secondary" type="button" @click="closeMkdir">取消</button><button class="button primary" type="submit" :disabled="creatingFolder">{{ creatingFolder ? '正在创建…' : '创建文件夹' }}</button></footer></form>
    </BaseModal>
    <BaseModal :model-value="!!deleteTarget" title="删除远端项目" :subtitle="deleteContext ? listingLabel(deleteContext) : '此操作会修改远端存储。'" @update:model-value="(v) => { if (!v) closeDelete() }">
      <div class="confirm-panel"><div class="confirm-warning"><Trash2 :size="18" /></div><p>确定删除 <b>{{ deleteContext?.targetPath }}</b> 吗？{{ deleteTarget?.isDir ? '下一步会先列出目录内所有文件和子目录。' : keepRecovery ? '连接已启用恢复副本设置，将按此设置处理。' : '此操作不能一键撤销。' }}</p><footer class="modal-actions"><button class="button secondary" @click="closeDelete">取消</button><button class="button danger" :disabled="deleting || treeBusy" @click="deleteEntry">{{ treeBusy ? '正在生成预览…' : deleteTarget?.isDir ? '查看删除清单' : deleting ? '正在删除…' : '确认删除文件' }}</button></footer></div>
    </BaseModal>
    <BaseModal :model-value="showTransfer" :title="transferKind === 'copy' ? '复制文件或目录' : '移动文件或目录'" :subtitle="transferContext ? `${transferContext.connectionName} / ${transferContext.folderPath || '根目录'}` : ''" width="560px" @update:model-value="(v) => { if (!v) closeTransfer() }">
      <form class="form-stack" @submit.prevent="executeTransfer"><div class="transfer-preview"><span>源路径</span><code>{{ transferContext?.sourcePath }}</code><ChevronRight :size="15" /><span>目标路径</span><code>{{ transferDestination || '等待填写' }}</code></div><label class="field"><span>目标路径 <small>从连接根目录起</small></span><input v-model="transferDestination" required placeholder="例如：归档/文件名.ext" /></label><label class="check-row overwrite-option"><input v-model="overwrite" type="checkbox" />目标存在时允许替换{{ keepRecovery ? '（已启用恢复副本设置）' : '' }}</label><div class="file-action-note"><ShieldCheck :size="15" /><span>覆盖和移动使用版本检查。若移动中断，源和目标状态需要分别核对后再重试。</span></div><footer class="modal-actions"><button class="button secondary" type="button" @click="closeTransfer">取消</button><button class="button primary" type="submit" :disabled="transferBusy || (transferKind === 'copy' ? !canRemoteCopy : !canMove) || !transferDestination.trim()">{{ transferBusy ? '正在处理…' : transferKind === 'copy' ? '确认复制' : '确认移动' }}</button></footer></form>
    </BaseModal>
    <BaseModal :model-value="showTreePreview" title="删除目录预览" :subtitle="treePreview ? `将删除 ${treePreview.path} 下的全部项目` : ''" width="620px" :dismissible="!treeBusy" @update:model-value="(v) => { if (!v) closeTreePreview() }">
      <div v-if="treePreview" class="tree-delete-review"><div class="tree-delete-summary"><AlertTriangle :size="17" /><span><b>{{ treePreview.files.length }} 个文件 · {{ treePreview.directories.length }} 个目录</b><small>请逐项检查。此操作不能一键撤销。{{ keepRecovery ? '连接已启用恢复副本设置。' : '' }}</small></span></div><div class="tree-delete-paths"><div v-for="dir in treePreview.directories" :key="`d:${dir}`"><Folder :size="14" /><code>{{ dir }}/</code><small>目录</small></div><div v-for="item in treePreview.files" :key="`f:${item.path}`"><File :size="14" /><code>{{ item.path }}</code><small>{{ formatSize(item.size) }}</small></div></div><footer class="modal-actions"><button class="button secondary" :disabled="treeBusy" @click="closeTreePreview">取消</button><button class="button danger" :disabled="treeBusy || !canDelete" @click="deleteTree">{{ treeBusy ? '正在删除…' : `确认删除 ${treePreview.files.length + treePreview.directories.length} 项` }}</button></footer></div>
    </BaseModal>
    <BaseModal :model-value="versionsOpen" title="S3 版本历史" :subtitle="versionContext?.targetPath" width="620px" @update:model-value="(v) => { if (!v) closeVersions() }">
      <div v-if="versionsLoading" class="feature-loading squirrel-inline-loading" role="status"><SquirrelMark variant="eat" :size="34" /><span>正在读取版本列表…</span></div><div v-else-if="versionsError" class="feature-error"><AlertTriangle :size="16" /><span>{{ versionsError }}</span><button class="button secondary small" @click="versionTarget && openVersions(versionTarget)">重试</button></div><div v-else-if="versions.length" class="versions-list"><article v-for="version in versions" :key="version.versionId" class="version-row"><div class="version-marker"><span :class="version.deleteMarker ? 'marker-delete' : version.isLatest ? 'marker-latest' : ''"></span></div><div class="version-copy"><b>{{ version.deleteMarker ? '删除标记' : version.isLatest ? '当前版本' : '历史版本' }}</b><small>{{ formatDate(version.entry.modified) }} · {{ version.deleteMarker ? '无文件内容' : formatSize(version.entry.size) }}</small><code>{{ version.versionId }}</code><small v-if="version.entry.etag">ETag {{ version.entry.etag }}</small></div><button v-if="!version.deleteMarker" class="button secondary small" :disabled="versionBusy !== ''" @click="saveVersion(version)"><ArrowDownToLine :size="14" />{{ versionBusy === version.versionId ? '正在另存…' : '另存到本机' }}</button><button v-if="!version.deleteMarker" class="button primary small" :disabled="versionBusy !== '' || !canConditionalWrite" :title="canConditionalWrite ? '先预览目标当前版本，再条件恢复到原路径' : '此服务暂不支持条件写入'" @click="previewVersionRestore(version)"><RotateCcw :size="14" />{{ versionBusy === `preview:${version.versionId}` ? '正在预览…' : '恢复到远端' }}</button></article></div><div v-else class="cache-empty">此对象没有可读取的历史版本，或 Bucket 未启用版本控制。</div>
      <div v-if="versionRestoreError" class="feature-error"><AlertTriangle :size="15"/><span>{{ versionRestoreError }}</span><button class="button secondary small" @click="versionRestoreError = ''">关闭</button></div>
    </BaseModal>
    <BaseModal :model-value="!!versionRestorePreview" title="确认恢复历史版本" :subtitle="versionRestorePreview?.path" width="560px" :dismissible="versionBusy === ''" @update:model-value="v => { if (!v) versionRestorePreview = null }">
      <div v-if="versionRestorePreview" class="confirm-panel"><div class="confirm-warning"><RotateCcw :size="18"/></div><p>将版本 <code>{{ versionRestorePreview.versionId }}</code> 写回远端原路径 <b>{{ versionRestorePreview.path }}</b>。目标会按预览时的 ETag 条件更新；若预览时目标不存在，则只在路径仍不存在时创建。</p><p v-if="versionRestorePreview.currentExists">当前内容（{{ formatSize(versionRestorePreview.current?.size || 0) }}）将被替换。{{ keepRecovery ? '连接已启用恢复副本设置。' : '自动恢复副本已关闭。' }}所选版本大小为 {{ formatSize(versionRestorePreview.version.size) }}。</p><p v-else>目标当前不存在；恢复会以“路径仍不存在”为条件创建对象。</p><p class="field-hint">如果文件在预览后发生变化，恢复会停止，请重新预览。</p><footer class="modal-actions"><button class="button secondary" :disabled="versionBusy !== ''" @click="versionRestorePreview = null">取消</button><button class="button primary" :disabled="versionBusy !== '' || !canConditionalWrite" @click="restoreVersion"><RefreshCw v-if="versionBusy.startsWith('restore:')" :size="14" class="spin"/><RotateCcw v-else :size="14"/>{{ versionBusy.startsWith('restore:') ? '正在恢复…' : '确认恢复到原路径' }}</button></footer></div>
    </BaseModal>
    <BaseModal :model-value="!!cacheTarget && !!cacheAction" :title="cacheAction === 'upload' ? '写回编辑后的离线文件' : cacheAction === 'refresh' ? '下载远端最新版本' : '移除离线文件'" :subtitle="cacheTarget?.path || ''" @update:model-value="(v) => { if (!v) { cacheTarget = null; cacheAction = '' } }">
      <div v-if="cacheTarget" class="confirm-panel"><div class="confirm-warning"><AlertTriangle :size="18" /></div><p v-if="cacheAction === 'upload'">将把本机编辑内容写回此远端路径。当前远端版本必须仍与此离线文件一致。{{ keepRecovery ? '连接已启用恢复副本设置。' : '自动恢复副本已关闭。' }}</p><p v-else-if="cacheAction === 'refresh'">将拉取远端当前版本。有未提交编辑时服务会拒绝刷新；最近一次远端检查状态会显示在列表中。</p><p v-else>将从本机移除这份离线文件。远端对象不会改变；本地编辑内容请先另存。</p><p v-if="cacheTarget.remoteChanged && cacheAction === 'upload'" class="cache-warning">检测到远端版本已变化，不能安全写回此离线文件。</p><footer class="modal-actions"><button class="button secondary" :disabled="cacheBusy !== ''" @click="cacheTarget = null; cacheAction = ''">返回</button><button class="button danger" :disabled="cacheBusy !== '' || cacheAction === 'upload' && cacheTarget.remoteChanged" @click="runCacheAction">{{ cacheBusy === cacheTarget.id ? '处理中…' : cacheAction === 'upload' ? '确认写回' : cacheAction === 'refresh' ? '确认下载' : '确认移除' }}</button></footer></div>
    </BaseModal>
  </div>
</template>

<style scoped>
.file-subtabs{display:flex;gap:8px;margin:0 0 18px;padding:4px;width:max-content;border-radius:12px;background:#f2eff8}.file-subtabs button{display:inline-flex;align-items:center;gap:7px;padding:8px 13px;border:0;border-radius:9px;background:transparent;color:#716d82;font:inherit;cursor:pointer}.file-subtabs button.active{background:#fff;color:#59478e;box-shadow:0 2px 8px #45336714}.file-row-actions{display:flex;justify-content:flex-end;gap:2px}.file-row-actions .icon-button{width:30px;height:30px}.cache-section{display:grid;gap:14px}.cache-intro,.file-action-note,.tree-delete-summary{display:flex;align-items:center;gap:11px;padding:14px 16px;border:1px solid #eae6f2;border-radius:13px;background:#fbfaff;color:#5b5670}.cache-intro>svg,.file-action-note>svg{flex:none;color:#8168ca}.cache-intro span,.tree-delete-summary span{display:grid;gap:3px;flex:1}.cache-intro b,.tree-delete-summary b{font-size:13px;color:#3d3852}.cache-intro small,.tree-delete-summary small{font-size:12px;color:#817d91}.cache-list{display:grid;gap:10px}.cache-card{display:flex;align-items:flex-start;gap:13px;padding:16px;border:1px solid #eae6f2;border-radius:14px;background:#fff;box-shadow:0 3px 12px #38275f08}.cache-file-icon{width:36px;height:36px;display:grid;place-items:center;flex:none;border-radius:11px;background:#f0edfa;color:#806bc4}.cache-copy{flex:1;min-width:0;display:grid;gap:5px}.cache-copy>b{overflow-wrap:anywhere;color:#3b364b;font-size:14px}.cache-copy>small{font-size:12px;color:#888497}.cache-badges{display:flex;flex-wrap:wrap;gap:6px;margin-top:3px}.cache-badges span{padding:3px 7px;border-radius:20px;background:#f2f0f5;color:#726e7d;font-size:11px}.cache-badges .is-pinned{background:#f0edff;color:#6853b2}.cache-badges .cache-dirty{background:#fff1dc;color:#9a651c}.cache-badges .cache-changed{background:#fff0ed;color:#a64c42}.cache-badges .cache-offline{background:#eaf3ff;color:#476d9a}.cache-warning{color:#a34f45!important}.cache-actions{display:flex;flex-wrap:wrap;justify-content:flex-end;gap:6px}.cache-empty,.feature-loading,.feature-error{display:flex;align-items:center;justify-content:center;gap:10px;min-height:100px;padding:18px;border:1px dashed #ddd7ea;border-radius:14px;background:#fcfbfe;color:#7a7689;font-size:13px}.feature-error{justify-content:space-between;color:#a34f45}.transfer-preview{display:grid;grid-template-columns:auto 1fr;align-items:center;gap:8px 12px;padding:13px;border:1px solid #e9e5f1;border-radius:12px;background:#faf9fd}.transfer-preview span{color:#898598;font-size:12px}.transfer-preview code{overflow-wrap:anywhere;color:#413b55;font-size:13px}.transfer-preview svg{display:none}.overwrite-option{padding:9px 0}.file-action-note{align-items:flex-start;font-size:12px;line-height:1.5}.tree-delete-review{display:grid;gap:12px}.tree-delete-summary{border-color:#f0d9d4;background:#fff8f6}.tree-delete-summary>svg{color:#b65952;flex:none}.tree-delete-paths{display:grid;gap:5px;max-height:350px;overflow:auto;padding:10px;border:1px solid #eeeaf4;border-radius:12px;background:#fcfbfd}.tree-delete-paths>div{display:grid;grid-template-columns:18px minmax(0,1fr) auto;align-items:center;gap:8px;padding:6px 2px;border-bottom:1px solid #f1eef5}.tree-delete-paths>div:last-child{border-bottom:0}.tree-delete-paths svg{color:#8979bd}.tree-delete-paths code{overflow-wrap:anywhere;color:#48425c;font-size:12px}.tree-delete-paths small{color:#8b8798;font-size:11px}.versions-list{display:grid;gap:8px;max-height:500px;overflow:auto}.version-row{display:flex;align-items:center;gap:12px;padding:12px;border:1px solid #ebe7f2;border-radius:12px;background:#fff}.version-marker{width:10px;flex:none}.version-marker span{display:block;width:8px;height:8px;border-radius:50%;background:#bcb7ca}.version-marker .marker-latest{background:#65a78a}.version-marker .marker-delete{background:#cf746b}.version-copy{flex:1;min-width:0;display:grid;gap:3px}.version-copy b{color:#3d3850;font-size:13px}.version-copy small{color:#827e90;font-size:11px}.version-copy code{overflow-wrap:anywhere;color:#645a7c;font-size:11px}@media(max-width:900px){.cache-card{flex-wrap:wrap}.cache-actions{width:100%;justify-content:flex-start}.file-row-actions{width:112px;max-width:112px;flex-wrap:wrap;justify-content:flex-end}.file-row-actions .icon-button{flex:0 0 30px;width:30px;height:32px}.file-table-head,.file-row{grid-template-columns:minmax(0,1fr) 90px 55px 112px;column-gap:8px}.file-subtabs{max-width:100%}}
.file-toolbar{min-width:0}.breadcrumbs{flex:1 1 auto;width:0;min-width:0;overflow-x:auto;overflow-y:hidden;scrollbar-width:thin;scrollbar-color:#c9c3d7 transparent;overscroll-behavior-x:contain}.breadcrumbs>*{flex:0 0 auto}.breadcrumbs button{max-width:clamp(100px,20vw,190px)}.breadcrumbs::-webkit-scrollbar{height:4px}.breadcrumbs::-webkit-scrollbar-thumb{border-radius:4px;background:#c9c3d7}
.file-limit-note{display:flex;align-items:center;gap:8px;margin:-2px 0 10px;padding:9px 11px;border:1px solid #eadfcb;border-radius:9px;background:#fffaf1;color:#806b46;font-size:12px;line-height:1.45}.file-limit-note>svg{flex:none;color:#b48236}.file-limit-note>span{flex:1;min-width:0}.file-limit-note>button{flex:none;border:0;background:transparent;color:#6354d9;font-size:12px;cursor:pointer}.file-row-actions{width:auto;max-width:none;min-width:0;flex-wrap:nowrap;align-items:center;gap:6px;opacity:1}.file-row-actions>.button{flex:none}.file-more-actions,.cache-manage-actions,.cache-guidance{position:relative}.file-more-actions>summary,.cache-manage-actions>summary,.cache-guidance>summary,.file-permission-disclosure>summary{list-style:none;cursor:pointer}.file-more-actions>summary::-webkit-details-marker,.cache-manage-actions>summary::-webkit-details-marker,.cache-guidance>summary::-webkit-details-marker,.file-permission-disclosure>summary::-webkit-details-marker{display:none}.file-more-actions>summary::marker,.cache-manage-actions>summary::marker,.cache-guidance>summary::marker,.file-permission-disclosure>summary::marker{content:''}.file-row-menu,.cache-action-menu{position:absolute;top:calc(100% + 5px);right:0;z-index:12;min-width:178px;display:grid;gap:2px;padding:5px;border:1px solid #e8e6ef;border-radius:10px;background:#fff;box-shadow:0 8px 22px #29283720}.file-row-menu button,.cache-action-menu button{width:100%;min-height:34px;padding:0 9px;display:flex;align-items:center;gap:8px;border:0;border-radius:7px;background:transparent;color:#555461;text-align:left;font:inherit;font-size:12px;cursor:pointer}.file-row-menu button:not(:disabled):hover,.cache-action-menu button:not(:disabled):hover{background:#f5f3fb;color:#5949d4}.file-row-menu button:disabled,.cache-action-menu button:disabled{color:#aaa9b3;cursor:not-allowed}.file-row-menu button.danger-text,.cache-action-menu button.danger-text{color:#b85a68!important}.file-permission-disclosure{margin:0 0 10px}.file-permission-disclosure>summary{display:flex;align-items:center;gap:6px;color:#716f80;font-size:11px}.file-permission-disclosure[open]>summary{color:#5949d4}.file-permission-note{margin-top:6px;display:flex;align-items:center;justify-content:space-between;gap:10px;padding:8px 10px;border:1px solid #eae8f0;border-radius:9px;background:#fbfaff;color:#6f6d80;font-size:11px}.file-permission-note>span{line-height:1.5}.file-permission-note button{display:inline-flex;align-items:center;gap:3px;flex:none;border:0;background:transparent;color:#6354d9;font-size:11px;cursor:pointer}.cache-intro{flex-wrap:wrap}.cache-intro>span{min-width:90px}.cache-guidance{margin-left:auto}.cache-guidance>summary{padding:5px 7px;border:1px solid #e8e6ef;border-radius:7px;background:#fff;color:#777684;font-size:11px}.cache-guidance[open]>summary{border-color:#dcd8ff;color:#5949d4}.cache-guidance>div{position:absolute;top:calc(100% + 5px);right:0;z-index:12;width:min(320px,70vw);padding:11px 12px;border:1px solid #e8e6ef;border-radius:9px;background:#fff;box-shadow:0 8px 22px #29283720;color:#6f6d80;font-size:12px;line-height:1.55}.cache-actions{align-items:center}.cache-manage-actions>summary{white-space:nowrap}.file-table-head,.file-row{grid-template-columns:minmax(140px,1fr) 126px 65px 148px;column-gap:8px}@media(max-width:900px){.file-row-actions{width:auto;max-width:none;flex-wrap:nowrap}.file-row-actions .icon-button{flex:none;width:auto;height:auto}.file-table-head,.file-row{grid-template-columns:minmax(110px,1fr) 92px 52px 145px;column-gap:8px}}
.file-row-menu,.cache-action-menu{position:fixed;top:0;right:auto;left:0;max-height:min(240px,calc(100vh - 16px));overflow-y:auto}
.file-row-menu button small{margin-left:auto;color:#9a95a8;font-size:10px;font-weight:400;white-space:nowrap}.cache-empty{flex-wrap:wrap;text-align:center}@media(max-width:560px){.file-table-head{display:none}.file-table-head,.file-row{grid-template-columns:minmax(0,1fr);column-gap:0}.file-row{align-items:stretch;gap:3px;padding:9px 12px}.file-row .file-name-cell{width:100%;min-height:32px}.file-row .file-modified,.file-row .file-size{grid-column:1;padding-left:38px}.file-row .file-modified::before{content:'修改时间 · ';color:#9a98a5}.file-row .file-size::before{content:'大小 · ';color:#9a98a5}.file-row-actions{grid-column:1;justify-content:flex-start;flex-wrap:wrap;margin:3px 0 0 38px}.file-row-actions .row-download{min-height:32px}.file-row-actions .file-more-actions>summary{min-height:32px}}
.file-subtabs{display:flex;align-items:center;gap:6px;width:max-content;max-width:100%;margin:0 0 14px;padding:4px;border:1px solid #eae7f1;border-radius:11px;background:#f2eff8}.file-subtabs>button{appearance:none;min-height:34px;padding:0 12px;border:1px solid transparent;border-radius:8px;background:transparent;color:#716d82;font:inherit;font-size:12px;cursor:pointer}.file-subtabs>button.active{border-color:#ebe8f4;background:#fff;color:#59478e;box-shadow:0 2px 8px #45336714}.cache-section{display:grid;min-width:0;gap:12px}.cache-list{display:grid;min-width:0;gap:9px}.cache-card{display:flex;align-items:flex-start;min-width:0;gap:11px;padding:13px;border:1px solid #eae6f2;border-radius:12px;background:#fff;box-shadow:0 2px 8px #38275f08}.cache-copy{display:grid;flex:1;min-width:0;gap:5px}.cache-actions{display:flex;align-items:center;justify-content:flex-end;flex-wrap:wrap;gap:6px}.cache-empty{display:flex;align-items:center;justify-content:center;min-width:0;min-height:84px;gap:10px;padding:14px;border:1px dashed #ddd7ea;border-radius:12px;background:#fcfbfe;color:#7a7689;font-size:12px}.transfer-preview{display:grid;grid-template-columns:auto minmax(0,1fr);min-width:0;gap:8px 12px;padding:12px;border:1px solid #e9e5f1;border-radius:11px;background:#faf9fd}.transfer-preview code,.version-copy code{min-width:0;overflow-wrap:anywhere;word-break:break-word}.versions-list{display:grid;max-height:min(420px,60vh);min-width:0;gap:8px;overflow:auto}.version-row{display:flex;align-items:center;min-width:0;flex-wrap:wrap;gap:8px;padding:11px;border:1px solid #ebe7f2;border-radius:11px;background:#fff}.version-copy{display:grid;flex:1 1 210px;min-width:0;gap:3px}@media(max-width:620px){.cache-card{padding:11px}.cache-actions{width:100%;justify-content:flex-start}.transfer-preview{grid-template-columns:76px minmax(0,1fr);gap:7px 9px}.version-row>button{flex:1 1 calc(50% - 8px);justify-content:center}}
</style>
