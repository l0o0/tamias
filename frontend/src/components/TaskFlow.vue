<script setup lang="ts">
import { computed } from 'vue'
import type { Job } from '../types'

const props = defineProps<{ job: Pick<Job, 'status' | 'direction' | 'enabled' | 'queueDone' | 'queueTotal' | 'activeAction'> }>()

const activeTransfer = computed(() => props.job.status === 'running' && props.job.enabled
  && (props.job.activeAction === 'upload' || props.job.activeAction === 'download')
  ? props.job.activeAction
  : '')
const configuredDirection = computed(() => {
  if (props.job.direction === 'upload' || props.job.direction === 'mirror-upload') return 'upload'
  if (props.job.direction === 'download' || props.job.direction === 'mirror-download') return 'download'
  return ''
})
const scanning = computed(() => ['scanning', 'retrying'].includes(props.job.status))
const direction = computed(() => scanning.value
  ? ''
  : props.job.status === 'running' ? activeTransfer.value : configuredDirection.value)
const hasTotal = computed(() => props.job.status === 'running' && Number.isFinite(props.job.queueTotal) && (props.job.queueTotal || 0) > 0)
const completed = computed(() => Math.max(0, props.job.queueDone || 0))
const total = computed(() => Math.max(0, props.job.queueTotal || 0))
const headline = computed(() => {
  if (hasTotal.value) return `${completed.value} / ${total.value}`
  if (scanning.value) return '核对中'
  if (props.job.status === 'running') {
    if (activeTransfer.value === 'upload') return '上传中'
    if (activeTransfer.value === 'download') return '下载中'
    if (!props.job.enabled) return '正在停止'
    return '处理中'
  }
  if (props.job.status === 'paused') return '已暂停'
  if (props.job.status === 'waiting') return '等待网络'
  if (['failed', 'error'].includes(props.job.status)) return '失败'
  if (['attention', 'needs_attention'].includes(props.job.status)) return '需要处理'
  return ''
})
const directionText = computed(() => direction.value === 'upload' ? '上传 →' : direction.value === 'download' ? '← 下载' : '')
const accessibleLabel = computed(() => {
  if (scanning.value) return '同步进度，正在核对本地与远端目录'
  const flow = direction.value === 'upload' ? '上传，文件由本地流向远端'
    : direction.value === 'download' ? '下载，文件由远端流向本地' : '当前没有文件传输方向'
  return hasTotal.value
    ? `已完成 ${completed.value} 项，共 ${total.value} 项。${flow}`
    : `${headline.value || '同步进度'}。${flow}`
})
</script>

<template>
  <div
    class="task-flow"
    :class="[
      { 'is-scanning': scanning, 'is-active': !!activeTransfer },
      direction ? `flow-${direction}` : '',
    ]"
    role="img"
    :aria-label="accessibleLabel"
  >
    <span class="flow-headline" aria-hidden="true">{{ headline }}</span>
    <div class="flow-line" aria-hidden="true">
      <svg class="flow-track" viewBox="0 0 200 10" preserveAspectRatio="none" focusable="false">
        <path class="flow-base-path" d="M 5 5 H 195" />
        <path class="flow-active-path" d="M 5 5 H 195" />
      </svg>
      <span class="flow-endpoint flow-origin"></span>
      <span class="flow-endpoint flow-destination"></span>
      <i class="flow-packet"></i>
      <i class="flow-packet flow-packet-secondary"></i>
    </div>
    <span v-if="directionText" class="flow-direction" aria-hidden="true">{{ directionText }}</span>
  </div>
</template>

<style scoped>
.task-flow {
  display: grid;
  grid-template-rows: 15px 10px 14px;
  align-items: center;
  justify-items: stretch;
  min-width: 0;
  width: 100%;
  color: #777684;
  text-align: center;
  font-size: 11px;
  line-height: 1;
  font-variant-numeric: tabular-nums;
}
.flow-headline,
.flow-direction {
  min-width: 0;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}
.flow-headline { font-weight: 600; }
.flow-direction { color: #777684; font-size: 10px; }
.flow-line {
  position: relative;
  height: 10px;
  margin: 0 5px;
  color: #777684;
}
.flow-track {
  position: absolute;
  inset: 0;
  display: block;
  width: 100%;
  height: 100%;
  overflow: visible;
}
.flow-base-path,
.flow-active-path {
  fill: none;
  stroke: #d9d7e3;
  stroke-linecap: round;
  vector-effect: non-scaling-stroke;
}
.flow-base-path { stroke-width: 1.5; }
.flow-active-path {
  stroke: currentColor;
  stroke-width: 1.5;
  stroke-dasharray: 3 5;
  stroke-dashoffset: 0;
  opacity: 0;
}
.flow-upload .flow-line { color: #348d68; }
.flow-upload .flow-base-path { stroke: #b8d8c8; }
.flow-download .flow-line { color: #5878cf; }
.flow-download .flow-base-path { stroke: #c5ceed; }
.flow-endpoint {
  position: absolute;
  top: 50%;
  width: 4px;
  height: 4px;
  border-radius: 50%;
  background: currentColor;
  opacity: .72;
  transform: translate(-50%, -50%);
}
.flow-origin { left: 2.5%; }
.flow-destination { left: 97.5%; }
.flow-line i {
  position: absolute;
  top: 50%;
  left: 2.5%;
  z-index: 1;
  display: none;
  width: 4px;
  height: 4px;
  border-radius: 50%;
  background: currentColor;
  transform: translate(-50%, -50%);
}
.is-active .flow-active-path {
  opacity: 1;
  animation: dash-right .7s linear infinite;
}
.is-active .flow-endpoint { animation: endpoint-breathe 1.4s ease-in-out infinite; }
.is-active .flow-line i {
  display: block;
  animation: packet-right 1.4s linear infinite;
}
.is-active .flow-line .flow-packet-secondary { animation-delay: -.7s; }
.is-active.flow-download .flow-active-path { animation-name: dash-left; }
.is-active.flow-download .flow-line i { animation-name: packet-left; }
.is-scanning .flow-base-path { stroke: #ccc9dd; }
.is-scanning .flow-active-path,
.is-scanning .flow-endpoint { display: none; }
.is-scanning .flow-line .flow-packet-secondary { display: none; }
.is-scanning .flow-line i {
  display: block;
  left: 50%;
  width: 5px;
  height: 5px;
  background: #8b84b8;
  animation: scan-pulse 1.1s ease-in-out infinite;
}
.is-scanning .flow-headline { color: #817b9e; }
@keyframes dash-right {
  to { stroke-dashoffset: -8; }
}
@keyframes dash-left {
  to { stroke-dashoffset: 8; }
}
@keyframes packet-right {
  0% { left: 2.5%; opacity: 0; }
  8%, 92% { opacity: 1; }
  100% { left: 97.5%; opacity: 0; }
}
@keyframes packet-left {
  0% { left: 97.5%; opacity: 0; }
  8%, 92% { opacity: 1; }
  100% { left: 2.5%; opacity: 0; }
}
@keyframes endpoint-breathe {
  0%, 100% { opacity: .55; transform: translate(-50%, -50%) scale(.82); }
  50% { opacity: 1; transform: translate(-50%, -50%) scale(1.2); }
}
@keyframes scan-pulse {
  0%, 100% { opacity: .4; transform: translate(-50%, -50%) scale(.8); }
  50% { opacity: 1; transform: translate(-50%, -50%) scale(1.15); }
}
@media (prefers-reduced-motion: reduce) {
  .task-flow.is-active .flow-line .flow-active-path,
  .task-flow.is-active .flow-line .flow-endpoint,
  .task-flow.is-active .flow-line .flow-packet,
  .task-flow.is-scanning .flow-line .flow-packet { animation: none; }
  .task-flow.is-active .flow-line .flow-packet,
  .task-flow.is-scanning .flow-line .flow-packet { display: none; }
}
</style>
