<script setup lang="ts">
import eatingSvg from '../assets/squirrel-eating.svg?raw'
import logoSvg from '../assets/squirrel-logo.svg?raw'

withDefaults(defineProps<{
  variant?: 'logo' | 'eat'
  size?: number
  label?: string
}>(), { variant: 'logo', size: 40 })
</script>

<template>
  <!-- Both SVG strings are bundled, trusted assets; no remote/user markup is inserted. -->
  <span
    class="squirrel-mark"
    :class="`squirrel-mark--${variant}`"
    :style="{ width: `${size}px`, height: `${size}px` }"
    :role="label ? 'img' : undefined"
    :aria-label="label"
    :aria-hidden="label ? undefined : true"
  ><span class="squirrel-art" aria-hidden="true" v-html="variant === 'eat' ? eatingSvg : logoSvg"></span></span>
</template>

<style scoped>
.squirrel-mark { display: inline-flex; flex: 0 0 auto; vertical-align: middle; line-height: 0; }
.squirrel-art { display: block; width: 100%; height: 100%; }
.squirrel-mark :deep(svg) { display: block; width: 100%; height: 100%; overflow: visible; }
</style>
