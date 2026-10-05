<script setup lang="ts">
import { Archive, ChevronRight, Cloud } from 'lucide-vue-next'
defineEmits<{ navigate: [page: 'gateways' | 'backups'] }>()
</script>

<template>
  <div class="page-view tools-page">
    <header class="page-heading"><h1>工具</h1></header>
    <div class="tool-list">
      <button class="tool-entry" @click="$emit('navigate', 'gateways')">
        <span class="tool-symbol"><Cloud :size="21" /></span>
        <span class="tool-copy"><b>WebDAV 共享</b><small>让其他应用通过 WebDAV 访问存储。</small></span>
        <ChevronRight :size="17" />
      </button>
      <button class="tool-entry" @click="$emit('navigate', 'backups')">
        <span class="tool-symbol"><Archive :size="21" /></span>
        <span class="tool-copy"><b>备份与迁移</b><small>保留独立备份，或将文件转移到另一个存储。</small></span>
        <ChevronRight :size="17" />
      </button>
    </div>
  </div>
</template>

<style scoped>
.tool-list { display: grid; gap: 12px; }
.tool-entry { display: flex; align-items: center; gap: 14px; width: 100%; padding: 18px; border: 1px solid #e9e8ef; border-radius: 12px; background: white; text-align: left; color: #9694a2; box-shadow: 0 2px 5px #29283704; }
.tool-entry:hover { border-color: #d7d2f7; background: #fdfcff; }
.tool-symbol { display: grid; place-items: center; flex: 0 0 42px; height: 42px; border-radius: 11px; background: #f3f1ff; color: #7867da; }
.tool-copy { flex: 1; min-width: 0; display: grid; gap: 6px; }
.tool-copy b { font-size: 14px; font-weight: 650; color: #454251; }
.tool-copy small { color: #777684; font-size: 12px; line-height: 1.6; }
.tool-entry > svg { flex-shrink: 0; }
</style>
