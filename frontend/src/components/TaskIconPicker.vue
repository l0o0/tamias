<script setup lang="ts">
import type { Direction } from '../types'
import { normalizeTaskIcon, taskIconOptions, type TaskIconName } from '../taskIcons'
import BaseModal from './BaseModal.vue'
import TaskIcon from './TaskIcon.vue'

defineProps<{
  modelValue: boolean
  icon: string
  direction: Direction
  busy?: boolean
  error?: string
}>()
const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  select: [icon: TaskIconName]
}>()
</script>

<template>
  <BaseModal :model-value="modelValue" title="选择任务图标" width="430px" :dismissible="!busy" @update:model-value="emit('update:modelValue', $event)">
    <div class="task-icon-picker">
      <div class="task-icon-picker-grid" role="group" aria-label="内置任务图标">
        <button
          v-for="option in taskIconOptions"
          :key="option.id || 'default'"
          type="button"
          class="task-icon-option"
          :class="{ selected: normalizeTaskIcon(icon) === option.id }"
          :aria-label="`选择任务图标：${option.label}`"
          :aria-pressed="normalizeTaskIcon(icon) === option.id"
          :disabled="busy"
          @click="emit('select', option.id)"
        >
          <span class="task-icon-option-art"><TaskIcon :icon="option.id" :direction="direction" :size="21" /></span>
          <span class="task-icon-option-label">{{ option.label }}</span>
        </button>
      </div>
      <div v-if="busy" class="task-icon-picker-state" role="status">正在保存图标…</div>
      <div v-if="error" class="task-icon-picker-error" role="alert">{{ error }}</div>
      <footer class="modal-actions"><button class="button secondary" type="button" :disabled="busy" @click="emit('update:modelValue', false)">关闭</button></footer>
    </div>
  </BaseModal>
</template>

<style scoped>
.task-icon-picker{display:grid;gap:12px}
.task-icon-picker-grid{display:grid;grid-template-columns:repeat(5,minmax(0,1fr));gap:7px}
.task-icon-option{display:grid;justify-items:center;align-content:center;gap:5px;min-width:0;min-height:63px;padding:6px 3px;border:1px solid #e9e7f0;border-radius:9px;background:#fff;color:#686576}
.task-icon-option:hover:not(:disabled){border-color:#c9c2f5;background:#faf9ff;color:#5548df}
.task-icon-option.selected{border-color:#a89cf4;background:#f4f1ff;color:#5948da;box-shadow:0 0 0 1px rgba(99,84,255,.08)}
.task-icon-option:focus-visible{outline:3px solid rgba(99,84,255,.24);outline-offset:2px}
.task-icon-option-art{display:grid;place-items:center;height:24px}
.task-icon-option-label{max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:10px;line-height:1.2}
.task-icon-picker-state,.task-icon-picker-error{padding:8px 10px;border-radius:8px;font-size:12px;line-height:1.4}
.task-icon-picker-state{background:#f7f6fc;color:#777487}
.task-icon-picker-error{border:1px solid #efd4d0;background:#fff7f6;color:#a34f45;overflow-wrap:anywhere}
@media(max-width:480px){.task-icon-picker-grid{grid-template-columns:repeat(4,minmax(0,1fr))}}
</style>
