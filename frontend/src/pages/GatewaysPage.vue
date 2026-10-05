<script setup lang="ts">
import { computed, nextTick, reactive, ref, watch } from 'vue'
import { Activity, AlertTriangle, Check, Clipboard, ExternalLink, Eye, EyeOff, Globe2, HardDrive, KeyRound, Link2, LoaderCircle, Pause, Play, Plus, RefreshCw, Server, Settings2, Trash2 } from 'lucide-vue-next'
import { api } from '../api'
import BaseModal from '../components/BaseModal.vue'
import SquirrelMark from '../components/SquirrelMark.vue'
import { notify, refreshState, store } from '../store'
import type { Connection, Gateway, GatewayAccess } from '../types'
interface GatewayCheckStep { name: string; status: string; error?: string }
interface GatewayCheckResult { gatewayId: string; started: string; completed: string; readOnly: boolean; passed: boolean; steps: GatewayCheckStep[]; errors: string[]; residual: string[] }
const emit = defineEmits<{ addConnection: [] }>()
const showCreate = ref(false)
const saving = ref(false)
const createError = ref('')
const createAdvanced = ref<HTMLDetailsElement>()
const portInput = ref<HTMLInputElement>()
const actionId = ref('')
const deleting = ref(false)
let accessRequest = 0
const deleteTarget = ref<Gateway | null>(null)
const resetTarget = ref<Gateway | null>(null)
const generated = ref<{ gateway: Gateway; password: string } | null>(null)
const resetSecret = ref<{ gateway: Gateway; password: string } | null>(null)
const accessTarget = ref<Gateway | null>(null)
const accessRows = ref<GatewayAccess[]>([])
const accessLoading = ref(false)
const accessError = ref('')
const checkingId = ref('')
const checkResult = ref<GatewayCheckResult | null>(null)
const form = reactive({ name: '', connectionId: '', prefix: '', port: 19080, username: 'tamias', readOnly: true, listenHost: '127.0.0.1', tlsCert: '', tlsKey: '' })
const eligible = computed(() => store.data.connections.filter((c) => c.kind === 's3' || c.kind === 'demo'))
const formConnection = computed<Connection | undefined>(() => eligible.value.find((c) => c.id === form.connectionId))
const writableAllowed = computed(() => formConnection.value?.kind === 'demo' || !!(formConnection.value?.tested && formConnection.value?.capabilities?.conditionalWrite && formConnection.value?.capabilities?.conditionalDelete))
watch(showCreate, (open) => { if (open && !generated.value) { form.name = ''; form.connectionId = eligible.value.find((c) => c.id === store.selectedConnectionId)?.id || eligible.value[0]?.id || ''; form.prefix = ''; form.port = 19080; form.username = 'tamias'; form.readOnly = true; form.listenHost = '127.0.0.1'; form.tlsCert = ''; form.tlsKey = '' } })
watch(() => form.connectionId, () => { if (!writableAllowed.value) form.readOnly = true })
function openCreate() { generated.value = null; createError.value = ''; showCreate.value = true }
async function createGateway() {
  if (!form.connectionId || saving.value) return
  saving.value = true; createError.value = ''
  try {
    const result = await api.post<{ gateway: Gateway; password: string }>('/api/gateways', { name: form.name.trim(), connectionId: form.connectionId, prefix: form.prefix.trim().replace(/^\/+|\/+$/g, ''), port: Number(form.port), username: form.username.trim(), readOnly: form.readOnly || !writableAllowed.value, listenHost: form.listenHost.trim(), tlsCert: form.tlsCert.trim(), tlsKey: form.tlsKey.trim() })
    generated.value = result
    await refreshState(true)
    notify('共享已创建，请现在保存密码', 'success')
  } catch (error) {
    createError.value = error instanceof Error ? error.message : '创建共享失败'
    if (/端口|port|bind|IP|HTTPS|TLS|证书|私钥/i.test(createError.value) && createAdvanced.value) {
      createAdvanced.value.open = true
      await nextTick()
      if (/端口|port|bind/i.test(createError.value)) portInput.value?.focus()
    }
  }
  finally { saving.value = false }
}
async function toggleGateway(gateway: Gateway) {
  if (actionId.value) return
  actionId.value = gateway.id
  try { await api.post(`/api/gateways/${gateway.running ? 'stop' : 'start'}`, { id: gateway.id }); await refreshState(true); notify(gateway.running ? '共享已停止' : '共享已启动', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '共享操作失败') }
  finally { actionId.value = '' }
}
async function checkGateway(gateway: Gateway) {
  if (actionId.value || checkingId.value) return
  checkingId.value = gateway.id
  checkResult.value = null
  try { checkResult.value = await api.post<GatewayCheckResult>('/api/gateways/check', { id: gateway.id }); await refreshState(true) }
  catch (error) { notify(error instanceof Error ? error.message : '共享自检失败') }
  finally { checkingId.value = '' }
}
async function restartAfterReset() {
  if (!resetSecret.value || actionId.value) return
  const gateway = resetSecret.value.gateway
  actionId.value = gateway.id
  try { await api.post('/api/gateways/start', { id: gateway.id }); resetSecret.value = null; await refreshState(true); notify('共享已使用新密码启动', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '启动共享失败；请检查端口和证书配置') }
  finally { actionId.value = '' }
}
async function deleteGateway() {
  if (!deleteTarget.value || deleting.value) return
  const id = deleteTarget.value.id
  deleting.value = true
  try { await api.post('/api/gateways/delete', { id }); deleteTarget.value = null; await refreshState(true); notify('共享已删除', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '删除共享失败') }
  finally { deleting.value = false }
}
function resetPassword(gateway: Gateway) {
  if (actionId.value) return
  resetTarget.value = gateway
}
async function confirmPasswordReset() {
  const gateway = resetTarget.value
  if (!gateway || actionId.value) return
  actionId.value = `reset:${gateway.id}`
  try { const result = await api.post<{ password: string }>('/api/gateways/reset-password', { id: gateway.id }); resetTarget.value = null; resetSecret.value = { gateway, password: result.password }; await refreshState(true); notify('已重置密码；共享已停止，请保存新密码后重新启动。', 'success') }
  catch (error) { notify(error instanceof Error ? error.message : '重置共享密码失败') }
  finally { actionId.value = '' }
}
async function showAccess(gateway: Gateway) {
  const request = ++accessRequest
  accessTarget.value = gateway; accessRows.value = []; accessError.value = ''; accessLoading.value = true
  const current = () => request === accessRequest && accessTarget.value?.id === gateway.id
  try { const rows = await api.get<GatewayAccess[]>(`/api/gateways/access?id=${encodeURIComponent(gateway.id)}`); if (current()) accessRows.value = rows || [] }
  catch (error) { if (current()) accessError.value = error instanceof Error ? error.message : '读取访问记录失败' }
  finally { if (current()) accessLoading.value = false }
}
function bytes(value: number) { if (!value) return '0 B'; const units = ['B', 'KB', 'MB', 'GB', 'TB']; let n=value, i=0; while(n>=1024&&i<units.length-1){n/=1024;i++} return `${n.toFixed(i===0||n>=10?0:1)} ${units[i]}` }
function accessTime(value:string) { const d=new Date(value); return Number.isNaN(+d)?value:new Intl.DateTimeFormat('zh-CN',{month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit'}).format(d) }
async function copy(value: string, label: string) {
  try { await navigator.clipboard.writeText(value); notify(`${label}已复制`, 'success') }
  catch { notify('复制失败，请手动选择内容') }
}
function connectionName(id: string) { return store.data.connections.find((c) => c.id === id)?.name || '已移除的连接' }
</script>
<template>
  <div class="page-view gateways-page">
    <header class="page-heading"><div><h1>WebDAV 共享</h1></div><button class="button primary" :disabled="!eligible.length" @click="openCreate"><Plus :size="16" />创建共享</button></header>
    <div v-if="store.loading" class="gateway-loading-state" role="status"><SquirrelMark variant="eat" :size="42" /><span>正在读取共享…</span></div>
    <div v-else-if="store.data.gateways.length" class="gateway-grid">
      <article v-for="gateway in store.data.gateways" :key="gateway.id" class="gateway-card">
        <div class="gateway-card-head"><div class="gateway-mark"><Globe2 :size="18" /></div><div class="gateway-title"><h2>{{ gateway.name }}</h2><span class="status-pill" :class="gateway.running ? 'green' : 'gray'"><i></i>{{ gateway.running ? '运行中' : '已停止' }}</span></div></div>
        <div class="gateway-url-block"><div class="gateway-field-label">WebDAV 地址</div><div class="gateway-url-row"><code>{{ gateway.url || `http://127.0.0.1:${gateway.port}/` }}</code><button class="icon-button" title="复制地址" @click="copy(gateway.url || `http://127.0.0.1:${gateway.port}/`, '地址')"><Clipboard :size="15" /></button><a v-if="gateway.running && gateway.url" class="icon-button" :href="gateway.url" target="_blank" rel="noreferrer" title="在浏览器打开"><ExternalLink :size="15" /></a></div></div>
        <div class="gateway-meta"><div><span>映射到</span><b><HardDrive :size="14" />{{ connectionName(gateway.connectionId) }}<small>{{ gateway.prefix ? `/${gateway.prefix}` : '/' }}</small></b></div><div><span>访问权限</span><b>{{ gateway.readOnly ? '只读' : '读写' }}</b></div><div><span>独立账号</span><b><KeyRound :size="14" />{{ gateway.username }}<button class="mini-copy" title="复制用户名" @click="copy(gateway.username, '用户名')"><Clipboard :size="12" /></button></b></div></div>
        <div v-if="!gateway.readOnly" class="write-callout"><Settings2 :size="14" />读写入口已启用；操作会直接修改远端存储。</div>
        <footer class="gateway-card-footer"><span class="gateway-permission"><Eye v-if="gateway.readOnly" :size="14" /><EyeOff v-else :size="14" />{{ gateway.readOnly ? '只读访问' : '读写访问' }}</span><div class="gateway-footer-actions"><button class="button small" :class="gateway.running ? 'secondary' : 'primary'" :disabled="actionId !== '' || checkingId!==''" @click="toggleGateway(gateway)"><LoaderCircle v-if="actionId === gateway.id" :size="14" class="spin" /><Pause v-else-if="gateway.running" :size="14" /><Play v-else :size="14" />{{ actionId === gateway.id ? '请稍候…' : gateway.running ? '停止共享' : '启动共享' }}</button></div></footer>
        <details class="tool-disclosure gateway-maintenance"><summary>更多操作</summary><div class="gateway-maintenance-actions"><button class="button secondary small" :title="gateway.readOnly ? '验证读取与拒绝写入' : '自检会创建并清理临时文件'" :disabled="!gateway.running || actionId!=='' || checkingId!==''" @click="checkGateway(gateway)"><LoaderCircle v-if="checkingId===gateway.id" :size="14" class="spin"/><RefreshCw v-else :size="14"/>{{checkingId===gateway.id?'自检中…':'自检'}}</button><button class="button secondary small" @click="showAccess(gateway)"><Activity :size="14" />访问记录</button><button class="button secondary small" :disabled="actionId!==''" @click="resetPassword(gateway)"><KeyRound :size="14" />重置密码</button><button class="button subtle small danger-text" @click="deleteTarget = gateway"><Trash2 :size="14" />删除共享</button></div><p>{{gateway.access?.requests||0}} 次访问 · {{gateway.access?.errors||0}} 次错误 · {{bytes(gateway.access?.bytesIn||0)}} 上传 · {{bytes(gateway.access?.bytesOut||0)}} 下载</p></details>
      </article>
    </div>
    <div v-else-if="eligible.length" class="empty-card gateway-empty"><div class="gateway-empty-icon"><Link2 :size="24" /></div><h2>还没有共享</h2><p>将 S3 或演示空间路径映射为 WebDAV 目录。新共享默认只读。</p><button class="button primary" @click="openCreate"><Plus :size="16" />创建共享</button></div>
    <div v-else class="empty-card gateway-empty"><div class="gateway-empty-icon"><Server :size="24" /></div><h2>先添加 S3 存储连接</h2><p>共享需要 S3 或演示空间作为数据来源。</p><div class="empty-actions"><button class="button primary" @click="emit('addConnection')"><Plus :size="16" />添加存储连接</button></div></div>

    <BaseModal v-model="showCreate" :dismissible="!saving && !generated" :title="generated ? '共享已创建' : '创建 WebDAV 共享'" :subtitle="generated ? '密码只显示这一次，请先复制并保存在安全的位置。' : '将 S3 目录提供给本机应用，也可使用演示空间体验。'" width="540px">
      <div v-if="generated" class="secret-created"><div class="created-check"><Check :size="20" /></div><h3>{{ generated.gateway.name }}</h3><p class="secret-once">请立即复制密码。关闭后无法再次查看。</p><div class="credential-row"><span>访问地址</span><code>{{ generated.gateway.url || `http://127.0.0.1:${generated.gateway.port}/` }}</code><button class="icon-button" title="复制访问地址" aria-label="复制访问地址" @click="copy(generated.gateway.url || `http://127.0.0.1:${generated.gateway.port}/`, '地址')"><Clipboard :size="15" /></button></div><div class="credential-row"><span>用户名</span><code>{{ generated.gateway.username }}</code><button class="icon-button" title="复制用户名" aria-label="复制用户名" @click="copy(generated.gateway.username, '用户名')"><Clipboard :size="15" /></button></div><div class="credential-row password"><span>一次性密码</span><code>{{ generated.password }}</code><button class="icon-button" title="复制密码" aria-label="复制密码" @click="copy(generated.password, '密码')"><Clipboard :size="15" /></button></div><footer class="modal-actions"><button class="button secondary" @click="copy(`${generated.gateway.username}:${generated.password}`, '账号和密码')"><Clipboard :size="15" />复制账号密码</button><button class="button primary" @click="showCreate = false; generated = null">已安全保存</button></footer></div>
      <form v-else class="form-stack" :aria-busy="saving" @submit.prevent="createGateway"><label class="field"><span>入口名称</span><input v-model="form.name" required maxlength="64" placeholder="例如：阅读器" /></label><label class="field"><span>存储连接</span><select v-model="form.connectionId" required><option v-for="c in eligible" :key="c.id" :value="c.id">{{ c.name }} · {{ c.kind === 'demo' ? '隔离演示空间' : c.kind === 's3' ? 'S3' : 'WebDAV' }}</option></select></label><label class="field"><span>共享目录</span><input v-model="form.prefix" placeholder="留空表示整个存储" /></label><fieldset class="access-choice"><legend>访问权限</legend><label :class="{ chosen: form.readOnly }"><input v-model="form.readOnly" type="radio" :value="true" /><Eye :size="16" /><span><b>只读</b><small>适合查看和下载</small></span></label><label :class="{ chosen: !form.readOnly, disabled: !writableAllowed }"><input v-model="form.readOnly" type="radio" :value="false" :disabled="!writableAllowed" /><EyeOff :size="16" /><span><b>读写</b><small>{{ writableAllowed ? '允许修改远端文件' : '需服务端支持条件写入和删除' }}</small></span></label></fieldset><details ref="createAdvanced" class="tool-disclosure"><summary>高级设置</summary><div class="form-stack"><label class="field"><span>监听端口</span><input ref="portInput" v-model.number="form.port" type="number" min="1024" max="65535" required /><small class="field-hint">选择未占用端口，例如 19081</small></label><label class="field"><span>监听 IP 地址</span><input v-model="form.listenHost" required placeholder="127.0.0.1"/><small class="field-hint">默认仅本机访问。要局域网访问，请填写 0.0.0.0 或本机网卡 IP 并配置 HTTPS。</small></label><template v-if="form.listenHost.trim() && form.listenHost.trim()!=='127.0.0.1' && form.listenHost.trim()!=='::1'"><div class="gateway-tls-note"><AlertTriangle :size="15"/>局域网监听要求有效 HTTPS 证书和私钥，浏览器/客户端还需信任该证书。</div><label class="field"><span>HTTPS 证书文件路径</span><input v-model="form.tlsCert" required placeholder="本机证书文件完整路径"/></label><label class="field"><span>HTTPS 私钥文件路径</span><input v-model="form.tlsKey" required placeholder="本机私钥文件完整路径"/></label></template><label class="field"><span>独立用户名</span><input v-model="form.username" required maxlength="64" /></label></div></details><p v-if="createError" class="gateway-tls-note" role="alert"><AlertTriangle :size="15"/>{{createError}}</p><footer class="modal-actions"><button class="button secondary" type="button" :disabled="saving" @click="showCreate = false">取消</button><button class="button primary" type="submit" :disabled="saving || !formConnection">{{ saving ? '正在创建…' : '创建共享' }}</button></footer></form>
    </BaseModal>
    <BaseModal :model-value="!!resetTarget" :dismissible="actionId===''" title="重置共享密码" @update:model-value="v=>{if(!v)resetTarget=null}"><div v-if="resetTarget" class="confirm-panel"><div class="confirm-warning"><KeyRound :size="18"/></div><p><b>{{resetTarget.name}}</b> 的旧密码会立即失效，共享会停止，已连接的应用需要更新凭据。新密码只显示一次。</p><footer class="modal-actions"><button class="button secondary" :disabled="actionId!==''" @click="resetTarget=null">取消</button><button class="button danger" :disabled="actionId!==''" @click="confirmPasswordReset">{{actionId===`reset:${resetTarget.id}`?'正在重置…':'确认重置密码'}}</button></footer></div></BaseModal>
    <BaseModal :model-value="!!resetSecret" :dismissible="false" title="新的共享密码" @update:model-value="v=>{if(!v)resetSecret=null}"><div v-if="resetSecret" class="secret-created"><div class="created-check"><KeyRound :size="19"/></div><h3>{{resetSecret.gateway.name}}</h3><p class="secret-once">新密码只会显示这一次。复制并保存后，可选择重新启动共享。</p><div class="credential-row"><span>独立用户名</span><code>{{resetSecret.gateway.username}}</code><button class="icon-button" title="复制用户名" aria-label="复制用户名" @click="copy(resetSecret!.gateway.username,'用户名')"><Clipboard :size="14"/></button></div><div class="credential-row password"><span>新密码</span><code>{{resetSecret.password}}</code><button class="icon-button" title="复制新密码" aria-label="复制新密码" @click="copy(resetSecret!.password,'新密码')"><Clipboard :size="14"/></button></div><footer class="modal-actions"><button class="button secondary" @click="copy(`${resetSecret.gateway.username}:${resetSecret.password}`,'账号和密码')"><Clipboard :size="14"/>复制账号密码</button><button class="button secondary" :disabled="actionId!==''" @click="resetSecret=null">已保存，保持停止</button><button class="button primary" :disabled="actionId!==''" @click="restartAfterReset"><LoaderCircle v-if="actionId===resetSecret.gateway.id" :size="14" class="spin"/>已保存，重新启动</button></footer></div></BaseModal>
    <BaseModal :model-value="!!accessTarget" title="共享访问统计" :subtitle="accessTarget?.name" width="720px" @update:model-value="v=>{if(!v)accessTarget=null}"><div class="gateway-access-panel"><div v-if="accessTarget?.access" class="gateway-access-summary"><span>请求 <b>{{accessTarget.access.requests}}</b></span><span>错误 <b>{{accessTarget.access.errors}}</b></span><span>上传 <b>{{bytes(accessTarget.access.bytesIn)}}</b></span><span>下载 <b>{{bytes(accessTarget.access.bytesOut)}}</b></span><span>活跃 <b>{{accessTarget.access.active}}</b></span></div><div v-if="accessLoading" class="gateway-access-loading" role="status"><SquirrelMark variant="eat" :size="30"/><span>正在读取最近 100 条访问记录…</span></div><div v-else-if="accessError" class="settings-inline-error"><AlertTriangle :size="14"/>{{accessError}}<button class="button secondary small" @click="accessTarget&&showAccess(accessTarget)">重试</button></div><div v-else-if="accessRows.length" class="gateway-access-list"><article v-for="(row,index) in accessRows" :key="row.time+row.method+index"><span class="access-status" :class="row.status>=400?'red':'green'">{{row.status}}</span><b>{{row.method}}</b><code>{{row.path}}</code><span>{{bytes(row.bytesIn)}} 入 / {{bytes(row.bytesOut)}} 出</span><small>{{row.milliseconds}} ms · {{accessTime(row.time)}}</small></article></div><div v-else class="settings-inline-state">还没有访问记录。</div></div></BaseModal>
    <BaseModal :model-value="!!checkResult" :title="checkResult?.passed?'共享自检通过':'共享自检结果'" :subtitle="store.data.gateways.find(g=>g.id===checkResult?.gatewayId)?.name" width="560px" @update:model-value="v=>{if(!v)checkResult=null}"><div v-if="checkResult" class="gateway-check-results"><p>{{checkResult.passed?(checkResult.readOnly?'只读入口已验证读取权限和写入拒绝。':'认证、读写操作和清理均已通过 HTTP 入口验证。'):'部分步骤未通过，请查看结果和遗留探针。'}}</p><div v-for="step in checkResult.steps" :key="step.name" class="gateway-check-step" :class="step.status"><span class="gateway-check-step-icon"><Check v-if="step.status==='passed'" :size="14"/><AlertTriangle v-else-if="step.status==='failed'" :size="14"/><span v-else>·</span></span><div><b>{{step.name}}</b><small v-if="step.error">{{step.error}}</small></div><span>{{step.status==='passed'?'通过':step.status==='failed'?'失败':'跳过'}}</span></div><div v-if="checkResult.errors.length" class="gateway-check-errors"><b>问题</b><span v-for="error in checkResult.errors" :key="error">{{error}}</span></div><div v-if="checkResult.residual.length" class="gateway-check-errors residual"><b>未清理或状态不明的探针</b><code v-for="item in checkResult.residual" :key="item">{{item}}</code></div><footer class="modal-actions"><button class="button secondary" @click="checkResult=null">关闭</button></footer></div></BaseModal>
    <BaseModal :model-value="!!deleteTarget" :dismissible="!deleting" title="删除共享" subtitle="删除入口后，使用它的应用将无法继续连接。" @update:model-value="(v) => { if (!v) deleteTarget = null }"><div class="confirm-panel"><div class="confirm-warning"><Trash2 :size="18" /></div><p>确定删除“{{ deleteTarget?.name }}”吗？不会删除映射的存储文件。</p><footer class="modal-actions"><button class="button secondary" :disabled="deleting" @click="deleteTarget = null">取消</button><button class="button danger" :disabled="deleting" @click="deleteGateway">{{ deleting ? '正在删除…' : '删除共享' }}</button></footer></div></BaseModal>
  </div>
</template>
<style scoped>
.gateway-maintenance { margin-top: 10px; }
.gateway-maintenance-actions { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 10px; }
.gateway-maintenance p { color: #777684; font-size: 11px; line-height: 1.6; }

.gateway-check-note{margin:12px 0;color:#817d91;font-size:11px;line-height:1.5}.gateway-loading-state,.gateway-access-loading{display:flex;align-items:center;justify-content:center;gap:10px;min-height:82px;padding:14px;border:1px dashed #ddd7ea;border-radius:12px;color:#7a7689;font-size:12px}.gateway-access-loading{min-height:64px}.gateway-footer-actions{display:flex;align-items:center;gap:7px}.gateway-check-results{display:grid;gap:9px}.gateway-check-results>p{margin:0 0 4px;color:#777287;font-size:12px;line-height:1.5}.gateway-check-step{display:grid;grid-template-columns:22px minmax(0,1fr) auto;align-items:center;gap:8px;padding:9px 10px;border:1px solid #eeeaf5;border-radius:9px;background:#fff;color:#766f83;font-size:11px}.gateway-check-step>div{display:grid;gap:3px;min-width:0}.gateway-check-step b{color:#554d6a;font-size:12px}.gateway-check-step small{color:#a34f45;overflow-wrap:anywhere}.gateway-check-step.passed .gateway-check-step-icon{color:#4e9878}.gateway-check-step.failed .gateway-check-step-icon{color:#a34f45}.gateway-check-step.skipped .gateway-check-step-icon{color:#9a94a8;font-weight:700}.gateway-check-errors{display:grid;gap:4px;padding:10px;border-radius:9px;background:#fff5f3;color:#a34f45;font-size:11px}.gateway-check-errors code{overflow-wrap:anywhere;color:#6b5360}.gateway-check-errors.residual{background:#fff9ee;color:#8a6839}
.gateway-tls-note{display:flex;align-items:flex-start;gap:8px;padding:10px 12px;border:1px solid #f0d9b6;border-radius:10px;background:#fff9ee;color:#8a6839;font-size:12px;line-height:1.5}.gateway-tls-note svg{flex:none}.gateway-access-panel{display:grid;gap:12px}.gateway-access-summary{display:flex;flex-wrap:wrap;gap:7px}.gateway-access-summary span{padding:7px 10px;border-radius:8px;background:#f6f3fa;color:#756f84;font-size:11px}.gateway-access-summary b{color:#4d4561}.gateway-access-list{display:grid;max-height:480px;overflow:auto;border:1px solid #eae6f2;border-radius:11px;background:#fff}.gateway-access-list article{display:grid;grid-template-columns:44px 55px minmax(100px,1fr) 110px 150px;align-items:center;gap:9px;padding:10px;border-bottom:1px solid #f0edf5;font-size:10px}.gateway-access-list article:last-child{border-bottom:0}.gateway-access-list b{color:#554d6a}.gateway-access-list code{overflow-wrap:anywhere;color:#605876}.gateway-access-list span,.gateway-access-list small{color:#827e90}.gateway-access-list .access-status{font-weight:700}.gateway-access-list .access-status.red{color:#a34f45}.gateway-access-list .access-status.green{color:#4e9878}.settings-inline-state,.settings-inline-error{display:flex;align-items:center;justify-content:center;gap:8px;min-height:60px;padding:12px;border:1px dashed #ddd7ea;border-radius:12px;color:#7a7689;font-size:12px}.settings-inline-error{justify-content:space-between;color:#a34f45}@media(max-width:740px){.gateway-access-list article{grid-template-columns:42px 48px minmax(150px,1fr);align-items:start}.gateway-access-list article>span:nth-of-type(2),.gateway-access-list article small{grid-column:2/-1}}

.gateway-maintenance>summary:focus-visible,.technical-detail summary:focus-visible{outline:2px solid #6354ff;outline-offset:3px;border-radius:3px}.technical-detail{min-width:0;color:#777684;font-size:10px;line-height:1.5}.technical-detail summary{width:fit-content;max-width:100%;cursor:pointer;color:#777684;overflow-wrap:anywhere}.technical-detail code{display:block;max-width:100%;margin-top:4px;overflow-wrap:anywhere;white-space:normal;color:#655e78;font-size:10px}.gateway-maintenance-actions button{max-width:100%}.gateway-maintenance p{margin:8px 0 0;overflow-wrap:anywhere}
</style>
