<script setup>
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useDisplay } from 'vuetify'
import { intlLocale, lang, setLang, t } from '../i18n'
import * as api from '../api.js'

const display = useDisplay()
const lgAndUp = computed(() => display.lgAndUp.value)
const activeSection = ref('accounts')

function showSection(s) {
  return !lgAndUp.value || activeSection.value === s
}

const SECTIONS = computed(() => [
  { key: 'accounts', label: t('账号'), icon: 'mdi-account-multiple' },
  { key: 'appearance', label: t('外观'), icon: 'mdi-palette' },
  { key: 'compose', label: t('写信与发送'), icon: 'mdi-email-edit' },
  { key: 'push', label: t('推送'), icon: 'mdi-bell' },
  { key: 'advanced', label: t('高级'), icon: 'mdi-cog' },
])

function kindText(a) {
  if (a.kind === 'proton') return 'Proton Mail'
  if (a.kind === 'tuta') return 'Tuta Mail'
  return (
    'IMAP ' + a.imap_host + ':' + a.imap_port + ' (' + a.imap_tls + ')' + t('，') +
    'SMTP ' + a.smtp_host + ':' + a.smtp_port + ' (' + a.smtp_tls + ')'
  )
}


function statusText(a) {
  const st = props.statuses[a.id]
  if (!st) return ''
  if (st.state === 'idle') return t('已同步 {time}', { time: fmtListTime(st.last_sync_at) })
  if (st.state === 'syncing') return t('同步中')
  if (st.state === 'connecting') return t('连接中')
  if (st.state === 'repair') return t('需要手动修复')
  if (st.state === 'error') return t('同步失败')
  return t('未同步')
}

const testState = reactive({})

async function runTest(a) {
  testState[a.id] = { busy: true, ok: false, text: t('连接中') }
  try {
    const res = await api.testAccount(a.id)
    testState[a.id] = {
      busy: false,
      ok: true,
      text: t('连接成功：{folders} 个文件夹，能力 {caps} 项（含 {sample}）', {
        folders: (res.folders || []).length,
        caps: (res.capabilities || []).length,
        sample: (res.capabilities || [])
          .filter((c) => ['IDLE', 'CONDSTORE', 'QRESYNC', 'MOVE', 'SPECIAL-USE'].includes(c))
          .join('/'),
      }),
    }
  } catch (err) {
    testState[a.id] = { busy: false, ok: false, text: t('连接失败：{error}', { error: err.message }) }
  }
}

const reconnecting = ref(null)

const rcOpen = ref(false)
const rcTarget = ref(null)
const rcForm = ref({ password: '', mailbox_password: '', totp: '', captcha_token: '' })
const rcNeedsTotp = ref(false)
const rcNeedsCaptcha = ref(false)
const rcCaptchaOpened = ref(false)
const rcCaptchaUrl = ref('')
const rcError = ref('')
const rcBusy = ref(false)

async function reconnect(a) {
  if (a.kind === 'proton') {
    rcTarget.value = a
    rcForm.value = { password: '', mailbox_password: '', totp: '', captcha_token: '' }
    rcNeedsTotp.value = false
    rcNeedsCaptcha.value = false
    rcCaptchaOpened.value = false
    rcCaptchaUrl.value = ''
    rcError.value = ''
    rcOpen.value = true
    return
  }
  reconnecting.value = a.id
  try {
    await api.reconnectAccount(a.id)
    showSnack(t('已发起重新连接'))
    emit('changed')
  } catch (err) {
    showSnack(t('重新连接失败：{error}', { error: err.message }))
  } finally {
    reconnecting.value = null
  }
}

async function rcSubmit() {
  if (!rcTarget.value || rcBusy.value) return
  rcBusy.value = true
  rcError.value = ''
  const sentToken = rcForm.value.captcha_token
  try {
    const res = await api.reconnectAccount(rcTarget.value.id, { ...rcForm.value })
    if (res && res.needs_captcha) {
      rcForm.value.captcha_token = res.captcha_token
      rcCaptchaOpened.value = false
      rcCaptchaUrl.value = res.captcha_url
      rcNeedsCaptcha.value = true
      return
    }
    if (sentToken && rcForm.value.captcha_token === sentToken) {
      rcForm.value.captcha_token = ''
      rcCaptchaUrl.value = ''
      rcNeedsCaptcha.value = false
    }
    if (res && res.needs_totp) {
      rcNeedsTotp.value = true
      rcError.value = t('该账号开启了两步验证，请输入验证码')
      return
    }
    rcOpen.value = false
    showSnack(t('已重新连接'))
    emit('changed')
  } catch (err) {
    if (sentToken && rcForm.value.captcha_token === sentToken) {
      rcForm.value.captcha_token = ''
    }
    rcError.value = err.message
  } finally {
    rcBusy.value = false
  }
}

function cacheText(a) {
  const st = props.statuses[a.id]
  if (!st || !(st.cache_bodies_total > 0)) return ''
  return (
    t('正文缓存 ({a}/{b}) 附件 ({c}/{d})', {
      a: st.cache_bodies_cached,
      b: st.cache_bodies_total,
      c: st.cache_attachments_cached,
      d: st.cache_attachments_total,
    })
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
  from_name: '',
  tls_skip_verify: false,
  kind: 'imap',
  totp: '',
  mailbox_password: '',
  captcha_token: '',
})

const tlsItems = computed(() => [
  { title: 'TLS', value: 'tls' },
  { title: 'STARTTLS', value: 'starttls' },
  { title: t('明文'), value: 'none' },
])

const dialogOpen = ref(false)
const editing = ref(null)
const form = ref(emptyForm())
const formError = ref('')
const needsTotp = ref(false)
const needsCaptcha = ref(false)
const captchaOpened = ref(false)
const captchaUrl = ref('')
const deleteTarget = ref(null)
const deleteOpen = ref(false)

function openCreate() {
  editing.value = null
  form.value = emptyForm()
  formError.value = ''
  needsTotp.value = false
  needsCaptcha.value = false
  captchaUrl.value = ''
  captchaCtx = ''
  resetTestState()
  dialogOpen.value = true
}

function openEdit(a) {
  editing.value = a.id
  form.value = { ...emptyForm(), ...a, password: '' }
  formError.value = ''
  needsTotp.value = false
  needsCaptcha.value = false
  captchaUrl.value = ''
  captchaCtx = ''
  resetTestState()
  dialogOpen.value = true
}

const testBusy = ref(false)
const testOk = ref(false)
const testResult = ref('')
const pendingMode = ref('save')
let testRun = 0

function resetTestState() {
  testRun++
  testBusy.value = false
  testOk.value = false
  testResult.value = ''
}

let captchaCtx = ''

function captchaFingerprint() {
  return JSON.stringify([form.value.email, form.value.password, form.value.mailbox_password, form.value.totp])
}

function consumeCaptcha(sentToken) {
  if (sentToken && form.value.captcha_token === sentToken) {
    form.value.captcha_token = ''
    captchaUrl.value = ''
    captchaOpened.value = false
    needsCaptcha.value = false
    captchaCtx = ''
  }
}

watch(
  form,
  () => {
    testOk.value = false
    testResult.value = ''
    if (form.value.captcha_token && captchaCtx && captchaFingerprint() !== captchaCtx) {
      form.value.captcha_token = ''
      captchaUrl.value = ''
      needsCaptcha.value = false
      captchaCtx = ''
    }
  },
  { deep: true },
)

async function submit(mode) {
  pendingMode.value = mode
  formError.value = ''
  const payload = {
    ...form.value,
    imap_port: Number(form.value.imap_port),
    smtp_port: Number(form.value.smtp_port),
  }
  delete payload.id
  const isTest = mode === 'test'
  const sentToken = payload.captcha_token
  let run = 0
  if (isTest) {
    run = ++testRun
    testBusy.value = true
    testOk.value = false
    testResult.value = ''
  }
  try {
    let res
    if (isTest) {
      res = await api.testAccountForm({ ...payload, id: editing.value || 0 })
    } else if (editing.value) {
      res = await api.updateAccount(editing.value, payload)
    } else {
      res = await api.createAccount(payload)
    }
    if (isTest && run !== testRun) return
    if (res && res.needs_captcha) {
      if (sentToken) showSnack(t('验证已过期，已生成新的验证链接'))
      form.value.captcha_token = res.captcha_token
      captchaOpened.value = false
      captchaUrl.value = res.captcha_url
      captchaCtx = captchaFingerprint()
      needsCaptcha.value = true
      return
    }
    consumeCaptcha(sentToken)
    if (res && res.needs_totp) {
      needsTotp.value = true
      formError.value = t('该账号开启了两步验证，请输入验证码')
      return
    }
    if (isTest) {
      reportTestResult(res)
      return
    }
    dialogOpen.value = false
    emit('changed')
  } catch (err) {
    consumeCaptcha(sentToken)
    if (isTest && run !== testRun) return
    if (isTest) {
      formError.value = t('连接失败：{error}', { error: err.message })
    } else {
      formError.value = err.message
    }
  } finally {
    if (isTest && run === testRun) testBusy.value = false
  }
}

function save() {
  return submit('save')
}

function testConnection() {
  return submit('test')
}

function resumePending() {
  return submit(pendingMode.value)
}

function captchaStep() {
  if (!captchaOpened.value) {
    window.open(captchaUrl.value, '_blank', 'noopener')
    captchaOpened.value = true
    return
  }
  resumePending()
}

function rcCaptchaStep() {
  if (!rcCaptchaOpened.value) {
    window.open(rcCaptchaUrl.value, '_blank', 'noopener')
    rcCaptchaOpened.value = true
    return
  }
  rcSubmit()
}

function reportTestResult(res) {
  needsCaptcha.value = false
  needsTotp.value = false
  formError.value = ''
  testResult.value = ''
  if (form.value.kind === 'proton' || form.value.kind === 'tuta') {
    testOk.value = true
    if (res && res.folders != null) {
      showTestSuccess(res.folders, res.capabilities)
    }
    return
  }
  if (!res) return
  const parts = []
  if (!res.imap.ok) parts.push(t('IMAP 连接失败：{error}', { error: res.imap.error }))
  if (!res.smtp.ok) parts.push(t('SMTP 连接失败：{error}', { error: res.smtp.error }))
  if (parts.length) {
    formError.value = parts.join('；')
  } else {
    testOk.value = true
    showTestSuccess(res.imap.folders, res.imap.capabilities)
  }
}

function showTestSuccess(folders, capabilities) {
  const caps = capabilities || []
  testResult.value = t('连接成功：{folders} 个文件夹，能力 {caps} 项（含 {sample}）', {
    folders,
    caps: caps.length,
    sample: caps
      .filter((c) => ['IDLE', 'CONDSTORE', 'QRESYNC', 'MOVE', 'SPECIAL-USE'].includes(c))
      .join('/'),
  })
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


const tg = ref({ enabled: false, chat_id: '', bot_token: '', has_token: false })
const tgStatus = ref('loading')
const tgMsg = ref('')
const tgBusy = ref('')
const snack = ref({ show: false, text: '', timeout: 3000 })

function showSnack(text, timeout = 3000) {
  snack.value = { show: true, text, timeout }
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
    showSnack(t('已保存'))
  } catch (err) {
    showSnack(t('保存失败：{error}', { error: err.message }))
  } finally {
    tgBusy.value = ''
  }
}

async function testTG() {
  tgBusy.value = 'test'
  try {
    await api.testTelegram({ bot_token: tg.value.bot_token, chat_id: tg.value.chat_id })
    showSnack(t('测试消息已发送'))
  } catch (err) {
    showSnack(t('发送失败：{error}', { error: err.message }))
  } finally {
    tgBusy.value = ''
  }
}


const wpSupported =
  typeof window !== 'undefined' &&
  'serviceWorker' in navigator &&
  'PushManager' in window &&
  'Notification' in window

const wp = ref({ public_key: '', subscriptions: 0 })
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
  if (perm !== 'granted') throw new Error(t('通知权限未授予，无法开启推送'))
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

async function toggleThisBrowser(v) {
  wpBusy.value = true
  wpMsg.value = ''
  try {
    if (v) {
      await subscribeThisInner()
    } else {
      await unsubscribeThisInner()
    }
    const s = await api.getWebPush()
    wp.value.subscriptions = s.subscriptions || 0
  } catch (err) {
    showSnack(t('保存失败：{error}', { error: err.message }))
    try {
      const s = await api.getWebPush()
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
    showSnack(t('测试推送已发出（送达 {n} 个订阅）', { n: res.sent }))
  } catch (err) {
    showSnack(t('发送失败：{error}', { error: err.message }))
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

const appSetState = ref('loading')
const appSetError = ref('')
const appSetBusy = ref(false)
const appSet = ref({ cache_budget_gb: 4, sync_batch: 100, max_attachment_mb: 20, push_window_minutes: 15 })
const appSetUsed = ref(0)
const appSetBudgetBytes = ref(0)
const appSetLeft = computed(() => Math.max(0, appSetBudgetBytes.value - appSetUsed.value))

function fmtBytes(n) {
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB'
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}

async function loadAppSet() {
  appSetState.value = 'loading'
  try {
    const res = await api.getAppSettings()
    appSet.value = { ...res.settings }
    appSetUsed.value = res.cache_used_bytes
    appSetBudgetBytes.value = res.cache_budget_bytes
    appSetState.value = 'ready'
  } catch (err) {
    appSetError.value = err.message
    appSetState.value = 'error'
  }
}

async function saveAppSet() {
  appSetBusy.value = true
  try {
    const payload = {
      cache_budget_gb: Number(appSet.value.cache_budget_gb),
      sync_batch: Number(appSet.value.sync_batch),
      max_attachment_mb: Number(appSet.value.max_attachment_mb),
      push_window_minutes: Number(appSet.value.push_window_minutes),
    }
    await api.saveAppSettings(payload)
    emit('appSettings', payload)
    showSnack(t('已保存'))
    loadAppSet()
  } catch (err) {
    showSnack(t('保存失败：{error}', { error: err.message }))
  } finally {
    appSetBusy.value = false
  }
}

onMounted(() => {
  loadAppSet()
  loadTG()
  loadWP()
  loadScheduled()
})
</script>

<template>
  <div class="settings-root">
    <nav v-if="lgAndUp" class="settings-nav">
      <v-list nav density="comfortable" class="bg-transparent">
        <v-list-item
          v-for="s in SECTIONS"
          :key="s.key"
          :prepend-icon="s.icon"
          :title="s.label"
          :active="activeSection === s.key"
          @click="activeSection = s.key"
        />
      </v-list>
    </nav>
    <div class="settings-content">
    <section v-if="showSection('accounts')">
    <div class="d-flex align-center justify-space-between mb-3">
      <h2 class="text-h6">{{ t('邮箱账号') }}</h2>
      <v-btn color="primary" prepend-icon="mdi-plus" @click="openCreate">{{ t('添加账号') }}</v-btn>
    </div>

    <template v-if="accountsStatus === 'loading'">
      <v-skeleton-loader type="card@2" />
    </template>
    <template v-else-if="accountsStatus === 'error'">
      <v-alert type="error" variant="tonal" density="compact">
        {{ t('账号加载失败') }}
        <v-btn variant="text" size="small" @click="emit('changed')">{{ t('重试') }}</v-btn>
      </v-alert>
    </template>
    <p v-else-if="accounts.length === 0" class="text-body-2 text-medium-emphasis">
      {{ t('还没有账号。添加第一个邮箱后，它的邮件会自动同步进聚合收件箱。') }}
    </p>
    <div v-else class="d-flex flex-column ga-3">
      <v-card v-for="a in accounts" :key="a.id" variant="outlined" rounded="lg">
        <v-card-text>
          <div class="d-flex align-start">
            <div style="min-width: 0">
              <div class="text-body-1 font-weight-medium text-truncate">{{ a.label }}</div>
              <div class="text-body-2 text-medium-emphasis" style="overflow-wrap: anywhere">{{ a.email }}</div>
            </div>
            <v-spacer />
            <v-btn icon="mdi-connection" variant="text" size="small" :title="t('测试连接')" :loading="!!(testState[a.id] && testState[a.id].busy)" @click="runTest(a)" />
            <v-btn icon="mdi-pencil" variant="text" size="small" :title="t('编辑')" @click="openEdit(a)" />
            <v-btn icon="mdi-delete" variant="text" size="small" :title="t('删除')" @click="askDelete(a)" />
          </div>
          <div class="text-body-2 text-medium-emphasis mt-2" style="overflow-wrap: anywhere">
            {{ kindText(a) }}
          </div>
          <div
            v-if="statuses[a.id]"
            class="text-body-2 mt-1"
            :class="(statuses[a.id].state === 'error' || statuses[a.id].state === 'repair') ? 'text-error font-weight-medium' : 'text-medium-emphasis'"
            style="overflow-wrap: anywhere"
          >
            {{ statusText(a) }}<template v-if="cacheText(a)">{{ t('，') }}{{ cacheText(a) }}</template>
          </div>
          <div
            v-if="statuses[a.id] && statuses[a.id].last_error && (statuses[a.id].state === 'error' || statuses[a.id].state === 'repair')"
            class="text-body-2 text-error mt-1"
            style="overflow-wrap: anywhere"
          >
            {{ statuses[a.id].last_error }}
          </div>
          <div
            v-if="testState[a.id] && testState[a.id].text"
            class="text-body-2 mt-1"
            :class="testState[a.id].ok ? '' : 'text-error'"
            style="overflow-wrap: anywhere"
          >
            {{ testState[a.id].text }}
          </div>
          <div v-if="statuses[a.id] && statuses[a.id].state === 'repair'" class="mt-2">
            <v-btn size="small" variant="outlined" color="primary" :loading="reconnecting === a.id" @click="reconnect(a)">
              {{ t('重新连接') }}
            </v-btn>
          </div>
        </v-card-text>
      </v-card>
    </div>
    </section>

    <section v-if="showSection('appearance')" class="settings-sec">
    <h2 class="text-h6 mb-3">{{ t('外观') }}</h2>
    <div style="max-width: 640px">
      <div class="text-body-2 mb-2">{{ t('深色模式') }}</div>
      <v-btn-toggle
        :model-value="themeMode"
        mandatory
        divided
        density="compact"
        color="primary"
        class="mb-4"
        @update:model-value="emit('update:themeMode', $event)"
      >
        <v-btn value="system" prepend-icon="mdi-theme-light-dark">{{ t('跟随系统') }}</v-btn>
        <v-btn value="light" prepend-icon="mdi-weather-sunny">{{ t('浅色') }}</v-btn>
        <v-btn value="dark" prepend-icon="mdi-weather-night">{{ t('深色') }}</v-btn>
      </v-btn-toggle>
      <v-switch
        :model-value="threaded"
        :label="t('按线程显示邮件')"
        color="primary"
        hide-details
        class="mb-2"
        @update:model-value="emit('update:threaded', $event)"
      />
      <v-select
        :model-value="lang"
        :items="[
          { title: '中文', value: 'zh' },
          { title: 'English', value: 'en' },
        ]"
        :label="t('语言')"
        variant="outlined"
        density="comfortable"
        hide-details
        style="max-width: 260px"
        class="mt-4"
        @update:model-value="setLang"
      />
    </div>

    </section>

    <section v-if="showSection('advanced')" class="settings-sec">
    <h2 class="text-h6 mb-3">{{ t('同步与缓存') }}</h2>
    <v-skeleton-loader v-if="appSetState === 'loading'" type="article" class="skel-clip" />
    <v-alert v-else-if="appSetState === 'error'" type="error" variant="tonal" class="mb-3">
      {{ t('设置加载失败：{error}', { error: appSetError }) }}
      <v-btn variant="text" size="small" @click="loadAppSet">{{ t('重试') }}</v-btn>
    </v-alert>
    <div v-else style="max-width: 640px">
      <p class="text-body-2 text-medium-emphasis mb-3">
        {{ t('已用 {used}，剩余 {left}', { used: fmtBytes(appSetUsed), left: fmtBytes(appSetLeft) }) }}
      </p>
      <div class="d-flex flex-wrap ga-3 mb-3">
        <v-text-field
          v-model.number="appSet.cache_budget_gb"
          type="number"
          :label="t('缓存预算 (GB)')"
          variant="outlined"
          density="comfortable"
          hide-details
          style="max-width: 200px"
        />
        <v-text-field
          v-model.number="appSet.sync_batch"
          type="number"
          :label="t('同步批量')"
          variant="outlined"
          density="comfortable"
          hide-details
          style="max-width: 200px"
        />
        <v-text-field
          v-model.number="appSet.max_attachment_mb"
          type="number"
          :label="t('附件上限 (MB)')"
          variant="outlined"
          density="comfortable"
          hide-details
          style="max-width: 200px"
        />
        <v-text-field
          v-model.number="appSet.push_window_minutes"
          type="number"
          :label="t('推送窗口 (分钟)')"
          variant="outlined"
          density="comfortable"
          hide-details
          style="max-width: 200px"
        />
      </div>
      <v-btn color="primary" :loading="appSetBusy" @click="saveAppSet">{{ t('保存') }}</v-btn>
    </div>

    </section>

    <section v-if="showSection('compose')" class="settings-sec">
    <h2 class="text-h6 mb-3">{{ t('写信与发送') }}</h2>
    <div style="max-width: 640px" class="mb-6">
      <v-select
        :model-value="undoSeconds"
        :items="undoItems"
        :label="t('撤销发送等待')"
        variant="outlined"
        density="comfortable"
        hide-details
        style="max-width: 260px"
        @update:model-value="emit('update:undoSeconds', $event)"
      />
    </div>
    <h2 class="text-h6 mb-3">{{ t('定时发送') }}</h2>
    <template v-if="schStatus === 'loading'">
      <v-skeleton-loader type="list-item-two-line@2" max-width="640" class="skel-clip" />
    </template>
    <template v-else-if="schStatus === 'error'">
      <v-alert type="error" variant="tonal" density="compact" max-width="640">
        {{ t('定时发送加载失败：{error}', { error: schError }) }}
        <v-btn variant="text" size="small" @click="loadScheduled">{{ t('重试') }}</v-btn>
      </v-alert>
    </template>
    <template v-else>
      <p v-if="sch.length === 0" class="text-body-2 text-medium-emphasis">{{ t('没有定时发送的邮件。') }}</p>
      <v-list v-else density="comfortable" class="pa-0" style="max-width: 640px">
        <v-list-item v-for="it in sch" :key="it.id" class="px-0">
          <v-list-item-title class="text-body-2">{{ it.subject || t('(无主题)') }}</v-list-item-title>
          <v-list-item-subtitle>
            {{ t('发给 {to}，计划 {time}', { to: it.to, time: fmtDateTime(it.send_at) }) }}
            <template v-if="it.status === 'sent'">{{ t('，已发送') }}</template>
            <template v-else-if="it.status === 'failed'">{{ t('，发送失败') }}<template v-if="it.error">（{{ it.error }}）</template></template>
            <template v-else>{{ t('，待发送') }}</template>
          </v-list-item-subtitle>
          <template v-if="it.status === 'pending'" #append>
            <v-btn variant="text" size="small" color="error" @click="cancelSch(it)">{{ t('取消') }}</v-btn>
          </template>
        </v-list-item>
      </v-list>
    </template>

    </section>

    <section v-if="showSection('push')" class="settings-sec">
    <h2 class="text-h6 mb-3">{{ t('推送') }}</h2>
    <div class="text-subtitle-1 font-weight-medium mb-3">Telegram</div>
    <template v-if="tgStatus === 'loading'">
      <v-skeleton-loader type="article" max-width="640" class="skel-clip" />
    </template>
    <template v-else-if="tgStatus === 'error'">
      <v-alert type="error" variant="tonal" density="compact" max-width="640">
        {{ t('设置加载失败：{error}', { error: tgMsg }) }}
        <v-btn variant="text" size="small" @click="loadTG">{{ t('重试') }}</v-btn>
      </v-alert>
    </template>
    <div v-else style="max-width: 640px">
      <v-switch v-model="tg.enabled" :label="t('启用新邮件推送')" color="primary" hide-details class="mb-2" />
      <v-text-field
        v-model="tg.bot_token"
        label="Bot Token"
        type="password"
        variant="outlined"
        density="comfortable"
        autocomplete="off"
        :placeholder="tg.has_token ? t('已保存，留空不改') : t('如 123456:ABC-DEF...')"
        class="mb-2"
      />
      <v-text-field
        v-model="tg.chat_id"
        label="Chat ID"
        variant="outlined"
        density="comfortable"
        autocomplete="off"
        :placeholder="t('如 123456789')"
        class="mb-2"
      />
      <div class="d-flex align-center ga-2">
        <v-btn color="primary" :disabled="tgBusy !== ''" :loading="tgBusy === 'save'" @click="saveTG">
          {{ t('保存') }}
        </v-btn>
        <v-btn variant="outlined" :disabled="tgBusy !== ''" :loading="tgBusy === 'test'" @click="testTG">
          {{ t('发送测试消息') }}
        </v-btn>
      </div>
    </div>

    <div class="text-subtitle-1 font-weight-medium mb-3 mt-8">{{ t('浏览器推送') }}</div>
    <template v-if="wpStatus === 'loading'">
      <v-skeleton-loader type="article" max-width="640" class="skel-clip" />
    </template>
    <template v-else-if="wpStatus === 'error'">
      <v-alert type="error" variant="tonal" density="compact" max-width="640">
        {{ t('设置加载失败：{error}', { error: wpMsg }) }}
        <v-btn variant="text" size="small" @click="loadWP">{{ t('重试') }}</v-btn>
      </v-alert>
    </template>
    <div v-else style="max-width: 640px">
      <v-switch
        :model-value="thisSub === true"
        :label="t('本浏览器接收推送')"
        color="primary"
        hide-details
        class="mb-2"
        :disabled="wpBusy || !wpSupported"
        @update:model-value="toggleThisBrowser"
      />
      <p class="text-body-2 mb-2">
        {{ t('当前共有 {n} 个浏览器订阅。', { n: wp.subscriptions }) }}
        <template v-if="thisSub === true">{{ t('本浏览器已订阅。') }}</template>
        <template v-else-if="thisSub === false">{{ t('本浏览器未订阅。') }}</template>
      </p>
      <v-alert v-if="!wpSupported" type="warning" variant="tonal" density="compact" class="mb-2">
        {{ t('当前浏览器不支持 Web Push。') }}
      </v-alert>
      <div class="d-flex align-center ga-2">
        <v-btn variant="outlined" :disabled="wpBusy || !wpSupported" @click="testWP">{{ t('发送测试推送') }}</v-btn>
      </div>
    </div>

    </section>

    <div class="text-center text-caption text-medium-emphasis pb-10 mt-8">
      <div>Namakani</div>
      <div v-if="version" class="mt-1">{{ t('版本 {version}', { version }) }}</div>
      <div class="mt-1">© 2026 namakani</div>
    </div>
    </div>
  </div>

  <v-dialog v-model="dialogOpen" max-width="560">
    <v-card>
      <v-card-title>{{ editing ? t('编辑账号') : t('添加账号') }}</v-card-title>
      <v-card-text>
        <v-form @submit.prevent="save">
          <v-select
            v-model="form.kind"
            :label="t('账号类型')"
            :items="[
              { title: t('IMAP/SMTP'), value: 'imap' },
              { title: 'Proton Mail', value: 'proton' },
              { title: 'Tuta Mail', value: 'tuta' },
            ]"
            :disabled="!!editing"
            variant="outlined"
            density="comfortable"
            class="mb-2"
          />
          <v-text-field v-model="form.label" :label="t('名称')" variant="outlined" density="comfortable" :placeholder="t('如 工作邮箱')" required class="mb-2" />
          <v-text-field v-model="form.email" :label="t('邮箱')" type="email" variant="outlined" density="comfortable" required class="mb-2" />
          <v-text-field v-model="form.from_name" :label="t('发件人名称')" variant="outlined" density="comfortable" :placeholder="t('对方收件箱里显示的名字')" class="mb-2" />
          <v-textarea
            v-model="form.signature"
            :label="t('签名')"
            variant="outlined"
            density="comfortable"
            rows="3"
            class="mb-2"
          />
          <v-divider class="my-3" />
          <div class="text-subtitle-2 mb-2">{{ t('认证') }}</div>
          <template v-if="form.kind === 'proton'">
            <v-text-field
              v-model="form.password"
              :label="t('登录密码')"
              type="password"
              variant="outlined"
              density="comfortable"
              :placeholder="editing ? t('留空 = 不修改') : ''"
              :required="!editing"
              class="mb-2"
            />
            <v-text-field
              v-model="form.mailbox_password"
              :label="t('邮箱密码（可选）')"
              type="password"
              variant="outlined"
              density="comfortable"
              :placeholder="t('留空 = 与登录密码相同')"
              class="mb-2"
            />
            <v-text-field
              v-if="needsTotp"
              v-model="form.totp"
              :label="t('两步验证码')"
              variant="outlined"
              density="comfortable"
              class="mb-2"
            />
            <div v-if="needsCaptcha" class="mb-2">
              <v-btn block color="primary" variant="tonal" prepend-icon="mdi-open-in-new" @click="captchaStep">
                {{ captchaOpened ? t('完成验证并继续') : t('打开人机验证') }}
              </v-btn>
            </div>
          </template>
          <template v-else-if="form.kind === 'tuta'">
            <v-text-field
              v-model="form.password"
              :label="t('登录密码')"
              type="password"
              variant="outlined"
              density="comfortable"
              :placeholder="editing ? t('留空 = 不修改') : ''"
              :required="!editing"
              class="mb-2"
            />
            <v-text-field
              v-if="needsTotp"
              v-model="form.totp"
              :label="t('两步验证码')"
              variant="outlined"
              density="comfortable"
              class="mb-2"
            />
          </template>
          <template v-else>
            <v-text-field v-model="form.username" :label="t('用户名')" variant="outlined" density="comfortable" required class="mb-2" />
            <v-text-field
              v-model="form.password"
              :label="t('密码')"
              type="password"
              variant="outlined"
              density="comfortable"
              :placeholder="editing ? t('留空 = 不修改') : ''"
              :required="!editing"
              class="mb-2"
            />
            <v-row dense class="mb-2">
              <v-col cols="12" sm="6">
                <v-text-field v-model="form.imap_host" :label="t('IMAP 服务器')" variant="outlined" density="comfortable" placeholder="imap.example.com" required hide-details />
              </v-col>
              <v-col cols="6" sm="3">
                <v-text-field v-model.number="form.imap_port" :label="t('端口')" type="number" variant="outlined" density="comfortable" required hide-details />
              </v-col>
              <v-col cols="6" sm="3">
                <v-select v-model="form.imap_tls" :items="tlsItems" :label="t('加密')" variant="outlined" density="comfortable" hide-details />
              </v-col>
              <v-col cols="12" sm="6">
                <v-text-field v-model="form.smtp_host" :label="t('SMTP 服务器')" variant="outlined" density="comfortable" placeholder="smtp.example.com" required hide-details />
              </v-col>
              <v-col cols="6" sm="3">
                <v-text-field v-model.number="form.smtp_port" :label="t('端口')" type="number" variant="outlined" density="comfortable" required hide-details />
              </v-col>
              <v-col cols="6" sm="3">
                <v-select v-model="form.smtp_tls" :items="tlsItems" :label="t('加密')" variant="outlined" density="comfortable" hide-details />
              </v-col>
            </v-row>
            <v-switch v-model="form.tls_skip_verify" :label="t('跳过证书校验')" color="primary" hide-details class="mb-2" />
          </template>
          <div class="d-flex align-center ga-2 mb-2">
            <v-btn
              variant="tonal"
              :prepend-icon="testOk ? 'mdi-check' : 'mdi-connection'"
              :color="testOk ? 'success' : undefined"
              :loading="testBusy"
              @click="testConnection"
            >
              {{ testOk ? t('连接成功') : t('测试连接') }}
            </v-btn>
          </div>
          <div v-if="testResult" class="text-body-2 text-success mb-2">{{ testResult }}</div>
          <v-alert v-if="formError" type="error" variant="tonal" density="compact">{{ formError }}</v-alert>
        </v-form>
      </v-card-text>
      <v-card-actions>
        <v-spacer />
        <v-btn variant="text" @click="dialogOpen = false">{{ t('取消') }}</v-btn>
        <v-btn color="primary" @click="save">{{ t('保存') }}</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>

  <v-dialog v-model="rcOpen" max-width="480">
    <v-card>
      <v-card-title>{{ t('重新连接') }}</v-card-title>
      <v-card-text>
        <p class="text-body-2 mb-3">{{ rcTarget?.label }}（{{ rcTarget?.email }}）</p>
        <v-text-field
          v-model="rcForm.password"
          :label="t('登录密码')"
          type="password"
          variant="outlined"
          density="comfortable"
          class="mb-2"
        />
        <v-text-field
          v-model="rcForm.mailbox_password"
          :label="t('邮箱密码（可选）')"
          type="password"
          variant="outlined"
          density="comfortable"
          :placeholder="t('留空 = 与登录密码相同')"
          class="mb-2"
        />
        <v-text-field
          v-if="rcNeedsTotp"
          v-model="rcForm.totp"
          :label="t('两步验证码')"
          variant="outlined"
          density="comfortable"
          class="mb-2"
        />
        <div v-if="rcNeedsCaptcha" class="mb-2">
          <v-btn block color="primary" variant="tonal" prepend-icon="mdi-open-in-new" :loading="rcBusy" @click="rcCaptchaStep">
            {{ rcCaptchaOpened ? t('完成验证并继续') : t('打开人机验证') }}
          </v-btn>
        </div>
        <v-alert v-if="rcError" type="error" variant="tonal" density="compact">{{ rcError }}</v-alert>
      </v-card-text>
      <v-card-actions>
        <v-spacer />
        <v-btn variant="text" @click="rcOpen = false">{{ t('取消') }}</v-btn>
        <v-btn color="primary" :loading="rcBusy" @click="rcSubmit">{{ t('重新连接') }}</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>

  <v-dialog v-model="deleteOpen" max-width="440">
    <v-card>
      <v-card-title>{{ t('删除账号') }}</v-card-title>
      <v-card-text>
        {{ t('删除账号「{label}」？本地同步的邮件会一并删除，服务器上的邮件不受影响。', { label: deleteTarget?.label }) }}
      </v-card-text>
      <v-card-actions>
        <v-spacer />
        <v-btn variant="text" @click="deleteOpen = false">{{ t('取消') }}</v-btn>
        <v-btn color="error" @click="confirmDelete">{{ t('删除') }}</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>

  <v-snackbar v-model="snack.show" :timeout="snack.timeout" location="bottom">
    {{ snack.text }}
  </v-snackbar>
</template>

<style>
.settings-root {
  display: flex;
  align-items: stretch;
  width: 100%;
  height: 100%;
  min-height: 0;
}
.settings-nav {
  width: 240px;
  flex: none;
  overflow-y: auto;
}
.settings-content {
  flex: 1;
  min-width: 0;
  max-width: 880px;
  margin-inline: auto;
  padding: 16px;
  overflow-y: auto;
  min-height: 0;
}
.settings-sec {
  margin-top: 24px;
}
.settings-content > section:first-child {
  margin-top: 0;
}


</style>
