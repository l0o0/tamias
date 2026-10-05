import { createApp } from 'vue'
import App from './App.vue'
import './style.css'

// Wails injects its native bridge, but the DOM drag handlers live in this module.
// Load it only inside the desktop host; browser previews have no native runtime.
if (window.location.protocol === 'wails:' || window.location.hostname === 'wails.localhost') {
  const runtime = document.createElement('script')
  runtime.type = 'module'
  runtime.src = '/wails/runtime.js'
  document.head.appendChild(runtime)
}

createApp(App).mount('#app')
