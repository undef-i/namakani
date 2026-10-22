<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useDisplay, useTheme } from 'vuetify'
import Auth from './components/Auth.vue'
import Accounts from './components/Accounts.vue'
import Compose from './components/Compose.vue'
import * as api from './api.js'

const SCOPES = [
  { key: 'inbox', label: '收件箱', icon: 'mdi-inbox' },
  { key: 'sent', label: '已发送', icon: 'mdi-send' },
  { key: 'drafts', label: '草稿箱', icon: 'mdi-file-document-edit-outline' },
  { key: 'archive', label: '归档', icon: 'mdi-archive' },
  { key: 'junk', label: '垃圾邮件', icon: 'mdi-alert-octagon-outline' },
  { key: 'trash', label: '回收站', icon: 'mdi-delete' },
]
const PAGE_SIZE = 60

const display = useDisplay()
const theme = useTheme()
const lgAndUp = computed(() => display.lgAndUp.value)
const wideLayout = computed(() => display.width.value >= 1440)

const view = ref('loading')
const appVersion = ref('')
const bootError = ref('')
const authMode = ref('login')
const accounts = ref([])
const accountsStatus = ref('loading')
const folders = ref([])
const foldersStatus = ref('loading')
const foldersError = ref('')
const messages = ref([])
const listStatus = ref('loading')
const listError = ref('')
const statuses = ref({})
const settingsOpen = ref(false)
const scope = ref('inbox')
const selected = ref(null)
const detail = ref(null)
const detailState = ref('idle')
const detailError = ref('')
const opBusy = ref(false)
let detailToken = 0
const frameEl = ref(null)
const drawerOpen = ref(false)
const loadingMore = ref(false)
const listHasMore = ref(false)
const themeMode = ref('system')
let systemDarkMQ = null
const composeOpen = ref(false)
const composeInit = ref({})
const searching = ref(false)
const searchQuery = ref('')
let searchTimer = null
const threaded = ref(false)
const expandedKey = ref('')
const threadMsgs = ref([])
const cursorIdx = ref(0)
const helpOpen = ref(false)
const listEl = ref(null)
const searchEl = ref(null)
let pendingReply = false
let listOffset = 0
const detailCache = new Map()
let es = null
let esTimer = null
let esDelay = 1000
let statusTimer = null
let booted = false

const scopeLabel = computed(() => SCOPES.find((s) => s.key === scope.value)?.label ?? scope.value)
const scopeStats = computed(() => {
  const map = {}
  for (const f of folders.value) {
    if (!f.special_use) continue
    const cur = map[f.special_use] ?? { total: 0, unread: 0, present: false }
    cur.total += f.total
    cur.unread += f.unread
    cur.present = true
    map[f.special_use] = cur
  }
  return map
})
const visibleScopes = computed(() => SCOPES.filter((s) => scopeStats.value[s.key]?.present))

function applyTheme() {
  const dark =
    themeMode.value === 'dark' || (themeMode.value === 'system' && !!systemDarkMQ?.matches)
  theme.global.name.value = dark ? 'dark' : 'light'
}

function setThemeMode(v) {
  if (!['system', 'light', 'dark'].includes(v)) return
  themeMode.value = v
  localStorage.setItem('wm_theme_mode', v)
  applyTheme()
}

function initTheme() {
  let stored = localStorage.getItem('wm_theme_mode')
  if (!stored) {
    const old = localStorage.getItem('wm_dark')
    if (old !== null) {
      stored = old === '1' ? 'dark' : 'light'
      localStorage.setItem('wm_theme_mode', stored)
    }
    localStorage.removeItem('wm_dark')
  }
  themeMode.value = ['system', 'light', 'dark'].includes(stored) ? stored : 'system'
  systemDarkMQ = window.matchMedia('(prefers-color-scheme: dark)')
  systemDarkMQ.addEventListener('change', applyTheme)
  applyTheme()
}

const AVATAR_COLORS = [
  '#c62828', '#ad1457', '#6a1b9a', '#4527a0', '#283593', '#1565c0',
  '#0277bd', '#00838f', '#00695c', '#2e7d32', '#558b2f', '#9e9d24',
  '#f9a825', '#ef6c00', '#d84315', '#4e342e', '#616161', '#546e7a',
]

function avatarColor(email) {
  const s = email || ''
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0
  return AVATAR_COLORS[h % AVATAR_COLORS.length]
}

function avatarLetter(m) {
  const s = (m.from_name || m.from_email || '?').trim()
  return s ? s.charAt(0).toUpperCase() : '?'
}

function fmtListTime(ts) {
  const d = new Date(ts * 1000)
  const now = new Date()
  const pad = (n) => String(n).padStart(2, '0')
  if (d.toDateString() === now.toDateString()) return pad(d.getHours()) + ':' + pad(d.getMinutes())
  if (d.getFullYear() === now.getFullYear()) return d.getMonth() + 1 + '/' + d.getDate()
  return d.getFullYear() + '/' + (d.getMonth() + 1) + '/' + d.getDate()
}

function fmtFull(ts) {
  const d = new Date(ts * 1000)
  const pad = (n) => String(n).padStart(2, '0')
  return (
    d.getFullYear() +
    '-' +
    pad(d.getMonth() + 1) +
    '-' +
    pad(d.getDate()) +
    ' ' +
    pad(d.getHours()) +
    ':' +
    pad(d.getMinutes())
  )
}

function fmtSize(n) {
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1024 / 1024).toFixed(1) + ' MB'
}

function fmtAddrList(list) {
  return (list ?? []).map((a) => (a.name ? a.name + ' <' + a.email + '>' : a.email)).join(', ')
}


async function loadMessages() {
  listStatus.value = 'loading'
  listError.value = ''
  listOffset = 0
  expandedKey.value = ''
  threadMsgs.value = []
  cursorIdx.value = 0
  try {
    if (searching.value) {
      messages.value = await api.searchMessages(searchQuery.value)
      listHasMore.value = false
    } else {
      const batch = await api.getMessages(scope.value, 0, PAGE_SIZE, threaded.value)
      messages.value = batch
      listOffset = batch.length
      listHasMore.value = batch.length === PAGE_SIZE
    }
    listStatus.value = 'ready'
  } catch (err) {
    messages.value = []
    listHasMore.value = false
    listStatus.value = 'error'
    listError.value = err.message
  }
}

async function loadMore() {
  if (searching.value || listStatus.value !== 'ready' || loadingMore.value || !listHasMore.value) return
  loadingMore.value = true
  try {
    const batch = await api.getMessages(scope.value, listOffset, PAGE_SIZE, threaded.value)
    const seen = new Set(messages.value.map((m) => m.id))
    messages.value = [...messages.value, ...batch.filter((m) => !seen.has(m.id))]
    listOffset += batch.length
    listHasMore.value = batch.length === PAGE_SIZE
  } catch {
  } finally {
    loadingMore.value = false
  }
}

async function reloadLoaded() {
  if (searching.value || listStatus.value === 'loading') return
  try {
    const n = Math.max(PAGE_SIZE, messages.value.length)
    const batch = await api.getMessages(scope.value, 0, n, threaded.value)
    messages.value = batch
    listOffset = batch.length
    listHasMore.value = batch.length === n
  } catch {
  }
}

function onListScroll() {
  const el = listEl.value
  if (!el) return
  if (el.scrollTop + el.clientHeight > el.scrollHeight - 500) loadMore()
}

const PTR_TRIGGER = 64
const ptrRaw = ref(0)
const ptrBusy = ref(false)
const ptrTracking = ref(false)
let ptrStartY = -1
let ptrAttachedEl = null

const ptrTotal = computed(() => ptrRaw.value * 0.5)
const ptrDragPercent = computed(() => Math.min(1, Math.abs(ptrTotal.value) / PTR_TRIGGER))
const ptrAdjusted = computed(() => Math.max(ptrDragPercent.value - 0.4, 0) * (5 / 3))
const ptrTension = computed(() => {
  const extraOS = Math.abs(ptrTotal.value) - PTR_TRIGGER
  const tsp = Math.max(0, Math.min(extraOS, PTR_TRIGGER * 2) / PTR_TRIGGER)
  return ((tsp / 4) - (tsp / 4) ** 2) * 2
})
const ptrY = computed(() => {
  if (ptrBusy.value) return 14
  return -58 + PTR_TRIGGER * ptrDragPercent.value + PTR_TRIGGER * ptrTension.value * 2
})
const ptrArc = computed(() => {
  const adj = ptrAdjusted.value
  const sweepDeg = Math.min(0.8, adj * 0.8) * 360
  const startDeg = (-0.25 + 0.4 * adj + ptrTension.value * 2) * 0.5 * 360
  const cx = 20
  const cy = 20
  const r = 8.75
  const rad = (d) => (d * Math.PI) / 180
  const sx = cx + r * Math.cos(rad(startDeg))
  const sy = cy + r * Math.sin(rad(startDeg))
  const ex = cx + r * Math.cos(rad(startDeg + sweepDeg))
  const ey = cy + r * Math.sin(rad(startDeg + sweepDeg))
  const path =
    sweepDeg <= 0.1
      ? ''
      : 'M ' + sx.toFixed(2) + ' ' + sy.toFixed(2) + ' A ' + r + ' ' + r + ' 0 ' + (sweepDeg > 180 ? 1 : 0) + ' 1 ' + ex.toFixed(2) + ' ' + ey.toFixed(2)
  const s = Math.min(1, adj)
  let arrow = ''
  if (s > 0) {
    const w = 10 * s
    const h = 5 * s
    const ox = r + cx - w / 2
    const oy = cy + 1.25
    const a = rad(startDeg + sweepDeg)
    const cos = Math.cos(a)
    const sin = Math.sin(a)
    arrow = [
      [ox, oy],
      [ox + w, oy],
      [ox + w / 2, oy + h],
    ]
      .map(([x, y]) => {
        const dx = x - cx
        const dy = y - cy
        return (cx + dx * cos - dy * sin).toFixed(2) + ',' + (cy + dx * sin + dy * cos).toFixed(2)
      })
      .join(' ')
  }
  return { path, arrow }
})

function ptrStart(e) {
  if (ptrBusy.value) return
  const el = listEl.value
  if (!el || el.scrollTop > 0) {
    ptrTracking.value = false
    return
  }
  ptrStartY = e.touches[0].clientY
  ptrTracking.value = true
}

function ptrMove(e) {
  if (!ptrTracking.value || ptrBusy.value) return
  const el = listEl.value
  if (!el) return
  if (el.scrollTop > 0) {
    ptrRaw.value = 0
    return
  }
  const dy = e.touches[0].clientY - ptrStartY
  if (dy <= 0) {
    ptrRaw.value = 0
    return
  }
  ptrRaw.value = dy
  if (e.cancelable) e.preventDefault()
}

async function ptrEnd() {
  if (!ptrTracking.value) return
  ptrTracking.value = false
  if (ptrBusy.value) return
  if (ptrTotal.value >= PTR_TRIGGER) {
    ptrBusy.value = true
    ptrRaw.value = 0
    try {
      await refreshData()
    } finally {
      ptrBusy.value = false
    }
  } else {
    ptrRaw.value = 0
  }
}

watch(listEl, (el) => {
  if (!el || el === ptrAttachedEl) return
  ptrAttachedEl = el
  el.addEventListener('touchstart', ptrStart, { passive: true })
  el.addEventListener('touchmove', ptrMove, { passive: false })
  el.addEventListener('touchend', ptrEnd)
  el.addEventListener('touchcancel', ptrEnd)
})

function onSearchInput() {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    searching.value = searchQuery.value.trim().length > 0
    selected.value = null
    detail.value = null
    loadMessages()
  }, 300)
}

function clearSearch() {
  searchQuery.value = ''
  searching.value = false
  loadMessages()
}

function openCompose(init) {
  composeInit.value = init
  drawerOpen.value = false
  composeOpen.value = true
}

async function loadFolders() {
  try {
    folders.value = await api.getFolders()
    foldersStatus.value = 'ready'
    foldersError.value = ''
  } catch (err) {
    foldersStatus.value = 'error'
    foldersError.value = err.message
  }
}

async function loadStatuses() {
  const entries = await Promise.all(
    accounts.value.map(async (a) => {
      try {
        return [a.id, await api.getSyncStatus(a.id)]
      } catch {
        return [a.id, null]
      }
    }),
  )
  statuses.value = Object.fromEntries(entries)
}

async function refreshData() {
  await Promise.all([loadFolders(), loadMessages(), loadStatuses()])
}

async function refreshAccounts() {
  try {
    accounts.value = await api.listAccounts()
    accountsStatus.value = 'ready'
  } catch {
    accountsStatus.value = 'error'
  }
  await refreshData()
}


function scheduleStatusRefresh() {
  clearTimeout(statusTimer)
  statusTimer = setTimeout(() => {
    if (view.value === 'app') loadStatuses()
  }, 600)
}

function rowMatchesScope(item) {
  return searching.value ? false : item.folder_special === scope.value
}

function insertFlat(item) {
  const i = messages.value.findIndex((m) => m.id === item.id)
  if (i >= 0) {
    messages.value[i] = { ...messages.value[i], ...item }
    messages.value = [...messages.value]
    return
  }
  if (!rowMatchesScope(item)) return
  messages.value = [...messages.value, item].sort((a, b) => b.date_ts - a.date_ts || b.id - a.id)
}

function insertThreaded(item) {
  const key = item.thread_key || 'id:' + item.id
  const i = messages.value.findIndex(
    (m) => m.account_id === item.account_id && (m.thread_key || 'id:' + m.id) === key,
  )
  if (i < 0) {
    if (!rowMatchesScope(item)) return
    const row = { ...item, thread_key: key, thread_count: 1, thread_unread: item.flags & 1 ? 0 : 1 }
    messages.value = [...messages.value, row].sort((a, b) => b.date_ts - a.date_ts || b.id - a.id)
    return
  }
  const row = { ...messages.value[i] }
  if (messages.value.some((m) => m.id === item.id && m !== messages.value[i])) return
  row.thread_count = (row.thread_count ?? 1) + 1
  if (!(item.flags & 1)) row.thread_unread = (row.thread_unread ?? 0) + 1
  if (item.date_ts > row.date_ts || (item.date_ts === row.date_ts && item.id > row.id)) {
    const counts = { thread_count: row.thread_count, thread_unread: row.thread_unread }
    Object.assign(row, item, counts)
  }
  messages.value[i] = row
  messages.value = [...messages.value].sort((a, b) => b.date_ts - a.date_ts || b.id - a.id)
  if (expandedKey.value === item.account_id + ':' + key) {
    threadMsgs.value = [...threadMsgs.value, item].sort((a, b) => a.date_ts - b.date_ts || a.id - b.id)
  }
}

function applyFlagsEvent(items) {
  const byId = new Map(items.map((it) => [it.id, it.flags]))
  const patch = (m) => (byId.has(m.id) ? { ...m, flags: byId.get(m.id) } : m)
  messages.value = messages.value.map((m) => {
    if (!byId.has(m.id)) return m
    const next = patch(m)
    if (threaded.value && m.thread_count > 1 && (m.flags & 1) !== (next.flags & 1)) {
      reloadLoaded()
    }
    return next
  })
  threadMsgs.value = threadMsgs.value.map(patch)
  if (selected.value && byId.has(selected.value.id)) {
    selected.value = { ...selected.value, flags: byId.get(selected.value.id) }
    if (detail.value) detail.value = { ...detail.value, flags: selected.value.flags }
  }
}

function applyRemovedEvent(ev) {
  const ids = new Set(ev.ids ?? [])
  if (ids.size === 0) return
  if (threaded.value && messages.value.some((m) => ids.has(m.id))) {
    reloadLoaded()
  } else {
    messages.value = messages.value.filter((m) => !ids.has(m.id))
  }
  threadMsgs.value = threadMsgs.value.filter((m) => !ids.has(m.id))
  if (selected.value && ids.has(selected.value.id)) {
    selected.value = null
    detail.value = null
  }
}

function applyFolderEvent(folder) {
  const i = folders.value.findIndex((f) => f.id === folder.id)
  if (i >= 0) {
    folders.value[i] = folder
    folders.value = [...folders.value]
  } else {
    folders.value = [...folders.value, folder]
  }
  if (foldersStatus.value !== 'ready') foldersStatus.value = 'ready'
}

function onServerEvent(ev) {
  if (view.value !== 'app') return
  if (ev.type === 'messages') {
    for (const item of ev.items ?? []) {
      if (threaded.value && !searching.value) insertThreaded(item)
      else insertFlat(item)
    }
    scheduleStatusRefresh()
  } else if (ev.type === 'flags') {
    applyFlagsEvent(ev.items ?? [])
  } else if (ev.type === 'removed') {
    applyRemovedEvent(ev)
  } else if (ev.type === 'folder') {
    applyFolderEvent(ev.folder)
  }
}

function connectEvents() {
  disconnectEvents()
  try {
    es = new EventSource('/api/events')
  } catch {
    scheduleReconnect()
    return
  }
  es.onopen = () => {
    const wasReconnect = esDelay > 1000
    esDelay = 1000
    if (wasReconnect && view.value === 'app') {
      loadFolders()
      reloadLoaded()
      loadStatuses()
    }
  }
  es.onmessage = (e) => {
    try {
      onServerEvent(JSON.parse(e.data))
    } catch {
    }
  }
  es.onerror = () => {
    es?.close()
    es = null
    scheduleReconnect()
  }
}

function scheduleReconnect() {
  clearTimeout(esTimer)
  esTimer = setTimeout(() => {
    if (view.value === 'app') connectEvents()
  }, esDelay)
  esDelay = Math.min(esDelay * 2, 30000)
}

function disconnectEvents() {
  clearTimeout(esTimer)
  if (es) {
    es.close()
    es = null
  }
}


function selectScope(key) {
  scope.value = key
  selected.value = null
  detail.value = null
  drawerOpen.value = false
  searchQuery.value = ''
  searching.value = false
  loadMessages()
}

const FRAME_CSS =
  '<style>html,body{margin:0;padding:0;background:#fff;color:#000;' +
  'font:14px/1.5 Arial,Helvetica,"PingFang SC","Microsoft YaHei",sans-serif;' +
  'overflow-wrap:anywhere}img{max-width:100%;height:auto}table{max-width:100%}a{color:#0000ee}</style>'

function frameLoaded() {
  try {
    const doc = frameEl.value?.contentDocument
    if (doc) frameEl.value.style.height = doc.documentElement.scrollHeight + 'px'
  } catch {
  }
}

function patchLocalFlags(id, flags) {
  messages.value = messages.value.map((x) => (x.id === id ? { ...x, flags } : x))
  threadMsgs.value = threadMsgs.value.map((x) => (x.id === id ? { ...x, flags } : x))
  if (selected.value?.id === id) {
    selected.value = { ...selected.value, flags }
    if (detail.value) detail.value = { ...detail.value, flags }
  }
}

async function markSeen(m) {
  if (m.flags & 1) return
  try {
    const res = await api.setFlags(m.id, { seen: true })
    patchLocalFlags(m.id, res.flags)
    loadFolders()
  } catch {
  }
}

function prefetchNeighbors(m) {
  const pool =
    threadMsgs.value.length && threadMsgs.value.some((x) => x.id === m.id)
      ? threadMsgs.value
      : messages.value
  const i = pool.findIndex((x) => x.id === m.id)
  if (i < 0) return
  for (const n of [pool[i - 1], pool[i + 1]]) {
    if (n && !detailCache.has(n.id)) {
      api
        .getMessage(n.id)
        .then((d) => detailCache.set(n.id, d))
        .catch(() => {})
    }
  }
}

function afterOpen(m) {
  if (pendingReply) {
    pendingReply = false
    replySelected()
  }
  prefetchNeighbors(m)
}


async function openMessageById(id) {
  try {
    const d = await api.getMessage(id)
    detailCache.set(id, d)
    await openMessage({
      id: d.id,
      account_id: d.account_id,
      account_label: d.account_label,
      folder_id: d.folder_id,
      folder_name: d.folder_name,
      folder_special: '',
      thread_key: '',
      subject: d.subject || '',
      from_name: d.from?.name || '',
      from_email: d.from?.email || '',
      date_ts: d.date_ts,
      flags: d.flags,
      has_attach: !!d.has_attach,
      snippet: '',
      size: d.size,
    })
  } catch {
  }
}

function onHashChange() {
  if (!booted) return
  const m = /^#\/mail\/(\d+)$/.exec(location.hash) || /^#m-(\d+)$/.exec(location.hash)
  if (m) {
    const id = +m[1]
    if (selected.value?.id !== id) openMessageById(id)
  } else if (selected.value) {
    selected.value = null
    detail.value = null
  }
}

watch(selected, (sel) => {
  if (!booted) return
  const want = sel ? '#/mail/' + sel.id : ''
  const cur =
    location.hash.startsWith('#/mail/') || location.hash.startsWith('#m-') ? location.hash : ''
  if (want !== cur) {
    history.pushState(null, '', want === '' ? location.pathname + location.search : want)
  }
})

async function openMessage(m) {
  if (scope.value === 'drafts' && !searching.value) {
    try {
      const d = await api.getMessage(m.id)
      openCompose({
        account_id: m.account_id,
        to: fmtAddrList(d.to),
        cc: fmtAddrList(d.cc),
        subject: d.subject || m.subject || '',
        text: d.text || '',
        html: d.html || '',
        draft_id: m.id,
      })
      return
    } catch {
    }
  }
  selected.value = m
  detailError.value = ''
  const cached = detailCache.get(m.id)
  if (cached) {
    detail.value = cached
    detailState.value = 'ready'
    markSeen(m)
    afterOpen(m)
    return
  }
  detail.value = null
  detailState.value = 'loading'
  const tk = ++detailToken
  try {
    const d = await api.getMessage(m.id)
    if (tk !== detailToken) return
    detail.value = d
    detailState.value = 'ready'
    detailCache.set(m.id, d)
    markSeen(m)
    afterOpen(m)
  } catch (err) {
    if (tk === detailToken) {
      detailError.value = err.message
      detailState.value = 'error'
    }
  }
}

async function toggleThread(m) {
  const key = m.account_id + ':' + (m.thread_key || 'id:' + m.id)
  if (expandedKey.value === key) {
    expandedKey.value = ''
    threadMsgs.value = []
    return
  }
  expandedKey.value = key
  threadMsgs.value = []
  try {
    threadMsgs.value = await api.getThreadMessages(scope.value, m.account_id, m.thread_key || 'id:' + m.id)
  } catch {
    threadMsgs.value = []
  }
}

function rowClick(m, idx) {
  cursorIdx.value = idx
  if (threaded.value && !searching.value && (m.thread_count ?? 1) > 1) {
    toggleThread(m)
    return
  }
  openMessage(m)
}

async function toggleSeen() {
  if (!selected.value || opBusy.value) return
  opBusy.value = true
  try {
    const res = await api.setFlags(selected.value.id, { seen: !(selected.value.flags & 1) })
    patchLocalFlags(selected.value.id, res.flags)
    loadFolders()
  } finally {
    opBusy.value = false
  }
}

async function toggleStar() {
  if (!selected.value || opBusy.value) return
  opBusy.value = true
  try {
    const res = await api.setFlags(selected.value.id, { flagged: !(selected.value.flags & 2) })
    patchLocalFlags(selected.value.id, res.flags)
  } finally {
    opBusy.value = false
  }
}

async function starFor(m) {
  try {
    const res = await api.setFlags(m.id, { flagged: !(m.flags & 2) })
    patchLocalFlags(m.id, res.flags)
  } catch {
  }
}

function quoteText() {
  const from = detail.value.from
    ? (detail.value.from.name ? detail.value.from.name + ' ' : '') + '<' + detail.value.from.email + '>'
    : ''
  return (
    '\n\n----- 原邮件 -----\n发件人：' +
    from +
    '\n时间：' +
    fmtFull(selected.value.date_ts) +
    '\n主题：' +
    (selected.value.subject || '') +
    '\n\n' +
    (detail.value.text || '')
  )
}

function replySelected() {
  if (!selected.value || !detail.value) return
  const subj = selected.value.subject || ''
  openCompose({
    account_id: selected.value.account_id,
    to: detail.value.from
      ? detail.value.from.name
        ? detail.value.from.name + ' <' + detail.value.from.email + '>'
        : detail.value.from.email
      : '',
    subject: /^re:/i.test(subj) ? subj : 'Re: ' + subj,
    text: quoteText(),
    reply_to_id: selected.value.id,
  })
}

function forwardSelected() {
  if (!selected.value || !detail.value) return
  const subj = selected.value.subject || ''
  openCompose({
    account_id: selected.value.account_id,
    subject: /^(fwd|fw):/i.test(subj) ? subj : 'Fwd: ' + subj,
    text: quoteText(),
  })
}

function afterRemoval(id) {
  threadMsgs.value = threadMsgs.value.filter((m) => m.id !== id)
  if (selected.value?.id === id) {
    selected.value = null
    detail.value = null
  }
  if (threaded.value && !searching.value) {
    reloadLoaded()
  } else {
    messages.value = messages.value.filter((m) => m.id !== id)
  }
  loadFolders()
}

async function archiveFor(m) {
  if (!m || opBusy.value) return
  opBusy.value = true
  try {
    await api.moveMessage(m.id, 'archive')
    afterRemoval(m.id)
  } finally {
    opBusy.value = false
  }
}

async function deleteFor(m) {
  if (!m || opBusy.value) return
  opBusy.value = true
  try {
    await api.deleteMessage(m.id)
    afterRemoval(m.id)
  } finally {
    opBusy.value = false
  }
}

async function markAllRead() {
  if (searching.value || opBusy.value) return
  opBusy.value = true
  try {
    await api.markAllRead(scope.value)
    messages.value = messages.value.map((m) =>
      threaded.value
        ? { ...m, flags: m.flags | 1, thread_unread: 0 }
        : { ...m, flags: m.flags | 1 },
    )
    threadMsgs.value = threadMsgs.value.map((m) => ({ ...m, flags: m.flags | 1 }))
    if (selected.value) patchLocalFlags(selected.value.id, selected.value.flags | 1)
    loadFolders()
  } catch {
  } finally {
    opBusy.value = false
  }
}


function activeMessage() {
  if (selected.value) return selected.value
  return messages.value[cursorIdx.value] ?? null
}

function replyToMessage(m) {
  if (selected.value?.id === m.id && detail.value) {
    replySelected()
    return
  }
  pendingReply = true
  openMessage(m)
}

function moveCursor(d) {
  if (messages.value.length === 0) return
  cursorIdx.value = Math.max(0, Math.min(messages.value.length - 1, cursorIdx.value + d))
  requestAnimationFrame(() => {
    listEl.value
      ?.querySelector('[data-rowidx="' + cursorIdx.value + '"]')
      ?.scrollIntoView({ block: 'nearest' })
  })
}

function onGlobalKey(e) {
  if (view.value !== 'app' || settingsOpen.value) return
  if (composeOpen.value || helpOpen.value) return
  const t = e.target
  if (
    t &&
    (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable)
  ) {
    return
  }
  const m = activeMessage()
  switch (e.key) {
    case 'j':
      moveCursor(1)
      break
    case 'k':
      moveCursor(-1)
      break
    case 'Enter': {
      const row = messages.value[cursorIdx.value]
      if (row) rowClick(row, cursorIdx.value)
      break
    }
    case 'e':
      if (m) archiveFor(m)
      break
    case 'Delete':
      if (m) deleteFor(m)
      break
    case 's':
      if (m) starFor(m)
      break
    case 'r':
      if (m) replyToMessage(m)
      break
    case 'a':
      markAllRead()
      break
    case 'c':
      openCompose({})
      break
    case '/':
      e.preventDefault()
      searchEl.value?.focus()
      break
    case '?':
      helpOpen.value = true
      break
    default:
      return
  }
}


function toggleDrawer() {
  drawerOpen.value = !drawerOpen.value
}
watch(wideLayout, (w) => {
  drawerOpen.value = w
}, { immediate: true })

let readerTouch = null
function onReaderTouchStart(e) {
  if (lgAndUp.value) return
  const t = e.touches[0]
  readerTouch = { x: t.clientX, y: t.clientY }
}
function onReaderTouchEnd(e) {
  if (!readerTouch || lgAndUp.value) return
  const t = e.changedTouches[0]
  const dx = t.clientX - readerTouch.x
  const dy = t.clientY - readerTouch.y
  readerTouch = null
  const scroller = e.currentTarget.querySelector('.reader-scroll')
  const atTop = !scroller || scroller.scrollTop <= 0
  if ((dx > 90 && Math.abs(dy) < 60) || (atTop && dy > 110 && Math.abs(dx) < 60)) {
    selected.value = null
    detail.value = null
  }
}

function openAccounts() {
  settingsOpen.value = true
  drawerOpen.value = false
}

async function enter() {
  view.value = 'app'
  threaded.value = localStorage.getItem('wm_threaded') === '1'
  await refreshAccounts()
  connectEvents()
  booted = true
  onHashChange()
}

async function boot() {
  view.value = 'loading'
  try {
    const st = await api.getState()
    appVersion.value = st.version || ''
    if (!st.setup) {
      authMode.value = 'setup'
      view.value = 'auth'
    } else if (!st.authed) {
      authMode.value = 'login'
      view.value = 'auth'
    } else {
      await enter()
    }
  } catch (err) {
    bootError.value = err.message
    view.value = 'error'
  }
}

async function doLogout() {
  booted = false
  disconnectEvents()
  await api.logout().catch(() => {})
  accounts.value = []
  folders.value = []
  messages.value = []
  selected.value = null
  settingsOpen.value = false
  authMode.value = 'login'
  view.value = 'auth'
}

function onThreadedChange(v) {
  threaded.value = v
  localStorage.setItem('wm_threaded', v ? '1' : '0')
  if (!searching.value) loadMessages()
}

onMounted(() => {
  window.addEventListener('hashchange', onHashChange)
  window.addEventListener('keydown', onGlobalKey)
  initTheme()
  boot()
})

onBeforeUnmount(() => {
  window.removeEventListener('hashchange', onHashChange)
  window.removeEventListener('keydown', onGlobalKey)
  systemDarkMQ?.removeEventListener('change', applyTheme)
  disconnectEvents()
})
</script>

<template>
  <v-app>
    <div v-if="view === 'loading'" class="boot-center">
      <v-progress-circular indeterminate color="primary" />
      <div class="mt-3 text-body-2 text-medium-emphasis">加载中</div>
    </div>

    <div v-else-if="view === 'error'" class="boot-center px-4">
      <v-alert type="error" variant="tonal" max-width="440" title="启动失败">
        {{ bootError }}
        <div class="mt-2">
          <v-btn size="small" variant="outlined" @click="boot">重试</v-btn>
        </div>
      </v-alert>
    </div>

    <Auth v-else-if="view === 'auth'" :mode="authMode" @authed="enter" />

    <template v-else>
      <v-app-bar height="64" flat>
        <div class="appbar-search" :class="{ 'search-after-drawer': wideLayout && drawerOpen }">
          <div class="search-wrap">
            <v-btn
              icon="mdi-menu"
              variant="text"
              size="small"
              aria-label="展开菜单"
              class="search-menu-btn"
              @click="toggleDrawer"
            />
            <v-text-field
              ref="searchEl"
              v-model="searchQuery"
              placeholder="搜索主题、发件人、正文"
              variant="solo-filled"
              rounded="pill"
              density="compact"
              hide-details
              class="search-field"
              @input="onSearchInput"
            >
              <template v-if="searching" #append-inner>
              <v-btn
                icon="mdi-close"
                variant="text"
                size="x-small"
                aria-label="清除搜索"
                @click="clearSearch"
              />
              </template>
            </v-text-field>
          </div>
        </div>
      </v-app-bar>

      <v-navigation-drawer
        v-model="drawerOpen"
        :permanent="wideLayout"
        :temporary="!wideLayout"
        width="280"
      >
        <div class="d-flex flex-column pa-3 drawer-inner">
          <v-btn
            color="primary"
            block
            rounded="xl"
            prepend-icon="mdi-pencil"
            class="mb-4"
            style="height: 40px; flex: none"
            @click="openCompose({})"
          >
            写信
          </v-btn>

          <div class="text-caption text-medium-emphasis px-1 pb-1">聚合文件夹</div>
          <template v-if="foldersStatus === 'loading'">
            <v-skeleton-loader type="list-item@3" />
          </template>
          <template v-else-if="foldersStatus === 'error'">
            <p class="text-body-2 text-error px-1">
              文件夹加载失败：{{ foldersError }}
              <v-btn variant="text" size="small" @click="loadFolders">重试</v-btn>
            </p>
          </template>
          <template v-else>
            <v-list density="compact" nav class="pa-0">
              <v-list-item
                v-for="s in visibleScopes"
                :key="s.key"
                :prepend-icon="s.icon"
                :title="s.label"
                :active="scope === s.key"
                rounded="pill"
                @click="selectScope(s.key)"
              >
                <template #append>
                  <v-badge
                    v-if="scopeStats[s.key].unread > 0"
                    :content="scopeStats[s.key].unread"
                    color="primary"
                    inline
                  />
                </template>
              </v-list-item>
            </v-list>
            <p v-if="visibleScopes.length === 0" class="text-body-2 text-medium-emphasis px-1">
              暂无文件夹（同步后出现）
            </p>
          </template>

          <div class="mt-auto">
            <v-divider class="mb-2" />
            <v-list density="compact" nav class="pa-0">
              <v-list-item
                prepend-icon="mdi-cog"
                title="设置"
                :active="settingsOpen"
                rounded="pill"
                @click="openAccounts"
              />
              <v-list-item prepend-icon="mdi-logout" title="退出登录" rounded="pill" @click="doLogout" />
            </v-list>
          </div>
        </div>
      </v-navigation-drawer>

      <v-main>
        <div class="mail-layout">
          <section class="list-section">
            <div class="d-flex align-center ga-1 px-2 py-2 border-b">
              <h1 class="text-body-1 font-weight-bold text-truncate px-1">
                {{ searching ? '搜索结果' : scopeLabel }}
              </h1>
              <span v-if="searching && listStatus === 'ready'" class="text-caption text-medium-emphasis">
                {{ messages.length }} 封
              </span>
              <span
                v-else-if="!searching && foldersStatus === 'ready' && scopeStats[scope]"
                class="text-caption text-medium-emphasis text-no-wrap"
              >
                {{ scopeStats[scope].total }} 封<template v-if="scopeStats[scope].unread">（{{ scopeStats[scope].unread }} 未读）</template>
              </span>
            </div>

            <div
              class="ptr-ind"
              :style="{
                transform: 'translate(-50%, ' + ptrY + 'px)',
                opacity: ptrRaw > 2 || ptrBusy ? 1 : 0,
                transition: ptrTracking ? 'none' : 'transform 0.25s ease, opacity 0.2s ease',
              }"
            >
              <v-progress-circular
                v-if="ptrBusy"
                indeterminate
                size="22"
                width="2.5"
                color="primary"
              />
              <svg v-else width="40" height="40" viewBox="0 0 40 40" aria-hidden="true">
                <path
                  v-if="ptrArc.path"
                  :d="ptrArc.path"
                  fill="none"
                  stroke="rgb(var(--v-theme-primary))"
                  stroke-width="2.5"
                />
                <polygon
                  v-if="ptrArc.arrow"
                  :points="ptrArc.arrow"
                  fill="rgb(var(--v-theme-primary))"
                />
              </svg>
            </div>

            <div ref="listEl" class="list-scroll" @scroll="onListScroll">
              <div>
              <template v-if="listStatus === 'loading'">
                <v-skeleton-loader type="list-item-three-line@6" />
              </template>
              <template v-else-if="listStatus === 'error'">
                <div class="pa-4">
                  <v-alert type="error" variant="tonal" density="compact">
                    邮件列表加载失败：{{ listError }}
                    <div class="mt-2">
                      <v-btn size="small" variant="outlined" @click="loadMessages">重试</v-btn>
                    </div>
                  </v-alert>
                </div>
              </template>
              <template v-else-if="messages.length === 0">
                <p class="pa-6 text-body-2 text-medium-emphasis">
                  {{ searching ? '没有找到匹配的邮件。' : '这里还没有邮件。添加账号后会自动同步进来。' }}
                </p>
              </template>
              <template v-else>
                <template v-for="(m, idx) in messages" :key="m.id">
                  <div
                    class="mail-row"
                    :data-rowidx="idx"
                    :class="{ 'mail-row-active': selected?.id === m.id || idx === cursorIdx }"
                    @click="rowClick(m, idx)"
                  >
                    <v-avatar :color="avatarColor(m.from_email)" size="38" class="mail-avatar">
                      <span class="text-white text-body-2 font-weight-medium">{{ avatarLetter(m) }}</span>
                    </v-avatar>
                    <div class="flex-1-1" style="min-width: 0">
                      <div class="d-flex align-center ga-2">
                        <span v-if="!(m.flags & 1)" class="unread-dot" />
                        <span
                          class="text-body-2 text-truncate flex-1-1"
                          :class="{ 'font-weight-bold': !(m.flags & 1) }"
                        >
                          {{ m.from_name || m.from_email || '(未知发件人)' }}
                        </span>
                        <span
                          v-if="threaded && !searching && (m.thread_count ?? 1) > 1"
                          class="text-caption text-medium-emphasis text-no-wrap"
                        >
                          共 {{ m.thread_count }} 封<template v-if="m.thread_unread">，{{ m.thread_unread }} 未读</template>
                        </span>
                        <v-icon v-if="m.has_attach" icon="mdi-paperclip" size="14" class="text-medium-emphasis" />
                        <span class="text-caption text-medium-emphasis text-no-wrap">{{ fmtListTime(m.date_ts) }}</span>
                        <v-btn
                          icon
                          variant="text"
                          size="x-small"
                          density="compact"
                          :aria-label="(m.flags & 2) ? '取消星标' : '星标'"
                          @click.stop="starFor(m)"
                        >
                          <v-icon
                            :icon="(m.flags & 2) ? 'mdi-star' : 'mdi-star-outline'"
                            size="16"
                            :color="(m.flags & 2) ? 'amber-darken-2' : 'medium-emphasis'"
                          />
                        </v-btn>
                      </div>
                      <div class="text-body-2 text-truncate" :class="{ 'font-weight-bold': !(m.flags & 1) }">
                        {{ m.subject || '(无主题)' }}
                      </div>
                      <div class="text-caption text-medium-emphasis text-truncate">
                        {{ m.account_label }}<template v-if="searching">，{{ m.folder_name }}</template><template v-if="m.snippet">，{{ m.snippet }}</template>
                      </div>
                    </div>
                  </div>
                  <template v-if="threaded && !searching && expandedKey === m.account_id + ':' + (m.thread_key || 'id:' + m.id)">
                    <div
                      v-for="sub in threadMsgs"
                      :key="sub.id"
                      class="mail-row mail-row-sub"
                      :class="{ 'mail-row-active': selected?.id === sub.id }"
                      @click="openMessage(sub)"
                    >
                      <div class="d-flex align-center ga-2">
                        <span v-if="!(sub.flags & 1)" class="unread-dot" />
                        <span
                          class="text-body-2 text-truncate flex-1-1"
                          :class="{ 'font-weight-bold': !(sub.flags & 1) }"
                        >
                          {{ sub.from_name || sub.from_email || '(未知发件人)' }}
                        </span>
                        <span class="text-caption text-medium-emphasis text-no-wrap">{{ fmtListTime(sub.date_ts) }}</span>
                      </div>
                      <div class="text-body-2 text-truncate" :class="{ 'font-weight-bold': !(sub.flags & 1) }">
                        {{ sub.subject || '(无主题)' }}
                      </div>
                    </div>
                  </template>
                </template>
                <v-btn
                  v-if="listHasMore && !searching"
                  block
                  variant="text"
                  :loading="loadingMore"
                  @click="loadMore"
                >
                  加载更多
                </v-btn>
              </template>
              </div>
            </div>
          </section>

          <section class="reader-section" :class="{ 'd-none': !lgAndUp && !selected, 'reader-overlay': !lgAndUp && !!selected }">
            <div v-if="!selected" class="h-100 d-flex align-center justify-center">
              <p class="text-body-2 text-medium-emphasis">未选择邮件</p>
            </div>
            <div
              v-else
              class="reader-main"
              @touchstart.passive="onReaderTouchStart"
              @touchend.passive="onReaderTouchEnd"
            >
              <div v-if="!lgAndUp" class="reader-head">
                <v-spacer />
                <v-btn
                  icon="mdi-close"
                  size="small"
                  variant="text"
                  aria-label="关闭"
                  @click="selected = null; detail = null"
                />
              </div>
              <div class="reader-scroll px-3 pb-3 px-sm-4 pb-sm-4">
              <h2 class="text-h6 font-weight-bold mb-2" style="overflow-wrap: anywhere">
                {{ selected.subject || '(无主题)' }}
              </h2>
              <div class="meta-grid text-body-2 mb-3">
                <span class="text-medium-emphasis">发件人</span>
                <span style="overflow-wrap: anywhere">
                  {{
                    detail?.from
                      ? detail.from.name
                        ? detail.from.name + ' <' + detail.from.email + '>'
                        : detail.from.email
                      : selected.from_name
                        ? selected.from_name + ' <' + selected.from_email + '>'
                        : selected.from_email
                  }}
                </span>
                <template v-if="detail?.to?.length">
                  <span class="text-medium-emphasis">收件人</span>
                  <span style="overflow-wrap: anywhere">{{ fmtAddrList(detail.to) }}</span>
                </template>
                <template v-if="detail?.cc?.length">
                  <span class="text-medium-emphasis">抄送</span>
                  <span style="overflow-wrap: anywhere">{{ fmtAddrList(detail.cc) }}</span>
                </template>
                <span class="text-medium-emphasis">时间</span>
                <span>{{ fmtFull(selected.date_ts) }}</span>
                <span class="text-medium-emphasis">账号</span>
                <span style="overflow-wrap: anywhere">{{ selected.account_label }}，{{ selected.folder_name }}</span>
              </div>

              <div class="d-flex align-center ga-1 border-y py-1 mb-2">
                <v-btn icon variant="text" :disabled="opBusy" @click="toggleSeen">
                  <v-icon :icon="selected.flags & 1 ? 'mdi-email' : 'mdi-email-open'" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ selected.flags & 1 ? '标为未读' : '标为已读' }}
                  </v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="opBusy || !detail" @click="replySelected">
                  <v-icon icon="mdi-reply" />
                  <v-tooltip activator="parent" location="bottom">回复</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="opBusy || !detail" @click="forwardSelected">
                  <v-icon icon="mdi-share" />
                  <v-tooltip activator="parent" location="bottom">转发</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="opBusy" @click="toggleStar">
                  <v-icon
                    :icon="selected.flags & 2 ? 'mdi-star' : 'mdi-star-outline'"
                    :color="selected.flags & 2 ? 'amber-darken-2' : undefined"
                  />
                  <v-tooltip activator="parent" location="bottom">
                    {{ selected.flags & 2 ? '取消星标' : '星标' }}
                  </v-tooltip>
                </v-btn>
                <v-btn
                  v-if="scope !== 'archive' && scope !== 'trash'"
                  icon
                  variant="text"
                  :disabled="opBusy"
                  @click="archiveFor(selected)"
                >
                  <v-icon icon="mdi-archive" />
                  <v-tooltip activator="parent" location="bottom">归档</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="opBusy" @click="deleteFor(selected)">
                  <v-icon icon="mdi-delete" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ scope === 'trash' || scope === 'junk' ? '彻底删除' : '删除' }}
                  </v-tooltip>
                </v-btn>
              </div>

              <template v-if="detailState === 'loading'">
                <v-skeleton-loader type="paragraph@4" class="py-4" />
                <p class="text-body-2 text-medium-emphasis">正在从服务器取正文</p>
              </template>
              <template v-else-if="detailState === 'error'">
                <v-alert type="error" variant="tonal" density="compact" class="my-4">
                  正文加载失败：{{ detailError }}
                  <div class="mt-2">
                    <v-btn size="small" variant="outlined" @click="openMessage(selected)">重试</v-btn>
                  </div>
                </v-alert>
              </template>
              <template v-else-if="detail">
                <div v-if="detail.attachments.length > 0" class="d-flex flex-wrap ga-2 py-3">
                  <v-btn
                    v-for="a in detail.attachments"
                    :key="a.id"
                    variant="outlined"
                    size="small"
                    prepend-icon="mdi-paperclip"
                    :href="api.attachmentURL(detail.id, a.id)"
                  >
                    <span class="text-truncate" style="max-width: 55vw">{{ a.filename }}</span>
                    <span class="text-medium-emphasis ml-1">{{ fmtSize(a.size) }}</span>
                  </v-btn>
                </div>
                <div v-if="detail.html" style="overflow: hidden">
                  <iframe
                    ref="frameEl"
                    title="邮件正文"
                    class="mt-2 d-block w-100 border-0"
                    style="height: 480px"
                    sandbox="allow-same-origin"
                    :srcdoc="FRAME_CSS + detail.html"
                    @load="frameLoaded"
                  />
                </div>
                <div v-else-if="detail.text" class="py-3 text-body-2" style="white-space: pre-wrap; line-height: 1.6">
                  {{ detail.text }}
                </div>
                <p v-else class="py-6 text-body-2 text-medium-emphasis">这封邮件没有可显示的正文。</p>
              </template>
              </div>
            </div>
          </section>
        </div>
      </v-main>

      <v-btn
        v-if="!lgAndUp && !selected"
        icon="mdi-pencil"
        color="primary"
        position="fixed"
        location="bottom right"
        class="fab-compose"
        style="width: 52px; height: 52px; flex: none"
        aria-label="写信"
        @click="openCompose({})"
      />

      <v-dialog v-model="settingsOpen" :fullscreen="!lgAndUp" max-width="920" scrollable>
        <v-card>
          <v-card-title class="d-flex align-center">
            设置
            <v-spacer />
            <v-btn icon="mdi-close" size="small" variant="text" aria-label="关闭" @click="settingsOpen = false" />
          </v-card-title>
          <v-card-text class="pa-0">
            <Accounts
              :accounts="accounts"
              :accounts-status="accountsStatus"
              :statuses="statuses"
              :theme-mode="themeMode"
              :threaded="threaded"
              :version="appVersion"
              @changed="refreshAccounts"
              @update:theme-mode="setThemeMode"
              @update:threaded="onThreadedChange"
            />
          </v-card-text>
        </v-card>
      </v-dialog>

      <v-dialog v-model="helpOpen" max-width="420">
        <v-card>
          <v-card-title>键盘快捷键</v-card-title>
          <v-card-text>
            <v-table density="compact">
              <tbody>
                <tr><td class="font-mono">j / k</td><td>下移 / 上移</td></tr>
                <tr><td class="font-mono">Enter</td><td>打开邮件（线程则展开）</td></tr>
                <tr><td class="font-mono">e</td><td>归档</td></tr>
                <tr><td class="font-mono">Delete</td><td>删除</td></tr>
                <tr><td class="font-mono">s</td><td>星标 / 取消星标</td></tr>
                <tr><td class="font-mono">r</td><td>回复</td></tr>
                <tr><td class="font-mono">a</td><td>当前文件夹全部标为已读</td></tr>
                <tr><td class="font-mono">c</td><td>写新邮件</td></tr>
                <tr><td class="font-mono">/</td><td>聚焦搜索框</td></tr>
                <tr><td class="font-mono">?</td><td>本帮助</td></tr>
              </tbody>
            </v-table>
          </v-card-text>
        </v-card>
      </v-dialog>

      <Compose v-model="composeOpen" :accounts="accounts" :initial="composeInit" @done="refreshData" />
    </template>
  </v-app>
</template>

<style>
html,
body {
  height: 100%;
  overscroll-behavior: none;
}
.boot-center {
  min-height: 100dvh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
}
.drawer-inner {
  min-height: 100%;
}
.drawer-inner > * {
  flex-shrink: 0;
}
.mail-layout {
  display: flex;
  height: calc(100dvh - 64px);
  width: 100%;
  overflow: hidden;
}
.search-wrap {
  position: relative;
}
.search-menu-btn {
  position: absolute;
  left: 3px;
  top: 50%;
  transform: translateY(-50%);
  z-index: 2;
}
.search-field .v-field__input {
  padding-inline-start: 46px;
}
.appbar-search {
  flex: 0 0 auto;
  width: 100%;
  padding: 0 12px;
}
@media (min-width: 1280px) {
  .appbar-search {
    width: 380px;
  }
}
@media (min-width: 1440px) {
  .appbar-search.search-after-drawer {
    margin-inline-start: 280px;
  }
}
.list-section {
  display: flex;
  flex-direction: column;
  flex: 1 1 auto;
  min-width: 0;
  height: 100%;
  position: relative;
}
.ptr-ind {
  position: absolute;
  top: 56px;
  left: 50%;
  z-index: 5;
  width: 40px;
  height: 40px;
  border-radius: 50%;
  background: rgb(var(--v-theme-surface));
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.3);
  display: flex;
  align-items: center;
  justify-content: center;
  pointer-events: none;
}
@media (min-width: 1280px) {
  .list-section {
    flex: 0 0 380px;
    border-right: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
  }
}
.list-scroll {
  flex: 1;
  overflow-y: auto;
  overscroll-behavior-y: none;
}
.drawer-inner .v-list--nav {
  padding-inline: 0;
}
.reader-section {
  flex: 1 1 auto;
  min-width: 0;
  height: 100%;
}
.reader-overlay {
  position: fixed;
  inset: 0;
  z-index: 2000;
  background: rgb(var(--v-theme-surface));
}
@media (min-width: 1440px) {
  .reader-scroll > * {
    max-width: 900px;
    margin-inline: auto;
  }
}
.reader-main {
  display: flex;
  flex-direction: column;
  height: 100%;
}
.reader-head {
  display: flex;
  align-items: center;
  flex: none;
  padding: 6px 6px 0;
}
.reader-scroll {
  flex: 1 1 auto;
  min-height: 0;
  overflow-y: auto;
  overflow-x: hidden;
}
.mail-row {
  display: flex;
  flex-direction: row;
  gap: 10px;
  width: 100%;
  padding: 6px 10px;
  border-bottom: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
  cursor: pointer;
  text-align: left;
}
.mail-avatar {
  flex: none;
  margin-top: 2px;
}
.mail-row:hover {
  background: rgba(var(--v-theme-on-surface), 0.04);
}
.mail-row-active {
  background: rgba(var(--v-theme-primary), 0.1);
}
.mail-row-sub {
  flex-direction: column;
  gap: 2px;
  padding-left: 24px;
  background: rgba(var(--v-theme-on-surface), 0.02);
}
.unread-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: rgb(var(--v-theme-primary));
  flex: none;
}
.meta-grid {
  display: grid;
  grid-template-columns: 52px minmax(0, 1fr);
  row-gap: 4px;
}
.fab-compose {
  margin: 0 16px 16px 0;
  z-index: 40;
}
.border-b {
  border-bottom: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
}
.border-y {
  border-top: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
  border-bottom: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
}
.font-mono {
  font-family: ui-monospace, monospace;
}
</style>
