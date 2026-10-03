<script lang="ts">
import { reactive } from 'vue'
// Shared by all instances, including nested confirmation dialogs.
const modalStack = reactive<symbol[]>([])
const modalBackdrops = new Map<symbol, HTMLElement>()
const originalInertStates = new Map<HTMLElement, boolean>()
</script>
<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { X } from 'lucide-vue-next'
const props = withDefaults(defineProps<{ modelValue: boolean; title: string; subtitle?: string; width?: string; dismissible?: boolean }>(), { dismissible: true })
const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>()
const modalId = Symbol('base-modal')
const shell = ref<HTMLElement>()
const backdrop = ref<HTMLElement>()
let returnFocus: HTMLElement | null = null
let lastDialogFocus: HTMLElement | null = null

function activateModal() {
  const existingIndex = modalStack.indexOf(modalId)
  if (existingIndex >= 0) modalStack.splice(existingIndex, 1)
  modalStack.push(modalId)
}

function deactivateModal() {
  const index = modalStack.indexOf(modalId)
  if (index < 0) return
  modalStack.splice(index, 1)
  modalBackdrops.delete(modalId)
  syncModalBackground()
}

function isTopModal() {
  return modalStack[modalStack.length - 1] === modalId
}

const focusableSelector = [
  'a[href]', 'area[href]', 'button:not(:disabled)', 'input:not(:disabled):not([type="hidden"])',
  'select:not(:disabled)', 'textarea:not(:disabled)', 'iframe', '[contenteditable="true"]',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

function focusableElements() {
  return visibleFocusableElements(shell.value)
}

function toastFocusableElements() {
  return visibleFocusableElements(document.querySelector<HTMLElement>('[data-toast-region]'))
}

function visibleFocusableElements(root: HTMLElement | null | undefined) {
  return [...(root?.querySelectorAll<HTMLElement>(focusableSelector) ?? [])].filter((element) =>
    element.getClientRects().length > 0 && getComputedStyle(element).visibility !== 'hidden' &&
    !element.closest('[inert], [aria-hidden="true"]'),
  )
}

function syncModalBackground() {
  if (!modalStack.length) {
    for (const [element, wasInert] of originalInertStates) {
      if (element.isConnected) element.inert = wasInert
    }
    originalInertStates.clear()
    return
  }

  const activeBackdrop = modalBackdrops.get(modalStack[modalStack.length - 1]!)
  for (const element of document.body.children) {
    if (!(element instanceof HTMLElement) || element.hasAttribute('data-toast-region')) continue
    if (!originalInertStates.has(element)) originalInertStates.set(element, element.inert)
    element.inert = element !== activeBackdrop
  }
}

function restoreFocus() {
  if (returnFocus?.isConnected && !returnFocus.closest('[inert]')) returnFocus.focus()
  returnFocus = null
}

function close() {
  if (!props.dismissible) return
  emit('update:modelValue', false)
}

function focusLastDialogElement() {
  const dialogElements = focusableElements()
  if (lastDialogFocus && dialogElements.includes(lastDialogFocus)) lastDialogFocus.focus()
  else (dialogElements[0] ?? shell.value)?.focus()
}

function onFocusIn(event: FocusEvent) {
  if (!isTopModal()) return
  const target = event.target
  if (target instanceof HTMLElement && shell.value?.contains(target)) lastDialogFocus = target
}

function onClick(event: MouseEvent) {
  if (!isTopModal()) return
  const target = event.target
  if (!(target instanceof Element) || !target.closest('[data-toast-region]') || !target.closest('button')) return
  focusLastDialogElement()
}

function onKeydown(event: KeyboardEvent) {
  if (!props.modelValue || !isTopModal()) return
  if (event.key === 'Escape') {
    event.preventDefault()
    event.stopPropagation()
    close()
    return
  }
  if (event.key !== 'Tab' || !shell.value) return
  const focusables = focusableElements()
  const dialogList = focusables.length ? focusables : [shell.value]
  const toastList = toastFocusableElements()
  const active = document.activeElement
  const firstDialog = dialogList[0]!, lastDialog = dialogList[dialogList.length - 1]!
  const firstToast = toastList[0]
  const lastToast = toastList[toastList.length - 1]
  if (active === shell.value || (!shell.value.contains(active) && !toastList.includes(active as HTMLElement))) {
    event.preventDefault()
    ;(event.shiftKey ? lastToast ?? lastDialog : firstDialog).focus()
  } else if (toastList.includes(active as HTMLElement)) {
    if (event.shiftKey && active === firstToast) {
      event.preventDefault()
      lastDialog.focus()
    } else if (!event.shiftKey && active === lastToast) {
      event.preventDefault()
      firstDialog.focus()
    }
  } else if (event.shiftKey && active === firstDialog) {
    event.preventDefault()
    ;(lastToast ?? lastDialog).focus()
  } else if (!event.shiftKey && active === lastDialog) {
    event.preventDefault()
    ;(firstToast ?? firstDialog).focus()
  }
}

function addModalListeners() {
  document.addEventListener('keydown', onKeydown, true)
  document.addEventListener('focusin', onFocusIn, true)
  document.addEventListener('click', onClick, true)
}

function removeModalListeners() {
  document.removeEventListener('keydown', onKeydown, true)
  document.removeEventListener('focusin', onFocusIn, true)
  document.removeEventListener('click', onClick, true)
}

watch(() => props.modelValue, async (open) => {
  if (open) {
    returnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
    activateModal()
    await nextTick()
    if (backdrop.value) modalBackdrops.set(modalId, backdrop.value)
    syncModalBackground()
    addModalListeners()
    lastDialogFocus = null
    const firstInput = shell.value?.querySelector<HTMLElement>('input:not([type="hidden"]):not(:disabled),select:not(:disabled),textarea:not(:disabled)')
    ;(firstInput && focusableElements().includes(firstInput) ? firstInput : focusableElements()[0] ?? shell.value)?.focus()
  } else {
    deactivateModal()
    removeModalListeners()
    await nextTick()
    restoreFocus()
    lastDialogFocus = null
  }
}, { immediate: true })
onBeforeUnmount(() => {
  deactivateModal()
  removeModalListeners()
  restoreFocus()
})
</script>
<template>
  <Teleport to="body">
    <div v-if="modelValue" ref="backdrop" class="modal-backdrop" @mousedown.self="close">
      <section ref="shell" class="modal-shell" :style="{ '--modal-width': width || '520px' }" role="dialog" :aria-modal="isTopModal() ? 'true' : undefined" :aria-label="title" :aria-owns="isTopModal() ? 'app-toast-region' : undefined" tabindex="-1">
        <header class="modal-header">
          <div><h2>{{ title }}</h2><p v-if="subtitle">{{ subtitle }}</p></div>
          <button class="icon-button modal-close" aria-label="关闭弹窗" :disabled="!dismissible" @click="close"><X :size="18" /></button>
        </header>
        <div class="modal-content"><slot /></div>
      </section>
    </div>
  </Teleport>
</template>
