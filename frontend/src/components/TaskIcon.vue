<script setup lang="ts">
import { computed, type Component } from 'vue'
import { Archive, ArrowDownToLine, ArrowLeftRight, ArrowUpFromLine, BookOpen, Briefcase, Cloud, Code2, FileText, Folder, GraduationCap, Heart, Image, Music2, Star, Video } from 'lucide-vue-next'
import type { Direction } from '../types'
import { normalizeTaskIcon } from '../taskIcons'
import SquirrelMark from './SquirrelMark.vue'

const props = withDefaults(defineProps<{
  icon?: string
  direction: Direction
  size?: number
  running?: boolean
}>(), { icon: '', size: 18, running: false })

const iconComponents: Record<string, Component> = {
  folder: Folder,
  document: FileText,
  book: BookOpen,
  graduation: GraduationCap,
  code: Code2,
  image: Image,
  music: Music2,
  video: Video,
  archive: Archive,
  cloud: Cloud,
  briefcase: Briefcase,
  heart: Heart,
  star: Star,
}
const directionComponents: Record<Direction, Component> = {
  both: ArrowLeftRight,
  upload: ArrowUpFromLine,
  download: ArrowDownToLine,
  'mirror-upload': ArrowUpFromLine,
  'mirror-download': ArrowDownToLine,
}
const icon = computed(() => normalizeTaskIcon(props.icon))
const isDefaultRunning = computed(() => !icon.value && props.running)
const component = computed(() => icon.value ? iconComponents[icon.value] : directionComponents[props.direction])
</script>

<template>
  <SquirrelMark v-if="isDefaultRunning" variant="eat" :size="size" />
  <SquirrelMark v-else-if="icon === 'squirrel'" variant="logo" :size="size" />
  <component v-else :is="component" :size="size" />
</template>
