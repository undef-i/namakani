<script setup>
import { onMounted, reactive, ref } from 'vue'
import * as api from '../api.js'

const props = defineProps({
  accounts: { type: Array, default: () => [] },
  accountsStatus: { type: String, default: 'ready' },
  statuses: { type: Object, default: () => ({}) },
  themeMode: { type: String, default: 'system' },
  threaded: { type: Boolean, default: false },
  version: { type: String, default: '' },
})
const emit = defineEmits(['changed', 'update:themeMode', 'update:threaded'])

function fmtListTime(ts) {
  const d = new Date(ts * 1000)
  const now = new Date()
  const pad = (n) => String(n).padStart(2, '0')
  if (d.toDateString() === now.toDateString()) return pad(d.getHours()) + ':' + pad(d.getMinutes())
  if (d.getFullYear() === now.getFullYear()) return d.getMonth() + 1 + '/' + d.getDate()
  return d.getFullYear() + '/' + (d.getMonth() + 1) + '/' + d.getDate()
}

function fmtDateTime(ts) {
  const d = new Date(ts * 1000)
  const pad = (n) => String(n).padStart(2, '0')
  return (
    d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
    pad(d.getHours()) + ':' + pad(d.getMinutes())
  )
}

function statusText(a) {
  const st = props.statuses[a.id]
  if (!st) return ''
  if (st.state === 'idle') return '已同步 ' + fmtListTime(st.last_sync_at)
  if (st.state === 'syncing') return '同步中'
  if (st.state === 'connecting') return '连接中'
  if (st.state === 'error') return '同步失败'
  return '未同步'
}

function cacheText(a) {
  const st = props.statuses[a.id]
  if (!st || !(st.cache_bodies_total > 0)) return ''
  return (
    '正文缓存 ' + st.cache_bodies_cached + '/' + st.cache_bodies_total +
    '，附件 ' + st.cache_attachments_cached + '/' + st.cache_attachments_total
  )
}


const emptyForm = () => ({
  label: '',
  email: '',
  imap_host: '',
  imap_port: 993,
  imap_tls: 'tls',
  smtp_host: '',
  smtp_port: 465,
  smtp_tls: 'tls',
  username: '',
  password: '',
  signature: '',
})

const tlsItems = [
  { title: 'TLS', value: 'tls' },
  { title: 'STARTTLS', value: 'starttls' },
  { title: '明文', value: 'none' },
]

const dialogOpen = ref(false)
const editing = ref(null)
const form = ref(emptyForm())
const formError = ref('')
const deleteTarget = ref(null)
const deleteOpen = ref(false)
const testState = reactive({})

function openCreate() {
  editing.value = null
  form.value = emptyForm()
  formError.value = ''
  dialogOpen.value = true
}

function openEdit(a) {
  editing.value = a.id
  form.value = { ...a, password: '' }
  formError.value = ''
  dialogOpen.value = true
}

async function save() {
  formError.value = ''
  const payload = {
    ...form.value,
    imap_port: Number(form.value.imap_port),
    smtp_port: Number(form.value.smtp_port),
  }
  delete payload.id
  try {
    if (editing.value) await api.updateAccount(editing.value, payload)
    else await api.createAccount(payload)
    dialogOpen.value = false
    emit('changed')
  } catch (err) {
    formError.value = err.message
  }
}

function askDelete(a) {
  deleteTarget.value = a
  deleteOpen.value = true
}

async function confirmDelete() {
  if (!deleteTarget.value) return
  await api.deleteAccount(deleteTarget.value.id)
  deleteTarget.value = null
  deleteOpen.value = false
  emit('changed')
}

async function runTest(a) {
  testState[a.id] = { busy: true, ok: false, text: '连接中' }
  try {
    const res = await api.testAccount(a.id)
    testState[a.id] = {
      busy: false,
      ok: true,
      text:
        '连接成功：' +
        res.folders.length +
        ' 个文件夹，能力 ' +
        res.capabilities.length +
        ' 项（含 ' +
        res.capabilities
          .filter((c) => ['IDLE', 'CONDSTORE', 'QRESYNC', 'MOVE', 'SPECIAL-USE'].includes(c))
          .join('/') +
        '）',
    }
  } catch (err) {
    testState[a.id] = { busy: false, ok: false, text: '连接失败：' + err.message }
  }
}


const tg = ref({ enabled: false, chat_id: '', bot_token: '', has_token: false })
const tgStatus = ref('loading')
const tgMsg = ref('')
const tgBusy = ref('')
const snack = ref({ show: false, text: '' })

function showSnack(text) {
  snack.value = { show: true, text }
}

async function loadTG() {
  tgStatus.value = 'loading'
  try {
    const s = await api.getTelegram()
    tg.value = {
      enabled: !!s.enabled,
      chat_id: s.chat_id || '',
      bot_token: '',
      has_token: !!s.has_token,
    }
    tgStatus.value = 'ready'
  } catch (err) {
    tgStatus.value = 'error'
    tgMsg.value = err.message
  }
}

async function saveTG() {
  tgBusy.value = 'save'
  try {
    const s = await api.saveTelegram({
      enabled: tg.value.enabled,
      chat_id: tg.value.chat_id,
      bot_token: tg.value.bot_token,
    })
    tg.value.has_token = !!s.has_token
    tg.value.bot_token = ''
    showSnack('已保存')
  } catch (err) {
    showSnack('保存失败：' + err.message)
  } finally {
    tgBusy.value = ''
  }
}

async function testTG() {
  tgBusy.value = 'test'
  try {
    await api.testTelegram({ bot_token: tg.value.bot_token, chat_id: tg.value.chat_id })
    showSnack('测试消息已发送')
  } catch (err) {
    showSnack('发送失败：' + err.message)
  } finally {
    tgBusy.value = ''
  }
}


const wpSupported =
  typeof window !== 'undefined' &&
  'serviceWorker' in navigator &&
  'PushManager' in window &&
  'Notification' in window

const wp = ref({ enabled: false, public_key: '', subscriptions: 0 })
const wpStatus = ref('loading')
const wpMsg = ref('')
const wpBusy = ref(false)
const thisSub = ref(null)
let thisEndpoint = ''

function urlB64ToUint8Array(base64String) {
  const padding = '='.repeat((4 - (base64String.length % 4)) % 4)
  const base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/')
  const raw = window.atob(base64)
  const out = new Uint8Array(raw.length)
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i)
  return out
}

async function checkThisBrowser() {
  if (!wpSupported) {
    thisSub.value = false
    return
  }
  try {
    const reg = await navigator.serviceWorker.getRegistration()
    const sub = reg ? await reg.pushManager.getSubscription() : null
    thisSub.value = !!sub
    thisEndpoint = sub ? sub.endpoint : ''
    if (sub) {
      try {
        const s = await api.subscribePush(sub.toJSON())
        wp.value.subscriptions = s.subscriptions || 0
      } catch {
      }
    }
  } catch {
    thisSub.value = false
  }
}

async function loadWP() {
  wpStatus.value = 'loading'
  try {
    const s = await api.getWebPush()
    wp.value = {
      enabled: !!s.enabled,
      public_key: s.public_key || '',
      subscriptions: s.subscriptions || 0,
    }
    wpStatus.value = 'ready'
    await checkThisBrowser()
  } catch (err) {
    wpStatus.value = 'error'
    wpMsg.value = err.message
  }
}

async function subscribeThisInner() {
  let perm = Notification.permission
  if (perm === 'default') perm = await Notification.requestPermission()
  if (perm !== 'granted') throw new Error('通知权限未授予，无法开启推送')
  const reg = await navigator.serviceWorker.ready
  let sub = await reg.pushManager.getSubscription()
  if (!sub) {
    sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlB64ToUint8Array(wp.value.public_key),
    })
  }
  const s = await api.subscribePush(sub.toJSON())
  wp.value.subscriptions = s.subscriptions || 0
  thisSub.value = true
  thisEndpoint = sub.endpoint
}

async function unsubscribeThisInner() {
  const reg = await navigator.serviceWorker.getRegistration()
  const sub = reg ? await reg.pushManager.getSubscription() : null
  if (sub) {
    await api.unsubscribePush(sub.endpoint)
    await sub.unsubscribe()
  } else if (thisEndpoint) {
    await api.unsubscribePush(thisEndpoint)
  }
  thisSub.value = false
  thisEndpoint = ''
}

async function toggleWPEnabled(v) {
  wpBusy.value = true
  wpMsg.value = ''
  try {
    if (v) {
      if (wpSupported) await subscribeThisInner()
    } else if (thisSub.value === true) {
      await unsubscribeThisInner()
    }
    const s = await api.saveWebPush({ enabled: v })
    wp.value.enabled = !!s.enabled
    wp.value.subscriptions = s.subscriptions || 0
  } catch (err) {
    showSnack('保存失败：' + err.message)
    try {
      const s = await api.getWebPush()
      wp.value.enabled = !!s.enabled
      wp.value.subscriptions = s.subscriptions || 0
      await checkThisBrowser()
    } catch {
    }
  } finally {
    wpBusy.value = false
  }
}

async function testWP() {
  wpBusy.value = true
  wpMsg.value = ''
  try {
    const res = await api.testWebPush()
    wp.value.subscriptions = res.subscriptions || 0
    showSnack('测试推送已发出（送达 ' + res.sent + ' 个订阅）')
  } catch (err) {
    showSnack('发送失败：' + err.message)
  } finally {
    wpBusy.value = false
  }
}


const sch = ref([])
const schStatus = ref('loading')
const schError = ref('')

async function loadScheduled() {
  schStatus.value = 'loading'
  try {
    sch.value = await api.listScheduled()
    schStatus.value = 'ready'
  } catch (err) {
    schStatus.value = 'error'
    schError.value = err.message
  }
}

async function cancelSch(it) {
  try {
    await api.cancelScheduled(it.id)
    sch.value = sch.value.filter((x) => x.id !== it.id)
  } catch (err) {
    schError.value = err.message
    schStatus.value = 'error'
  }
}

onMounted(() => {
  loadTG()
  loadWP()
  loadScheduled()
})
</script>

<template>
  <div class="mx-auto w-100 pa-4" style="max-width: 880px">
    <div class="d-flex align-center justify-space-between mb-3">
      <h2 class="text-h6">邮箱账号</h2>
      <v-btn color="primary" prepend-icon="mdi-plus" @click="openCreate">添加账号</v-btn>
    </div>

    <template v-if="accountsStatus === 'loading'">
      <v-skeleton-loader type="card@2" />
    </template>
    <template v-else-if="accountsStatus === 'error'">
      <v-alert type="error" variant="tonal" density="compact">
        账号加载失败
        <v-btn variant="text" size="small" @click="emit('changed')">重试</v-btn>
      </v-alert>
    </template>
    <p v-else-if="accounts.length === 0" class="text-body-2 text-medium-emphasis">
      还没有账号。添加第一个邮箱后，它的邮件会自动同步进聚合收件箱。
    </p>
    <div v-else class="d-flex flex-column ga-3">
      <v-card v-for="a in accounts" :key="a.id" variant="outlined" rounded="lg">
        <v-card-text>
          <div class="d-flex align-start">
            <div style="min-width: 0">
              <div class="text-body-1 font-weight-medium text-truncate">{{ a.label }}</div>
              <div class="text-body-2 text-medium-emphasis text-truncate">{{ a.email }}</div>
            </div>
            <v-spacer />
            <v-btn icon="mdi-connection" variant="text" size="small" title="测试连接" @click="runTest(a)" />
            <v-btn icon="mdi-pencil" variant="text" size="small" title="编辑" @click="openEdit(a)" />
            <v-btn icon="mdi-delete" variant="text" size="small" title="删除" @click="askDelete(a)" />
          </div>
          <div class="text-body-2 text-medium-emphasis mt-2" style="overflow-wrap: anywhere">
            IMAP {{ a.imap_host }}:{{ a.imap_port }} ({{ a.imap_tls }})，SMTP {{ a.smtp_host }}:{{ a.smtp_port }} ({{ a.smtp_tls }})
          </div>
          <div
            v-if="statuses[a.id]"
            class="text-body-2 mt-1"
            :class="statuses[a.id].state === 'error' ? 'text-error font-weight-medium' : 'text-medium-emphasis'"
            style="overflow-wrap: anywhere"
          >
            {{ statusText(a) }}<template v-if="cacheText(a)">，{{ cacheText(a) }}</template>
          </div>
          <div
            v-if="testState[a.id]"
            class="text-body-2 mt-1"
            :class="testState[a.id].ok ? '' : 'text-error'"
            style="overflow-wrap: anywhere"
          >
            {{ testState[a.id].text }}
          </div>
        </v-card-text>
      </v-card>
    </div>

    <v-divider class="my-6" />

    <h2 class="text-h6 mb-3">外观</h2>
    <div style="max-width: 640px">
      <div class="text-body-2 mb-2">深色模式</div>
      <v-btn-toggle
        :model-value="themeMode"
        mandatory
        divided
        density="compact"
        color="primary"
        class="mb-4"
        @update:model-value="emit('update:themeMode', $event)"
      >
        <v-btn value="system" prepend-icon="mdi-theme-light-dark">跟随系统</v-btn>
        <v-btn value="light" prepend-icon="mdi-weather-sunny">浅色</v-btn>
        <v-btn value="dark" prepend-icon="mdi-weather-night">深色</v-btn>
      </v-btn-toggle>
      <v-switch
        :model-value="threaded"
        label="按线程显示邮件"
        color="primary"
        hide-details
        @update:model-value="emit('update:threaded', $event)"
      />
    </div>

    <v-divider class="my-6" />

    <h2 class="text-h6 mb-3">定时发送</h2>
    <template v-if="schStatus === 'loading'">
      <v-skeleton-loader type="list-item-two-line@2" max-width="640" />
    </template>
    <template v-else-if="schStatus === 'error'">
      <v-alert type="error" variant="tonal" density="compact" max-width="640">
        定时发送加载失败：{{ schError }}
        <v-btn variant="text" size="small" @click="loadScheduled">重试</v-btn>
      </v-alert>
    </template>
    <template v-else>
      <p v-if="sch.length === 0" class="text-body-2 text-medium-emphasis">没有定时发送的邮件。</p>
      <v-list v-else density="comfortable" class="pa-0" style="max-width: 640px">
        <v-list-item v-for="it in sch" :key="it.id" class="px-0">
          <v-list-item-title class="text-body-2">{{ it.subject || '(无主题)' }}</v-list-item-title>
          <v-list-item-subtitle>
            发给 {{ it.to }}，计划 {{ fmtDateTime(it.send_at) }}
            <template v-if="it.status === 'sent'">，已发送</template>
            <template v-else-if="it.status === 'failed'">，发送失败<template v-if="it.error">（{{ it.error }}）</template></template>
            <template v-else>，待发送</template>
          </v-list-item-subtitle>
          <template v-if="it.status === 'pending'" #append>
            <v-btn variant="text" size="small" color="error" @click="cancelSch(it)">取消</v-btn>
          </template>
        </v-list-item>
      </v-list>
    </template>

    <v-divider class="my-6" />

    <h2 class="text-h6 mb-3">Telegram 推送</h2>
    <template v-if="tgStatus === 'loading'">
      <v-skeleton-loader type="article" max-width="640" />
    </template>
    <template v-else-if="tgStatus === 'error'">
      <v-alert type="error" variant="tonal" density="compact" max-width="640">
        设置加载失败：{{ tgMsg }}
        <v-btn variant="text" size="small" @click="loadTG">重试</v-btn>
      </v-alert>
    </template>
    <div v-else style="max-width: 640px">
      <v-switch v-model="tg.enabled" label="启用新邮件推送" color="primary" hide-details class="mb-2" />
      <v-text-field
        v-model="tg.bot_token"
        label="Bot Token"
        type="password"
        variant="outlined"
        density="comfortable"
        autocomplete="off"
        :placeholder="tg.has_token ? '已保存，留空不改' : '如 123456:ABC-DEF...'"
        class="mb-2"
      />
      <v-text-field
        v-model="tg.chat_id"
        label="Chat ID"
        variant="outlined"
        density="comfortable"
        autocomplete="off"
        placeholder="如 123456789"
        class="mb-2"
      />
      <div class="d-flex align-center ga-2">
        <v-btn color="primary" :disabled="tgBusy !== ''" :loading="tgBusy === 'save'" @click="saveTG">
          保存
        </v-btn>
        <v-btn variant="outlined" :disabled="tgBusy !== ''" :loading="tgBusy === 'test'" @click="testTG">
          发送测试消息
        </v-btn>
      </div>
    </div>

    <v-divider class="my-6" />

    <h2 class="text-h6 mb-3">浏览器推送</h2>
    <template v-if="wpStatus === 'loading'">
      <v-skeleton-loader type="article" max-width="640" />
    </template>
    <template v-else-if="wpStatus === 'error'">
      <v-alert type="error" variant="tonal" density="compact" max-width="640">
        设置加载失败：{{ wpMsg }}
        <v-btn variant="text" size="small" @click="loadWP">重试</v-btn>
      </v-alert>
    </template>
    <div v-else style="max-width: 640px">
      <v-switch
        :model-value="wp.enabled"
        label="启用浏览器推送"
        color="primary"
        hide-details
        class="mb-2"
        :disabled="wpBusy || !wpSupported"
        @update:model-value="toggleWPEnabled"
      />
      <p class="text-body-2 mb-2">
        当前共有 {{ wp.subscriptions }} 个浏览器订阅。
        <template v-if="thisSub === true">本浏览器已订阅。</template>
        <template v-else-if="thisSub === false">本浏览器未订阅。</template>
      </p>
      <v-alert v-if="!wpSupported" type="warning" variant="tonal" density="compact" class="mb-2">
        当前浏览器不支持 Web Push。
      </v-alert>
      <div class="d-flex align-center ga-2">
        <v-btn variant="outlined" :disabled="wpBusy || !wpSupported" @click="testWP">发送测试推送</v-btn>
      </div>
    </div>

    <v-divider class="my-6" />
    <div class="text-center text-caption text-medium-emphasis pb-10">
      <div>Namakani 邮箱</div>
      <div v-if="version" class="mt-1">版本 {{ version }}</div>
      <div class="mt-1">© 2026 namakani</div>
    </div>
  </div>

  <v-dialog v-model="dialogOpen" max-width="560">
    <v-card>
      <v-card-title>{{ editing ? '编辑账号' : '添加账号' }}</v-card-title>
      <v-card-text>
        <v-form @submit.prevent="save">
          <v-text-field v-model="form.label" label="名称" variant="outlined" density="comfortable" placeholder="如 工作邮箱" required class="mb-2" />
          <v-text-field v-model="form.email" label="邮箱" type="email" variant="outlined" density="comfortable" required class="mb-2" />
          <v-text-field v-model="form.username" label="用户名" variant="outlined" density="comfortable" required class="mb-2" />
          <v-text-field
            v-model="form.password"
            label="密码"
            type="password"
            variant="outlined"
            density="comfortable"
            :placeholder="editing ? '留空 = 不修改' : ''"
            :required="!editing"
            class="mb-2"
          />
          <v-textarea
            v-model="form.signature"
            label="签名（纯文本，写信时自动加到正文末尾）"
            variant="outlined"
            density="comfortable"
            rows="3"
            class="mb-2"
          />
          <v-row dense class="mb-2">
            <v-col cols="12" sm="6">
              <v-text-field v-model="form.imap_host" label="IMAP 服务器" variant="outlined" density="comfortable" placeholder="imap.example.com" required hide-details />
            </v-col>
            <v-col cols="6" sm="3">
              <v-text-field v-model.number="form.imap_port" label="端口" type="number" variant="outlined" density="comfortable" required hide-details />
            </v-col>
            <v-col cols="6" sm="3">
              <v-select v-model="form.imap_tls" :items="tlsItems" label="加密" variant="outlined" density="comfortable" hide-details />
            </v-col>
            <v-col cols="12" sm="6">
              <v-text-field v-model="form.smtp_host" label="SMTP 服务器" variant="outlined" density="comfortable" placeholder="smtp.example.com" required hide-details />
            </v-col>
            <v-col cols="6" sm="3">
              <v-text-field v-model.number="form.smtp_port" label="端口" type="number" variant="outlined" density="comfortable" required hide-details />
            </v-col>
            <v-col cols="6" sm="3">
              <v-select v-model="form.smtp_tls" :items="tlsItems" label="加密" variant="outlined" density="comfortable" hide-details />
            </v-col>
          </v-row>
          <v-alert v-if="formError" type="error" variant="tonal" density="compact">{{ formError }}</v-alert>
        </v-form>
      </v-card-text>
      <v-card-actions>
        <v-spacer />
        <v-btn variant="text" @click="dialogOpen = false">取消</v-btn>
        <v-btn color="primary" @click="save">保存</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>

  <v-dialog v-model="deleteOpen" max-width="440">
    <v-card>
      <v-card-title>删除账号</v-card-title>
      <v-card-text>
        删除账号「{{ deleteTarget?.label }}」？本地同步的邮件会一并删除，服务器上的邮件不受影响。
      </v-card-text>
      <v-card-actions>
        <v-spacer />
        <v-btn variant="text" @click="deleteOpen = false">取消</v-btn>
        <v-btn color="error" @click="confirmDelete">删除</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>

  <v-snackbar v-model="snack.show" :timeout="3000" location="bottom">
    {{ snack.text }}
  </v-snackbar>
</template>
