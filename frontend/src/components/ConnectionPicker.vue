<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Check, ChevronDown, CirclePlus, HardDrive, Plus, Server, Sparkles } from 'lucide-vue-next'
import { setSelectedConnection, store } from '../store'
import type { Connection } from '../types'
const emit = defineEmits<{ add: []; demo: [] }>()
const open = ref(false)
const root = ref<HTMLElement>()
const trigger = ref<HTMLButtonElement>()
const current = computed(() => store.data.connections.find((c) => c.id === store.selectedConnectionId))
function outside(event: MouseEvent) { if (root.value && !root.value.contains(event.target as Node)) open.value = false }
function keydown(event: KeyboardEvent) {
  if (event.key !== 'Escape' || !open.value) return
  event.preventDefault()
  event.stopPropagation()
  open.value = false
  trigger.value?.focus()
}
onMounted(() => { document.addEventListener('mousedown', outside); document.addEventListener('keydown', keydown, true) })
onBeforeUnmount(() => { document.removeEventListener('mousedown', outside); document.removeEventListener('keydown', keydown, true) })
function choose(c: Connection) { setSelectedConnection(c.id); open.value = false; trigger.value?.focus() }
function addConnection() { open.value = false; trigger.value?.focus(); emit('add') }
function makeDemo() { open.value = false; trigger.value?.focus(); emit('demo') }
</script>
<template>
  <div ref="root" class="connection-picker">
    <button ref="trigger" class="connection-trigger" :aria-expanded="open" @click="open = !open">
      <span class="connection-trigger-icon"><HardDrive v-if="current?.kind === 'demo'" :size="16" /><Server v-else :size="16" /></span>
      <span class="connection-trigger-copy"><span class="connection-trigger-label">当前存储</span><strong>{{ current?.name || '选择存储连接' }}</strong></span>
      <span v-if="current?.kind === 'demo'" class="demo-mark">演示</span><ChevronDown :size="15" class="muted-icon" />
    </button>
    <div v-if="open" class="connection-menu">
      <div class="menu-heading">切换存储连接 <span>{{ store.data.connections.length }} 个</span></div>
      <button v-for="connection in store.data.connections" :key="connection.id" class="connection-option" @click="choose(connection)">
        <span class="option-icon" :class="connection.kind"><Sparkles v-if="connection.kind === 'demo'" :size="16" /><HardDrive v-else-if="connection.kind === 's3'" :size="16" /><Server v-else :size="16" /></span>
        <span class="option-copy"><strong>{{ connection.name }}</strong><small>{{ connection.kind === 'demo' ? '隔离演示空间' : connection.kind === 's3' ? 'S3 兼容存储' : 'WebDAV' }}</small></span>
        <Check v-if="connection.id === store.selectedConnectionId" :size="16" class="purple-icon" />
      </button>
      <div v-if="!store.data.connections.length" class="menu-empty">还没有存储连接</div>
      <div class="menu-divider"></div>
      <button class="menu-action" @click="addConnection"><Plus :size="16" />添加存储连接</button>
      <button class="menu-action" @click="makeDemo"><CirclePlus :size="16" />建立演示空间</button>
    </div>
  </div>
</template>
