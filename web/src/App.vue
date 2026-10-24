<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useDisplay, useLocale, useTheme } from 'vuetify'
import { intlLocale, lang, t } from './i18n'
import Auth from './components/Auth.vue'
import Accounts from './components/Accounts.vue'
import Compose from './components/Compose.vue'
import * as api from './api.js'
import { scrollEl } from './scrollbars.js'
import { avatarColorFor, avatarLetterFor } from './avatar.js'

const SCOPES = computed(() => [
  { key: 'inbox', label: t('收件箱'), icon: 'mdi-inbox' },
  { key: 'sent', label: t('已发送'), icon: 'mdi-send' },
  { key: 'drafts', label: t('草稿箱'), icon: 'mdi-file-document-edit-outline' },
  { key: 'archive', label: t('归档'), icon: 'mdi-archive' },
  { key: 'junk', label: t('垃圾邮件'), icon: 'mdi-alert-octagon-outline' },
  { key: 'trash', label: t('回收站'), icon: 'mdi-delete' },
  { key: 'scheduled', label: t('定时发件'), icon: 'mdi-clock-outline' },
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

function markTombstone(id) {
  tombstones.set(id, snapCtxKey(listSnap.value))
}
function tombstoneLive(id, ctx) {
  return tombstones.get(id) === ctx
}
function dropTombstoned(batch, snap) {
  const ctx = snapCtxKey(snap)
  return batch.filter((m) => !tombstoneLive(m.id, ctx))
}

const scopeCache = new Map()
let listEpoch = 0
function listCacheKey(snap) {
  return snapCtxKey(snap) + '|t' + (threaded.value ? '1' : '0')
}
function stashList() {
  const snap = listSnap.value
  if (snap.kind !== 'folder' || listStatus.value !== 'ready') return
  scopeCache.set(listCacheKey(snap), {
    messages: messages.value,
    offset: listOffset,
    hasMore: listHasMore.value,
    scroll: scrollEl(listEl.value)?.scrollTop ?? 0,
    epoch: listEpoch,
  })
  if (scopeCache.size > 12) scopeCache.delete(scopeCache.keys().next().value)
}
const statuses = ref({})
const settingsOpen = ref(location.hash === '#/settings')
const scope = ref('inbox')
const selected = ref(null)
const detail = ref(null)
const barTitleShow = ref(false)
const barTitleText = computed(() => {
  if (selected.value) return selected.value.subject || t('(无主题)')
  if (convo.value) return convo.value.subject || t('(无主题)')
  return ''
})
let subjObserver = null
let subjEl = null
function observeSubject(el, root) {
  subjObserver = new IntersectionObserver(
    (entries) => {
      for (const e of entries) barTitleShow.value = !e.isIntersecting
    },
    { root, threshold: 0 },
  )
  subjObserver.observe(el)
}
function setSubjectEl(el) {
  if (el === subjEl) return
  if (subjObserver) {
    subjObserver.disconnect()
    subjObserver = null
  }
  subjEl = el
  barTitleShow.value = false
  if (!el) return
  const host = el.closest('.reader-scroll')
  const root = host ? host.querySelector('[data-overlayscrollbars-viewport]') : null
  if (root) {
    observeSubject(el, root)
    return
  }
  requestAnimationFrame(() => {
    if (subjEl !== el || subjObserver) return
    const h2 = el.closest('.reader-scroll')
    const r2 = h2 ? h2.querySelector('[data-overlayscrollbars-viewport]') : null
    observeSubject(el, r2)
  })
}
const detailState = ref('idle')
const detailError = ref('')
const actionError = ref('')
const purgeConfirm = ref(false)
const purging = ref(false)
async function doPurge() {
  const f = activeFolder.value
  if (!f || purging.value) return
  purging.value = true
  try {
    await api.purgeFolder(f.id)
    purgeConfirm.value = false
    showToast(f.special_use === 'trash' ? t('已清空回收站') : t('已清空垃圾箱'))
    loadFolders()
    loadMessages(true)
  } catch (err) {
    purgeConfirm.value = false
    showToast(f.special_use === 'trash' ? t('清空回收站失败') : t('清空垃圾箱失败'))
    loadFolders()
    loadMessages(true)
  } finally {
    purging.value = false
  }
}
const opBusy = ref(false)
let detailToken = 0

function clearSelection() {
  detailToken++
  selected.value = null
  detail.value = null
  detailState.value = 'idle'
  detailError.value = ''
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
  const removable = kind === 'archive' || kind === 'delete'
  const rows = removable ? displayMessages.value.filter((m) => selectedIds.value.has(m.id)) : []
  const openRemoved = !!(selected.value && ids.includes(selected.value.id))
  let opIds = ids
  const needsExpansion =
    removable && threaded.value && !searching.value && rows.some((x) => x.thread_count > 1)
  const destFor = (x) => (x.folder_special === 'trash' || x.folder_special === 'junk' ? 'inbox' : 'archive')
  const toastKind =
    kind === 'delete' && listSnap.value.scope === 'scheduled'
      ? 'cancel'
      : kind === 'archive' && rows.length && rows.every((x) => destFor(x) === 'inbox')
        ? 'inbox'
        : kind
  const specialById = new Map(rows.map((x) => [x.id, x.folder_special]))
  let work = null
  const movedItems = []
  const itemById = new Map()
  if (removable) {
    work = (async () => {
      if (needsExpansion) {
        const per = await Promise.all(
          rows.map(async (x) => {
            if (!(x.thread_count > 1)) return [x]
            const all = await api.getThreadMessagesAll(x.account_id, x.thread_key || 'id:' + x.id)
            const inTrashView = x.folder_special === 'trash' || x.folder_special === 'junk'
            const targets = all.filter((y) =>
              inTrashView
                ? y.folder_special === 'trash' || y.folder_special === 'junk'
                : y.folder_special !== 'trash' && y.folder_special !== 'junk',
            )
            return targets.length ? targets : [x]
          }),
        )
        const expanded = per.flat()
        for (const x of expanded) specialById.set(x.id, x.folder_special)
        opIds = [...new Set(expanded.map((x) => x.id))]
        const extraItems = expanded
          .filter(
            (x) =>
              x.id > 0 &&
              x.folder_special !== 'scheduled' &&
              (x.folder_id || x.folder_special) &&
              (kind === 'archive'
                ? x.folder_special !== destFor(x)
                : x.folder_special !== 'trash' && x.folder_special !== 'junk'),
          )
          .map((x) => ({ id: x.id, special: x.folder_special, folder_id: x.folder_id, row: x }))
        for (const it of extraItems) itemById.set(it.id, it)
        const have = new Set(rows.map((x) => x.id))
        const restItems = extraItems.filter((x) => !have.has(x.id))
        if ((opIds.length !== ids.length || restItems.length) && toastShow.value && Date.now() < toastHideAt) {
          showCountToast(
            toastKind,
            opIds.length - ids.length,
            restItems.length ? { items: restItems, settled: settledMoved } : null,
          )
        }
      }
      for (const id of opIds) {
        if (kind === 'archive')
          await api.moveMessage(id, specialById.get(id) === 'trash' || specialById.get(id) === 'junk' ? 'inbox' : 'archive')
        else if (listSnap.value.scope === 'scheduled') await api.cancelScheduled(-id)
        else await api.deleteMessage(id)
        markTombstone(id)
        const ui = itemById.get(id)
        if (ui) movedItems.push(ui)
      }
    })()
    clearSelectionIds()
    if (openRemoved) clearSelection()
    const undoItems = rows
      .filter(
        (x) =>
          x.id > 0 &&
          x.folder_special !== 'scheduled' &&
          (x.folder_id || x.folder_special) &&
          (kind === 'archive'
            ? x.folder_special !== destFor(x)
            : x.folder_special !== 'trash' && x.folder_special !== 'junk'),
      )
      .map((x) => ({ id: x.id, special: x.folder_special, folder_id: x.folder_id, row: x }))
    for (const it of undoItems) itemById.set(it.id, it)
    const settledMoved = work.then(
      () => movedItems,
      () => movedItems,
    )
    showCountToast(toastKind, opIds.length, undoItems.length ? { items: undoItems, settled: settledMoved } : null)
  }
  try {
    if (!work) {
      work = (async () => {
        for (const id of ids) {
          if (kind === 'seen') await api.setFlags(id, { seen: true })
          else if (kind === 'unseen') await api.setFlags(id, { seen: false })
        }
      })()
    }
    if (removable) {
      await Promise.all([collapseRows(rows), work])
      messages.value = messages.value.filter((m) => !ids.includes(m.id))
      exitMap.value = new Map()
      exitAnim.value = false
      for (const id of opIds) tombstones.delete(id)
      for (const id of ids) exitNatural.delete(id)
      reloadLoaded()
    } else {
      await work
      clearSelectionIds()
      if (openRemoved) clearSelection()
      loadMessages(true)
    }
    loadFolders()
  } catch (err) {
    if (removable) {
      toastShow.value = false
      for (const id of opIds) tombstones.delete(id)
      const back = new Map()
      for (const id of ids) if (exitNatural.has(id)) back.set(id, exitNatural.get(id))
      if (back.size) {
        exitMap.value = back
        await sleep(250)
      }
      for (const id of ids) exitNatural.delete(id)
      loadMessages(true)
    }
    exitMap.value = new Map()
    exitAnim.value = false
    if (removable && (toastKind === 'delete' || toastKind === 'archive' || toastKind === 'cancel' || toastKind === 'inbox')) {
      showRemovalFailure(toastKind, opIds.length - movedItems.length)
    } else {
      actionError.value = err.message
    }
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
function pinAnimating(batch) {
  if (!exitMap.value.size) return batch
  const have = new Set(batch.map((x) => x.id))
  const out = [...batch]
  messages.value.forEach((row, i) => {
    if (exitMap.value.has(row.id) && !have.has(row.id)) {
      out.splice(Math.min(i, out.length), 0, row)
    }
  })
  return out
}
function resetSearchFilter() {
  searchFilter.value = { attach: false, unread: false, accountId: 0 }
}
const newMailPending = ref(0)
function jumpToNewMail() {
  newMailPending.value = 0
  scrollEl(listEl.value)?.scrollTo({ top: 0 })
  reloadLoaded()
}
const listErrorBar = ref('')
const liveScopeLabel = computed(
  () => SCOPES.value.find((s) => s.key === scope.value)?.label ?? scope.value,
)
const listHeadTitle = computed(() => {
  if (searching.value) {
    if (listSnap.value.kind === 'search') {
      return t('搜索结果') + ' (' + t('{n} 封', { n: messages.value.length }) + ')'
    }
    return t('搜索结果')
  }
  const label = liveScopeLabel.value
  const st = scopeStats.value[scope.value]
  if (!st || foldersStatus.value !== 'ready') return label
  return (
    label +
    ' (' +
    t('{n} 封', { n: st.total }) +
    (st.unread ? t('，{n} 未读', { n: st.unread }) : '') +
    ')'
  )
})

const selectionHasUnread = computed(() =>
  displayMessages.value.some((m) => selectedIds.value.has(m.id) && !(m.flags & 1)),
)
const mobileSearch = ref(false)
const mobileBarTitle = computed(() => {
  if (searching.value) return t('搜索结果')
  return liveScopeLabel.value
})

function openMobileSearch() {
  mobileSearch.value = true
  nextTick(() => {
    searchEl.value?.focus?.()
  })
}

function closeMobileSearch() {
  mobileSearch.value = false
  clearSearch()
}
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
let esWasDown = false
let statusTimer = null
let booted = false

function deepIdFromHash() {
  const m = /^#\/mail\/(\d+)$/.exec(location.hash) || /^#m-(\d+)$/.exec(location.hash)
  return m ? +m[1] : 0
}

function parseHash() {
  const h = location.hash
  if (h === '#/settings') return { type: 'settings' }
  const mail = deepIdFromHash()
  if (mail) return { type: 'mail', id: mail }
  const sm = /^#\/search\/(.*)$/.exec(h)
  if (sm) {
    try {
      return { type: 'search', query: decodeURIComponent(sm[1]) }
    } catch {
      return { type: 'search', query: sm[1] }
    }
  }
  const sc = /^#\/([a-z]+)(?:\/(\d+))?$/.exec(h)
  if (sc && SCOPES.value.some((s) => s.key === sc[1])) {
    return { type: 'scope', scope: sc[1], account: sc[2] ? +sc[2] : 0 }
  }
  return { type: 'scope', scope: scope.value, account: rangeId.value }
}

function viewHash() {
  if (settingsOpen.value) return '#/settings'
  if (selected.value) return '#/mail/' + selected.value.id
  if (convo.value) return '#/mail/' + convo.value.anchor
  if (searching.value) return '#/search/' + encodeURIComponent(searchQuery.value)
  return '#/' + scope.value + (rangeId.value ? '/' + rangeId.value : '')
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
const scheduledPending = ref(0)
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
  map['scheduled'] = { total: scheduledPending.value, unread: scheduledPending.value, present: true }
  return map
})

async function refreshScheduledCount() {
  try {
    const list = await api.listScheduled()
    scheduledPending.value = list.length
  } catch {
  }
}
const visibleScopes = computed(() => SCOPES.value.filter((s) => scopeStats.value[s.key]?.present))

function applyTheme() {
  const dark =
    themeMode.value === 'dark' || (themeMode.value === 'system' && !!systemDarkMQ?.matches)
  theme.global.name.value = dark ? 'dark' : 'light'
  const bg = dark ? '#1F2022' : '#E7EDF4'
  document.documentElement.style.background = bg
  const tg = window.Telegram?.WebApp
  if (tg?.setBackgroundColor) {
    try {
      tg.setBackgroundColor(bg)
    } catch {
    }
  }
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
        folderId: scope.value === 'scheduled' ? 0 : rangeId.value ? (activeFolder.value?.id ?? -1) : 0,
      }
  stashList()
  const gen = ++listGen
  listBusy.value = true
  if (!soft) {
    clearSelectionIds()
    newMailPending.value = 0
  }
  const keepReady =
    (soft || target.kind === 'search') && listStatus.value === 'ready' && messages.value.length > 0
  if (!keepReady) listStatus.value = 'loading'
  listError.value = ''
  listOffset = 0
  convo.value = null
  convoOpenIds.value = new Set()
  convoDetails.value = {}
  cursorIdx.value = 0
  const rawHit = !soft && target.kind === 'folder' ? scopeCache.get(listCacheKey(target)) : null
  const hit = rawHit && rawHit.epoch === listEpoch ? rawHit : null
  if (hit) {
    messages.value = hit.messages
    listOffset = hit.offset
    listHasMore.value = hit.hasMore
    listSnap.value = target
    listStatus.value = 'ready'
    listBusy.value = false
    nextTick(() => {
      const el = scrollEl(listEl.value)
      if (el) el.scrollTop = hit.scroll
    })
    reloadLoaded()
    return
  }
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
    messages.value = pinAnimating(dropTombstoned(batch, target))
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
    messages.value = pinAnimating(dropTombstoned(batch, snap))
    listOffset = batch.length
    listHasMore.value = batch.length === n
    listSnap.value = snap
  } catch {
  }
}

function onListScroll() {
  const el = scrollEl(listEl.value)
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
  const el = scrollEl(listEl.value)
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
  const el = scrollEl(listEl.value)
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

let listVp = null
function bindListVp(vp) {
  if (!vp || vp === listVp) return
  if (listVp) listVp.removeEventListener('scroll', onListScroll)
  listVp = vp
  vp.addEventListener('scroll', onListScroll, { passive: true })
}

watch(listEl, (el) => {
  if (!el || el === ptrAttachedEl) return
  ptrAttachedEl = el
  el.addEventListener('touchstart', ptrStart, { passive: true })
  el.addEventListener('touchmove', ptrMove, { passive: false })
  el.addEventListener('touchend', ptrEnd)
  el.addEventListener('touchcancel', ptrEnd)
  el.addEventListener('os-viewport', (e) => bindListVp(e.detail))
  const vp = scrollEl(el)
  if (vp && vp !== el) bindListVp(vp)
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
let pressTimer = 0

const exitMap = ref(new Map())
const exitAnim = ref(false)
const toastText = ref('')
const toastShow = ref(false)
let toastTimer = 0
function showRemovalFailure(kind, n) {
  pendingUndo.value = null
  if (kind === 'archive') showToast(n > 1 ? t('{n} 封邮件归档失败，已恢复', { n }) : t('归档失败，邮件已恢复'))
  else if (kind === 'inbox') showToast(n > 1 ? t('{n} 封邮件移回收件箱失败，已恢复', { n }) : t('移回收件箱失败，邮件已恢复'))
  else if (kind === 'cancel') showToast(t('取消发送失败'))
  else showToast(n > 1 ? t('{n} 封邮件删除失败，已恢复', { n }) : t('删除失败，邮件已恢复'))
}
function onSnackSurfaceClick(e) {
  if (e.target.closest('.v-btn')) return
  dismissToast()
}
function dismissToast() {
  clearTimeout(toastTimer)
  toastShow.value = false
  pendingUndo.value = null
}
function showToast(text) {
  toastText.value = text
  toastShow.value = true
  clearTimeout(toastTimer)
  toastTimer = setTimeout(() => { toastShow.value = false }, toastTimeout.value)
}
let toastKind = ''
let toastCount = 0
let toastHideAt = 0
const pendingUndo = ref(null)
const undoBusy = ref(false)
const toastTimeout = computed(() => (pendingUndo.value ? 6000 : 3500))
watch(toastShow, (v) => {
  if (!v) {
    pendingUndo.value = null
  }
})
function showCountToast(kind, n, undo = null) {
  const now = Date.now()
  const continuing = toastKind === kind && toastShow.value && now < toastHideAt
  if (continuing) {
    toastCount += n
    if (undo && pendingUndo.value) {
      pendingUndo.value = {
        items: [...pendingUndo.value.items, ...undo.items],
        settled: Promise.all([pendingUndo.value.settled, undo.settled]).then(([a, b]) => [...a, ...b]),
      }
    } else if (undo) {
      pendingUndo.value = undo
    }
  } else {
    toastKind = kind
    toastCount = n
    pendingUndo.value = undo
  }
  toastHideAt = now + (pendingUndo.value ? 6000 : 3500)
  if (kind === 'delete') showToast(t('已删除 {n} 封邮件', { n: toastCount }))
  else if (kind === 'archive') showToast(t('已归档 {n} 封邮件', { n: toastCount }))
  else if (kind === 'inbox') showToast(t('已移回收件箱 {n} 封邮件', { n: toastCount }))
  else showToast(t('已取消 {n} 封定时发送', { n: toastCount }))
}
function rowBelongsToCurrentView(row) {
  if (searching.value) return false
  if (rangeId.value && row.account_id !== rangeId.value) return false
  if (activeFolder.value?.id) return row.folder_id === activeFolder.value.id
  if (scope.value === 'all') return true
  return row.folder_special === scope.value
}

function restoreRowsLocally(items) {
  const have = new Set(messages.value.map((x) => x.id))
  const back = []
  for (const it of items) {
    if (!it.row || have.has(it.id) || !rowBelongsToCurrentView(it.row)) continue
    back.push(it.row)
  }
  if (!back.length) return
  messages.value = [...messages.value, ...back].sort((a, b) => b.date_ts - a.date_ts || b.id - a.id)
}

async function undoRemoval() {
  const u = pendingUndo.value
  toastShow.value = false
  pendingUndo.value = null
  if (!u || undoBusy.value) return
  undoBusy.value = true
  for (const it of u.items) tombstones.delete(it.id)
  restoreRowsLocally(u.items)
  animateRowsIn(u.items.map((it) => it.id))
  try {
    const moved = await u.settled
    for (const it of moved) {
      if (it.folder_id) await api.moveMessageToFolder(it.id, it.folder_id)
      else await api.moveMessage(it.id, it.special)
    }
    if (moved.length) loadFolders()
    loadMessages(true)
  } catch (err) {
    actionError.value = err.message
    loadMessages(true)
  } finally {
    undoBusy.value = false
  }
}

const exitNatural = new Map()
const enterMap = ref(new Map())
const enterAnim = ref(false)
async function animateRowsIn(ids) {
  await nextTick()
  const naturals = new Map()
  for (const id of ids) {
    const i = displayMessages.value.findIndex((x) => x.id === id)
    if (i < 0) continue
    const wrap = document.querySelector('[data-rowidx="' + i + '"]')?.closest('.mail-row-wrap')
    if (wrap) naturals.set(id, wrap.offsetHeight)
  }
  if (!naturals.size) return
  enterAnim.value = false
  enterMap.value = new Map([...naturals.keys()].map((id) => [id, 0]))
  await nextTick()
  await new Promise((r) => requestAnimationFrame(r))
  enterAnim.value = true
  enterMap.value = naturals
  await sleep(250)
  enterMap.value = new Map()
  enterAnim.value = false
}

async function collapseRows(rows) {
  const heights = new Map()
  for (const m of rows) {
    const idx = displayMessages.value.indexOf(m)
    const el = idx >= 0 ? document.querySelector('[data-rowidx="' + idx + '"]')?.closest('.mail-row-wrap') : null
    heights.set(m.id, el ? el.offsetHeight : 0)
  }
  if (!heights.size) return
  for (const [id, h] of heights) exitNatural.set(id, h)
  exitAnim.value = false
  exitMap.value = heights
  await nextTick()
  await new Promise((r) => requestAnimationFrame(r))
  exitAnim.value = true
  exitMap.value = new Map([...heights.keys()].map((id) => [id, 0]))
  await sleep(240)
}

function rowWrapStyle(m) {
  if (exitMap.value.has(m.id)) {
    return {
      height: exitMap.value.get(m.id) + 'px',
      transition: exitAnim.value ? 'height 0.22s ease' : 'none',
      overflow: 'hidden',
    }
  }
  if (enterMap.value.has(m.id)) {
    return {
      height: enterMap.value.get(m.id) + 'px',
      transition: enterAnim.value ? 'height 0.22s ease' : 'none',
      overflow: 'hidden',
    }
  }
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
  clearTimeout(pressTimer)
  pressTimer = setTimeout(() => {
    toggleRowSelection(m)
    navigator.vibrate?.(15)
    suppressClickUntil = Date.now() + 400
  }, 450)
  swipeId.value = m.id
  swipeDx.value = 0
  swipeDragging.value = false
}

function rowTouchMove(e, m) {
  if (!swipeStart || swipeStart.id !== m.id) return
  const dx = e.touches[0].clientX - swipeStart.x
  const dy = e.touches[0].clientY - swipeStart.y
  if (Math.abs(dx) > 10 || Math.abs(dy) > 10) clearTimeout(pressTimer)
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
  clearTimeout(pressTimer)
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
  const canArchive = scope.value !== 'archive'
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
  const p = action === 'archive' ? archiveFor(m) : deleteFor(m)
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
    await p
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

let discardRowId = 0
function onDiscarded({ row_id, kind }) {
  if (kind === 'draft') {
    toastKind = ''
    pendingUndo.value = { items: [{ id: row_id, special: 'drafts' }], settled: Promise.resolve([{ id: row_id, special: 'drafts' }]) }
    toastHideAt = Date.now() + 6000
    showToast(t('草稿已删除'))
  } else {
    showCountToast('cancel', 1, null)
  }
  const row = displayMessages.value.find((x) => x.id === row_id)
  if (!row) return
  discardRowId = row_id
  animateRowRemoval(row, Promise.resolve()).then(() => {
    discardRowId = 0
    reloadLoaded()
  })
}

async function onComposeDone() {
  const jobs = [loadFolders(), loadStatuses(), refreshScheduledCount()]
  if ((scope.value === 'drafts' || scope.value === 'sent' || scope.value === 'scheduled') && !discardRowId) jobs.push(loadMessages(true))
  await Promise.all(jobs)
}

async function refreshAccounts() {
  const accDone = (async () => {
    try {
      accounts.value = await api.listAccounts()
      accountsStatus.value = 'ready'
    } catch {
      accountsStatus.value = 'error'
    }
    await loadStatuses()
  })()
  await Promise.all([accDone, loadFolders(), loadMessages()])
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
  if (tombstoneLive(item.id, snapCtxKey(listSnap.value))) return
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
  if (tombstoneLive(item.id, snapCtxKey(listSnap.value))) return
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
    messages.value = messages.value.filter((m) => !ids.has(m.id) || exitMap.value.has(m.id))
  }
  if (convo.value) {
    convo.value = { ...convo.value, msgs: convo.value.msgs.filter((m) => !ids.has(m.id)) }
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
  if (ev.type === 'messages' || ev.type === 'removed') listEpoch++
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
      scrollEl(listEl.value).scrollTop > 80
    ) {
      newMailPending.value += messages.value.length - beforeCount
    }
    scheduleStatusRefresh()
    refreshScheduledCount()
    if (scope.value === 'scheduled') loadMessages(true)
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
    const wasReconnect = esWasDown
    esWasDown = false
    if (wasReconnect && view.value === 'app') {
      checkVersion()
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
    esWasDown = true
    scheduleReconnect()
  }
}

async function checkVersion() {
  try {
    const st = await api.getState()
    if (!st.version || !appVersion.value || st.version === appVersion.value) return
    if (sessionStorage.getItem('wm_reloaded_for') === st.version) {
      appVersion.value = st.version
      return
    }
    sessionStorage.setItem('wm_reloaded_for', st.version)
    location.reload()
  } catch {
  }
}

function onVisibility() {
  if (!document.hidden && view.value === 'app') checkVersion()
}

function scheduleReconnect() {
  clearTimeout(esTimer)
  esTimer = setTimeout(() => {
    if (view.value === 'app') connectEvents()
  }, 1000)
}

function disconnectEvents() {
  clearTimeout(esTimer)
  if (es) {
    es.close()
    es = null
  }
}


function goScope(key) {
  mobileSearch.value = false
  if (!lgAndUp.value) drawerOpen.value = false
  const want = '#/' + key + (rangeId.value ? '/' + rangeId.value : '')
  if (location.hash === want) selectScope(key)
  else location.hash = want
}

function goRange(id) {
  const want = '#/' + scope.value + (id ? '/' + id : '')
  if (location.hash === want) setRange(id)
  else location.hash = want
}

function selectScope(key) {
  mobileSearch.value = false
  scope.value = key
  clearSelection()
  if (!lgAndUp.value) drawerOpen.value = false
  searchQuery.value = ''
  searching.value = false
  loadMessages()
}

let schOpenSeq = 0
async function openSchEditor(it) {
  if (it.status !== 'pending') return
  if (composeOpen.value && composeInit.value?.sched_id === it.id) return
  const tk = ++schOpenSeq
  openCompose({
    account_id: it.account_id,
    subject: it.subject || '',
    sched_id: it.id,
    sched_send_at: it.send_at,
    draft_loading: true,
  })
  try {
    const d = await api.getScheduled(it.id)
    if (tk !== schOpenSeq) return
    composeInit.value = {
      account_id: d.account_id,
      to: d.to || '',
      cc: d.cc || '',
      bcc: d.bcc || '',
      subject: d.subject || '',
      text: d.text || '',
      html: d.html || '',
      attachments: d.attachments || [],
      sched_id: it.id,
      sched_send_at: d.send_at,
      fill_nonce: tk,
    }
  } catch (err) {
    if (tk !== schOpenSeq) return
    composeInit.value = {
      account_id: it.account_id,
      subject: it.subject || '',
      sched_id: it.id,
      sched_send_at: it.send_at,
      fill_nonce: tk,
      load_error: t('定时邮件加载失败，请关闭后重试') + ' (' + err.message + ')',
    }
  }
}

const frameCss = computed(() => {
  const fg = '#000'
  const link = '#0b57d0'
  const scheme = 'light'
  const font = lgAndUp.value
    ? 'font:15px/1.8 Arial,Helvetica,"PingFang SC","Microsoft YaHei",sans-serif;'
    : 'font:14px/1.5 Arial,Helvetica,"PingFang SC","Microsoft YaHei",sans-serif;'
  const quote = lgAndUp.value
    ? 'blockquote{border-left:2px solid ' +
      '#74787c' +
      ';color:' +
      '#43474c' +
      ';margin-left:0;padding-left:12px}'
    : ''
  return (
    '<style>:root{color-scheme:' + scheme + '}html,body{margin:0;padding:0;background:#ffffff;color:' + fg + ';' +
    font +
    'overflow-wrap:anywhere;scrollbar-width:thin}::-webkit-scrollbar{width:8px;height:8px}::-webkit-scrollbar-thumb{background:rgba(128,128,128,0.4);border-radius:4px}::-webkit-scrollbar-track{background:transparent}img{max-width:100%;height:auto}a{color:' + link + ';text-decoration:none}a:hover{text-decoration:underline}' +
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

const frameObservers = new WeakMap()

function fitFrame(el) {
  try {
    const doc = el?.contentDocument
    if (!doc || !doc.documentElement) return
    const h = Math.max(doc.documentElement.scrollHeight || 0, doc.body?.scrollHeight || 0)
    const wrap = el.parentElement
    const avail = wrap ? wrap.clientWidth : 0
    const w = Math.max(doc.documentElement.scrollWidth || 0, doc.body?.scrollWidth || 0)
    if (h > 0 && avail > 0 && w > avail + 1) {
      const sc = avail / w
      el.style.setProperty('width', w + 'px', 'important')
      el.style.height = h + 'px'
      el.style.transform = 'scale(' + sc + ')'
      el.style.transformOrigin = 'top left'
      if (wrap) wrap.style.height = Math.ceil(h * sc) + 'px'
    } else if (h > 0) {
      el.style.removeProperty('width')
      el.style.transform = ''
      el.style.height = h + 'px'
      if (wrap) wrap.style.height = ''
    }
    const prev = frameObservers.get(el)
    if (prev && prev.doc === doc) return
    if (prev) prev.ro.disconnect()
    if (doc.body) {
      const ro = new ResizeObserver(() => fitFrame(el))
      ro.observe(doc.body)
      frameObservers.set(el, { ro, doc })
    }
  } catch {
  }
}

function frameLoaded() {
  fitFrame(frameEl.value)
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
  openMessage(placeholderMessage(id), { upgradeToThread: true })
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
}

function onHashChange() {
  if (!booted) return
  const route = parseHash()
  if (route.type === 'settings') {
    settingsOpen.value = true
    return
  }
  if (settingsOpen.value) settingsOpen.value = false
  if (route.type === 'mail') {
    if (selected.value?.id === route.id) return
    if (convo.value && convo.value.msgs.some((x) => x.id === route.id)) {
      if (selected.value) clearSelection()
      if (route.id !== convo.value.anchor) {
        convo.value = { ...convo.value, anchor: route.id }
        convoOpenIds.value = new Set([route.id])
        loadCardBody(route.id)
        nextTick(() => {
          document.querySelector('[data-convo-card="' + route.id + '"]')?.scrollIntoView({ block: 'start' })
        })
      }
      return
    }
    openMessageById(route.id)
    return
  }
  if (selected.value || convo.value) leaveReader()
  if (route.type === 'search') {
    if (!searching.value || searchQuery.value !== route.query) {
      searchQuery.value = route.query
      searching.value = route.query.trim().length > 0
      loadMessages(true)
    }
    return
  }
  if (searching.value) {
    searching.value = false
    searchQuery.value = ''
  }
  if (route.scope !== scope.value && route.account !== rangeId.value) {
    rangeId.value = route.account
    localStorage.setItem('wm_range', String(route.account))
    selectScope(route.scope)
  } else if (route.scope !== scope.value) {
    selectScope(route.scope)
  } else if (route.account !== rangeId.value) {
    setRange(route.account)
  }
}

watch([selected, convo, settingsOpen, searching, scope, rangeId], () => {
  if (!booted) return
  const want = viewHash()
  if (want !== location.hash) {
    if (hashReplaceOnLeave) {
      hashReplaceOnLeave = false
      history.replaceState(null, '', want)
    } else {
      history.pushState(null, '', want)
    }
  }
})

async function openMessage(m, opts) {
  hashReplaceOnLeave = false
  cancelReaderLeave()
  closeConvo()
  if (scope.value === 'scheduled' && !searching.value) {
    openSchEditor({ id: -m.id, account_id: m.account_id, subject: m.subject || '', send_at: m.date_ts, status: 'pending' })
    return
  }
  if (scope.value === 'drafts' && !searching.value) {
    const hit = detailCache.get(m.id)
    if (hit) {
      openCompose({
        account_id: m.account_id,
        to: fmtAddrList(hit.to),
        cc: fmtAddrList(hit.cc),
        subject: hit.subject || m.subject || '',
        text: hit.text || '',
        html: hit.html || '',
        draft_id: m.id,
      })
      return
    }
    openCompose({
      account_id: m.account_id,
      subject: m.subject || '',
      draft_id: m.id,
      draft_loading: true,
    })
    const tk = ++detailToken
    try {
      const d = await api.getMessage(m.id)
      detailCache.set(m.id, d)
      if (tk !== detailToken) return
      composeInit.value = {
        account_id: m.account_id,
        to: fmtAddrList(d.to),
        cc: fmtAddrList(d.cc),
        subject: d.subject || m.subject || '',
        text: d.text || '',
        html: d.html || '',
        draft_id: m.id,
        fill_nonce: tk,
      }
    } catch (err) {
      if (tk !== detailToken) return
      composeInit.value = {
        account_id: m.account_id,
        subject: m.subject || '',
        draft_id: m.id,
        fill_nonce: tk,
        load_error: t('草稿加载失败，请关闭后重试') + ' (' + err.message + ')',
      }
    }
    return
  }
  selected.value = m
  detailError.value = ''
  markSeen(m)
  const cached = detailCache.get(m.id)
  if (cached) {
    if (opts?.upgradeToThread && (cached.thread_count ?? 1) > 1 && cached.thread_key) {
      openConversation({ id: m.id, account_id: cached.account_id, thread_key: cached.thread_key, subject: cached.subject || m.subject || '', folder_special: cached.folder_special, flags: cached.flags ?? m.flags })
      return
    }
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
    if (opts?.upgradeToThread && (d.thread_count ?? 1) > 1 && d.thread_key) {
      openConversation({ id: m.id, account_id: d.account_id, thread_key: d.thread_key, subject: d.subject || m.subject || '', folder_special: d.folder_special, flags: d.flags ?? m.flags })
      return
    }
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
  hashReplaceOnLeave = false
  cancelReaderLeave()
  clearSelection()
  const threadKey = m.thread_key || 'id:' + m.id
  convo.value = {
    accountId: m.account_id,
    threadKey,
    anchor: m.id,
    subject: m.subject || '',
    status: 'loading',
    msgs: [],
  }
  convoOpenIds.value = new Set()
  convoDetails.value = {}
  try {
    const allMsgs = await api.getThreadMessagesAll(m.account_id, threadKey)
    const inTrashView = m.folder_special === 'trash' || m.folder_special === 'junk'
    let msgs = allMsgs.filter((x) =>
      inTrashView
        ? x.folder_special === 'trash' || x.folder_special === 'junk'
        : x.folder_special !== 'trash' && x.folder_special !== 'junk',
    )
    if (!msgs.length) msgs = allMsgs
    if (!convo.value || convo.value.threadKey !== threadKey) return
    const sorted = [...msgs].sort((a, b) => a.date_ts - b.date_ts || a.id - b.id)
    convo.value = { ...convo.value, status: 'ready', msgs: sorted }
    const open = new Set([m.id])
    convoOpenIds.value = open
    const bodyLoads = []
    for (const msg of sorted) {
      if (open.has(msg.id)) {
        bodyLoads.push(loadCardBody(msg.id))
        markSeen(msg)
      }
    }
    if (sorted.length > 1) {
      await Promise.all(bodyLoads)
      await nextTick()
      if (convo.value && convo.value.threadKey === threadKey) {
        const cardEl = document.querySelector('[data-convo-card="' + m.id + '"]')
        if (cardEl) {
          const host = cardEl.closest('.reader-scroll')
          const r = cardEl.getBoundingClientRect()
          const hr = host ? host.getBoundingClientRect() : null
          if (!hr || r.top < hr.top || r.bottom > hr.bottom) {
            cardEl.scrollIntoView({ block: 'start' })
          }
        }
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

const readerLeaving = ref(false)
const readerNoAnim = ref(false)
let hashReplaceOnLeave = false
let readerLeaveTimer = null

function closeReaderNow() {
  readerNoAnim.value = false
  if (convo.value) closeConvo()
  else if (selected.value) clearSelection()
}

function leaveReader() {
  hashReplaceOnLeave = true
  if (lgAndUp.value || readerLeaving.value) {
    closeReaderNow()
    return
  }
  readerLeaving.value = true
  readerLeaveTimer = setTimeout(() => {
    readerLeaveTimer = null
    readerLeaving.value = false
    closeReaderNow()
  }, 200)
}

function cancelReaderLeave() {
  if (readerLeaveTimer) {
    clearTimeout(readerLeaveTimer)
    readerLeaveTimer = null
  }
  readerLeaving.value = false
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
  fitFrame(e.target)
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
  if (!lgAndUp.value && selectionActive.value) {
    cursorIdx.value = idx
    toggleRowSelection(m)
    return
  }
  cursorIdx.value = idx
  cursorActive.value = false
  if ((threaded.value || searching.value) && (m.thread_count ?? 1) > 1) {
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
  markTombstone(id)
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

async function animateRowRemoval(m, p, extraIds = [], failKind = '') {
  const idx = displayMessages.value.indexOf(m)
  const wrap = idx >= 0 ? document.querySelector('[data-rowidx="' + idx + '"]')?.closest('.mail-row-wrap') : null
  const h = wrap ? wrap.offsetHeight : 0
  markTombstone(m.id)
  listGen++
  if (convo.value) {
    const left = convo.value.msgs.filter((x) => x.id !== m.id)
    if (left.length === 0) closeConvo()
    else convo.value = { ...convo.value, msgs: left }
  }
  if (selected.value?.id === m.id) clearSelection()
  loadFolders()
  let perr = null
  const settled = p.catch((e) => { perr = e })
  if (h > 0) {
    exitAnim.value = false
    exitMap.value = new Map([[m.id, h]])
    await sleep(250)
    await new Promise((r) => requestAnimationFrame(r))
    exitAnim.value = true
    exitMap.value = new Map([[m.id, 0]])
    await sleep(240)
  }
  if (threaded.value && !searching.value) {
    reloadLoaded()
  } else {
    messages.value = messages.value.filter((x) => x.id !== m.id)
  }
  exitMap.value = new Map()
  exitAnim.value = false
  await settled
  for (const id of extraIds) tombstones.delete(id)
  if (perr) {
    tombstones.delete(m.id)
    if (failKind) showRemovalFailure(failKind, 1)
    else {
      toastShow.value = false
      actionError.value = perr.message
    }
    restoreRowsLocally([{ id: m.id, row: m }])
    animateRowsIn([m.id])
    loadMessages(true)
  } else {
    tombstones.delete(m.id)
    loadMessages(true)
  }
}

async function actOnThread(m, kind, animate = false) {
  if (opBusy.value) return
  opBusy.value = true
  const inTrashView = m.folder_special === 'trash' || m.folder_special === 'junk'
  let settleResolve
  const settledAll = new Promise((res) => { settleResolve = res })
  const repUndoable =
    (m.folder_id || m.folder_special) &&
    (kind === 'archive' ? m.folder_special !== 'archive' : kind === 'inbox' ? true : !inTrashView)
  markTombstone(m.id)
  showCountToast(
    kind,
    m.thread_count || 1,
    repUndoable
      ? { items: [{ id: m.id, special: m.folder_special, folder_id: m.folder_id, row: m }], settled: settledAll }
      : null,
  )
  opBusy.value = false
  if (animate) {
    animateRowRemoval(m, settledAll, [], kind)
  } else {
    if (selected.value?.id === m.id) clearSelection()
    reloadLoaded()
  }
  let members
  try {
    const all = await api.getThreadMessagesAll(m.account_id, m.thread_key || 'id:' + m.id)
    members = all.filter((x) =>
      inTrashView
        ? x.folder_special === 'trash' || x.folder_special === 'junk'
        : x.folder_special !== 'trash' && x.folder_special !== 'junk',
    )
  } catch (err) {
    tombstones.delete(m.id)
    showRemovalFailure(kind, m.thread_count || 1)
    restoreRowsLocally([{ id: m.id, row: m }])
    animateRowsIn([m.id])
    loadMessages(true)
    settleResolve([])
    return
  }
  if (!members.length) members = [m]
  const ids = members.map((x) => x.id)
  for (const mem of members) markTombstone(mem.id)
  const undoItems = members
    .filter(
      (x) =>
        (x.folder_id || x.folder_special) &&
        (kind === 'archive'
          ? x.folder_special !== 'archive'
          : kind === 'inbox'
            ? x.folder_special !== 'inbox'
            : x.folder_special !== 'trash' && x.folder_special !== 'junk'),
    )
    .map((x) => ({ id: x.id, special: x.folder_special, folder_id: x.folder_id }))
  const restItems = undoItems.filter((x) => x.id !== m.id)
  if (
    (members.length !== (m.thread_count || 1) || restItems.length) &&
    toastShow.value &&
    toastKind === kind &&
    Date.now() < toastHideAt
  ) {
    showCountToast(
      kind,
      members.length - (m.thread_count || 1),
      restItems.length ? { items: restItems, settled: settledAll } : null,
    )
  }
  const movedItems = []
  try {
    for (const mem of members) {
      if (kind === 'archive' || kind === 'inbox') await api.moveMessage(mem.id, kind)
      else await api.deleteMessage(mem.id)
      const ui = undoItems.find((x) => x.id === mem.id)
      if (ui) movedItems.push(ui)
    }
    for (const id of ids) tombstones.delete(id)
    loadMessages(true)
  } catch (err) {
    for (const id of ids) tombstones.delete(id)
    showRemovalFailure(kind, members.length - movedItems.length)
    const movedIds = new Set(movedItems.map((x) => x.id))
    const backMembers = members.filter((x) => !movedIds.has(x.id))
    if (threaded.value && !searching.value) {
      await loadMessages(true)
      const rep = backMembers
        .filter((x) => rowBelongsToCurrentView(x))
        .sort((a, b) => b.date_ts - a.date_ts || b.id - a.id)[0]
      if (rep) animateRowsIn([rep.id])
    } else {
      const backItems = backMembers.map((x) => ({ id: x.id, row: x }))
      restoreRowsLocally(backItems)
      animateRowsIn(backItems.map((x) => x.id))
      loadMessages(true)
    }
  } finally {
    settleResolve(movedItems)
  }
}

async function archiveFor(m, animate = false) {
  if (!m || opBusy.value) return
  if (m.folder_special === 'trash' || m.folder_special === 'junk') return inboxFor(m, animate)
  if (threaded.value && !searching.value && m.thread_count > 1) return actOnThread(m, 'archive', animate)
  if (m.folder_special === 'scheduled') return deleteFor(m, animate)
  return moveSingle(m, 'archive', animate)
}

function inboxFor(m, animate = false) {
  if (threaded.value && !searching.value && m.thread_count > 1) return actOnThread(m, 'inbox', animate)
  return moveSingle(m, 'inbox', animate)
}

async function moveSingle(m, dest, animate = false) {
  opBusy.value = true
  const p = api.moveMessage(m.id, dest)
  const undoable = !!m.folder_special && m.folder_special !== dest
  showCountToast(
    dest,
    1,
    undoable
      ? {
          items: [{ id: m.id, special: m.folder_special, folder_id: m.folder_id, row: m }],
          settled: p.then(
            () => [{ id: m.id, special: m.folder_special, folder_id: m.folder_id }],
            () => [],
          ),
        }
      : null,
  )
  if (animate) {
    opBusy.value = false
    animateRowRemoval(m, p, [], dest)
    return
  }
  afterRemoval(m.id)
  opBusy.value = false
  try {
    await p
    tombstones.delete(m.id)
    loadMessages(true)
  } catch (err) {
    tombstones.delete(m.id)
    showRemovalFailure(dest, 1)
    restoreRowsLocally([{ id: m.id, row: m }])
    animateRowsIn([m.id])
    loadMessages(true)
  }
}

async function deleteFor(m, animate = false) {
  if (!m || opBusy.value) return
  if (threaded.value && !searching.value && m.thread_count > 1) return actOnThread(m, 'delete', animate)
  opBusy.value = true
  const p = m.folder_special === 'scheduled' ? api.cancelScheduled(-m.id) : api.deleteMessage(m.id)
  const okKind = m.folder_special === 'scheduled' ? 'cancel' : 'delete'
  const undoable = !!m.folder_special && m.folder_special !== 'scheduled' && m.folder_special !== 'trash' && m.folder_special !== 'junk'
  showCountToast(
    okKind,
    1,
    undoable
      ? {
          items: [{ id: m.id, special: m.folder_special, folder_id: m.folder_id, row: m }],
          settled: p.then(
            () => [{ id: m.id, special: m.folder_special, folder_id: m.folder_id }],
            () => [],
          ),
        }
      : null,
  )
  if (animate) {
    opBusy.value = false
    animateRowRemoval(m, p, [], okKind)
    return
  }
  afterRemoval(m.id)
  opBusy.value = false
  try {
    await p
    tombstones.delete(m.id)
    loadMessages(true)
  } catch (err) {
    tombstones.delete(m.id)
    showRemovalFailure(okKind, 1)
    restoreRowsLocally([{ id: m.id, row: m }])
    animateRowsIn([m.id])
    loadMessages(true)
  }
}

async function markAllRead() {
  if (searching.value || opBusy.value || rangeId.value || scope.value === 'scheduled') return
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
  const scroller = scrollEl(e.currentTarget.querySelector('.reader-scroll'))
  const atTop = !scroller || scroller.scrollTop <= 0
  if ((dx > 90 && Math.abs(dy) < 60) || (atTop && dy > 110 && Math.abs(dx) < 60)) {
    leaveReader()
  }
}

function openAccounts() {
  openSettings()
  if (!lgAndUp.value) drawerOpen.value = false
}

async function enter(st, deepId = 0) {
  view.value = 'app'
  const r0 = parseHash()
  if (r0.type === 'scope') {
    scope.value = r0.scope
    if (r0.account !== rangeId.value) {
      rangeId.value = r0.account
      localStorage.setItem('wm_range', String(r0.account))
    }
  } else if (r0.type === 'search') {
    searchQuery.value = r0.query
    searching.value = r0.query.trim().length > 0
  }
  const did = deepId || initialDeepId
  if (did) {
    readerNoAnim.value = true
    history.replaceState(null, '', '#/' + scope.value + (rangeId.value ? '/' + rangeId.value : ''))
    history.pushState(null, '', '#/mail/' + did)
    openMessage(placeholderMessage(did))
  }
  if (st && st.app_settings) maxAttachmentMB.value = st.app_settings.max_attachment_mb
  threaded.value = localStorage.getItem('wm_threaded') === '1'
  const storedUndo = parseInt(localStorage.getItem('wm_undo_seconds') ?? '', 10)
  if ([0, 5, 10, 20, 30].includes(storedUndo)) undoSeconds.value = storedUndo
  await refreshAccounts()
  connectEvents()
  refreshScheduledCount()
  booted = true
  onHashChange()
}

async function boot() {
  view.value = 'loading'
  try {
    let st = await api.getState()
    appVersion.value = st.version || ''
    const tg = window.Telegram?.WebApp
    const inTG = !!(tg || navigator.userAgent.includes('Telegram'))
    if (tg?.initData) {
      try {
        tg.disableVerticalSwipes?.()
      } catch {}
    }
    if (st.setup && !st.authed) {
      if (tg?.initData) {
        try {
          await api.telegramWebAppAuth(tg.initData)
          st = await api.getState()
          appVersion.value = st.version || ''
        } catch (err) {
          tgAuthError.value = err.message
        }
        try {
          tg.ready()
        } catch {}
      }
    }
    const pathMatch = /^\/m\/(\d+)$/.exec(location.pathname)
    const mParam = pathMatch ? pathMatch[1] : new URLSearchParams(location.search).get('m') || ''
    let bootDeepId = /^\d+$/.test(mParam) ? Number(mParam) : 0
    if (pathMatch) history.replaceState(null, '', '/' + location.hash)
    if (!bootDeepId) {
      const sp = tgStartParam()
      if (sp) bootDeepId = Number(sp)
    }
    if (!st.setup) {
      authMode.value = 'setup'
      view.value = 'auth'
    } else if (!st.authed) {
      authMode.value = 'login'
      view.value = 'auth'
    } else {
      await enter(st, bootDeepId)
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
  document.addEventListener('visibilitychange', onVisibility)
  window.addEventListener('keydown', onGlobalKey)
  window.addEventListener('online', onOnlineChange)
  window.addEventListener('offline', onOnlineChange)
  initTheme()
  boot()
})

onBeforeUnmount(() => {
  window.removeEventListener('hashchange', onHashChange)
  document.removeEventListener('visibilitychange', onVisibility)
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
      <v-app-bar flat class="app-topbar">
        <div v-if="lgAndUp" class="topbar-desktop">
          <v-btn icon="mdi-menu" variant="text" :aria-label="t('展开菜单')" @click="toggleDrawer" />
          <span v-if="wideDesktop" class="topbar-title">Namakani</span>
          <v-spacer />
          <div class="topbar-search">
            <v-text-field
              ref="searchEl"
              v-model="searchQuery"
              :placeholder="t('搜索')"
              variant="solo-filled"
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
        </div>
        <div v-else class="appbar-search">
          <div v-if="selectionActive" class="d-flex align-center ga-1 mx-n2">
            <v-btn icon="mdi-close" variant="text" :aria-label="t('取消选择')" @click="clearSelectionIds" />
            <v-toolbar-title class="ml-4">{{ selectedIds.size }}</v-toolbar-title>
            <v-btn v-if="scope !== 'scheduled'" :icon="(scope === 'trash' || scope === 'junk') ? 'mdi-inbox' : 'mdi-archive'" variant="text" :disabled="opBusy || scope === 'archive'" :aria-label="(scope === 'trash' || scope === 'junk') ? t('移回收件箱') : t('归档')" @click="bulkAct('archive')" />
            <v-btn icon="mdi-delete" variant="text" :disabled="opBusy" :aria-label="t('删除')" @click="bulkAct('delete')" />
            <v-btn v-if="scope !== 'scheduled'" :icon="selectionHasUnread ? 'mdi-email-open' : 'mdi-email'" variant="text" :disabled="opBusy" :aria-label="selectionHasUnread ? t('标为已读') : t('标为未读')" @click="bulkAct(selectionHasUnread ? 'seen' : 'unseen')" />
          </div>
          <div v-else-if="mobileSearch || searching" class="search-wrap">
            <v-btn icon="mdi-arrow-left" variant="text" :aria-label="t('返回')" class="search-back-btn" @click="closeMobileSearch" />
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
          <div v-else class="d-flex align-center ga-1 mx-n2">
            <v-btn icon="mdi-menu" variant="text" :aria-label="t('展开菜单')" @click="toggleDrawer" />
            <v-toolbar-title class="ml-4">{{ mobileBarTitle }}</v-toolbar-title>
            <v-btn icon="mdi-magnify" variant="text" :aria-label="t('搜索')" @click="openMobileSearch" />
          </div>
        </div>
      </v-app-bar>

      <v-navigation-drawer
        v-model="drawerOpen"
        :permanent="lgAndUp"
        :temporary="!lgAndUp"
        :width="lgAndUp ? 288 : 280"
        :touchless="!lgAndUp && composeOpen"
      >
        <div class="d-flex flex-column drawer-inner">
          <div v-if="lgAndUp" class="pa-3 pb-0">
            <v-btn
              block
              color="primary-container"
              prepend-icon="mdi-pencil"
              @click="openCompose({})"
            >
              {{ t('写信') }}
            </v-btn>
          </div>

          <v-list v-if="accounts.length > 1" density="compact" nav>
            <v-list-item :title="rangeId ? (rangeAccount?.label || rangeAccount?.email) : t('全部账号')">
              <template #append>
                <v-icon icon="mdi-chevron-down" size="x-small" />
              </template>
              <v-menu activator="parent">
                  <v-list density="compact">
                    <v-list-item :title="t('全部账号')" :active="rangeId === 0" @click="goRange(0)" />
                    <v-list-item
                      v-for="a in accounts"
                      :key="a.id"
                      :title="a.label || a.email"
                      :active="rangeId === a.id"
                      @click="goRange(a.id)"
                    />
                  </v-list>
              </v-menu>
            </v-list-item>
          </v-list>

          <template v-if="foldersStatus === 'loading'">
            <v-skeleton-loader type="list-item@10" class="skel-fill" />
          </template>
          <template v-else-if="foldersStatus === 'error'">
            <p class="text-body-2 text-error px-1">
              {{ t('文件夹加载失败：{error}', { error: foldersError }) }}
              <v-btn variant="text" size="small" @click="loadFolders">{{ t('重试') }}</v-btn>
            </p>
          </template>
          <template v-else>
            <v-list density="compact" nav>
              <v-list-item
                v-for="s in visibleScopes"
                :key="s.key"
                :prepend-icon="s.icon"
                :title="s.label"
                :active="scope === s.key"
                @click="goScope(s.key)"
              >
                <template #append>
                  <v-chip v-if="scopeStats[s.key].unread > 0" size="small">
                    {{ scopeStats[s.key].unread }}
                  </v-chip>
                </template>
              </v-list-item>
            </v-list>
            <p v-if="visibleScopes.length === 0" class="text-body-2 text-medium-emphasis px-1">
              {{ t('暂无文件夹（同步后出现）') }}
            </p>
          </template>

          <div class="mt-auto">
            <v-divider class="mb-2" />
            <v-list density="compact" nav>
              <v-list-item
                prepend-icon="mdi-cog"
                :title="t('设置')"
                :active="settingsOpen"
                @click="openAccounts"
              />
              <v-list-item prepend-icon="mdi-logout" :title="t('退出登录')" @click="doLogout" />
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
                  <span v-if="listBusy" class="head-skel" />
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
                    <v-list-item
                      v-if="activeFolder && (activeFolder.special_use === 'trash' || activeFolder.special_use === 'junk')"
                      :title="activeFolder.special_use === 'trash' ? t('清空回收站') : t('清空垃圾箱')"
                      @click="purgeConfirm = true"
                    />
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
                  <v-btn v-if="scope !== 'scheduled'" icon variant="text" size="small" :disabled="opBusy || scope === 'archive'" @click="bulkAct('archive')">
                    <v-icon :icon="(scope === 'trash' || scope === 'junk') ? 'mdi-inbox' : 'mdi-archive'" />
                    <v-tooltip activator="parent" location="bottom">{{ (scope === 'trash' || scope === 'junk') ? t('移回收件箱') : t('归档') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon variant="text" size="small" :disabled="opBusy" @click="bulkAct('delete')">
                    <v-icon icon="mdi-delete" />
                    <v-tooltip activator="parent" location="bottom">{{ t('删除') }}</v-tooltip>
                  </v-btn>
                  <v-btn v-if="scope !== 'scheduled'" icon variant="text" size="small" :disabled="opBusy" @click="bulkAct(selectionHasUnread ? 'seen' : 'unseen')">
                    <v-icon :icon="selectionHasUnread ? 'mdi-email-open' : 'mdi-email'" />
                    <v-tooltip activator="parent" location="bottom">{{ selectionHasUnread ? t('标为已读') : t('标为未读') }}</v-tooltip>
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
            </template>
            <div v-if="listBusy" class="list-progress">
              <v-progress-linear indeterminate color="primary" height="3" />
            </div>
            <div v-if="listErrorBar" class="list-error-bar">
              <span class="text-truncate">{{ t('加载失败：{error}', { error: listErrorBar }) }}</span>
              <v-btn variant="text" size="small" @click="loadMessages()">{{ t('重试') }}</v-btn>
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
              <div class="h-100">
              <template v-if="listStatus === 'loading'">
                <v-skeleton-loader v-if="lgAndUp" type="list-item-avatar-three-line@16" class="list-skel skel-fixed" />
                <v-skeleton-loader v-else type="list-item-three-line@24" class="skel-fixed" />
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
                <template v-for="(m, idx) in displayMessages" :key="threaded && listSnap.kind !== 'search' && m.thread_key ? 't' + m.account_id + ':' + m.thread_key : m.id">
                  <div class="mail-row-wrap" :style="rowWrapStyle(m)">
                    <div
                      v-if="scope !== 'archive' && scope !== 'scheduled'"
                      class="swipe-bg swipe-bg-left"
                      :style="{ opacity: swipeId === m.id && swipeDx > 0 ? 1 : 0 }"
                    >
                      <v-icon :icon="(m.folder_special === 'trash' || m.folder_special === 'junk') ? 'mdi-inbox' : 'mdi-archive'" size="20" />
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
                        <v-icon v-if="m.folder_special === 'scheduled'" icon="mdi-clock-outline" size="14" class="row-time-icon" />
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
                    :class="{ 'mail-row-active': (lgAndUp && selected?.id === m.id) || (cursorActive && idx === cursorIdx), 'mail-row-checked': selectedIds.has(m.id) }"
                    :style="rowSwipeStyle(m)"
                    @click="rowClick(m, idx)"
                    @touchstart.passive="rowTouchStart($event, m)"
                    @touchmove="rowTouchMove($event, m)"
                    @touchend="rowTouchEnd($event, m)"
                    @touchcancel="rowTouchEnd($event, m)"
                    @contextmenu.prevent
                  >
                    <v-avatar v-if="selectedIds.has(m.id)" color="primary" size="38" class="mail-avatar">
                      <v-icon icon="mdi-check" size="20" />
                    </v-avatar>
                    <v-avatar v-else :color="avatarColor(m.from_email)" size="38" class="mail-avatar">
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
                        <v-icon v-if="m.folder_special === 'scheduled'" icon="mdi-clock-outline" size="13" class="text-medium-emphasis" />
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
                  v-if="listHasMore && !searching"
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
            <div v-if="lgAndUp" class="list-drag" @pointerdown="onListDragStart" />
          </section>

          <section class="reader-section" :class="{ 'd-none': !lgAndUp && !selected && !convo, 'reader-overlay': !lgAndUp && (!!selected || !!convo), 'reader-leaving': readerLeaving, 'reader-noanim': readerNoAnim }">
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
                <v-btn
                  icon="mdi-arrow-left"
                  variant="text"
                  :aria-label="t('返回')"
                  @click="leaveReader"
                />
                <span v-if="barTitleShow" class="reader-bar-title text-truncate">{{ barTitleText }}</span>
                <v-spacer />
              </div>
              <div class="reader-scroll">
                <div data-overlayscrollbars-viewport class="reader-vp">
                  <div data-overlayscrollbars-content class="px-3 pb-3 px-sm-4 pb-sm-4">
                <h2 :ref="setSubjectEl" class="text-h6 font-weight-bold mb-1" style="overflow-wrap: anywhere">
                  {{ convo.subject || t('(无主题)') }}
                </h2>
                <p class="text-caption text-medium-emphasis mb-3">{{ t('共 {n} 封', { n: convo.msgs.length }) }}</p>
                <template v-if="convo.status === 'loading'">
                  <v-skeleton-loader type="list-item-three-line@12" class="skel-fixed" />
                </template>
                <v-alert v-else-if="convo.status === 'error'" type="error" variant="tonal" density="compact">
                  {{ t('会话加载失败') }}
                  <div class="mt-2">
                    <v-btn size="small" variant="outlined" @click="convo = null">{{ t('关闭') }}</v-btn>
                  </div>
                </v-alert>
                <div v-else class="convo-stack">
                  <div v-for="msg in convo.msgs" :key="msg.id" class="convo-card" :data-convo-card="msg.id">
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
                            v-if="scope !== 'archive'"
                            icon
                            variant="text"
                            size="small"
                            :disabled="opBusy"
                            @click="archiveFor(msg)"
                          >
                            <v-icon :icon="(msg.folder_special === 'trash' || msg.folder_special === 'junk') ? 'mdi-inbox' : 'mdi-archive'" />
                            <v-tooltip activator="parent" location="bottom">{{ msg.folder_special === 'junk' ? t('不是垃圾邮件') : msg.folder_special === 'trash' ? t('移回收件箱') : t('归档') }}</v-tooltip>
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
                            scrolling="no"
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
                      <v-skeleton-loader v-else type="paragraph@2" class="skel-clip" />
                    </div>
                  </div>
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
                <v-btn
                  icon="mdi-arrow-left"
                  variant="text"
                  :aria-label="t('返回')"
                  @click="leaveReader"
                />
                <span v-if="barTitleShow" class="reader-bar-title text-truncate">{{ barTitleText }}</span>
                <v-spacer />
              </div>
              <div v-if="lgAndUp" class="reader-toolbar">
                <v-btn
                  v-if="scope !== 'archive'"
                  icon
                  variant="text"
                  size="small"
                  :disabled="opBusy"
                  @click="archiveFor(selected, true)"
                >
                  <v-icon :icon="(selected.folder_special === 'trash' || selected.folder_special === 'junk') ? 'mdi-inbox' : 'mdi-archive'" />
                  <v-tooltip activator="parent" location="bottom">{{ selected.folder_special === 'junk' ? t('不是垃圾邮件') : selected.folder_special === 'trash' ? t('移回收件箱') : t('归档') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" size="small" :disabled="opBusy" @click="deleteFor(selected, true)">
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
                <span v-if="barTitleShow" class="reader-bar-title text-truncate">{{ barTitleText }}</span>
                <v-spacer />
                <v-btn icon="mdi-printer" variant="text" size="small" :disabled="!detail?.html" :aria-label="t('打印')" @click="printSelected" />
              </div>
              <div class="reader-scroll">
                <div data-overlayscrollbars-viewport class="reader-vp">
                  <div data-overlayscrollbars-content class="px-3 pb-3 px-sm-4 pb-sm-4">
              <template v-if="lgAndUp">
                <div class="reader-head-block">
                  <h2 :ref="setSubjectEl" class="reader-subject">{{ selected.subject || t('(无主题)') }}</h2>
                  <div class="reader-from-row">
                    <v-avatar :color="avatarColor(selected.from_email)" size="40" class="flex-none">
                      <span class="avatar-letter" style="font-size: 16px">{{ avatarLetter(selected) }}</span>
                    </v-avatar>
                    <span class="reader-from-name text-truncate">
                      {{ detail?.from?.name || selected.from_name || selected.from_email }}
                    </span>
                    <span class="reader-from-addr text-truncate">{{ detail?.from?.email || selected.from_email }}</span>
                    <span v-if="selected.date_ts" class="reader-time">{{ fmtFull(selected.date_ts) }}</span>
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
              <h2 :ref="setSubjectEl" class="text-h6 font-weight-bold mb-2" style="overflow-wrap: anywhere">
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
                <span v-if="selected.date_ts">{{ fmtFull(selected.date_ts) }}</span>
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
                  v-if="scope !== 'archive'"
                  icon
                  variant="text"
                  :disabled="opBusy"
                  @click="archiveFor(selected, true)"
                >
                  <v-icon :icon="(selected.folder_special === 'trash' || selected.folder_special === 'junk') ? 'mdi-inbox' : 'mdi-archive'" />
                  <v-tooltip activator="parent" location="bottom">{{ selected.folder_special === 'junk' ? t('不是垃圾邮件') : selected.folder_special === 'trash' ? t('移回收件箱') : t('归档') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="opBusy" @click="deleteFor(selected, true)">
                  <v-icon icon="mdi-delete" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ scope === 'trash' || scope === 'junk' ? t('彻底删除') : t('删除') }}
                  </v-tooltip>
                </v-btn>
              </div>
              </template>

              <template v-if="detailState === 'loading'">
                <v-skeleton-loader type="paragraph@10" class="py-4 reader-body-block skel-clip" />
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
                      scrolling="no"
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
              </div>
            </div>
          </section>
        </div>
      </v-main>

      <v-snackbar v-model="toastShow" :timeout="-1" location="bottom" @click="onSnackSurfaceClick">
        {{ toastText }}
        <template v-if="pendingUndo" #actions>
          <v-btn color="primary" variant="text" :disabled="undoBusy" @click="undoRemoval">{{ t('撤销') }}</v-btn>
        </template>
      </v-snackbar>
      <v-dialog v-model="purgeConfirm" max-width="420">
        <v-card>
          <v-card-title>{{ activeFolder?.special_use === 'trash' ? t('清空回收站') : t('清空垃圾箱') }}</v-card-title>
          <v-card-text>{{ activeFolder?.special_use === 'trash' ? t('回收站里的所有邮件将被永久删除，无法恢复。') : t('垃圾箱里的所有邮件将被永久删除，无法恢复。') }}</v-card-text>
          <v-card-actions>
            <v-spacer />
            <v-btn variant="text" :disabled="purging" @click="purgeConfirm = false">{{ t('取消') }}</v-btn>
            <v-btn color="error" variant="text" :loading="purging" @click="doPurge">{{ t('清空') }}</v-btn>
          </v-card-actions>
        </v-card>
      </v-dialog>

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

      <v-dialog v-model="settingsOpen" :z-index="settingsZ" :fullscreen="!lgAndUp" :max-width="lgAndUp ? 1080 : 920" scrollable @update:model-value="(v) => { if (!v) closeSettings() }">
        <v-card class="settings-card" :style="lgAndUp ? { height: 'min(760px, calc(100dvh - 48px))' } : undefined">
          <v-toolbar color="transparent">
            <v-btn :icon="lgAndUp ? 'mdi-close' : 'mdi-arrow-left'" variant="text" :aria-label="lgAndUp ? t('关闭') : t('返回')" @click="closeSettings()" />
            <v-toolbar-title>{{ t('设置') }}</v-toolbar-title>
          </v-toolbar>
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

      <Compose v-model="composeOpen" :z-index="composeZ" :accounts="accounts" :initial="composeInit" :undo-seconds="undoSeconds" :max-attachment-mb="maxAttachmentMB" @done="onComposeDone" @discarded="onDiscarded" />
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
.search-back-btn {
  position: absolute;
  left: 0;
  top: 50%;
  transform: translateY(-50%);
  z-index: 2;
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
.settings-card {
  display: flex;
  flex-direction: column;
}
.settings-card > .v-card-text {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.topbar-desktop {
  display: flex;
  align-items: center;
  gap: 4px;
  width: 100%;
  padding: 0 16px 0 8px;
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
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  z-index: 6;
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
  background: rgba(var(--v-theme-primary), var(--v-activated-opacity));
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
    background: rgba(var(--v-theme-primary), calc(var(--v-activated-opacity) + 0.05));
  }
}
.row-check {
  width: 28px;
  flex: none;
  display: flex;
  justify-content: center;
  padding-top: 1px;
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
  overflow-anchor: none;
  overscroll-behavior-y: none;
}
.reader-section {
  flex: 1 1 auto;
  min-width: 0;
  height: 100%;
  background: rgb(var(--v-theme-surface));
}
.reader-overlay {
  position: fixed;
  inset: 0;
  z-index: 2000;
  background: rgb(var(--v-theme-surface));
  animation: reader-in 0.28s cubic-bezier(0.32, 0.72, 0, 1);
}
.reader-overlay.reader-leaving {
  animation: reader-out 0.21s cubic-bezier(0.32, 0.72, 0, 1) forwards;
}
.reader-overlay.reader-noanim {
  animation: none;
}
@keyframes reader-in {
  from {
    transform: translateX(100%);
  }
  to {
    transform: translateX(0);
  }
}
@keyframes reader-out {
  from {
    transform: translateX(0);
  }
  to {
    transform: translateX(100%);
  }
}
@media (prefers-reduced-motion: reduce) {
  .reader-overlay,
  .reader-body-block,
  .reader-bar-title,
  .compose-root,
  .compose-root-mobile {
    animation: none !important;
    transition: none !important;
  }
}
.v-navigation-drawer__content {
  overflow-anchor: none;
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
.reader-body-block {
  animation: body-in 0.18s ease-out;
}
@keyframes body-in {
  from {
    opacity: 0;
  }
  to {
    opacity: 1;
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
.reader-bar-title {
  font-size: 15px;
  font-weight: 500;
  min-width: 0;
  margin-left: 2px;
  animation: bar-title-in 0.18s ease-out;
}
.reader-toolbar .reader-bar-title {
  margin-left: 8px;
}
@keyframes bar-title-in {
  from {
    opacity: 0;
    transform: translateY(4px);
  }
  to {
    opacity: 1;
  }
}
.reader-scroll {
  flex: 1 1 auto;
  min-height: 0;
  overflow-anchor: none;
}
.reader-vp {
  width: 100%;
  height: 100%;
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
  -webkit-touch-callout: none;
  user-select: none;
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
  background: rgba(var(--v-theme-primary), var(--v-activated-opacity));
}
.mail-row-checked {
  background: rgba(var(--v-theme-primary), var(--v-activated-opacity));
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
.font-mono {
  font-family: ui-monospace, monospace;
}
</style>
