import { reactive } from 'vue'
import { api } from './api'
import type { AppState } from './types'

const emptyState: AppState = {
  version: '', connections: [], jobs: [], gateways: [], activities: [],
  disk: { stagingBytes: 0, stagingLimit: 0, recoveryBytes: 0, recoveryLimit: 0, cacheBytes: 0, cacheLimit: 0, freeBytes: 0 },
  dataDir: '',
}
export const store = reactive({
  data: emptyState as AppState,
  loading: true,
  refreshing: false,
  selectedConnectionId: localStorage.getItem('tami-connection') || '',
  error: '',
  lastLoaded: '',
  toast: { id: 0, message: '', tone: 'error' as 'error' | 'success' | 'info' },
})
let toastTimer: number | undefined
export function notify(message: string, tone: 'error' | 'success' | 'info' = 'error') {
  store.toast = { id: Date.now(), message, tone }
  if (toastTimer) window.clearTimeout(toastTimer)
  toastTimer = undefined
  if (tone !== 'error') {
    toastTimer = window.setTimeout(() => {
      store.toast.message = ''
      toastTimer = undefined
    }, 3600)
  }
}
export function setSelectedConnection(id: string) {
  store.selectedConnectionId = id
  if (id) localStorage.setItem('tami-connection', id)
  else localStorage.removeItem('tami-connection')
}
export async function refreshState(quiet = false) {
  if (store.refreshing) return
  store.refreshing = true
  if (!store.lastLoaded) store.loading = true
  try {
    const data = await api.get<AppState>('/api/state')
    store.data = data
    store.lastLoaded = new Date().toISOString()
    if (!data.connections.some((c) => c.id === store.selectedConnectionId)) setSelectedConnection(data.connections[0]?.id || '')
    store.error = ''
  } catch (error) {
    const message = error instanceof Error ? error.message : '无法连接到本地服务'
    store.error = message
    if (!quiet) notify(message)
  } finally {
    store.loading = false
    store.refreshing = false
  }
}
export const selectedConnection = () => store.data.connections.find((c) => c.id === store.selectedConnectionId)
