<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ChevronLeft, ChevronRight, Folder, RefreshCw } from 'lucide-vue-next'
import { api } from '../api'
import BaseModal from './BaseModal.vue'
import type { FileEntry } from '../types'

const props = defineProps<{
  modelValue: boolean
  connectionId: string
  initialPath: string
}>()

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  select: [path: string]
}>()

const currentPath = ref('')
const folders = ref<FileEntry[]>([])
const loading = ref(false)
const error = ref('')
const verifiedPath = ref<string | null>(null)
const verifiedConnectionId = ref('')
const crumbs = computed(() => currentPath.value.split('/').filter(Boolean))
const parentPath = computed(() => crumbs.value.slice(0, -1).join('/'))
const canConfirm = computed(() =>
  props.modelValue && !!props.connectionId && !loading.value && !error.value &&
  verifiedPath.value === currentPath.value && verifiedConnectionId.value === props.connectionId,
)
let requestId = 0

function normalizePath(path: string) {
  return path.trim().replace(/^\/+|\/+$/g, '')
}

function clearListing() {
  requestId++
  folders.value = []
  loading.value = false
  error.value = ''
  verifiedPath.value = null
  verifiedConnectionId.value = ''
}

async function listDirectory(connectionId: string, path: string) {
  const params = new URLSearchParams({ connectionId, path })
  return api.get<{ entries: FileEntry[] }>(`/api/files?${params.toString()}`)
}

async function loadFolder() {
  const request = ++requestId
  const connectionId = props.connectionId
  const path = currentPath.value
  folders.value = []
  error.value = ''
  verifiedPath.value = null
  verifiedConnectionId.value = ''

  if (!connectionId) {
    loading.value = false
    error.value = '请先选择远端连接。'
    return
  }

  loading.value = true
  try {
    if (path) {
      const separator = path.lastIndexOf('/')
      const parent = separator >= 0 ? path.slice(0, separator) : ''
      const name = separator >= 0 ? path.slice(separator + 1) : path
      const parentResult = await listDirectory(connectionId, parent)
      if (request !== requestId || connectionId !== props.connectionId || path !== currentPath.value || !props.modelValue) return
      const existsAsDirectory = (parentResult.entries || []).some((entry) =>
        entry.isDir && normalizePath(entry.path || [parent, entry.name].filter(Boolean).join('/')) === path,
      )
      if (!existsAsDirectory) {
        error.value = `远端文件夹“${name}”已不存在或无法访问，请返回根目录重新选择。`
        return
      }
    }

    const result = await listDirectory(connectionId, path)
    if (request !== requestId || connectionId !== props.connectionId || path !== currentPath.value || !props.modelValue) return
    folders.value = (result.entries || []).filter((entry) => entry.isDir)
    verifiedPath.value = path
    verifiedConnectionId.value = connectionId
  } catch (cause) {
    if (request === requestId && connectionId === props.connectionId && path === currentPath.value && props.modelValue) {
      error.value = cause instanceof Error ? cause.message : '读取远端文件夹失败'
    }
  } finally {
    if (request === requestId) loading.value = false
  }
}

function navigate(path: string) {
  const nextPath = normalizePath(path)
  if (nextPath === currentPath.value && verifiedPath.value === nextPath && verifiedConnectionId.value === props.connectionId) return
  currentPath.value = nextPath
  void loadFolder()
}

function enterFolder(folder: FileEntry) {
  navigate(folder.path || [currentPath.value, folder.name].filter(Boolean).join('/'))
}

function close() {
  clearListing()
  emit('update:modelValue', false)
}

function onModalUpdate(open: boolean) {
  if (!open) close()
  else emit('update:modelValue', true)
}

function confirmSelection() {
  if (!canConfirm.value) return
  emit('select', currentPath.value)
  close()
}

watch(() => props.modelValue, (open) => {
  if (open) {
    currentPath.value = normalizePath(props.initialPath)
    void loadFolder()
  } else {
    clearListing()
  }
})

watch(() => props.connectionId, () => {
  if (!props.modelValue) return
  currentPath.value = ''
  void loadFolder()
})
</script>

<template>
  <BaseModal
    :model-value="modelValue"
    title="选择远端文件夹"
    width="560px"
    @update:model-value="onModalUpdate"
  >
    <div class="remote-folder-picker">
      <div class="remote-folder-toolbar">
        <button
          type="button"
          class="button secondary small"
          :disabled="!currentPath"
          aria-label="返回上一级文件夹"
          @click="navigate(parentPath)"
        >
          <ChevronLeft :size="14" />上一级
        </button>
        <nav class="remote-folder-crumbs" aria-label="远端文件夹路径">
          <button type="button" :class="{ current: !currentPath }" @click="navigate('')">根目录</button>
          <template v-for="(crumb, index) in crumbs" :key="`${index}-${crumb}`">
            <ChevronRight :size="13" aria-hidden="true" />
            <button
              type="button"
              :class="{ current: index === crumbs.length - 1 }"
              :aria-current="index === crumbs.length - 1 ? 'location' : undefined"
              @click="navigate(crumbs.slice(0, index + 1).join('/'))"
            >{{ crumb }}</button>
          </template>
        </nav>
        <button
          type="button"
          class="icon-button"
          :disabled="loading || !connectionId"
          aria-label="刷新当前文件夹"
          @click="loadFolder"
        >
          <RefreshCw :size="15" :class="{ spin: loading }" />
        </button>
      </div>

      <div class="remote-folder-list" :aria-busy="loading">
        <div v-if="loading" class="remote-folder-state" role="status">
          <RefreshCw :size="17" class="spin" />正在读取文件夹…
        </div>
        <div v-else-if="error" class="remote-folder-error" role="alert">
          <span>{{ error }}</span>
          <button type="button" class="button secondary small" :disabled="!connectionId" @click="loadFolder">重试</button>
        </div>
        <div v-else-if="folders.length" class="remote-folder-entries">
          <button
            v-for="folder in folders"
            :key="folder.path || folder.name"
            type="button"
            class="remote-folder-entry"
            :aria-label="`打开文件夹 ${folder.name}`"
            @click="enterFolder(folder)"
          >
            <Folder :size="16" aria-hidden="true" />
            <span>{{ folder.name }}</span>
            <ChevronRight :size="15" aria-hidden="true" />
          </button>
        </div>
        <div v-else class="remote-folder-empty">此处没有子文件夹，仍可选择当前目录。</div>
      </div>

      <div class="remote-folder-selection" role="status">
        <span>当前选择</span>
        <code>{{ currentPath || '根目录' }}</code>
      </div>
      <footer class="modal-actions">
        <button type="button" class="button secondary" @click="close">取消</button>
        <button type="button" class="button primary" :disabled="!canConfirm" @click="confirmSelection">选择此文件夹</button>
      </footer>
    </div>
  </BaseModal>
</template>

<style scoped>
.remote-folder-picker{display:grid;gap:12px;min-width:0}
.remote-folder-toolbar{display:flex;align-items:center;gap:8px;min-width:0;padding:8px;border:1px solid #eeebf4;border-radius:10px;background:#fbfaff}
.remote-folder-toolbar>.button{flex:0 0 auto}
.remote-folder-crumbs{display:flex;align-items:center;gap:4px;flex:1;min-width:0;overflow-x:auto;scrollbar-width:thin}
.remote-folder-crumbs svg{flex:0 0 auto;color:#bab7c6}
.remote-folder-crumbs button{max-width:120px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;padding:5px 3px;border:0;border-radius:5px;background:transparent;color:#777487;font-size:12px}
.remote-folder-crumbs button.current{color:#4d4565;font-weight:650}
.remote-folder-crumbs button:not(:disabled):hover{color:#5548df;background:#f2efff}
.remote-folder-list{min-height:190px;max-height:min(42vh,360px);overflow-y:auto;border:1px solid #eeebf4;border-radius:10px;background:#fff}
.remote-folder-state,.remote-folder-empty{display:flex;align-items:center;justify-content:center;gap:8px;min-height:190px;padding:18px;color:#858194;font-size:12px;text-align:center}
.remote-folder-error{display:flex;align-items:center;justify-content:space-between;gap:12px;min-height:190px;padding:18px;color:#a34f45;font-size:12px;overflow-wrap:anywhere}
.remote-folder-entries{padding:4px 10px}
.remote-folder-entry{display:flex;align-items:center;gap:10px;width:100%;min-height:42px;padding:7px 8px;border:0;border-bottom:1px solid #f0edf5;background:transparent;color:#575367;text-align:left}
.remote-folder-entry:last-child{border-bottom:0}
.remote-folder-entry>svg:first-child{flex:0 0 auto;color:#796de0}
.remote-folder-entry>span{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:12px}
.remote-folder-entry>svg:last-child{flex:0 0 auto;color:#b4b1c0}
.remote-folder-entry:hover{background:#faf9ff;color:#5548df}
.remote-folder-entry:focus-visible,.remote-folder-crumbs button:focus-visible{outline:3px solid rgba(99,84,255,.24);outline-offset:1px}
.remote-folder-selection{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:10px 12px;border-radius:9px;background:#f7f6fc;color:#777487;font-size:11px}
.remote-folder-selection code{min-width:0;color:#50496a;font-size:11px;overflow-wrap:anywhere;text-align:right}
@media(max-width:520px){.remote-folder-toolbar{flex-wrap:wrap}.remote-folder-crumbs{flex-basis:calc(100% - 105px);order:1}.remote-folder-toolbar>.icon-button{margin-left:auto;order:1}}
</style>
