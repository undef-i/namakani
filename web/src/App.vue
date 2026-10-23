<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useDisplay, useLocale, useTheme } from 'vuetify'
import { intlLocale, lang, t } from './i18n'
import Auth from './components/Auth.vue'
import Accounts from './components/Accounts.vue'
import Compose from './components/Compose.vue'
import * as api from './api.js'
import { avatarColorFor, avatarLetterFor } from './avatar.js'

const SCOPES = computed(() => [
  { key: 'inbox', label: t('收件箱'), icon: 'mdi-inbox' },
  { key: 'sent', label: t('已发送'), icon: 'mdi-send' },
  { key: 'drafts', label: t('草稿箱'), icon: 'mdi-file-document-edit-outline' },
  { key: 'archive', label: t('归档'), icon: 'mdi-archive' },
  { key: 'junk', label: t('垃圾邮件'), icon: 'mdi-alert-octagon-outline' },
  { key: 'trash', label: t('回收站'), icon: 'mdi-delete' },
])
const PAGE_SIZE = 60

const display = useDisplay()
const theme = useTheme()
const vuetifyLocale = useLocale()
watch(lang, (v) => (vuetifyLocale.current.value = v === 'zh' ? 'zhHans' : 'en'), { immediate: true })
const lgAndUp = computed(() => display.lgAndUp.value)

const view = ref('loading')
const appVersion = ref('')
const bootError = ref('')
const tgAuthError = ref('')
const authMode = ref('login')
const accounts = ref([])
const accountsStatus = ref('loading')
const folders = ref([])
const foldersStatus = ref('loading')
const foldersError = ref('')
const messages = ref([])
const listStatus = ref('loading')
const listError = ref('')
const listSnap = ref({ kind: 'folder', scope: 'inbox' })
const listBusy = ref(false)
let listGen = 0
const tombstones = new Map()

function snapCtxKey(snap) {
  return snap.kind === 'search'
    ? 'search|' + snap.query
    : 'folder|' + snap.scope + '|' + (snap.folderId ?? 0)
}

function dropTombstoned(batch, snap) {
  const ctx = snapCtxKey(snap)
  return batch.filter((m) => tombstones.get(m.id) !== ctx)
}

function clearTombstonesFor(snap) {
  const ctx = snapCtxKey(snap)
  for (const [id, c] of tombstones) {
    if (c === ctx) tombstones.delete(id)
  }
}
const statuses = ref({})
const settingsOpen = ref(location.hash === '#/settings')
const scope = ref('inbox')
const selected = ref(null)
const detail = ref(null)
const detailState = ref('idle')
const detailError = ref('')
const actionError = ref('')
const opBusy = ref(false)
let detailToken = 0

function clearSelection() {
  detailToken++
  selected.value = null
  detail.value = null
  if (!booted && (location.hash.startsWith('#/mail/') || location.hash.startsWith('#m-'))) {
    history.pushState(null, '', location.pathname + location.search)
  }
}
const frameEl = ref(null)
const drawerOpen = ref(false)
const navHidden = ref(localStorage.getItem('wm_nav_hidden') === '1')
const wideDesktop = computed(() => display.width.value >= 1440)
const rangeId = ref(parseInt(localStorage.getItem('wm_range') ?? '0', 10) || 0)
const rangeAccount = computed(() => accounts.value.find((a) => a.id === rangeId.value) ?? null)
const activeFolder = computed(() => {
  if (!rangeId.value) return null
  return (
    folders.value.find((f) => f.account_id === rangeId.value && f.special_use === scope.value) ??
    null
  )
})

watch(lgAndUp, (up) => {
  drawerOpen.value = up ? !navHidden.value : false
}, { immediate: true })

function setRange(id) {
  if (rangeId.value === id) return
  rangeId.value = id
  localStorage.setItem('wm_range', String(id))
  clearSelection()
  loadMessages()
}
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
const undoSeconds = ref(5)
const maxAttachmentMB = ref(20)
const convo = ref(null)
const convoOpenIds = ref(new Set())
const convoDetails = ref({})
const cursorIdx = ref(0)
const cursorActive = ref(false)
const helpOpen = ref(false)
const listWidth = ref(parseInt(localStorage.getItem('wm_list_width') ?? '0', 10) || 0)
const listWidthVal = computed(() => {
  if (listWidth.value) return Math.min(520, Math.max(320, listWidth.value))
  return display.width.value >= 1280 && display.width.value < 1440 ? 340 : 400
})
let listDrag = null
function onListDragStart(e) {
  listDrag = { x: e.clientX, w: listWidthVal.value }
  window.addEventListener('pointermove', onListDragMove)
  window.addEventListener('pointerup', onListDragEnd, { once: true })
}
function onListDragMove(e) {
  if (!listDrag) return
  listWidth.value = Math.min(520, Math.max(320, listDrag.w + (e.clientX - listDrag.x)))
}
function onListDragEnd() {
  window.removeEventListener('pointermove', onListDragMove)
  if (listWidth.value) localStorage.setItem('wm_list_width', String(Math.round(listWidth.value)))
  listDrag = null
}
const listDensity = ref(localStorage.getItem('wm_list_density') === 'compact' ? 'compact' : 'standard')
function setDensity(v) {
  listDensity.value = v
  localStorage.setItem('wm_list_density', v)
}
const selectedIds = ref(new Set())
const selectionActive = computed(() => selectedIds.value.size > 0)
function toggleRowSelection(m) {
  const s = new Set(selectedIds.value)
  if (s.has(m.id)) s.delete(m.id)
  else s.add(m.id)
  selectedIds.value = s
}
function clearSelectionIds() {
  if (selectedIds.value.size) selectedIds.value = new Set()
}
const allVisibleSelected = computed(
  () =>
    displayMessages.value.length > 0 &&
    displayMessages.value.every((m) => selectedIds.value.has(m.id)),
)
function toggleSelectAll() {
  if (allVisibleSelected.value) clearSelectionIds()
  else selectedIds.value = new Set(displayMessages.value.map((m) => m.id))
}
async function bulkAct(kind) {
  if (opBusy.value || !selectionActive.value) return
  opBusy.value = true
  const ids = [...selectedIds.value]
  try {
    for (const id of ids) {
      if (kind === 'archive') await api.moveMessage(id, 'archive')
      else if (kind === 'delete') await api.deleteMessage(id)
      else if (kind === 'seen') await api.setFlags(id, { seen: true })
      else if (kind === 'unseen') await api.setFlags(id, { seen: false })
      if (kind === 'archive' || kind === 'delete') tombstones.set(id, snapCtxKey(listSnap.value))
    }
    clearSelectionIds()
    if (selected.value && ids.includes(selected.value.id)) clearSelection()
    if (kind === 'archive' || kind === 'delete') reloadLoaded()
    else loadMessages(true)
    loadFolders()
  } finally {
    opBusy.value = false
  }
}
const searchFilter = ref({ attach: false, unread: false, accountId: 0 })
const displayMessages = computed(() => {
  if (!lgAndUp.value || listSnap.value.kind !== 'search') return messages.value
  const f = searchFilter.value
  if (!f.attach && !f.unread && !f.accountId) return messages.value
  return messages.value.filter(
    (m) =>
      (!f.attach || m.has_attach) &&
      (!f.unread || !(m.flags & 1)) &&
      (!f.accountId || m.account_id === f.accountId),
  )
})
function resetSearchFilter() {
  searchFilter.value = { attach: false, unread: false, accountId: 0 }
}
const newMailPending = ref(0)
function jumpToNewMail() {
  newMailPending.value = 0
  listEl.value?.scrollTo({ top: 0 })
  reloadLoaded()
}
const listErrorBar = ref('')
const listHeadTitle = computed(() => {
  if (listSnap.value.kind === 'search') {
    return t('搜索结果') + ' (' + t('{n} 封', { n: messages.value.length }) + ')'
  }
  const label = snapScopeLabel.value
  const st = scopeStats.value[listSnap.value.scope]
  if (!st || foldersStatus.value !== 'ready') return label
  return (
    label +
    ' (' +
    t('{n} 封', { n: st.total }) +
    (st.unread ? t('，{n} 未读', { n: st.unread }) : '') +
    ')'
  )
})
function accountShort(m) {
  const s = m.account_label || ''
  const at = s.indexOf('@')
  return at >= 0 ? s.slice(at + 1) : s
}
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

function deepIdFromHash() {
  const m = /^#\/mail\/(\d+)$/.exec(location.hash) || /^#m-(\d+)$/.exec(location.hash)
  return m ? +m[1] : 0
}

function tgStartParam() {
  const tg = window.Telegram?.WebApp
  if (!tg?.initData) return ''
  let p = tg.initDataUnsafe?.start_param || ''
  if (!p) {
    try {
      p = new URLSearchParams(tg.initData).get('start_param') || ''
    } catch {}
  }
  return /^\d+$/.test(p) ? p : ''
}

const initialDeepId = deepIdFromHash()

function placeholderMessage(id) {
  return {
    id,
    account_id: 0,
    account_label: '',
    folder_id: 0,
    folder_name: '',
    folder_special: '',
    thread_key: '',
    subject: '',
    from_name: '',
    from_email: '',
    date_ts: 0,
    flags: 0,
    has_attach: false,
    snippet: '',
    size: 0,
  }
}

function enrichSelected(d) {
  if (selected.value?.id !== d.id) return
  selected.value = {
    ...selected.value,
    account_id: d.account_id,
    account_label: d.account_label,
    folder_id: d.folder_id,
    folder_name: d.folder_name,
    subject: d.subject || '',
    from_name: d.from?.name || '',
    from_email: d.from?.email || '',
    date_ts: d.date_ts,
    flags: d.flags,
    has_attach: !!d.has_attach,
    size: d.size,
  }
}

const snapScopeLabel = computed(
  () => SCOPES.value.find((s) => s.key === listSnap.value.scope)?.label ?? listSnap.value.scope,
)
const scopeStats = computed(() => {
  const map = {}
  for (const f of folders.value) {
    if (rangeId.value && f.account_id !== rangeId.value) continue
    if (!f.special_use) continue
    const cur = map[f.special_use] ?? { total: 0, unread: 0, present: false }
    cur.total += f.total
    cur.unread += f.unread
    cur.present = true
    map[f.special_use] = cur
  }
  return map
})
const accountInboxUnread = computed(() => {
  const map = {}
  for (const f of folders.value) {
    if (f.special_use === 'inbox') map[f.account_id] = (map[f.account_id] ?? 0) + f.unread
  }
  return map
})
const syncIndicator = computed(() => {
  let anySync = false
  for (const a of accounts.value) {
    const st = statuses.value[a.id]
    if (!st) continue
    if (st.state === 'error' || st.state === 'repair') return 'error'
    if (st.state === 'syncing' || st.state === 'connecting') anySync = true
  }
  return anySync ? 'syncing' : 'idle'
})
const visibleScopes = computed(() => SCOPES.value.filter((s) => scopeStats.value[s.key]?.present))

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

function avatarColor(email) {
  return avatarColorFor(email)
}

function avatarLetter(m) {
  return avatarLetterFor(m.from_name, m.from_email)
}

function fmtListTime(ts) {
  const d = new Date(ts * 1000)
  const now = new Date()
  if (d.toDateString() === now.toDateString())
    return new Intl.DateTimeFormat(intlLocale(), { hour: '2-digit', minute: '2-digit', hour12: false }).format(d)
  if (d.getFullYear() === now.getFullYear())
    return new Intl.DateTimeFormat(intlLocale(), { month: 'numeric', day: 'numeric' }).format(d)
  return new Intl.DateTimeFormat(intlLocale(), { year: 'numeric', month: 'numeric', day: 'numeric' }).format(d)
}

function fmtFull(ts) {
  return new Intl.DateTimeFormat(intlLocale(), {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(new Date(ts * 1000))
}

function fmtFullEn(ts) {
  return new Intl.DateTimeFormat('en-US', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(new Date(ts * 1000))
}

function fmtSize(n) {
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1024 / 1024).toFixed(1) + ' MB'
}

function fmtAddrList(list) {
  return (list ?? []).map((a) => (a.name ? a.name + ' <' + a.email + '>' : a.email)).join(', ')
}


async function loadMessages(soft = false) {
  if (!searching.value && rangeId.value && foldersStatus.value !== 'ready') {
    await loadFolders()
  }
  const target = searching.value
    ? { kind: 'search', query: searchQuery.value.trim() }
    : {
        kind: 'folder',
        scope: scope.value,
        folderId: rangeId.value ? (activeFolder.value?.id ?? -1) : 0,
      }
  const gen = ++listGen
  listBusy.value = true
  if (!soft) {
    clearSelectionIds()
    newMailPending.value = 0
  }
  const keepReady = soft && listStatus.value === 'ready'
  if (!keepReady) listStatus.value = 'loading'
  listError.value = ''
  listOffset = 0
  convo.value = null
  convoOpenIds.value = new Set()
  convoDetails.value = {}
  cursorIdx.value = 0
  try {
    let batch
    if (target.kind === 'search') {
      batch = await api.searchMessages(target.query)
    } else if (target.folderId) {
      batch = await api.getFolderMessages(target.folderId, 0, PAGE_SIZE, threaded.value)
    } else {
      batch = await api.getMessages(target.scope, 0, PAGE_SIZE, threaded.value)
    }
    if (gen !== listGen) return
    messages.value = dropTombstoned(batch, target)
    clearTombstonesFor(target)
    if (target.kind === 'search') {
      listHasMore.value = false
    } else {
      listOffset = batch.length
      listHasMore.value = batch.length === PAGE_SIZE
    }
    listSnap.value = target
    listStatus.value = 'ready'
    listErrorBar.value = ''
  } catch (err) {
    if (gen !== listGen) return
    if (keepReady) {
      listStatus.value = 'ready'
      return
    }
    if (lgAndUp.value && messages.value.length > 0) {
      listErrorBar.value = err.message
      listStatus.value = 'ready'
      return
    }
    messages.value = []
    listHasMore.value = false
    listStatus.value = 'error'
    listError.value = err.message
  } finally {
    if (gen === listGen) listBusy.value = false
  }
}

async function loadMore() {
  if (searching.value || listStatus.value !== 'ready' || loadingMore.value || !listHasMore.value) return
  loadingMore.value = true
  const gen = listGen
  try {
    const batch = listSnap.value.folderId
      ? await api.getFolderMessages(listSnap.value.folderId, listOffset, PAGE_SIZE, threaded.value)
      : await api.getMessages(scope.value, listOffset, PAGE_SIZE, threaded.value)
    if (gen !== listGen) return
    const seen = new Set(messages.value.map((m) => m.id))
    messages.value = [
      ...messages.value,
      ...dropTombstoned(batch, listSnap.value).filter((m) => !seen.has(m.id)),
    ]
    listOffset += batch.length
    listHasMore.value = batch.length === PAGE_SIZE
  } catch {
  } finally {
    loadingMore.value = false
  }
}

async function reloadLoaded() {
  if (searching.value || listStatus.value === 'loading') return
  const gen = ++listGen
  try {
    const n = Math.max(PAGE_SIZE, messages.value.length)
    const folderId = rangeId.value ? (activeFolder.value?.id ?? -1) : 0
    const batch = folderId
      ? await api.getFolderMessages(folderId, 0, n, threaded.value)
      : await api.getMessages(scope.value, 0, n, threaded.value)
    if (gen !== listGen) return
    const snap = { kind: 'folder', scope: scope.value, folderId }
    messages.value = dropTombstoned(batch, snap)
    clearTombstonesFor(snap)
    listOffset = batch.length
    listHasMore.value = batch.length === n
    listSnap.value = snap
  } catch {
  }
}

function onListScroll() {
  const el = listEl.value
  if (!el) return
  if (el.scrollTop <= 80) newMailPending.value = 0
  if (el.scrollTop + el.clientHeight > el.scrollHeight - 500) loadMore()
}

const PTR_TRIGGER = 64
const ptrRaw = ref(0)
const ptrBusy = ref(false)
const ptrTracking = ref(false)
let ptrStartY = -1
let ptrStartX = -1
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
  if (ptrBusy.value) return 29
  return -81 + 102 * ptrDragPercent.value + 48 * ptrTension.value
})
const ptrOpacity = computed(() => {
  if (ptrBusy.value) return 1
  return Math.min(1, ptrDragPercent.value * 1.6)
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
    const a = rad(startDeg + sweepDeg)
    const tx = -Math.sin(a)
    const ty = Math.cos(a)
    const nx = Math.cos(a)
    const ny = Math.sin(a)
    const hw = 5 * s
    const hh = 5 * s
    arrow =
      (ex + tx * hh).toFixed(2) + ',' + (ey + ty * hh).toFixed(2) + ' ' +
      (ex + nx * hw).toFixed(2) + ',' + (ey + ny * hw).toFixed(2) + ' ' +
      (ex - nx * hw).toFixed(2) + ',' + (ey - ny * hw).toFixed(2)
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
  ptrStartX = e.touches[0].clientX
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
  const dx = e.touches[0].clientX - ptrStartX
  if (Math.abs(dx) > 10 && Math.abs(dx) > Math.abs(dy)) {
    ptrRaw.value = 0
    ptrTracking.value = false
    return
  }
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


const SWIPE_COMMIT = 96
const swipeId = ref(0)
const swipeDx = ref(0)
const swipeDragging = ref(false)
const exitId = ref(0)
const exitH = ref(0)
const exitTrans = ref(false)
let swipeStart = null
let swipeWidth = 0
let swipeHeight = 0
let suppressClickUntil = 0

function rowWrapStyle(m) {
  if (exitId.value !== m.id) return {}
  return {
    height: exitH.value + 'px',
    transition: exitTrans.value ? 'height 0.22s ease' : 'none',
  }
}

function rowSwipeStyle(m) {
  if (swipeId.value !== m.id) return {}
  return {
    transform: 'translateX(' + swipeDx.value + 'px)',
    transition: swipeDragging.value ? 'none' : 'transform 0.18s ease',
    background: 'rgb(var(--v-theme-surface))',
  }
}

function rowTouchStart(e, m) {
  if (exitId.value === m.id) return
  swipeStart = { id: m.id, x: e.touches[0].clientX, y: e.touches[0].clientY, engaged: false }
  swipeId.value = m.id
  swipeDx.value = 0
  swipeDragging.value = false
}

function rowTouchMove(e, m) {
  if (!swipeStart || swipeStart.id !== m.id) return
  const dx = e.touches[0].clientX - swipeStart.x
  const dy = e.touches[0].clientY - swipeStart.y
  if (!swipeStart.engaged) {
    if (Math.abs(dx) > 10 && Math.abs(dx) > Math.abs(dy) * 1.2) {
      swipeStart.engaged = true
      swipeDragging.value = true
    } else if (Math.abs(dy) > 10) {
      swipeStart = null
      swipeId.value = 0
      return
    } else {
      return
    }
  }
  if (e.cancelable) e.preventDefault()
  swipeDx.value = dx
}

function rowTouchEnd(e, m) {
  if (!swipeStart || swipeStart.id !== m.id) return
  const engaged = swipeStart.engaged
  swipeStart = null
  swipeDragging.value = false
  if (!engaged) {
    swipeId.value = 0
    swipeDx.value = 0
    return
  }
  suppressClickUntil = Date.now() + 900
  const dx = swipeDx.value
  swipeWidth = e.currentTarget.offsetWidth || window.innerWidth
  swipeHeight = e.currentTarget.parentElement?.offsetHeight ?? e.currentTarget.offsetHeight
  const canArchive = scope.value !== 'archive' && scope.value !== 'trash'
  if (dx >= SWIPE_COMMIT && canArchive) {
    settleSwipe(m, 'archive')
  } else if (dx <= -SWIPE_COMMIT) {
    settleSwipe(m, 'delete')
  } else {
    swipeDx.value = 0
    setTimeout(() => {
      if (!swipeStart) swipeId.value = 0
    }, 200)
  }
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms))
}

async function settleSwipe(m, action) {
  swipeDx.value = action === 'archive' ? swipeWidth : -swipeWidth
  await sleep(200)
  exitTrans.value = false
  exitId.value = m.id
  exitH.value = swipeHeight
  await sleep(30)
  exitTrans.value = true
  exitH.value = 0
  await sleep(240)
  try {
    if (action === 'archive') await archiveFor(m)
    else await deleteFor(m)
    for (let i = 0; i < 40 && messages.value.some((x) => x.id === m.id); i++) {
      await sleep(50)
    }
  } finally {
    swipeId.value = 0
    swipeDx.value = 0
    exitId.value = 0
    exitTrans.value = false
  }
}

function onSearchInput() {
  clearTimeout(searchTimer)
  listGen++
  listBusy.value = true
  searchTimer = setTimeout(() => {
    searching.value = searchQuery.value.trim().length > 0
    clearSelection()
    loadMessages(true)
  }, 300)
}

function clearSearch() {
  clearTimeout(searchTimer)
  searchQuery.value = ''
  searching.value = false
  resetSearchFilter()
  loadMessages(true)
}

function openCompose(init) {
  composeInit.value = init
  if (!lgAndUp.value) drawerOpen.value = false
  composeZ.value = nextLayer()
  composeOpen.value = true
}

const layerSeq = ref(0)
const composeZ = ref(2500)
const settingsZ = ref(2500)
function nextLayer() {
  layerSeq.value += 1
  return 2490 + layerSeq.value * 10
}
watch(settingsOpen, (v) => {
  if (v) settingsZ.value = nextLayer()
})
watch([composeOpen, settingsOpen], ([c, s]) => {
  if (!c && !s) layerSeq.value = 0
})

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

async function onComposeDone() {
  const jobs = [loadFolders(), loadStatuses()]
  if (scope.value === 'drafts' || scope.value === 'sent') jobs.push(loadMessages(true))
  await Promise.all(jobs)
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
  if (searching.value) return false
  if (rangeId.value) return item.folder_id === (activeFolder.value?.id ?? -1)
  return item.folder_special === scope.value
}

function insertFlat(item) {
  if (tombstones.get(item.id) === snapCtxKey(listSnap.value)) return
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
  if (tombstones.get(item.id) === snapCtxKey(listSnap.value)) return
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
  if (convo.value && convo.value.accountId === item.account_id && convo.value.threadKey === key) {
    convo.value = {
      ...convo.value,
      msgs: [...convo.value.msgs, item].sort((a, b) => a.date_ts - b.date_ts || a.id - b.id),
    }
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
  if (convo.value) {
    convo.value = { ...convo.value, msgs: convo.value.msgs.map(patch) }
  }
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
  if (convo.value) {
    convo.value = { ...convo.value, msgs: convo.value.msgs.filter((m) => !ids.has(m.id)) }
  }
  if (selected.value && ids.has(selected.value.id)) {
    clearSelection()
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
    const beforeCount = messages.value.length
    for (const item of ev.items ?? []) {
      if (threaded.value && !searching.value) insertThreaded(item)
      else insertFlat(item)
    }
    if (
      listSnap.value.kind === 'folder' &&
      messages.value.length > beforeCount &&
      listEl.value &&
      listEl.value.scrollTop > 80
    ) {
      newMailPending.value += messages.value.length - beforeCount
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
  clearSelection()
  if (!lgAndUp.value) drawerOpen.value = false
  searchQuery.value = ''
  searching.value = false
  loadMessages()
}

const frameCss = computed(() => {
  const dark = theme.global.current.value.dark
  const fg = dark ? '#e6e6e6' : '#000'
  const link = dark ? '#a8c7fa' : '#0b57d0'
  const scheme = dark ? 'dark' : 'light'
  const font = lgAndUp.value
    ? 'font:15px/1.8 Arial,Helvetica,"PingFang SC","Microsoft YaHei",sans-serif;'
    : 'font:14px/1.5 Arial,Helvetica,"PingFang SC","Microsoft YaHei",sans-serif;'
  const quote = lgAndUp.value
    ? 'blockquote{border-left:2px solid ' +
      (dark ? '#8e9196' : '#74787c') +
      ';color:' +
      (dark ? '#c4c7ca' : '#43474c') +
      ';margin-left:0;padding-left:12px}'
    : ''
  return (
    '<style>:root{color-scheme:' + scheme + '}html,body{margin:0;padding:0;background:transparent;color:' + fg + ';' +
    font +
    'overflow-wrap:anywhere}img{max-width:100%;height:auto}table{max-width:100%}a{color:' + link + ';text-decoration:none}a:hover{text-decoration:underline}' +
    quote +
    '</style>'
  )
})

const selIdx = computed(() => displayMessages.value.findIndex((m) => m.id === selected.value?.id))

function openNeighbor(dir) {
  if (selIdx.value < 0) return
  const n = displayMessages.value[selIdx.value + dir]
  if (n) openMessage(n)
}

function printSelected() {
  const w = frameEl.value?.contentWindow
  if (w) {
    w.focus()
    w.print()
  }
}

function downloadAll(d) {
  for (const a of d.attachments) {
    const el = document.createElement('a')
    el.href = api.attachmentURL(d.id, a.id)
    el.download = a.filename
    document.body.appendChild(el)
    el.click()
    el.remove()
  }
}

function frameLoaded() {
  try {
    const doc = frameEl.value?.contentDocument
    if (doc) frameEl.value.style.height = doc.documentElement.scrollHeight + 'px'
  } catch {
  }
}

function patchLocalFlags(id, flags) {
  messages.value = messages.value.map((x) => (x.id === id ? { ...x, flags } : x))
  if (convo.value) {
    convo.value = {
      ...convo.value,
      msgs: convo.value.msgs.map((x) => (x.id === id ? { ...x, flags } : x)),
    }
  }
  if (selected.value?.id === id) {
    selected.value = { ...selected.value, flags }
    if (detail.value) detail.value = { ...detail.value, flags }
  }
}

function bumpFolderUnread(m, delta) {
  if (!m.folder_id) return
  folders.value = folders.value.map((f) =>
    f.id === m.folder_id ? { ...f, unread: Math.max(0, f.unread + delta) } : f,
  )
}

function bumpThreadUnread(id, delta) {
  messages.value = messages.value.map((x) =>
    x.id === id && x.thread_unread != null
      ? { ...x, thread_unread: Math.max(0, x.thread_unread + delta) }
      : x,
  )
}

async function markSeen(m) {
  if (m.flags & 1) return
  const prevFlags = m.flags
  const row = messages.value.find((x) => x.id === m.id)
  const counted = !!(row && !(row.flags & 1) && row.thread_unread != null)
  actionError.value = ''
  patchLocalFlags(m.id, prevFlags | 1)
  if (counted) bumpThreadUnread(m.id, -1)
  bumpFolderUnread(m, -1)
  try {
    const res = await api.setFlags(m.id, { seen: true })
    patchLocalFlags(m.id, res.flags)
    loadFolders()
  } catch (err) {
    patchLocalFlags(m.id, prevFlags)
    if (counted) bumpThreadUnread(m.id, 1)
    bumpFolderUnread(m, 1)
    actionError.value = t('标为已读失败：{error}', { error: err.message })
  }
}

function prefetchNeighbors(m) {
  const pool =
    convo.value && convo.value.msgs.some((x) => x.id === m.id) ? convo.value.msgs : messages.value
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


function openMessageById(id) {
  openMessage(placeholderMessage(id))
}

let settingsPushed = false

function openSettings() {
  if (location.hash !== '#/settings') {
    settingsPushed = true
    location.hash = '#/settings'
  }
  settingsOpen.value = true
}

function closeSettings() {
  if (location.hash === '#/settings' && settingsPushed) {
    settingsPushed = false
    history.back()
    return
  }
  settingsOpen.value = false
  if (location.hash === '#/settings') {
    const want = selected.value ? '#/mail/' + selected.value.id : ''
    history.pushState(null, '', want === '' ? location.pathname + location.search : want)
  }
}

function onHashChange() {
  if (!booted) return
  if (location.hash === '#/settings') {
    settingsOpen.value = true
    return
  }
  if (settingsOpen.value) settingsOpen.value = false
  const id = deepIdFromHash()
  if (id) {
    if (selected.value?.id !== id) openMessageById(id)
  } else if (selected.value) {
    clearSelection()
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
  closeConvo()
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
  markSeen(m)
  const cached = detailCache.get(m.id)
  if (cached) {
    detail.value = cached
    detailState.value = 'ready'
    enrichSelected(cached)
    afterOpen(m)
    return
  }
  detail.value = null
  detailState.value = 'loading'
  const tk = ++detailToken
  try {
    const d = await api.getMessage(m.id)
    detailCache.set(m.id, d)
    if (tk !== detailToken || selected.value?.id !== m.id) return
    detail.value = d
    detailState.value = 'ready'
    enrichSelected(d)
    afterOpen(m)
  } catch (err) {
    if (tk === detailToken) {
      detailError.value = err.message
      detailState.value = 'error'
    }
  }
}

async function openConversation(m) {
  clearSelection()
  const threadKey = m.thread_key || 'id:' + m.id
  convo.value = {
    accountId: m.account_id,
    threadKey,
    subject: m.subject || '',
    status: 'loading',
    msgs: [],
  }
  convoOpenIds.value = new Set()
  convoDetails.value = {}
  try {
    const msgs = await api.getThreadMessages(scope.value, m.account_id, threadKey)
    if (!convo.value || convo.value.threadKey !== threadKey) return
    const sorted = [...msgs].sort((a, b) => a.date_ts - b.date_ts || a.id - b.id)
    convo.value = { ...convo.value, status: 'ready', msgs: sorted }
    const open = new Set()
    for (const msg of sorted) {
      if (!(msg.flags & 1)) open.add(msg.id)
    }
    if (sorted.length) open.add(sorted[sorted.length - 1].id)
    convoOpenIds.value = open
    for (const msg of sorted) {
      if (open.has(msg.id)) {
        loadCardBody(msg.id)
        markSeen(msg)
      }
    }
  } catch {
    if (convo.value && convo.value.threadKey === threadKey) {
      convo.value = { ...convo.value, status: 'error' }
    }
  }
}

function closeConvo() {
  convo.value = null
  convoOpenIds.value = new Set()
  convoDetails.value = {}
}

async function loadCardBody(id) {
  if (convoDetails.value[id]) return
  const cached = detailCache.get(id)
  if (cached) {
    convoDetails.value = { ...convoDetails.value, [id]: cached }
    return
  }
  try {
    const d = await api.getMessage(id)
    detailCache.set(id, d)
    convoDetails.value = { ...convoDetails.value, [id]: d }
  } catch {
  }
}

function toggleCard(msg) {
  const next = new Set(convoOpenIds.value)
  if (next.has(msg.id)) {
    next.delete(msg.id)
  } else {
    next.add(msg.id)
    loadCardBody(msg.id)
    markSeen(msg)
  }
  convoOpenIds.value = next
}

function cardFrameLoaded(e) {
  try {
    const el = e.target
    const doc = el?.contentDocument
    if (doc) el.style.height = doc.documentElement.scrollHeight + 'px'
  } catch {
  }
}

function replyCard(msg) {
  const d = convoDetails.value[msg.id]
  if (!d) return
  const subj = msg.subject || ''
  openCompose({
    account_id: msg.account_id,
    to: d.from ? (d.from.name ? d.from.name + ' <' + d.from.email + '>' : d.from.email) : '',
    subject: /^re:/i.test(subj) ? subj : 'Re: ' + subj,
    text: quoteFor(msg, d),
    reply_to_id: msg.id,
  })
}

function forwardCard(msg) {
  const d = convoDetails.value[msg.id]
  if (!d) return
  const subj = msg.subject || ''
  openCompose({
    account_id: msg.account_id,
    subject: /^(fwd|fw):/i.test(subj) ? subj : 'Fwd: ' + subj,
    text: quoteFor(msg, d),
  })
}

async function toggleCardSeen(msg) {
  try {
    const res = await api.setFlags(msg.id, { seen: !(msg.flags & 1) })
    patchLocalFlags(msg.id, res.flags)
    loadFolders()
  } catch {
  }
}

function rowClick(m, idx) {
  if (Date.now() < suppressClickUntil) return
  cursorIdx.value = idx
  cursorActive.value = false
  if (threaded.value && !searching.value && (m.thread_count ?? 1) > 1) {
    openConversation(m)
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

function quoteFor(msg, d) {
  const from = d.from
    ? (d.from.name ? d.from.name + ' ' : '') + '<' + d.from.email + '>'
    : ''
  return (
    '\n\n----- Original message -----\nFrom: ' +
    from +
    '\nTime: ' +
    fmtFullEn(msg.date_ts) +
    '\nSubject: ' +
    (msg.subject || '') +
    '\n\n' +
    (d.text || '')
  )
}

function quoteText() {
  return quoteFor(selected.value, detail.value)
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
  tombstones.set(id, snapCtxKey(listSnap.value))
  listGen++
  if (convo.value) {
    const left = convo.value.msgs.filter((m) => m.id !== id)
    if (left.length === 0) {
      closeConvo()
    } else {
      convo.value = { ...convo.value, msgs: left }
    }
  }
  if (selected.value?.id === id) {
    clearSelection()
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
  if (searching.value || opBusy.value || rangeId.value) return
  opBusy.value = true
  try {
    await api.markAllRead(scope.value)
    messages.value = messages.value.map((m) =>
      threaded.value
        ? { ...m, flags: m.flags | 1, thread_unread: 0 }
        : { ...m, flags: m.flags | 1 },
    )
    if (convo.value) {
      convo.value = {
        ...convo.value,
        msgs: convo.value.msgs.map((m) => ({ ...m, flags: m.flags | 1 })),
      }
    }
    if (selected.value) patchLocalFlags(selected.value.id, selected.value.flags | 1)
    loadFolders()
  } catch {
  } finally {
    opBusy.value = false
  }
}


function activeMessage() {
  if (selected.value) return selected.value
  return displayMessages.value[cursorIdx.value] ?? null
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
  if (displayMessages.value.length === 0) return
  cursorActive.value = true
  cursorIdx.value = Math.max(0, Math.min(displayMessages.value.length - 1, cursorIdx.value + d))
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
      const row = displayMessages.value[cursorIdx.value]
      if (row) rowClick(row, cursorIdx.value)
      break
    }
    case 'e':
      if (m) archiveFor(m)
      break
    case 'Delete':
      if (m) deleteFor(m)
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


function menuRipple(e) {
  const btn = e.currentTarget
  const rect = btn.getBoundingClientRect()
  const d = Math.max(rect.width, rect.height) * 2
  const x = e.clientX ? e.clientX - rect.left : rect.width / 2
  const y = e.clientY ? e.clientY - rect.top : rect.height / 2
  const span = document.createElement('span')
  span.className = 'menu-ripple'
  span.style.width = span.style.height = d + 'px'
  span.style.left = x - d / 2 + 'px'
  span.style.top = y - d / 2 + 'px'
  btn.appendChild(span)
  setTimeout(() => span.remove(), 500)
}

function toggleDrawer() {
  if (lgAndUp.value) {
    navHidden.value = !navHidden.value
    localStorage.setItem('wm_nav_hidden', navHidden.value ? '1' : '0')
    drawerOpen.value = !navHidden.value
    return
  }
  if (composeOpen.value) return
  drawerOpen.value = !drawerOpen.value
}

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
    if (convo.value) {
      closeConvo()
    } else {
      clearSelection()
    }
  }
}

function openAccounts() {
  openSettings()
  if (!lgAndUp.value) drawerOpen.value = false
}

async function enter() {
  view.value = 'app'
  if (initialDeepId) openMessage(placeholderMessage(initialDeepId))
  try {
    const st = await api.getState()
    if (st.app_settings) maxAttachmentMB.value = st.app_settings.max_attachment_mb
  } catch {}
  threaded.value = localStorage.getItem('wm_threaded') === '1'
  const storedUndo = parseInt(localStorage.getItem('wm_undo_seconds') ?? '', 10)
  if ([0, 5, 10, 20, 30].includes(storedUndo)) undoSeconds.value = storedUndo
  await refreshAccounts()
  connectEvents()
  booted = true
  onHashChange()
}

async function boot() {
  view.value = 'loading'
  try {
    let st = await api.getState()
    appVersion.value = st.version || ''
    const tg = window.Telegram?.WebApp
    if (st.setup && !st.authed) {
      if (tg?.initData) {
        tg.ready()
        try {
          await api.telegramWebAppAuth(tg.initData)
          st = await api.getState()
          appVersion.value = st.version || ''
        } catch (err) {
          tgAuthError.value = err.message
        }
      }
    }
    const startParam = tgStartParam()
    if (startParam && !deepIdFromHash()) {
      location.hash = '#/mail/' + startParam
    }
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
  clearSelection()
  settingsOpen.value = false
  authMode.value = 'login'
  view.value = 'auth'
}

function onThreadedChange(v) {
  threaded.value = v
  localStorage.setItem('wm_threaded', v ? '1' : '0')
  if (!searching.value) loadMessages()
}

function onAppSettings(s) {
  maxAttachmentMB.value = s.max_attachment_mb
}

function onUndoSecondsChange(v) {
  undoSeconds.value = v
  localStorage.setItem('wm_undo_seconds', String(v))
}

const online = ref(navigator.onLine)
function onOnlineChange() {
  online.value = navigator.onLine
  if (online.value && view.value === 'app') refreshData()
}

onMounted(() => {
  window.addEventListener('hashchange', onHashChange)
  window.addEventListener('keydown', onGlobalKey)
  window.addEventListener('online', onOnlineChange)
  window.addEventListener('offline', onOnlineChange)
  initTheme()
  boot()
})

onBeforeUnmount(() => {
  window.removeEventListener('hashchange', onHashChange)
  window.removeEventListener('keydown', onGlobalKey)
  window.removeEventListener('online', onOnlineChange)
  window.removeEventListener('offline', onOnlineChange)
  systemDarkMQ?.removeEventListener('change', applyTheme)
  disconnectEvents()
})
</script>

<template>
  <v-app>
    <div v-if="view === 'loading'" class="boot-center">
      <v-progress-circular indeterminate color="primary" />
      <div class="mt-3 text-body-2 text-medium-emphasis">{{ t('加载中') }}</div>
    </div>

    <div v-else-if="view === 'error'" class="boot-center px-4">
      <v-alert type="error" variant="tonal" max-width="440" :title="t('启动失败')">
        {{ bootError }}
        <div class="mt-2">
          <v-btn size="small" variant="outlined" @click="boot">{{ t('重试') }}</v-btn>
        </div>
      </v-alert>
    </div>

    <template v-else-if="view === 'auth'">
      <v-alert
        v-if="tgAuthError"
        type="error"
        variant="tonal"
        density="compact"
        class="tg-auth-alert"
      >
        {{ t('Telegram 登录失败：{error}', { error: tgAuthError }) }}
      </v-alert>
      <Auth :mode="authMode" @authed="enter" />
    </template>

    <template v-else>
      <v-app-bar height="64" flat class="app-topbar">
        <div v-if="lgAndUp" class="topbar-desktop">
          <button type="button" class="topbar-menu-btn" :aria-label="t('展开菜单')" @click="toggleDrawer">
            <v-icon icon="mdi-menu" size="24" />
          </button>
          <span v-if="wideDesktop" class="topbar-title">Namakani</span>
          <div class="topbar-search">
            <v-text-field
              ref="searchEl"
              v-model="searchQuery"
              :placeholder="t('搜索')"
              variant="solo-filled"
              rounded="pill"
              hide-details
              class="search-field-d"
              @input="onSearchInput"
            >
              <template #prepend-inner>
                <v-icon icon="mdi-magnify" size="22" class="text-medium-emphasis" />
              </template>
              <template v-if="searching" #append-inner>
                <v-btn
                  icon="mdi-close"
                  variant="text"
                  size="x-small"
                  :aria-label="t('清除搜索')"
                  @click="clearSearch"
                />
              </template>
            </v-text-field>
          </div>
          <v-spacer />
          <span v-if="syncIndicator === 'syncing'" class="topbar-sync" :title="t('同步中')">
            <v-icon icon="mdi-refresh" size="22" class="sync-spin" />
          </span>
          <v-btn
            v-else-if="syncIndicator === 'error'"
            icon="mdi-alert-circle"
            variant="text"
            color="error"
            size="small"
            :aria-label="t('同步失败')"
            @click="openSettings"
          />
          <v-btn icon="mdi-cog" variant="text" size="small" :aria-label="t('设置')" @click="openSettings" />
          <v-menu>
            <template #activator="{ props: mp }">
              <button
                type="button"
                v-bind="mp"
                class="topbar-avatar"
                :style="{ background: avatarColor(accounts[0]?.email || 'namakani') }"
                :aria-label="t('账号')"
              >
                <span class="avatar-letter font-weight-bold">{{ (accounts[0]?.email || 'W').charAt(0).toUpperCase() }}</span>
              </button>
            </template>
            <v-list density="compact">
              <v-list-item prepend-icon="mdi-cog" :title="t('设置')" @click="openSettings" />
              <v-list-item prepend-icon="mdi-logout" :title="t('退出登录')" @click="doLogout" />
            </v-list>
          </v-menu>
        </div>
        <div v-else class="appbar-search">
          <div class="search-wrap">
            <button type="button" class="search-menu-btn" :aria-label="t('展开菜单')" @pointerdown="menuRipple" @click="toggleDrawer">
              <v-icon icon="mdi-menu" size="22" />
            </button>
            <v-text-field
              ref="searchEl"
              v-model="searchQuery"
              :placeholder="t('搜索主题、发件人、正文')"
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
                :aria-label="t('清除搜索')"
                @click="clearSearch"
              />
              </template>
            </v-text-field>
          </div>
        </div>
      </v-app-bar>

      <v-navigation-drawer
        v-model="drawerOpen"
        :permanent="lgAndUp"
        :temporary="!lgAndUp"
        :width="lgAndUp ? 288 : 280"
        :elevation="0"
        color="surface-container-low"
        class="nav-drawer"
        :touchless="!lgAndUp && composeOpen"
      >
        <div class="d-flex flex-column pa-4 drawer-inner">
          <v-btn
            v-if="lgAndUp"
            color="primary-container"
            rounded="pill"
            prepend-icon="mdi-pencil"
            class="nav-compose mb-2"
            @click="openCompose({})"
          >
            {{ t('写信') }}
          </v-btn>

          <div v-if="accounts.length > 1" class="nav-range">
            <span class="nav-range-label text-truncate">
              {{ rangeId ? (rangeAccount?.label || rangeAccount?.email) : t('全部账号') }}
            </span>
            <v-menu>
              <template #activator="{ props: mp }">
                <v-btn v-bind="mp" icon="mdi-chevron-down" size="x-small" variant="text" :aria-label="t('切换范围')" />
              </template>
              <v-list density="compact">
                <v-list-item :title="t('全部账号')" :active="rangeId === 0" @click="setRange(0)" />
                <v-list-item
                  v-for="a in accounts"
                  :key="a.id"
                  :title="a.label || a.email"
                  :active="rangeId === a.id"
                  @click="setRange(a.id)"
                />
              </v-list>
            </v-menu>
          </div>

          <template v-if="foldersStatus === 'loading'">
            <v-skeleton-loader type="list-item@10" />
          </template>
          <template v-else-if="foldersStatus === 'error'">
            <p class="text-body-2 text-error px-1">
              {{ t('文件夹加载失败：{error}', { error: foldersError }) }}
              <v-btn variant="text" size="small" @click="loadFolders">{{ t('重试') }}</v-btn>
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
                  <span v-if="scopeStats[s.key].unread > 0" class="nav-count">
                    {{ scopeStats[s.key].unread }}
                  </span>
                </template>
              </v-list-item>
            </v-list>
            <p v-if="visibleScopes.length === 0" class="text-body-2 text-medium-emphasis px-1">
              {{ t('暂无文件夹（同步后出现）') }}
            </p>
          </template>

          <template v-if="accounts.length > 1">
            <div class="nav-group-label">{{ t('账号') }}</div>
            <v-list density="compact" nav class="pa-0">
              <v-list-item
                v-for="a in accounts"
                :key="a.id"
                :active="rangeId === a.id"
                rounded="pill"
                @click="setRange(rangeId === a.id ? 0 : a.id)"
              >
                <template #prepend>
                  <span class="nav-dot" :style="{ background: avatarColor(a.email || a.label || '') }" />
                </template>
                <v-list-item-title class="text-body-2">{{ a.label || a.email }}</v-list-item-title>
                <template #append>
                  <span v-if="accountInboxUnread[a.id]" class="nav-count">{{ accountInboxUnread[a.id] }}</span>
                </template>
              </v-list-item>
            </v-list>
          </template>

          <div class="mt-auto">
            <v-divider class="mb-2" />
            <v-list density="compact" nav class="pa-0">
              <v-list-item
                prepend-icon="mdi-cog"
                :title="t('设置')"
                :active="settingsOpen"
                rounded="pill"
                @click="openAccounts"
              />
              <v-list-item prepend-icon="mdi-logout" :title="t('退出登录')" rounded="pill" @click="doLogout" />
            </v-list>
          </div>
        </div>
      </v-navigation-drawer>

      <v-main>
        <div v-if="!online" class="offline-bar">
          <span>{{ t('网络连接已断开') }}</span>
          <v-btn variant="text" size="small" @click="refreshData">{{ t('重试') }}</v-btn>
        </div>
        <div class="mail-layout" :class="{ 'with-offline': !online }">
          <section
            class="list-section"
            :class="{ 'density-compact': listDensity === 'compact' }"
            :style="lgAndUp ? { flexBasis: listWidthVal + 'px' } : undefined"
          >
            <template v-if="lgAndUp">
              <div class="list-head">
                <h1 class="list-head-title text-truncate">
                  {{ listHeadTitle }}
                  <span v-if="listStatus === 'loading'" class="head-skel" />
                </h1>
                <v-btn
                  v-if="listSnap.kind === 'search'"
                  icon="mdi-close"
                  size="small"
                  variant="text"
                  :aria-label="t('清除搜索')"
                  @click="clearSearch"
                />
                <v-menu>
                  <template #activator="{ props: mp }">
                    <v-btn v-bind="mp" icon="mdi-dots-vertical" size="small" variant="text" :aria-label="t('更多')" />
                  </template>
                  <v-list density="compact">
                    <v-list-item :title="t('标准密度')" :append-icon="listDensity === 'standard' ? 'mdi-check' : undefined" @click="setDensity('standard')" />
                    <v-list-item :title="t('紧凑密度')" :append-icon="listDensity === 'compact' ? 'mdi-check' : undefined" @click="setDensity('compact')" />
                    <v-divider v-if="!searching && !rangeId" />
                    <v-list-item v-if="!searching && !rangeId" :title="t('全部标为已读')" @click="markAllRead" />
                  </v-list>
                </v-menu>
              </div>
              <div v-if="listSnap.kind === 'search'" class="search-chips">
                <v-chip
                  size="small"
                  :variant="!searchFilter.attach && !searchFilter.unread && !searchFilter.accountId ? 'flat' : 'outlined'"
                  :color="!searchFilter.attach && !searchFilter.unread && !searchFilter.accountId ? 'primary-container' : undefined"
                  @click="resetSearchFilter"
                >
                  {{ t('全部') }}
                </v-chip>
                <v-chip
                  size="small"
                  :variant="searchFilter.attach ? 'flat' : 'outlined'"
                  :color="searchFilter.attach ? 'primary-container' : undefined"
                  prepend-icon="mdi-paperclip"
                  @click="searchFilter = { ...searchFilter, attach: !searchFilter.attach }"
                >
                  {{ t('有附件') }}
                </v-chip>
                <v-chip
                  size="small"
                  :variant="searchFilter.unread ? 'flat' : 'outlined'"
                  :color="searchFilter.unread ? 'primary-container' : undefined"
                  @click="searchFilter = { ...searchFilter, unread: !searchFilter.unread }"
                >
                  {{ t('未读') }}
                </v-chip>
                <v-menu>
                  <template #activator="{ props: mp }">
                    <v-chip
                      v-bind="mp"
                      size="small"
                      :variant="searchFilter.accountId ? 'flat' : 'outlined'"
                      :color="searchFilter.accountId ? 'primary-container' : undefined"
                      append-icon="mdi-chevron-down"
                    >
                      {{ searchFilter.accountId ? (accounts.find((a) => a.id === searchFilter.accountId)?.label || t('按账号')) : t('按账号') }}
                    </v-chip>
                  </template>
                  <v-list density="compact">
                    <v-list-item :title="t('全部账号')" :active="!searchFilter.accountId" @click="searchFilter = { ...searchFilter, accountId: 0 }" />
                    <v-list-item
                      v-for="a in accounts"
                      :key="a.id"
                      :title="a.label || a.email"
                      :active="searchFilter.accountId === a.id"
                      @click="searchFilter = { ...searchFilter, accountId: a.id }"
                    />
                  </v-list>
                </v-menu>
              </div>
              <div class="list-toolbar">
                <v-checkbox-btn
                  :model-value="allVisibleSelected"
                  :indeterminate="selectionActive && !allVisibleSelected"
                  density="compact"
                  :aria-label="t('全选')"
                  @click.prevent="toggleSelectAll"
                />
                <template v-if="selectionActive">
                  <span class="text-body-2 ml-1">{{ t('已选 {n} 封', { n: selectedIds.size }) }}</span>
                  <v-btn icon variant="text" size="small" :disabled="opBusy || scope === 'archive' || scope === 'trash'" @click="bulkAct('archive')">
                    <v-icon icon="mdi-archive" />
                    <v-tooltip activator="parent" location="bottom">{{ t('归档') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon variant="text" size="small" :disabled="opBusy" @click="bulkAct('delete')">
                    <v-icon icon="mdi-delete" />
                    <v-tooltip activator="parent" location="bottom">{{ t('删除') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon variant="text" size="small" :disabled="opBusy" @click="bulkAct('seen')">
                    <v-icon icon="mdi-email-open" />
                    <v-tooltip activator="parent" location="bottom">{{ t('标为已读') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon variant="text" size="small" :disabled="opBusy" @click="bulkAct('unseen')">
                    <v-icon icon="mdi-email" />
                    <v-tooltip activator="parent" location="bottom">{{ t('标为未读') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon="mdi-close" variant="text" size="small" :aria-label="t('取消选择')" @click="clearSelectionIds" />
                </template>
                <template v-else>
                  <v-btn icon variant="text" size="small" disabled>
                    <v-icon icon="mdi-archive" />
                    <v-tooltip activator="parent" location="bottom">{{ t('归档') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon variant="text" size="small" disabled>
                    <v-icon icon="mdi-delete" />
                    <v-tooltip activator="parent" location="bottom">{{ t('删除') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon variant="text" size="small" disabled>
                    <v-icon icon="mdi-email-open" />
                    <v-tooltip activator="parent" location="bottom">{{ t('标为已读') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon variant="text" size="small" :disabled="listBusy" @click="refreshData">
                    <v-icon icon="mdi-refresh" />
                    <v-tooltip activator="parent" location="bottom">{{ t('刷新') }}</v-tooltip>
                  </v-btn>
                </template>
                <span v-if="listSnap.kind === 'folder' && messages.length" class="range-text">
                  1 {{ t('至') }} {{ displayMessages.length }}
                </span>
              </div>
              <div class="list-progress">
                <v-progress-linear v-if="listBusy" indeterminate color="primary" height="3" />
              </div>
              <div v-if="listErrorBar" class="list-error-bar">
                <span class="text-truncate">{{ t('加载失败：{error}', { error: listErrorBar }) }}</span>
                <v-btn variant="text" size="small" @click="loadMessages()">{{ t('重试') }}</v-btn>
              </div>
            </template>
            <div v-else class="d-flex align-center ga-1 px-2 py-2 border-b">
              <h1 class="text-body-1 font-weight-bold text-truncate px-1">
                {{ listSnap.kind === 'search' ? t('搜索结果') : snapScopeLabel }}
              </h1>
              <span v-if="listBusy" class="text-caption text-medium-emphasis d-flex align-center ga-1 text-no-wrap">
                <template v-if="searchQuery.trim()">{{ t('搜索中') }}</template>
              </span>
              <span v-else-if="listSnap.kind === 'search' && listStatus === 'ready'" class="text-caption text-medium-emphasis">
                {{ t('{n} 封', { n: messages.length }) }}
              </span>
              <span
                v-else-if="listSnap.kind === 'folder' && foldersStatus === 'ready' && scopeStats[listSnap.scope]"
                class="text-caption text-medium-emphasis text-no-wrap"
              >
                {{ t('{n} 封', { n: scopeStats[listSnap.scope].total }) }}<template v-if="scopeStats[listSnap.scope].unread">{{ t('（{n} 未读）', { n: scopeStats[listSnap.scope].unread }) }}</template>
              </span>
            </div>
            <div v-if="actionError" class="px-3 py-1 text-caption text-error border-b">{{ actionError }}</div>

            <div class="list-body">
            <button
              v-if="newMailPending > 0"
              type="button"
              class="newmail-pill"
              @click="jumpToNewMail"
            >
              {{ t('{n} 封新邮件', { n: newMailPending }) }}
            </button>
            <div
              class="ptr-ind"
              :style="{
                transform: 'translate(-50%, ' + ptrY + 'px)',
                opacity: ptrRaw > 0 || ptrBusy ? 1 : 0,
                transition: ptrTracking ? 'none' : 'transform 0.25s ease, opacity 0.2s ease',
              }"
            >
              <v-progress-circular
                v-if="ptrBusy"
                indeterminate
                size="22"
                width="2.5"
                color="primary"
                bg-color="transparent"
              />
              <svg v-else width="40" height="40" viewBox="0 0 40 40" aria-hidden="true" :style="{ opacity: ptrOpacity }">
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
                <v-skeleton-loader v-if="lgAndUp" type="list-item-avatar-three-line@12" class="list-skel" />
                <v-skeleton-loader v-else type="list-item-three-line@24" />
              </template>
              <template v-else-if="listStatus === 'error'">
                <template v-if="lgAndUp">
                  <div class="list-error-bar">
                    <span class="text-truncate">{{ t('加载失败：{error}', { error: listError }) }}</span>
                    <v-btn variant="text" size="small" @click="loadMessages()">{{ t('重试') }}</v-btn>
                  </div>
                  <div class="list-error-center">
                    <v-btn variant="outlined" @click="loadMessages()">{{ t('重试') }}</v-btn>
                  </div>
                </template>
                <div v-else class="pa-4">
                  <v-alert type="error" variant="tonal" density="compact">
                    {{ t('邮件列表加载失败：{error}', { error: listError }) }}
                    <div class="mt-2">
                      <v-btn size="small" variant="outlined" @click="loadMessages">{{ t('重试') }}</v-btn>
                    </div>
                  </v-alert>
                </div>
              </template>
              <template v-else-if="displayMessages.length === 0">
                <div v-if="lgAndUp" class="list-empty">
                  <v-icon icon="mdi-email-outline" size="48" />
                  <p>{{ listSnap.kind === 'search' ? t('没有匹配的邮件') : t('{scope}是空的', { scope: snapScopeLabel }) }}</p>
                </div>
                <p v-else class="pa-6 text-body-2 text-medium-emphasis">
                  {{ listSnap.kind === 'search' ? t('没有找到匹配的邮件。') : t('这里还没有邮件。添加账号后会自动同步进来。') }}
                </p>
              </template>
              <template v-else>
                <template v-for="(m, idx) in displayMessages" :key="m.id">
                  <div class="mail-row-wrap" :style="rowWrapStyle(m)">
                    <div
                      v-if="scope !== 'archive' && scope !== 'trash'"
                      class="swipe-bg swipe-bg-left"
                      :style="{ opacity: swipeId === m.id && swipeDx > 0 ? 1 : 0 }"
                    >
                      <v-icon icon="mdi-archive" size="20" />
                    </div>
                    <div
                      class="swipe-bg swipe-bg-right"
                      :style="{ opacity: swipeId === m.id && swipeDx < 0 ? 1 : 0 }"
                    >
                      <v-icon icon="mdi-delete" size="20" />
                    </div>
                  <div
                    v-if="lgAndUp"
                    class="mail-row-d"
                    :class="{
                      'row-unread': !(m.flags & 1),
                      'row-selected': selected?.id === m.id,
                      'row-cursor': cursorActive && idx === cursorIdx,
                    }"
                    :data-rowidx="idx"
                    :style="rowSwipeStyle(m)"
                    @click="rowClick(m, idx)"
                  >
                    <span
                      class="row-check"
                      :class="{ 'row-check-on': selectionActive || selectedIds.has(m.id) }"
                      @click.stop="toggleRowSelection(m)"
                    >
                      <v-checkbox-btn :model-value="selectedIds.has(m.id)" density="compact" tabindex="-1" />
                    </span>
                    <v-avatar :color="avatarColor(m.from_email)" :size="listDensity === 'compact' ? 24 : 32" class="mail-avatar-d">
                      <span class="avatar-letter row-avatar-letter">{{ avatarLetter(m) }}</span>
                    </v-avatar>
                    <div class="row-main">
                      <div class="row-l1">
                        <span class="row-sender" :class="{ w700: !(m.flags & 1) }">
                          {{ m.from_name || m.from_email || t('(未知发件人)') }}
                        </span>
                        <v-chip v-if="listDensity === 'compact'" class="row-chip" size="x-small" variant="flat" color="primary-container">
                          {{ accountShort(m) }}
                        </v-chip>
                        <span
                          v-if="threaded && !searching && (m.thread_count ?? 1) > 1"
                          class="row-thread"
                        >
                          {{ t('共 {n} 封', { n: m.thread_count }) }}<template v-if="m.thread_unread">{{ t('，{n} 未读', { n: m.thread_unread }) }}</template>
                        </span>
                        <span class="row-time">{{ fmtListTime(m.date_ts) }}</span>
                      </div>
                      <div class="row-l2" :class="{ w600: !(m.flags & 1) }">
                        <span class="row-subject">{{ m.subject || t('(无主题)') }}</span>
                        <span v-if="listDensity === 'compact' && m.snippet" class="row-l2-snippet">{{ m.snippet }}</span>
                        <v-icon v-if="listDensity === 'compact' && m.has_attach" icon="mdi-paperclip" size="14" class="text-medium-emphasis flex-none" />
                      </div>
                      <div v-if="listDensity === 'standard'" class="row-l3">
                        <span class="row-snippet">{{ m.snippet }}</span>
                        <v-chip class="row-chip" size="x-small" variant="flat" color="primary-container">
                          {{ accountShort(m) }}
                        </v-chip>
                        <v-chip v-if="m.has_attach" class="row-chip" size="x-small" variant="outlined" prepend-icon="mdi-paperclip">
                          {{ t('附件') }}
                        </v-chip>
                      </div>
                    </div>
                  </div>
                  <div
                    v-else
                    v-ripple
                    class="mail-row"
                    :data-rowidx="idx"
                    :class="{ 'mail-row-active': (lgAndUp && selected?.id === m.id) || (cursorActive && idx === cursorIdx) }"
                    :style="rowSwipeStyle(m)"
                    @click="rowClick(m, idx)"
                    @touchstart.passive="rowTouchStart($event, m)"
                    @touchmove="rowTouchMove($event, m)"
                    @touchend="rowTouchEnd($event, m)"
                    @touchcancel="rowTouchEnd($event, m)"
                  >
                    <v-avatar :color="avatarColor(m.from_email)" size="38" class="mail-avatar">
                      <span class="avatar-letter text-body-2 font-weight-medium">{{ avatarLetter(m) }}</span>
                    </v-avatar>
                    <div class="flex-1-1" style="min-width: 0">
                      <div class="d-flex align-center ga-2">
                        <span v-if="!(m.flags & 1)" class="unread-dot" />
                        <span
                          class="text-body-2 text-truncate flex-1-1"
                          :class="{ 'font-weight-bold': !(m.flags & 1) }"
                        >
                          {{ m.from_name || m.from_email || t('(未知发件人)') }}
                        </span>
                        <span
                          v-if="threaded && !searching && (m.thread_count ?? 1) > 1"
                          class="text-caption text-medium-emphasis text-no-wrap"
                        >
                          {{ t('共 {n} 封', { n: m.thread_count }) }}<template v-if="m.thread_unread">{{ t('，{n} 未读', { n: m.thread_unread }) }}</template>
                        </span>
                        <v-icon v-if="m.has_attach" icon="mdi-paperclip" size="14" class="text-medium-emphasis" />
                        <span class="text-caption text-medium-emphasis text-no-wrap">{{ fmtListTime(m.date_ts) }}</span>
                      </div>
                      <div class="text-body-2 text-truncate" :class="{ 'font-weight-bold': !(m.flags & 1) }">
                        {{ m.subject || t('(无主题)') }}
                      </div>
                      <div v-if="m.snippet" class="text-caption text-medium-emphasis text-truncate">
                        {{ m.snippet }}
                      </div>
                      <div class="text-caption text-medium-emphasis text-truncate">
                        {{ m.account_label }}<template v-if="searching">{{ t('，') }}{{ m.folder_name }}</template>
                      </div>
                    </div>
                  </div>
                  </div>
                </template>
                <v-btn
                  v-if="listHasMore && !searching && !lgAndUp"
                  block
                  variant="text"
                  :loading="loadingMore"
                  @click="loadMore"
                >
                  {{ t('加载更多') }}
                </v-btn>
              </template>
              </div>
            </div>
            </div>
            <div v-if="lgAndUp && listStatus === 'ready' && listHasMore && !searching" class="list-footer">
              <v-btn variant="text" block size="small" :loading="loadingMore" @click="loadMore">
                {{ t('加载更多') }}
              </v-btn>
            </div>
            <div v-if="lgAndUp" class="list-drag" @pointerdown="onListDragStart" />
          </section>

          <section class="reader-section" :class="{ 'd-none': !lgAndUp && !selected && !convo, 'reader-overlay': !lgAndUp && (!!selected || !!convo) }">
            <div v-if="!selected && !convo" class="h-100 d-flex align-center justify-center">
              <div v-if="lgAndUp" class="reader-empty">
                <v-icon icon="mdi-email-open-outline" size="48" />
                <p>{{ t('选择一封邮件阅读') }}</p>
              </div>
              <p v-else class="text-body-2 text-medium-emphasis">{{ t('未选择邮件') }}</p>
            </div>
            <div
              v-else-if="convo"
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
                  :aria-label="t('关闭')"
                  @click="closeConvo"
                />
              </div>
              <div class="reader-scroll px-3 pb-3 px-sm-4 pb-sm-4">
                <h2 class="text-h6 font-weight-bold mb-1" style="overflow-wrap: anywhere">
                  {{ convo.subject || t('(无主题)') }}
                </h2>
                <p class="text-caption text-medium-emphasis mb-3">{{ t('共 {n} 封', { n: convo.msgs.length }) }}</p>
                <template v-if="convo.status === 'loading'">
                  <v-skeleton-loader type="list-item-three-line@12" />
                </template>
                <v-alert v-else-if="convo.status === 'error'" type="error" variant="tonal" density="compact">
                  {{ t('会话加载失败') }}
                  <div class="mt-2">
                    <v-btn size="small" variant="outlined" @click="convo = null">{{ t('关闭') }}</v-btn>
                  </div>
                </v-alert>
                <div v-else class="convo-stack">
                  <div v-for="msg in convo.msgs" :key="msg.id" class="convo-card">
                    <div v-ripple class="convo-card-head" @click="toggleCard(msg)">
                      <v-avatar :color="avatarColor(msg.from_email)" size="32" class="flex-none">
                        <span class="avatar-letter text-caption font-weight-medium">{{ avatarLetter(msg) }}</span>
                      </v-avatar>
                      <div class="flex-1-1" style="min-width: 0">
                        <div class="d-flex align-center ga-2">
                          <span v-if="!(msg.flags & 1)" class="unread-dot" />
                          <span
                            class="text-body-2 text-truncate flex-1-1"
                            :class="{ 'font-weight-bold': !(msg.flags & 1) }"
                          >
                            {{ msg.from_name || msg.from_email || t('(未知发件人)') }}
                          </span>
                          <span class="text-caption text-medium-emphasis text-no-wrap">{{ fmtListTime(msg.date_ts) }}</span>
                          <v-icon
                            :icon="convoOpenIds.has(msg.id) ? 'mdi-chevron-up' : 'mdi-chevron-down'"
                            size="18"
                            class="text-medium-emphasis flex-none"
                          />
                        </div>
                        <div v-if="!convoOpenIds.has(msg.id)" class="text-caption text-medium-emphasis text-truncate">
                          {{ msg.snippet || '' }}
                        </div>
                      </div>
                    </div>
                    <div v-if="convoOpenIds.has(msg.id)" class="convo-card-body">
                      <template v-if="convoDetails[msg.id]">
                        <div class="text-caption text-medium-emphasis mb-1">
                          <template v-if="convoDetails[msg.id].to?.length">
                            {{ t('收件人') }} {{ fmtAddrList(convoDetails[msg.id].to) }}
                          </template>
                          <template v-if="convoDetails[msg.id].cc?.length">
                            {{ t('，抄送') }} {{ fmtAddrList(convoDetails[msg.id].cc) }}
                          </template>
                          {{ t('，') }}{{ fmtFull(msg.date_ts) }}
                        </div>
                        <div class="d-flex align-center ga-1 border-y py-1 mb-2">
                          <v-btn icon variant="text" size="small" :disabled="opBusy" @click="toggleCardSeen(msg)">
                            <v-icon :icon="msg.flags & 1 ? 'mdi-email' : 'mdi-email-open'" />
                            <v-tooltip activator="parent" location="bottom">
                              {{ msg.flags & 1 ? t('标为未读') : t('标为已读') }}
                            </v-tooltip>
                          </v-btn>
                          <v-btn icon variant="text" size="small" :disabled="!convoDetails[msg.id]" @click="replyCard(msg)">
                            <v-icon icon="mdi-reply" />
                            <v-tooltip activator="parent" location="bottom">{{ t('回复') }}</v-tooltip>
                          </v-btn>
                          <v-btn icon variant="text" size="small" :disabled="!convoDetails[msg.id]" @click="forwardCard(msg)">
                            <v-icon icon="mdi-share" />
                            <v-tooltip activator="parent" location="bottom">{{ t('转发') }}</v-tooltip>
                          </v-btn>
                          <v-btn
                            v-if="scope !== 'archive' && scope !== 'trash'"
                            icon
                            variant="text"
                            size="small"
                            :disabled="opBusy"
                            @click="archiveFor(msg)"
                          >
                            <v-icon icon="mdi-archive" />
                            <v-tooltip activator="parent" location="bottom">{{ t('归档') }}</v-tooltip>
                          </v-btn>
                          <v-btn icon variant="text" size="small" :disabled="opBusy" @click="deleteFor(msg)">
                            <v-icon icon="mdi-delete" />
                            <v-tooltip activator="parent" location="bottom">
                              {{ scope === 'trash' || scope === 'junk' ? t('彻底删除') : t('删除') }}
                            </v-tooltip>
                          </v-btn>
                        </div>
                        <div v-if="convoDetails[msg.id].attachments.length > 0" class="d-flex flex-wrap ga-2 py-2">
                          <v-btn
                            v-for="a in convoDetails[msg.id].attachments"
                            :key="a.id"
                            variant="outlined"
                            size="small"
                            prepend-icon="mdi-paperclip"
                            :href="api.attachmentURL(convoDetails[msg.id].id, a.id)"
                          >
                            <span class="text-truncate" style="max-width: 55vw">{{ a.filename }}</span>
                            <span class="text-medium-emphasis ml-1">{{ fmtSize(a.size) }}</span>
                          </v-btn>
                        </div>
                        <div v-if="convoDetails[msg.id].html" style="overflow: hidden">
                          <iframe
                            :title="t('邮件正文')"
                            class="d-block w-100 border-0"
                            style="height: 320px; background: transparent"
                            sandbox="allow-same-origin"
                            :srcdoc="frameCss + convoDetails[msg.id].html"
                            @load="cardFrameLoaded"
                          />
                        </div>
                        <div
                          v-else-if="convoDetails[msg.id].text"
                          class="py-2 text-body-2"
                          :class="{ 'reader-text': lgAndUp }"
                          :style="lgAndUp ? 'white-space: pre-wrap' : 'white-space: pre-wrap; line-height: 1.6'"
                        >
                          {{ convoDetails[msg.id].text }}
                        </div>
                        <p v-else class="py-4 text-body-2 text-medium-emphasis">{{ t('这封邮件没有可显示的正文。') }}</p>
                      </template>
                      <v-skeleton-loader v-else type="paragraph@2" />
                    </div>
                  </div>
                </div>
              </div>
            </div>
            <div
              v-else-if="selected"
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
                  :aria-label="t('关闭')"
                  @click="clearSelection()"
                />
              </div>
              <div v-if="lgAndUp" class="reader-toolbar">
                <v-btn
                  v-if="scope !== 'archive' && scope !== 'trash'"
                  icon
                  variant="text"
                  size="small"
                  :disabled="opBusy"
                  @click="archiveFor(selected)"
                >
                  <v-icon icon="mdi-archive" />
                  <v-tooltip activator="parent" location="bottom">{{ t('归档') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" size="small" :disabled="opBusy" @click="deleteFor(selected)">
                  <v-icon icon="mdi-delete" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ scope === 'trash' || scope === 'junk' ? t('彻底删除') : t('删除') }}
                  </v-tooltip>
                </v-btn>
                <v-btn icon variant="text" size="small" :disabled="opBusy" @click="toggleSeen">
                  <v-icon :icon="selected.flags & 1 ? 'mdi-email' : 'mdi-email-open'" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ selected.flags & 1 ? t('标为未读') : t('标为已读') }}
                  </v-tooltip>
                </v-btn>
                <v-btn icon="mdi-chevron-up" variant="text" size="small" :disabled="selIdx <= 0" :aria-label="t('上一封')" @click="openNeighbor(-1)" />
                <v-btn
                  icon="mdi-chevron-down"
                  variant="text"
                  size="small"
                  :disabled="selIdx < 0 || selIdx >= displayMessages.length - 1"
                  :aria-label="t('下一封')"
                  @click="openNeighbor(1)"
                />
                <v-spacer />
                <v-btn icon="mdi-printer" variant="text" size="small" :disabled="!detail?.html" :aria-label="t('打印')" @click="printSelected" />
              </div>
              <div class="reader-scroll px-3 pb-3 px-sm-4 pb-sm-4">
              <template v-if="lgAndUp">
                <div class="reader-head-block">
                  <h2 class="reader-subject">{{ selected.subject || t('(无主题)') }}</h2>
                  <div class="reader-from-row">
                    <v-avatar :color="avatarColor(selected.from_email)" size="40" class="flex-none">
                      <span class="avatar-letter" style="font-size: 16px">{{ avatarLetter(selected) }}</span>
                    </v-avatar>
                    <span class="reader-from-name text-truncate">
                      {{ detail?.from?.name || selected.from_name || selected.from_email }}
                    </span>
                    <span class="reader-from-addr text-truncate">{{ detail?.from?.email || selected.from_email }}</span>
                    <span class="reader-time">{{ fmtFull(selected.date_ts) }}</span>
                  </div>
                  <div class="reader-to-row">
                    <span v-if="detail?.to?.length" style="overflow-wrap: anywhere">
                      {{ t('收件人') }} {{ fmtAddrList(detail.to) }}
                    </span>
                    <v-chip size="x-small" variant="flat" color="primary-container">{{ selected.account_label }}</v-chip>
                    <v-chip size="x-small" variant="outlined">{{ selected.folder_name }}</v-chip>
                  </div>
                  <div class="reader-actions">
                    <v-btn variant="tonal" size="small" prepend-icon="mdi-reply" :disabled="opBusy || !detail" @click="replySelected">
                      {{ t('回复') }}
                    </v-btn>
                    <v-btn variant="tonal" size="small" prepend-icon="mdi-share" :disabled="opBusy || !detail" @click="forwardSelected">
                      {{ t('转发') }}
                    </v-btn>
                  </div>
                </div>
              </template>
              <template v-else>
              <h2 class="text-h6 font-weight-bold mb-2" style="overflow-wrap: anywhere">
                {{ selected.subject || t('(无主题)') }}
              </h2>
              <div class="meta-grid text-body-2 mb-3">
                <span class="text-medium-emphasis">{{ t('发件人') }}</span>
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
                  <span class="text-medium-emphasis">{{ t('收件人') }}</span>
                  <span style="overflow-wrap: anywhere">{{ fmtAddrList(detail.to) }}</span>
                </template>
                <template v-if="detail?.cc?.length">
                  <span class="text-medium-emphasis">{{ t('抄送') }}</span>
                  <span style="overflow-wrap: anywhere">{{ fmtAddrList(detail.cc) }}</span>
                </template>
                <span class="text-medium-emphasis">{{ t('时间') }}</span>
                <span>{{ fmtFull(selected.date_ts) }}</span>
                <span class="text-medium-emphasis">{{ t('账号') }}</span>
                <span style="overflow-wrap: anywhere">{{ selected.account_label }}{{ t('，') }}{{ selected.folder_name }}</span>
              </div>

              <div class="d-flex align-center ga-1 border-y py-1 mb-2">
                <v-btn icon variant="text" :disabled="opBusy" @click="toggleSeen">
                  <v-icon :icon="selected.flags & 1 ? 'mdi-email' : 'mdi-email-open'" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ selected.flags & 1 ? t('标为未读') : t('标为已读') }}
                  </v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="opBusy || !detail" @click="replySelected">
                  <v-icon icon="mdi-reply" />
                  <v-tooltip activator="parent" location="bottom">{{ t('回复') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="opBusy || !detail" @click="forwardSelected">
                  <v-icon icon="mdi-share" />
                  <v-tooltip activator="parent" location="bottom">{{ t('转发') }}</v-tooltip>
                </v-btn>
                <v-btn
                  v-if="scope !== 'archive' && scope !== 'trash'"
                  icon
                  variant="text"
                  :disabled="opBusy"
                  @click="archiveFor(selected)"
                >
                  <v-icon icon="mdi-archive" />
                  <v-tooltip activator="parent" location="bottom">{{ t('归档') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="opBusy" @click="deleteFor(selected)">
                  <v-icon icon="mdi-delete" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ scope === 'trash' || scope === 'junk' ? t('彻底删除') : t('删除') }}
                  </v-tooltip>
                </v-btn>
              </div>
              </template>

              <template v-if="detailState === 'loading'">
                <v-skeleton-loader type="paragraph@10" class="py-4 reader-body-block" />
              </template>
              <template v-else-if="detailState === 'error'">
                <v-alert type="error" variant="tonal" density="compact" class="my-4">
                  {{ t('正文加载失败：{error}', { error: detailError }) }}
                  <div class="mt-2">
                    <v-btn size="small" variant="outlined" @click="openMessage(selected)">{{ t('重试') }}</v-btn>
                  </div>
                </v-alert>
              </template>
              <template v-else-if="detail">
                <div class="reader-body-block">
                  <div v-if="detail.attachments.length > 0 && lgAndUp" class="attach-row">
                    <v-chip
                      v-for="a in detail.attachments"
                      :key="a.id"
                      variant="outlined"
                      prepend-icon="mdi-paperclip"
                      :href="api.attachmentURL(detail.id, a.id)"
                    >
                      <span class="text-truncate" style="max-width: 40vw">{{ a.filename }}</span>
                      <span class="text-medium-emphasis ml-1">{{ fmtSize(a.size) }}</span>
                    </v-chip>
                    <v-btn v-if="detail.attachments.length > 1" variant="text" size="small" @click="downloadAll(detail)">
                      {{ t('全部下载') }}
                    </v-btn>
                  </div>
                  <div v-else-if="detail.attachments.length > 0" class="d-flex flex-wrap ga-2 py-3">
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
                      :title="t('邮件正文')"
                      class="mt-2 d-block w-100 border-0"
                      style="height: 480px; background: transparent"
                      sandbox="allow-same-origin"
                      :srcdoc="frameCss + detail.html"
                      @load="frameLoaded"
                    />
                  </div>
                  <div
                    v-else-if="detail.text"
                    class="py-3 text-body-2"
                    :class="{ 'reader-text': lgAndUp }"
                    :style="lgAndUp ? 'white-space: pre-wrap' : 'white-space: pre-wrap; line-height: 1.6'"
                  >
                    {{ detail.text }}
                  </div>
                  <p v-else class="py-6 text-body-2 text-medium-emphasis">{{ t('这封邮件没有可显示的正文。') }}</p>
                </div>
              </template>
              </div>
            </div>
          </section>
        </div>
      </v-main>

      <v-btn
        v-if="!lgAndUp && !selected && !convo"
        icon="mdi-pencil"
        color="primary"
        position="fixed"
        location="bottom right"
        class="fab-compose"
        style="width: 52px; height: 52px; flex: none"
        :aria-label="t('写信')"
        @click="openCompose({})"
      />

      <v-dialog v-model="settingsOpen" :z-index="settingsZ" :fullscreen="!lgAndUp" :max-width="lgAndUp ? 1080 : 920" scrollable transition="false" @update:model-value="(v) => { if (!v) closeSettings() }">
        <v-card style="border-radius: 28px">
          <v-card-title class="d-flex align-center">
            {{ t('设置') }}
            <v-spacer />
            <v-btn icon="mdi-close" size="small" variant="text" :aria-label="t('关闭')" @click="closeSettings()" />
          </v-card-title>
          <v-card-text class="pa-0">
            <Accounts
              :accounts="accounts"
              :accounts-status="accountsStatus"
              :statuses="statuses"
              :theme-mode="themeMode"
              :threaded="threaded"
              :undo-seconds="undoSeconds"
              :version="appVersion"
              @changed="refreshAccounts"
              @update:theme-mode="setThemeMode"
              @update:threaded="onThreadedChange"
              @update:undo-seconds="onUndoSecondsChange"
              @app-settings="onAppSettings"
            />
          </v-card-text>
        </v-card>
      </v-dialog>

      <v-dialog v-model="helpOpen" max-width="420">
        <v-card>
          <v-card-title>{{ t('键盘快捷键') }}</v-card-title>
          <v-card-text>
            <v-table density="compact">
              <tbody>
                <tr><td class="font-mono">j / k</td><td>{{ t('下移 / 上移') }}</td></tr>
                <tr><td class="font-mono">Enter</td><td>{{ t('打开邮件（线程打开会话）') }}</td></tr>
                <tr><td class="font-mono">e</td><td>{{ t('归档') }}</td></tr>
                <tr><td class="font-mono">Delete</td><td>{{ t('删除') }}</td></tr>
                <tr><td class="font-mono">r</td><td>{{ t('回复') }}</td></tr>
                <tr><td class="font-mono">a</td><td>{{ t('当前文件夹全部标为已读') }}</td></tr>
                <tr><td class="font-mono">c</td><td>{{ t('写新邮件') }}</td></tr>
                <tr><td class="font-mono">/</td><td>{{ t('聚焦搜索框') }}</td></tr>
                <tr><td class="font-mono">?</td><td>{{ t('本帮助') }}</td></tr>
              </tbody>
            </v-table>
          </v-card-text>
        </v-card>
      </v-dialog>

      <Compose v-model="composeOpen" :z-index="composeZ" :accounts="accounts" :initial="composeInit" :undo-seconds="undoSeconds" :max-attachment-mb="maxAttachmentMB" @done="onComposeDone" />
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
.tg-auth-alert {
  position: fixed;
  top: 16px;
  left: 50%;
  transform: translateX(-50%);
  z-index: 10;
  width: calc(100% - 32px);
  max-width: 440px;
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
@media (min-width: 1280px) {
  .v-application__wrap {
    height: 100dvh;
    max-height: 100dvh;
    overflow: hidden;
  }
  .v-main {
    min-height: 0;
    overflow: hidden;
  }
  .reader-section {
    display: flex;
    flex-direction: column;
    min-height: 0;
    overflow: hidden;
  }
}
.search-wrap {
  position: relative;
}
.search-menu-btn {
  position: absolute;
  left: 4px;
  top: 50%;
  transform: translateY(-50%);
  z-index: 2;
  width: 36px;
  height: 36px;
  margin: 0;
  padding: 0;
  border: none;
  border-radius: 50%;
  background: transparent;
  color: rgba(var(--v-theme-on-surface), 0.75);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
}
.search-menu-btn:active {
  background: rgba(var(--v-theme-on-surface), 0.12);
}
.search-menu-btn {
  overflow: hidden;
}
.search-menu-btn .v-icon {
  position: relative;
  z-index: 1;
}
.menu-ripple {
  position: absolute;
  border-radius: 50%;
  background: rgba(var(--v-theme-on-surface), 0.28);
  transform: scale(0);
  opacity: 1;
  animation: menu-ripple 0.45s ease-out forwards;
  pointer-events: none;
}
@keyframes menu-ripple {
  to {
    transform: scale(1);
    opacity: 0;
  }
}
.search-field .v-field__input {
  padding-inline-start: 48px;
}
.appbar-search {
  flex: 0 0 auto;
  width: 100%;
  padding: 0 12px;
}
.app-topbar {
  background: rgb(var(--v-theme-surface));
  border-bottom: 1px solid rgb(var(--v-theme-outline-variant));
}
.topbar-desktop {
  display: flex;
  align-items: center;
  gap: 4px;
  width: 100%;
  padding: 0 16px 0 8px;
}
.topbar-menu-btn {
  width: 48px;
  height: 48px;
  flex: none;
  border: none;
  border-radius: 50%;
  background: transparent;
  color: rgba(var(--v-theme-on-surface), 0.75);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
}
.topbar-menu-btn:hover {
  background: rgba(var(--v-theme-on-surface), 0.06);
}
.topbar-title {
  font-size: 18px;
  font-weight: 600;
  margin: 0 12px 0 4px;
  color: rgb(var(--v-theme-on-surface));
}
.topbar-search {
  flex: 0 1 720px;
  min-width: 200px;
}
.topbar-search .v-field {
  height: 56px;
  border-radius: 28px;
  background: rgb(var(--v-theme-surface-container-high));
  box-shadow: none;
}
.topbar-search .v-field__input {
  min-height: 56px;
  padding-top: 0;
  padding-bottom: 0;
  font-size: 16px;
}
.topbar-sync {
  width: 40px;
  height: 40px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: rgba(var(--v-theme-on-surface), 0.65);
}
.sync-spin {
  animation: wm-spin 1.8s linear infinite;
}
@keyframes wm-spin {
  to {
    transform: rotate(360deg);
  }
}
.topbar-avatar {
  width: 36px;
  height: 36px;
  border-radius: 50%;
  border: none;
  margin-left: 8px;
  padding: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  font-size: 15px;
}
@media (min-width: 1280px) {
  .nav-drawer {
    border-right: 1px solid rgb(var(--v-theme-outline-variant));
  }
  .nav-drawer .v-list-item {
    min-height: 44px;
  }
  .nav-drawer .v-list-item--active {
    background: rgb(var(--v-theme-primary-container));
    color: rgb(var(--v-theme-on-primary-container));
  }
  .nav-drawer .v-list-item__prepend .v-icon {
    font-size: 20px;
  }
}
.nav-compose {
  height: 56px !important;
  width: 160px;
  flex: none;
  align-self: flex-start;
  font-size: 15px;
  margin-top: 4px;
}
.nav-range {
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 2px 4px 6px 12px;
}
.nav-range-label {
  font-size: 13px;
  color: rgb(var(--v-theme-on-surface-variant));
  flex: 1;
  min-width: 0;
}
.nav-group-label {
  font-size: 12px;
  color: rgb(var(--v-theme-on-surface-variant));
  padding: 14px 12px 4px;
}
.nav-count {
  font-size: 12px;
  font-weight: 600;
  color: rgb(var(--v-theme-on-surface));
}
.nav-dot {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  flex: none;
  display: block;
}
.list-section {
  display: flex;
  flex-direction: column;
  flex: 1 1 auto;
  min-width: 0;
  height: 100%;
  position: relative;
}
.list-body {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
.ptr-ind {
  position: absolute;
  top: 0;
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
    flex: 0 0 auto;
    border-right: 1px solid rgb(var(--v-theme-outline-variant));
    background: rgb(var(--v-theme-surface-container));
  }
}
.mail-layout.with-offline {
  height: calc(100dvh - 100px);
}
.offline-bar {
  height: 36px;
  background: rgb(var(--v-theme-error-container));
  color: rgb(var(--v-theme-on-error-container));
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  font-size: 13px;
}
.list-head {
  height: 48px;
  flex: none;
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 0 8px 0 16px;
  border-bottom: 1px solid rgb(var(--v-theme-outline-variant));
}
.list-head-title {
  font-size: 15px;
  font-weight: 600;
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
}
.head-skel {
  display: inline-block;
  width: 90px;
  height: 12px;
  border-radius: 6px;
  margin-left: 10px;
  background: rgba(var(--v-theme-on-surface), 0.18);
  animation: wm-pulse 1.2s ease-in-out infinite;
}
@keyframes wm-pulse {
  0%,
  100% {
    opacity: 0.6;
  }
  50% {
    opacity: 1;
  }
}
.search-chips {
  height: 40px;
  flex: none;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 0 12px;
  border-bottom: 1px solid rgb(var(--v-theme-outline-variant));
}
.list-toolbar {
  height: 48px;
  flex: none;
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 0 8px 0 12px;
  border-bottom: 1px solid rgb(var(--v-theme-outline-variant));
}
.range-text {
  margin-left: auto;
  font-size: 12px;
  color: rgb(var(--v-theme-on-surface-variant));
}
.list-progress {
  height: 3px;
  flex: none;
}
.list-error-bar {
  flex: none;
  background: rgb(var(--v-theme-error-container));
  color: rgb(var(--v-theme-on-error-container));
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 12px;
  font-size: 13px;
}
.list-error-center {
  display: flex;
  justify-content: center;
  padding: 32px 0;
}
.list-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  padding: 72px 16px;
  color: rgb(var(--v-theme-on-surface-variant));
  font-size: 14px;
}
.list-skel {
  background: transparent;
}
.newmail-pill {
  position: absolute;
  top: 10px;
  left: 50%;
  transform: translateX(-50%);
  z-index: 6;
  background: rgb(var(--v-theme-primary-container));
  color: rgb(var(--v-theme-on-primary-container));
  border: none;
  border-radius: 999px;
  padding: 6px 14px;
  font-size: 13px;
  font-weight: 600;
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.25);
  cursor: pointer;
}
.list-footer {
  height: 36px;
  flex: none;
  border-top: 1px solid rgb(var(--v-theme-outline-variant));
  display: flex;
  align-items: center;
}
.list-drag {
  position: absolute;
  top: 0;
  right: -3px;
  width: 6px;
  height: 100%;
  cursor: col-resize;
  z-index: 7;
  touch-action: none;
}
.list-drag:hover {
  background: rgb(var(--v-theme-primary));
  opacity: 0.5;
}
.mail-row-d {
  position: relative;
  display: flex;
  align-items: flex-start;
  gap: 8px;
  height: 84px;
  padding: 10px 12px;
  cursor: pointer;
  background: rgb(var(--v-theme-surface-container-low));
}
.mail-row-d.row-unread {
  background: rgb(var(--v-theme-surface));
}
.mail-row-d.row-selected {
  background: rgb(var(--v-theme-secondary-container));
  box-shadow: inset 3px 0 0 rgb(var(--v-theme-primary));
}
.mail-row-d.row-cursor {
  outline: 2px solid rgb(var(--v-theme-primary));
  outline-offset: -2px;
}
@media (hover: hover) {
  .mail-row-d:hover {
    background: color-mix(
      in srgb,
      rgb(var(--v-theme-surface-container-low)) 95%,
      rgb(var(--v-theme-on-surface))
    );
  }
  .mail-row-d.row-unread:hover {
    background: color-mix(
      in srgb,
      rgb(var(--v-theme-surface)) 95%,
      rgb(var(--v-theme-on-surface))
    );
  }
  .mail-row-d.row-selected:hover {
    background: color-mix(
      in srgb,
      rgb(var(--v-theme-secondary-container)) 95%,
      rgb(var(--v-theme-on-surface))
    );
  }
}
.row-check {
  width: 28px;
  flex: none;
  display: flex;
  justify-content: center;
  padding-top: 1px;
  opacity: 0;
}
.mail-row-d:hover .row-check,
.row-check.row-check-on {
  opacity: 1;
}
.mail-avatar-d {
  flex: none;
  margin-top: 1px;
}
.row-avatar-letter {
  font-size: 13px;
  font-weight: 500;
}
.row-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 1px;
}
.row-l1 {
  display: flex;
  align-items: center;
  gap: 8px;
}
.row-sender {
  font-size: 15px;
  line-height: 20px;
  font-weight: 500;
  flex: 1;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.row-sender.w700 {
  font-weight: 700;
}
.row-time {
  font-size: 12px;
  color: rgb(var(--v-theme-on-surface-variant));
  flex: none;
}
.row-thread {
  font-size: 12px;
  color: rgb(var(--v-theme-on-surface-variant));
  flex: none;
  white-space: nowrap;
}
.row-l2 {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 14px;
  line-height: 19px;
  font-weight: 400;
}
.row-l2.w600 {
  font-weight: 600;
}
.row-subject {
  flex: 0 1 auto;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.row-l2-snippet {
  flex: 1;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  color: rgb(var(--v-theme-on-surface-variant));
  font-weight: 400;
}
.row-l3 {
  display: flex;
  align-items: center;
  gap: 6px;
}
.row-snippet {
  font-size: 13px;
  line-height: 18px;
  color: rgb(var(--v-theme-on-surface-variant));
  flex: 0 1 auto;
  min-width: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.row-chip {
  flex: none;
  height: 20px;
  font-size: 11px;
}
.density-compact .mail-row-d {
  height: 64px;
  padding: 8px 12px;
  border-bottom: 1px solid rgb(var(--v-theme-outline-variant));
}
.density-compact .row-avatar-letter {
  font-size: 11px;
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
.v-navigation-drawer--temporary {
  top: 0 !important;
  height: 100% !important;
  z-index: 2000 !important;
}
.v-navigation-drawer__scrim {
  z-index: 1999 !important;
}
@media (min-width: 1280px) {
  .reader-head-block {
    max-width: 980px;
    margin-inline: auto;
  }
  .reader-body-block {
    max-width: 820px;
    margin-inline: auto;
    width: 100%;
  }
  .convo-card {
    border: none !important;
    border-bottom: 1px solid rgb(var(--v-theme-outline-variant)) !important;
    border-radius: 0 !important;
  }
}
.reader-toolbar {
  height: 48px;
  flex: none;
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 0 8px;
  border-bottom: 1px solid rgb(var(--v-theme-outline-variant));
  background: rgb(var(--v-theme-surface));
}
.reader-subject {
  font-size: 22px;
  line-height: 30px;
  font-weight: 400;
  margin: 16px 0 8px;
  overflow-wrap: anywhere;
}
.reader-from-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.reader-from-name {
  font-size: 15px;
  font-weight: 600;
}
.reader-from-addr {
  font-size: 13px;
  color: rgb(var(--v-theme-on-surface-variant));
}
.reader-time {
  margin-left: auto;
  font-size: 13px;
  color: rgb(var(--v-theme-on-surface-variant));
  flex: none;
}
.reader-to-row {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  font-size: 13px;
  color: rgb(var(--v-theme-on-surface-variant));
  margin-top: 8px;
}
.reader-actions {
  display: flex;
  gap: 8px;
  margin: 12px 0 4px;
}
.reader-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 12px;
  color: rgb(var(--v-theme-on-surface-variant));
  font-size: 14px;
}
.reader-text {
  font-size: 15px;
  line-height: 27px;
}
.attach-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  padding: 12px 0;
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
@media (hover: none) {
  .v-tooltip {
    display: none !important;
  }
}
.mail-row-wrap {
  position: relative;
  overflow: hidden;
}
.swipe-bg {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  padding: 0 18px;
  opacity: 0;
}
.swipe-bg-left {
  background: rgb(var(--v-theme-secondary-container));
  color: rgb(var(--v-theme-on-secondary-container));
  justify-content: flex-start;
}
.swipe-bg-right {
  background: rgb(var(--v-theme-error-container));
  color: rgb(var(--v-theme-on-error-container));
  justify-content: flex-end;
}
.mail-row {
  position: relative;
  overflow: hidden;
  background: rgb(var(--v-theme-surface));
  display: flex;
  flex-direction: row;
  gap: 10px;
  width: 100%;
  padding: 6px 10px;
  border-bottom: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
  cursor: pointer;
  text-align: left;
}
.convo-stack {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.convo-card {
  border: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
  border-radius: 12px;
  background: rgb(var(--v-theme-surface));
  overflow: hidden;
}
.convo-card-head {
  position: relative;
  overflow: hidden;
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 10px 12px;
  cursor: pointer;
}
.convo-card-body {
  padding: 2px 12px 12px;
}
.mail-avatar {
  flex: none;
  margin-top: 2px;
}
@media (hover: hover) {
  .mail-row:hover {
    background: rgba(var(--v-theme-on-surface), 0.04);
  }
}
.mail-row-active {
  background: rgba(var(--v-theme-primary), 0.1);
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
