<script setup lang="ts">
import { Activity, ChevronRight, LayoutDashboard, Settings2 } from 'lucide-vue-next'
import BaseModal from './BaseModal.vue'

defineProps<{ modelValue: boolean; version: string }>()
const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  navigate: [page: 'settings' | 'activity' | 'tasks']
}>()

const destinations = [
  { id: 'settings' as const, label: '设置', description: '连接与本机空间', icon: Settings2 },
  { id: 'activity' as const, label: '活动', description: '查看操作记录', icon: Activity },
  { id: 'tasks' as const, label: '同步', description: '管理同步任务', icon: LayoutDashboard },
]
</script>

<template>
  <BaseModal
    :model-value="modelValue"
    title="帮助与关于"
    subtitle="小花鼠 Tamias 本机桌面版"
    width="440px"
    @update:model-value="emit('update:modelValue', $event)"
  >
    <div class="app-help-content">
      <div class="app-help-version"><span>当前版本</span><b>{{ version }}</b></div>
      <section class="app-help-tips" aria-label="使用说明">
        <p><span>1</span>添加存储连接，填写服务地址和账号。</p>
        <p><span>2</span>新建同步任务，选择本地和远端文件夹。</p>
        <p><span>3</span>查看同步预览，确认后开始。遇到问题可查看活动记录。</p>
      </section>
      <nav class="app-help-nav" aria-label="快速打开">
        <button v-for="item in destinations" :key="item.id" type="button" :aria-label="`打开${item.label}：${item.description}`" :title="`打开${item.label}`" @click="emit('navigate', item.id)">
          <component :is="item.icon" :size="16" />
          <span><b>{{ item.label }}</b><small>{{ item.description }}</small></span>
          <ChevronRight :size="15" />
        </button>
      </nav>
    </div>
  </BaseModal>
</template>

<style scoped>
.app-help-content { display: grid; gap: 15px; }
.app-help-version { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 10px 12px; border: 1px solid #ebeaf0; border-radius: 10px; background: #fbfaff; color: #888694; font-size: 12px; }
.app-help-version b { color: #555360; font-size: 12px; font-weight: 650; }
.app-help-tips { display: grid; gap: 9px; }
.app-help-tips p { display: flex; align-items: flex-start; gap: 9px; margin: 0; color: #686775; font-size: 12px; line-height: 1.55; }
.app-help-tips p span { display: grid; place-items: center; flex: 0 0 18px; width: 18px; height: 18px; border-radius: 6px; background: #f0efff; color: #6658e4; font-size: 10px; font-weight: 700; }
.app-help-nav { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; padding-top: 13px; border-top: 1px solid #f0eff4; }
.app-help-nav button { display: flex; align-items: center; gap: 7px; min-width: 0; min-height: 48px; padding: 7px 8px; border: 1px solid #ebeaf0; border-radius: 9px; background: #fff; color: #777583; text-align: left; }
.app-help-nav button:hover { border-color: #dcd8f7; background: #faf9ff; color: #6354d8; }
.app-help-nav button > svg:first-child { flex: 0 0 auto; color: #8177d5; }
.app-help-nav button > svg:last-child { flex: 0 0 auto; margin-left: auto; color: #aaa8b4; }
.app-help-nav button span { display: grid; min-width: 0; gap: 2px; }
.app-help-nav button b { color: #555360; font-size: 11px; font-weight: 650; }
.app-help-nav button small { overflow: hidden; color: #9998a3; font-size: 9px; text-overflow: ellipsis; white-space: nowrap; }
@media (max-width: 480px) {
  .app-help-nav { grid-template-columns: 1fr; }
  .app-help-nav button { min-height: 42px; }
}
</style>
