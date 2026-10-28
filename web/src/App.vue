<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useDisplay, useLocale, useTheme } from 'vuetify'
import { intlLocale, lang, t } from './i18n'
import Auth from './components/Auth.vue'
import Accounts from './components/Accounts.vue'
import Compose from './components/Compose.vue'
import RawError from './components/RawError.vue'
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
watch(lang, (v) => (vuetifyLocale.current.value = v === 'zh-Hant' ? 'zhHant' : v === 'zh-Hans' ? 'zhHans' : 'en'), { immediate: true })
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
const baseRows = ref([])
const listStatus = ref('loading')
const listError = ref('')
const listSnap = ref({ kind: 'folder', scope: 'inbox' })
const listBusy = ref(false)
let loadToken = 0
let inputSeq = 0
const journal = []
const memberFlags = new Map()
const latestFlags = new Map()
const countedMembers = new Set()
let appendReplay = false
const memberThread = new Map()
const intents = ref(new Map())
const restoreIntentByOp = new Map()
const undoKeyWaiters = new Set()

function finishUndoRestore(restore) {
  if ((restore.undoWaiters ?? 0) > 0) return
  loadFolders()
  const failures = restore.undoFailures ?? []
  if (failures.length) {
    failRestoreItems(
      restore,
      failures.map((f) => f.it),
    )
    actionError.value = t(failures[0].err.message)
  }
  settleRestoreIntent(restore)
}

async function lookupUndoOp(w) {
  const res = await api.opsByKeys(w.keys).catch(() => null)
  if (!res) return 0
  const exact = res.find((r) => r.client_key.endsWith(':' + w.it.id))
  const bare = w.bareOk ? res.find((r) => w.entryKeys.includes(r.client_key)) : undefined
  const r = exact ?? bare
  if (!r) return 0
  if (r.status === 'undone' || r.status === 'failed') return 'closed'
  return r.op_id
}

async function undoWaiterViaOp(w, opId) {
  restoreIntentByOp.set(opId, w.restore.id)
  try {
    await api.undoOp(opId)
  } catch (err) {
    ;(w.restore.undoFailures ??= []).push({ it: w.it, err })
  }
}

async function parkUndoItems(restore, items, u, bareOk) {
  for (const it of items) {
    const w = {
      restore,
      it,
      keys: (u.obKeys ?? []).flatMap((k) => [k + ':' + it.id, k]),
      entryKeys: [...(u.obKeys ?? [])],
      bareOk,
    }
    const found = await lookupUndoOp(w)
    if (found === 'closed') continue
    if (found) {
      await undoWaiterViaOp(w, found)
      continue
    }
    if (!w.entryKeys.some((k) => obLive.has(k) || obList.value.some((e) => e.key === k))) continue
    undoKeyWaiters.add(w)
    restore.undoWaiters = (restore.undoWaiters ?? 0) + 1
  }
}

let undoKeyFlushRunning = false
async function flushUndoKeyWaiters() {
  if (undoKeyFlushRunning || !undoKeyWaiters.size) return
  undoKeyFlushRunning = true
  try {
    for (const w of [...undoKeyWaiters]) {
      if (!intents.value.has(w.restore.id)) {
        undoKeyWaiters.delete(w)
        w.restore.undoWaiters--
        finishUndoRestore(w.restore)
        continue
      }
      const found = await lookupUndoOp(w)
      if (found === 'closed') {
        undoKeyWaiters.delete(w)
        w.restore.undoWaiters--
        finishUndoRestore(w.restore)
      } else if (found) {
        undoKeyWaiters.delete(w)
        w.restore.undoWaiters--
        await undoWaiterViaOp(w, found)
        finishUndoRestore(w.restore)
      } else if (!w.entryKeys.some((k) => obLive.has(k) || obList.value.some((e) => e.key === k))) {
        undoKeyWaiters.delete(w)
        w.restore.undoWaiters--
        finishUndoRestore(w.restore)
      }
    }
  } catch {
  }
  undoKeyFlushRunning = false
}
let intentSeq = 0

function snapCtxKey(snap) {
  return snap.kind === 'search'
    ? 'search|' + snap.query
    : 'folder|' + snap.scope + '|' + (snap.folderId ?? 0)
}

const byDateDesc = (a, b) => b.date_ts - a.date_ts || b.id - a.id

function journalRows(rows) {
  for (const r of rows ?? []) {
    if (!r || r.id == null) continue
    if (r.flags != null) memberFlags.set(r.id, r.flags)
    if (r.thread_key) memberThread.set(r.id, { accountId: r.account_id, threadKey: r.thread_key })
  }
}

function journalDelta(d) {
  d.seq = ++inputSeq
  journal.push(d)
}

function touchIntents() {
  intents.value = new Map(intents.value)
}

function registerHideIntent({ ids, rows, exiting = [], thread = null }) {
  const proj = messages.value
  const exitIndex = new Map()
  for (const id of exiting) {
    const i = proj.findIndex((r) => r.id === id)
    if (i >= 0) exitIndex.set(id, i)
  }
  const intent = {
    id: ++intentSeq,
    kind: 'hide',
    ids: new Set(ids),
    rows: new Map((rows ?? []).map((r) => [r.id, r])),
    exiting: new Set(exiting),
    exitIndex,
    thread,
    status: 'live',
    confirmSeq: 0,
    superseded: false,
    opIds: new Set(),
  }
  intents.value.set(intent.id, intent)
  touchIntents()
  return intent
}

function confirmIntent(intent) {
  if (!intents.value.has(intent.id)) return
  intent.status = 'confirmed'
  intent.confirmSeq = ++inputSeq
  touchIntents()
}

function noteIntentOps(intent, items) {
  for (const it of items ?? []) if (it && it.op_id) intent.opIds.add(it.op_id)
  void flushUndoKeyWaiters()
}

function exitDoneIntent(intent, id) {
  if (!intents.value.has(intent.id)) return
  intent.exiting.delete(id)
  touchIntents()
}

function failIntent(intent, failedIds) {
  if (!intents.value.has(intent.id)) return
  const back = []
  for (const id of failedIds) {
    intent.ids.delete(id)
    intent.exiting.delete(id)
    const row = intent.rows.get(id)
    if (row && !baseRows.value.some((r) => r.id === id) && rowBelongsToCurrentView(row)) back.push(row)
  }
  if (back.length) {
    baseRows.value = [...baseRows.value, ...back].sort(byDateDesc)
    rebuildList()
  }
  if (!intent.ids.size) intents.value.delete(intent.id)
  touchIntents()
}

function registerRestoreIntent(items, hideIntents) {
  const intent = {
    id: ++intentSeq,
    kind: 'restore',
    ids: new Set(),
    rows: new Map(),
    exiting: new Set(),
    status: 'live',
    confirmSeq: 0,
    hideIntents: hideIntents ?? [],
  }
  for (const it of items ?? []) {
    if (!it || it.id == null) continue
    intent.ids.add(it.id)
    if (it.row) intent.rows.set(it.id, it.row)
  }
  for (const h of intent.hideIntents) h.superseded = true
  intents.value.set(intent.id, intent)
  touchIntents()
  return intent
}

function forgetRestoreIntent(intent) {
  for (const [opID, intentID] of restoreIntentByOp) {
    if (intentID === intent.id) restoreIntentByOp.delete(opID)
  }
}

function failRestoreIntent(intent) {
  if (!intents.value.has(intent.id)) return
  for (const h of intent.hideIntents) h.superseded = false
  intents.value.delete(intent.id)
  forgetRestoreIntent(intent)
  touchIntents()
}

function failRestoreItems(intent, failedItems) {
  if (!intents.value.has(intent.id)) return
  const failedIds = new Set(failedItems.map((it) => it.id))
  for (const id of failedIds) {
    intent.ids.delete(id)
    intent.rows.delete(id)
  }
  for (const h of intent.hideIntents) {
    if (![...h.ids].some((id) => failedIds.has(id))) continue
    for (const id of [...h.ids]) {
      if (!failedIds.has(id)) h.ids.delete(id)
    }
    h.superseded = false
  }
  if (!intent.ids.size) {
    intents.value.delete(intent.id)
    forgetRestoreIntent(intent)
  }
  touchIntents()
}

function settleRestoreIntent(intent) {
  if (!intents.value.has(intent.id)) return
  const have = new Set(baseRows.value.map((r) => r.id))
  if (![...intent.ids].every((id) => have.has(id) || !intent.rows.has(id))) return
  for (const h of intent.hideIntents) intents.value.delete(h.id)
  intents.value.delete(intent.id)
  forgetRestoreIntent(intent)
  touchIntents()
}

function registerFlagsIntent(id, flagsVal) {
  const intent = {
    id: ++intentSeq,
    kind: 'flags',
    ids: new Set([id]),
    rows: new Map(),
    exiting: new Set(),
    flagsVal,
    status: 'live',
    confirmSeq: 0,
  }
  intents.value.set(intent.id, intent)
  touchIntents()
  return intent
}

function dropIntent(intent) {
  if (!intent || !intents.value.has(intent.id)) return
  intents.value.delete(intent.id)
  touchIntents()
}

const OB_STORE_KEY = 'namakani_outbox_v1'
let obList = []
const obLive = new Map()
api.setFlushHook(() => {
  if (view.value === 'app') obFlush()
})
const queueTick = ref(0)
let obFlushRunning = false

function obLoad() {
  try {
    const v = JSON.parse(localStorage.getItem(OB_STORE_KEY) || '[]')
    obList = Array.isArray(v) ? v : []
  } catch {
    obList = []
  }
}

function obSave() {
  localStorage.setItem(OB_STORE_KEY, JSON.stringify(obList))
}

function obAdd(entry) {
  obList.push(entry)
  try {
    obSave()
  } catch (e) {
    obList.pop()
    throw e
  }
  queueTick.value++
}

function obDrop(key) {
  const i = obList.findIndex((e) => e.key === key)
  if (i >= 0) {
    obList.splice(i, 1)
    try {
      obSave()
    } catch {}
  }
  queueTick.value++
}

function obIsRefusal(err) {
  const s = err?.status ?? 0
  return s >= 400 && s < 500 && s !== 401
}

function obReqKey(entry, rq) {
  if (rq.op === 'batchflags' || entry.reqs.length === 1) return entry.key
  return entry.key + ':' + rq.id
}

async function obRunRequests(entry) {
  const items = []
  const okIds = new Set()
  let firstRefusal = null
  const refusedIds = []
  for (const rq of entry.reqs) {
    const key = obReqKey(entry, rq)
    let res
    try {
      if (rq.op === 'move') res = await api.moveMessage(rq.id, rq.to, key)
      else if (rq.op === 'restore') res = await api.restoreMessage(rq.id, key)
      else if (rq.op === 'delete') res = await api.deleteMessage(rq.id, key)
      else if (rq.op === 'expunge') res = await api.expungeMessage(rq.id, key)
      else if (rq.op === 'flags') res = await api.setFlags(rq.id, { seen: rq.seen, client_key: key })
      else if (rq.op === 'batchflags') res = await api.batchSetFlags(rq.ids, { seen: rq.seen, client_key: key })
      else throw new Error('unknown outbox op ' + rq.op)
    } catch (err) {
      if (obIsRefusal(err)) {
        if (!firstRefusal) firstRefusal = err
        if (rq.op === 'batchflags') refusedIds.push(...rq.ids)
        else refusedIds.push(rq.id)
        continue
      }
      return { okIds, items, held: err }
    }
    if (rq.op === 'batchflags') {
      const failed = res?.failed || []
      const failedSet = new Set(failed.map((f) => f.id))
      for (const it of res?.items || []) {
        if (failedSet.has(it.id)) continue
        okIds.add(it.id)
        items.push({ id: it.id, op_id: it.op_id, flags: it.flags })
      }
      if (failed.length) {
        const err = new Error(failed[0].error || '标记失败')
        err.status = 400
        return { okIds, items, refusal: err, failedIds: failed.map((f) => f.id) }
      }
    } else if (rq.op === 'flags') {
      okIds.add(rq.id)
      items.push({ id: rq.id, op_id: res?.op_id, flags: res?.flags })
    } else {
      okIds.add(rq.id)
      items.push({ id: rq.id, op_id: res?.op_id })
    }
  }
  if (firstRefusal) return { okIds, items, refusal: firstRefusal, failedIds: refusedIds }
  return { okIds, items, refusal: null, held: null }
}

function obDrive(entry, hooks) {
  let ctrl = obLive.get(entry.key)
  if (!ctrl) {
    ctrl = {
      entry,
      hooks: hooks || {},
      state: 'new',
      voided: false,
      driving: false,
      stepPromise: null,
      resolveUndo: null,
      undoPromise: null,
    }
    ctrl.undoPromise = new Promise((r) => {
      ctrl.resolveUndo = r
    })
    obLive.set(entry.key, ctrl)
  } else if (hooks) {
    ctrl.hooks = hooks
  }
  if (!ctrl.driving && !ctrl.voided && ctrl.state !== 'done' && ctrl.state !== 'refused') obStep(ctrl)
  return ctrl
}

function obStep(ctrl) {
  if (ctrl.driving || ctrl.voided) return ctrl.stepPromise
  ctrl.driving = true
  ctrl.state = 'sending'
  queueTick.value++
  ctrl.stepPromise = (async () => {
    let out
    try {
      out = await obRunRequests(ctrl.entry)
    } catch (err) {
      out = { okIds: new Set(), items: [], held: err }
    }
    ctrl.driving = false
    if (ctrl.voided) {
      ctrl.state = 'void'
      queueTick.value++
      if (!out.held && !out.refusal) {
        for (const it of out.items) {
          if (it.op_id) api.undoOp(it.op_id).catch(() => {})
        }
      }
      ctrl.resolveUndo([])
      return
    }
    if (out.held) {
      ctrl.state = 'held'
      queueTick.value++
      ctrl.hooks.onHeld?.(out.held)
      return
    }
    obDrop(ctrl.entry.key)
    obLive.delete(ctrl.entry.key)
    ctrl.state = out.refusal ? 'refused' : 'done'
    queueTick.value++
    if (out.refusal) ctrl.hooks.onRefused?.(out)
    else ctrl.hooks.onOk?.(out)
    ctrl.resolveUndo(out.items)
  })()
  return ctrl.stepPromise
}

async function obFlush() {
  if (obFlushRunning) return
  obFlushRunning = true
  try {
    for (const entry of [...obList]) {
      const ctrl = obLive.get(entry.key)
      if (!ctrl || ctrl.voided || ctrl.state === 'done' || ctrl.state === 'refused') continue
      await obStep(ctrl)
      if (ctrl.state === 'held') break
    }
  } finally {
    obFlushRunning = false
  }
}

function obVoid(key) {
  const ctrl = obLive.get(key)
  obDrop(key)
  if (!ctrl) return
  ctrl.voided = true
  queueTick.value++
  if (!ctrl.driving) {
    ctrl.state = 'void'
    ctrl.resolveUndo([])
    obLive.delete(key)
  }
}

const resumeSend = ref(null)
let resumeSendSeq = 0
const trackedSend = ref(null)

async function resumeSendCountdowns() {
  let pend
  try {
    pend = await api.listPending()
  } catch {
    return
  }
  const cands = (pend || []).filter(
    (p) => p.type === 'send' && !p.sent && !p.sealed && p.not_before - p.created_at <= 120,
  )
  if (!cands.length) return
  const p = cands[cands.length - 1]
  resumeSend.value = { op_id: p.op_id, not_before: p.not_before, seq: ++resumeSendSeq }
  trackedSend.value = { op_id: p.op_id, not_before: p.not_before }
}

async function rebuildSendState() {
  await resumeSendCountdowns()
  const tracked = trackedSend.value
  if (!tracked) return
  let pend
  try {
    pend = await api.listPending()
  } catch {
    return
  }
  if ((pend || []).some((p) => p.op_id === tracked.op_id)) return
  let st
  try {
    st = await api.getOp(tracked.op_id)
  } catch {
    return
  }
  if (!st || st.type !== 'send') return
  if (st.status === 'pending' || st.status === 'in_flight') return
  trackedSend.value = null
  lastOpEvent.value = {
    op_id: tracked.op_id,
    status: st.status,
    sent: !!st.sent,
    undo_ignored: false,
    undo_failed: false,
    seq: ++opEventSeq,
  }
}

function obRecover() {
  obLoad()
  for (const entry of [...obList]) {
    if (obLive.has(entry.key)) continue
    if (entry.kind === 'flags' || entry.kind === 'batchflags') {
      obDrive(entry, {
        onOk: (out) => {
          if (out.items.length) applyFlagsInput(out.items)
          loadFolders()
        },
        onRefused: () => {
          loadMessages(true)
          loadFolders()
        },
      })
      continue
    }
    const rows = entry.rows || []
    if (!rows.length) {
      obDrop(entry.key)
      continue
    }
    const intent = registerHideIntent({ ids: rows.map((r) => r.id), rows, exiting: [] })
    const ctrl = obDrive(entry, {
      onOk: (out) => {
        noteIntentOps(intent, out.items)
        confirmIntent(intent)
      },
      onRefused: (out) => {
        const failedIds = out.failedIds && out.failedIds.length ? out.failedIds : rows.map((r) => r.id)
        failIntent(intent, failedIds)
        if (entry.destLabel) showRemovalFailure(entry.destLabel, failedIds.length)
      },
    })
    if (entry.undoItems && entry.undoItems.length) {
      const items = entry.undoItems.map((x) => {
        const row = rows.find((r) => r.id === x.id)
        return row ? { ...x, row } : x
      })
      const dest = entry.destLabel || { verb: 'move', folder_id: 0, special: '', name: '', mixed: true, key: 'recover' }
      showCountToast(dest, items.length, {
        items,
        settled: ctrl.undoPromise,
        intentIds: [intent.id],
        obKeys: [entry.key],
      })
    }
  }
}

function withFlags(row, nf) {
  if (row.flags === nf) return row
  const out = { ...row, flags: nf }
  if ((row.thread_count ?? 1) > 1 && row.thread_unread != null && ((row.flags & 1) !== (nf & 1))) {
    out.thread_unread = Math.max(0, row.thread_unread + ((nf & 1) ? -1 : 1))
  }
  return out
}

const messages = computed(() => {
  const reg = intents.value
  let rows = baseRows.value
  if (!reg.size) return rows
  const hide = new Set()
  const exitingExtra = []
  const overlay = new Map()
  const restores = []
  const present = new Set(rows.map((r) => r.id))
  for (const it of reg.values()) {
    if (it.kind === 'hide') {
      if (it.superseded) continue
      for (const id of it.ids) {
        if (it.exiting.has(id)) {
          if (!present.has(id) && it.rows.has(id)) {
            exitingExtra.push({ index: it.exitIndex.get(id) ?? 0, row: it.rows.get(id) })
          }
        } else {
          hide.add(id)
        }
      }
    } else if (it.kind === 'flags') {
      for (const id of it.ids) overlay.set(id, it.flagsVal)
    } else if (it.kind === 'restore') {
      for (const [id, row] of it.rows) {
        if (!present.has(id)) restores.push(row)
      }
    }
  }
  let out = rows
  if (hide.size) out = out.filter((r) => !hide.has(r.id))
  if (!searching.value) {
    out = out.filter((r) => {
      const cov = threadHideIntent(r)
      return !cov || cov.exiting.has(r.id)
    })
  }
  if (overlay.size) out = out.map((r) => (overlay.has(r.id) ? withFlags(r, overlay.get(r.id)) : r))
  if (exitingExtra.length) {
    out = [...out]
    for (const e of exitingExtra.sort((a, b) => a.index - b.index)) {
      out.splice(Math.min(e.index, out.length), 0, e.row)
    }
  }
  const addRestores = restores.filter((r) => rowBelongsToCurrentView(r))
  if (addRestores.length) out = [...out, ...addRestores].sort(byDateDesc)
  return out
})

function rowsApplyFlags(rows, items) {
  const byId = new Map((items ?? []).map((it) => [it.id, it.flags]))
  if (!byId.size) return rows
  let out = rows.map((r) => (byId.has(r.id) ? withFlags(r, byId.get(r.id)) : r))
  const repDelta = new Map()
  for (const it of items ?? []) {
    if (out.some((r) => r.id === it.id)) continue
    const prior = memberFlags.get(it.id)
    const th = memberThread.get(it.id)
    if (prior === undefined || !th || ((prior & 1) === (it.flags & 1))) continue
    const rep = out.find(
      (r) => r.account_id === th.accountId && (r.thread_key || 'id:' + r.id) === th.threadKey,
    )
    if (rep && rep.thread_unread != null) {
      repDelta.set(rep.id, (repDelta.get(rep.id) ?? 0) + ((it.flags & 1) ? -1 : 1))
    }
  }
  if (repDelta.size) {
    out = out.map((r) =>
      repDelta.has(r.id)
        ? { ...r, thread_unread: Math.max(0, (r.thread_unread ?? 0) + repDelta.get(r.id)) }
        : r,
    )
  }
  return out
}

function deltaInCtx(d, snap) {
  if (snap.kind === 'search') return d.type === 'flags'
  if (d.type === 'msg') return true
  if (d.type === 'removed') {
    if (snap.folderId) return d.ev.folder_id === snap.folderId
    const f = folders.value.find((x) => x.id === d.ev.folder_id)
    return f ? f.special_use === snap.scope : false
  }
  return true
}

function snapBelongs(snap) {
  if (snap.kind === 'search') return () => false
  if (snap.folderId) return (item) => item.folder_id === snap.folderId
  return (item) => item.folder_special === snap.scope
}

function applySnapshot(batch, snap, startSeq, append = false, wm = null) {
  let rows
  if (append) {
    rows = [...baseRows.value]
    const seen = new Set(rows.map((r) => r.id))
    for (const r of batch) {
      if (!seen.has(r.id)) {
        rows.push(r)
        seen.add(r.id)
      }
    }
  } else {
    rows = [...batch]
  }
  const groupedSnap = snap.kind === 'folder'
  const prevAppendReplay = appendReplay
  appendReplay = append
  const placed = !append && wm != null
  for (const d of journal) {
    if (!deltaInCtx(d, snap)) continue
    if (d.type === 'msg') {
      if (!placed && d.seq <= startSeq) continue
      const items = placed ? d.items.filter((it) => (it.pv ?? 0) > wm) : d.items
      for (const item of items) {
        rows = groupedSnap
          ? rowsInsertThreaded(rows, item, snapBelongs(snap))
          : rowsInsertFlat(rows, item, snapBelongs(snap))
      }
    } else if (d.type === 'removed') {
      if (placed) {
        const ids = (d.ev.ids ?? []).filter((id) => (d.ev.pvs?.[id] ?? 0) > wm)
        if (ids.length) rows = rowsApplyRemoved(rows, { ...d.ev, ids }).rows
      } else {
        if (d.seq <= startSeq) continue
        rows = rowsApplyRemoved(rows, d.ev).rows
      }
    } else if (d.type === 'flags') {
      if (d.seq <= startSeq) continue
      rows = rowsApplyFlags(rows, d.items)
      for (const it of d.items) memberFlags.set(it.id, it.flags)
    }
  }
  appendReplay = prevAppendReplay
  journal.length = 0
  journalRows(rows)
  baseRows.value = rows
  snapEpoch = listEpoch
  const have = new Set(rows.map((r) => r.id))
  const batchById = new Map(batch.map((r) => [r.id, r]))
  const belongs = snapBelongs(snap)
  for (const [key, it] of [...intents.value]) {
    if (!append && it.kind === 'hide' && it.status === 'confirmed' && !it.superseded && !it.exiting.size && it.confirmSeq <= startSeq) {
      const proven = [...it.rows.values()].every((r) => {
        const b = batchById.get(r.id)
        if (b) return b.folder_id !== r.folder_id
        return belongs(r)
      })
      if (proven) intents.value.delete(key)
    } else if (it.kind === 'restore' && [...it.ids].every((id) => have.has(id) || !it.rows.has(id))) {
      intents.value.delete(key)
      for (const h of it.hideIntents ?? []) intents.value.delete(h.id)
    }
  }
  touchIntents()
}

const scopeCache = new Map()
let listEpoch = 0
let snapEpoch = 0
function listCacheKey(snap) {
  return snapCtxKey(snap)
}
function stashList() {
  const snap = listSnap.value
  if (snap.kind !== 'folder' || listStatus.value !== 'ready') return
  scopeCache.set(listCacheKey(snap), {
    messages: baseRows.value,
    offset: listOffset,
    hasMore: listHasMore.value,
    scroll: scrollEl(listEl.value)?.scrollTop ?? 0,
    epoch: snapEpoch,
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
const attentionCount = ref(0)
const lastOpEvent = ref(null)
let opEventSeq = 0
const syncOpen = ref(false)
const attentionItems = ref(null)
const attentionError = ref('')
const pendingItems = ref(null)
const pendingError = ref('')
async function loadAttention() {
  attentionError.value = ''
  try {
    attentionItems.value = await api.listAttention()
    attentionCount.value = attentionItems.value.length
  } catch (e) {
    attentionItems.value = null
    attentionError.value = t(e.message)
  }
}
async function loadPending() {
  pendingError.value = ''
  try {
    pendingItems.value = await api.listPending()
  } catch (e) {
    pendingItems.value = null
    pendingError.value = t(e.message)
  }
}
function loadSyncPanel() {
  loadAttention()
  loadPending()
}
function refreshSyncSources() {
  loadPending()
  if (syncOpen.value) loadAttention()
}
function openSyncPanel() {
  attentionItems.value = null
  pendingItems.value = null
  syncOpen.value = true
  loadSyncPanel()
}
async function retryAttention(it) {
  try {
    await api.retryOp(it.op_id)
    showToast(t('已重新排队，稍后自动执行'))
    loadSyncPanel()
  } catch (e) {
    await loadAttention()
    if (!attentionItems.value || attentionItems.value.some((x) => x.op_id === it.op_id)) {
      actionError.value = t(e.message)
    }
  }
}
async function discardAttention(it) {
  try {
    await api.discardOp(it.op_id)
    showToast(t('已丢弃这项操作'))
    loadSyncPanel()
    loadFolders()
  } catch (e) {
    await loadAttention()
    if (!attentionItems.value || attentionItems.value.some((x) => x.op_id === it.op_id)) {
      actionError.value = t(e.message)
    }
  }
}
async function undoPendingSend(row) {
  try {
    await api.undoOp(row.op_id)
    showToast(t('已撤销定时发送'))
    loadSyncPanel()
  } catch (e) {
    await loadPending()
    if (!pendingItems.value || pendingItems.value.some((x) => x.op_id === row.op_id)) {
      actionError.value = t(e.message)
    }
  }
}
const syncStage1Rows = computed(() => {
  void queueTick.value
  const serverKeys = new Set()
  for (const p of pendingItems.value ?? []) {
    if (p.client_key) serverKeys.add(p.client_key)
  }
  const rows = []
  for (const entry of obList) {
    const keys = entry.reqs.map((rq) => obReqKey(entry, rq))
    if (keys.length && keys.every((k) => serverKeys.has(k))) continue
    const first = entry.rows && entry.rows[0]
    rows.push({
      rowKey: 'ob:' + entry.key,
      kind: entry.kind,
      subject: first ? first.subject || '' : '',
      count: entry.rows ? entry.rows.length : 0,
      accountLabel: first ? first.account_label || '' : '',
      destLabel: entry.destLabel || null,
    })
  }
  return rows
})
const syncQueueRows = computed(() => {
  const now = Math.floor(Date.now() / 1000)
  const rows = syncStage1Rows.value.map((r) => ({ ...r, stage: 1 }))
  for (const p of pendingItems.value ?? []) {
    rows.push({
      rowKey: 'op:' + p.op_id,
      op_id: p.op_id,
      stage: 2,
      type: p.type,
      status: p.status,
      subject: p.subject || '',
      to: p.to || '',
      accountLabel: p.account_label || '',
      notBefore: p.not_before || 0,
      waiting: (p.next_attempt || 0) > now,
      scheduled: p.type === 'send' && (p.not_before || 0) > now,
    })
  }
  return rows
})
const syncInflightCount = computed(() => syncQueueRows.value.length)
function syncRowTitle(row) {
  if (row.stage === 1) {
    if (row.subject) return row.subject
    if (row.kind === 'flags' || row.kind === 'batchflags') return t('标记邮件')
    return t('移动邮件')
  }
  if (row.type === 'send') return row.subject || t('发送邮件')
  if (row.type === 'move') return row.subject || t('移动邮件')
  if (row.type === 'expunge') return row.subject || t('彻底删除邮件')
  return row.subject || t('标记邮件')
}
function syncRowSubtitle(row) {
  const parts = []
  if (row.accountLabel) parts.push(row.accountLabel)
  if (row.stage === 1) parts.push(t('待发'))
  else if (row.scheduled) parts.push(t('定时发送'))
  else parts.push(t('已提交，等待上游执行'))
  if (row.waiting) parts.push(t('等待重试'))
  return parts.join('，')
}
function attentionTitle(it) {
  if (it.type === 'send') return t('发送邮件失败')
  if (it.type === 'move') return t('移动邮件失败')
  if (it.type === 'expunge') return t('彻底删除邮件失败')
  return t('标记邮件失败')
}
function attentionDetail(it) {
  const parts = []
  if (it.subject) parts.push(it.subject)
  if (it.to) parts.push(t('收件人：{to}', { to: it.to }))
  if (it.account_label) parts.push(it.account_label)
  return parts.join('，')
}
const purging = ref(false)
async function doPurge() {
  const f = activeFolder.value
  if (!f || purging.value) return
  purging.value = true
  baseRows.value = baseRows.value.filter((r) => r.folder_id !== f.id)
  folders.value = folders.value.map((x) => (x.id === f.id ? { ...x, unread: 0 } : x))
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
const navHidden = ref(localStorage.getItem('namakani_nav_hidden') === '1')
const wideDesktop = computed(() => display.width.value >= 1440)
const rangeId = ref(parseInt(localStorage.getItem('namakani_range') ?? '0', 10) || 0)
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
  localStorage.setItem('namakani_range', String(id))
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
const undoSeconds = ref(5)
const maxAttachmentMB = ref(20)
const convo = ref(null)
const convoOpenIds = ref(new Set())
const convoDetails = ref({})
const convoTr = ref({})
const cursorIdx = ref(0)
const cursorActive = ref(false)
const helpOpen = ref(false)
const listWidth = ref(parseInt(localStorage.getItem('namakani_list_width') ?? '0', 10) || 0)
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
  if (listWidth.value) localStorage.setItem('namakani_list_width', String(Math.round(listWidth.value)))
  listDrag = null
}
const listDensity = ref(localStorage.getItem('namakani_list_density') === 'compact' ? 'compact' : 'standard')
function setDensity(v) {
  listDensity.value = v
  localStorage.setItem('namakani_list_density', v)
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
  if (!selectionActive.value) return
  const ids = [...selectedIds.value]
  const removable = kind === 'archive' || kind === 'delete'
  const rows = removable ? displayMessages.value.filter((m) => selectedIds.value.has(m.id)) : []
  const openRemoved = !!(selected.value && ids.includes(selected.value.id))
  let opIds = ids
  const needsExpansion =
    removable && !searching.value && rows.some((x) => x.thread_count > 1)
  const bulkDest = !removable
    ? null
    : listSnap.value.scope === 'scheduled'
      ? kind === 'delete'
        ? schedDeleteDest
        : cancelDest
      : kind === 'delete'
        ? specialDest('delete', rows[0]?.account_id ?? selected.value?.account_id ?? 0, 'trash')
        : bulkArchiveDest(rows)
  const rowById = new Map(rows.map((x) => [x.id, x]))
  const gestureById = new Map(rows.map((x) => [x.id, threadGestureFor(kind, x.folder_special)]))
  const enqueuedCount = () => {
    if (listSnap.value.scope === 'scheduled') return opIds.length
    return opIds.reduce((n, id) => {
      const mem = rowById.get(id)
      return n + (mem && threadGestureAction(gestureById.get(id), mem) ? 1 : 0)
    }, 0)
  }
  const bulkCount0 = enqueuedCount()
  let work = null
  const movedItems = []
  const failedIds = []
  let firstErr = null
  let settleFull
  const settledFullP = new Promise((r) => { settleFull = r })
  const itemById = new Map()
  const gestureKey = api.newClientKey()
  let intent = null
  let sendGateResolve = null
  let sendPending = null
  if (removable) {
    const undoItems = rows
      .filter((x) => {
        if (!(x.id > 0 && x.folder_special !== 'scheduled' && (x.folder_id || x.folder_special))) return false
        const a = threadGestureAction(threadGestureFor(kind, x.folder_special), x)
        return !!a && a !== 'expunge'
      })
      .map((x) => ({ id: x.id, special: x.folder_special, folder_id: x.folder_id, row: x }))
    for (const it of undoItems) itemById.set(it.id, it)
    intent = registerHideIntent({
      ids: ids.filter((id) => id > 0),
      rows: rows.filter((x) => x.id > 0),
      exiting: rows.map((x) => x.id),
    })
    const sendGate = new Promise((r) => { sendGateResolve = r })
    sendPending = { flush: () => sendGateResolve() }
    swipePendingSends.add(sendPending)
    const deferredToastFns = []
    let toastLive = false
    let toastDead = false
    const deferOrRun = (fn) => {
      if (toastLive) fn()
      else deferredToastFns.push(fn)
    }
    void sendGate.then(() => {
      toastLive = true
      if (toastDead) return
      for (const fn of deferredToastFns.splice(0)) fn()
    })
    work = (async () => {
      if (needsExpansion) {
        const per = await Promise.all(
          rows.map(async (x) => {
            const gesture = threadGestureFor(kind, x.folder_special)
            if (!(x.thread_count > 1)) return [{ row: x, gesture }]
            const all = await api.getThreadMessagesAll(x.account_id, x.thread_key || 'id:' + x.id)
            return (all.length ? all : [x]).map((y) => ({ row: y, gesture }))
          }),
        )
        const expanded = per.flat()
        journalRows(expanded.map((e) => e.row))
        for (const e of expanded) {
          rowById.set(e.row.id, e.row)
          gestureById.set(e.row.id, e.gesture)
        }
        opIds = [...new Set(expanded.map((e) => e.row.id))]
        const extraItems = expanded
          .filter((e) => {
            const x = e.row
            if (!(x.id > 0 && x.folder_special !== 'scheduled' && (x.folder_id || x.folder_special))) return false
            const a = threadGestureAction(e.gesture, x)
            return !!a && a !== 'expunge'
          })
          .map((e) => ({ id: e.row.id, special: e.row.folder_special, folder_id: e.row.folder_id, row: e.row }))
        for (const it of extraItems) itemById.set(it.id, it)
        for (const e of expanded) {
          intent.ids.add(e.row.id)
          if (!intent.rows.has(e.row.id)) intent.rows.set(e.row.id, e.row)
        }
        touchIntents()
        const have = new Set(rows.map((x) => x.id))
        const restItems = extraItems.filter((x) => !have.has(x.id))
        deferOrRun(() => {
          if ((enqueuedCount() !== bulkCount0 || restItems.length) && toastShow.value && Date.now() < toastHideAt) {
            showCountToast(
              bulkDest,
              enqueuedCount() - bulkCount0,
              restItems.length ? { items: restItems, settled: settledMoved, intentIds: [intent.id] } : null,
            )
          }
        })
      }
      await sendGate
      swipePendingSends.delete(sendPending)
      if (listSnap.value.scope === 'scheduled' && (kind === 'delete' || kind === 'archive')) {
        for (const id of opIds) {
          try {
            if (kind === 'delete') await api.deleteScheduled(id)
            else await api.cancelScheduled(id)
            const ui = itemById.get(id)
            if (ui) movedItems.push(ui)
          } catch (e) {
            failedIds.push(id)
            if (!firstErr) firstErr = e
          }
        }
        return
      }
      const reqs = []
      for (const id of opIds) {
        const mem = rowById.get(id)
        const action = mem ? threadGestureAction(gestureById.get(id), mem) : null
        if (!action) continue
        if (action === 'archive') reqs.push({ op: 'move', id, to: 'archive' })
        else if (action === 'inbox') reqs.push({ op: 'move', id, to: 'inbox' })
        else if (action === 'restore') reqs.push({ op: 'restore', id })
        else if (action === 'trashMove') reqs.push({ op: 'move', id, to: 'trash' })
        else if (action === 'delete') reqs.push({ op: 'delete', id })
        else if (action === 'expunge') reqs.push({ op: 'expunge', id })
      }
      const entry = {
        key: gestureKey,
        kind: 'bulk',
        ts: Date.now(),
        reqs,
        rows: [...rowById.values()],
        undoItems: [...itemById.values()].map((x) => ({ id: x.id, special: x.special, folder_id: x.folder_id })),
        destLabel: bulkDest,
      }
      obAdd(entry)
      if (pendingUndo.value && (pendingUndo.value.intentIds ?? []).includes(intent.id)) {
        pendingUndo.value.obKeys = [gestureKey]
      }
      await new Promise((resolve) => {
        const collect = (out) => {
          for (const it of out.items) {
            const ui = itemById.get(it.id)
            if (ui) {
              if (it.op_id) ui.op_id = it.op_id
              movedItems.push(ui)
            }
          }
        }
        obDrive(entry, {
          onOk: (out) => {
            collect(out)
            resolve()
          },
          onRefused: (out) => {
            collect(out)
            for (const id of out.failedIds || []) failedIds.push(id)
            resolve()
          },
        })
      })
    })()
    clearSelectionIds()
    if (openRemoved) clearSelection()
    const settledMoved = settledFullP
    deferOrRun(() =>
      showCountToast(bulkDest, bulkCount0, undoItems.length ? { items: undoItems, settled: settledMoved, intentIds: [intent.id], obKeys: [gestureKey] } : null),
    )
    work.catch(() => { toastDead = true })
  }
  const flagEntries = []
  const rollbackFlagEntries = (entries) => {
    for (const f of entries) if (f.intent) dropIntent(f.intent)
    const flipped = entries.filter((f) => f.row && f.prev !== f.next)
    if (flipped.length) patchAuxFlags(flipped.map((f) => ({ id: f.id, flags: f.prev })))
    for (const f of flipped) bumpFolderUnread(f.row, (f.next & 1) ? 1 : -1)
  }
  let flagIds = ids
  const flagRowById = new Map()
  if (!removable && !searching.value) {
    const reps = displayMessages.value.filter((x) => selectedIds.value.has(x.id) && (x.thread_count ?? 1) > 1)
    if (reps.length) {
      let members
      try {
        members = (await Promise.all(
          reps.map((x) => api.getThreadMessagesAll(x.account_id, x.thread_key || 'id:' + x.id)),
        )).flat()
      } catch (e) {
        actionError.value = t('标记失败：{error}', { error: t(e.message) })
        return
      }
      journalRows(members)
      for (const mem of members) flagRowById.set(mem.id, mem)
      flagIds = [...new Set([...ids, ...members.map((x) => x.id)])]
    }
  }
  if (!removable) {
    const wantSeen = kind === 'seen'
    for (const id of flagIds) {
      const row = flagRowById.get(id) || displayMessages.value.find((x) => x.id === id) || null
      if (!row) {
        flagEntries.push({ id, row: null, intent: null, prev: 0, next: 0 })
        continue
      }
      const prev = row.flags
      const next = wantSeen ? prev | 1 : prev & ~1
      flagEntries.push({ id, row, intent: registerFlagsIntent(id, next), prev, next })
    }
    const flipped = flagEntries.filter((f) => f.row && f.prev !== f.next)
    if (flipped.length) patchAuxFlags(flipped.map((f) => ({ id: f.id, flags: f.next })))
    for (const f of flipped) bumpFolderUnread(f.row, (f.next & 1) ? -1 : 1)
  }
  try {
    if (!work) {
      work = (async () => {
        const entry = {
          key: gestureKey,
          kind: 'batchflags',
          ts: Date.now(),
          reqs: [{ op: 'batchflags', ids: flagIds, seen: kind === 'seen' }],
          rows: [],
          undoItems: null,
          destLabel: null,
        }
        obAdd(entry)
        await new Promise((resolve, reject) => {
          obDrive(entry, {
            onOk: (out) => {
              for (const f of flagEntries) if (f.intent) dropIntent(f.intent)
              if (out.items.length) applyFlagsInput(out.items)
              resolve()
            },
            onRefused: (out) => {
              const failedSet = new Set(out.failedIds || [])
              for (const f of flagEntries) if (f.intent && !failedSet.has(f.id)) dropIntent(f.intent)
              if (out.items.length) applyFlagsInput(out.items)
              if (failedSet.size) {
                rollbackFlagEntries(flagEntries.filter((f) => failedSet.has(f.id)))
                reject(new Error(out.refusal?.message || '标记失败'))
                return
              }
              resolve()
            },
          })
        })
      })()
    }
    if (removable) {
      const foldDone = collapseRows(rows)
      void foldDone.then(
        () => { if (sendGateResolve) sendGateResolve() },
        () => { if (sendGateResolve) sendGateResolve() },
      )
      await Promise.all([foldDone, work.catch((e) => { if (!firstErr) firstErr = e })])
      if (sendPending) swipePendingSends.delete(sendPending)
      for (const id of ids) exitDoneIntent(intent, id)
      for (const id of ids) clearExitRow(id)
      for (const id of ids) exitNatural.delete(id)
      if (firstErr && !movedItems.length && !failedIds.length) {
        for (const id of [...intent.ids]) failedIds.push(id)
      }
      if (failedIds.length) {
        failIntent(intent, failedIds)
        animateRollback(failedIds)
        showRemovalFailure(bulkDest, failedIds.length)
      } else {
        noteIntentOps(intent, movedItems)
        confirmIntent(intent)
      }
    } else {
      await work
      clearSelectionIds()
      if (openRemoved) clearSelection()
    }
    loadFolders()
  } catch (err) {
    for (const id of ids) clearExitRow(id)
    if (removable) {
      showRemovalFailure(bulkDest, Math.max(1, opIds.length - movedItems.length))
    } else {
      actionError.value = t(err.message)
    }
  } finally {
    if (settleFull) settleFull(movedItems)
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
  scrollEl(listEl.value)?.scrollTo({ top: 0 })
  rebuildList()
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
  localStorage.setItem('namakani_theme_mode', v)
  applyTheme()
}

function initTheme() {
  const stored = localStorage.getItem('namakani_theme_mode')
  themeMode.value = ['system', 'light', 'dark'].includes(stored) ? stored : 'system'
  systemDarkMQ = window.matchMedia('(prefers-color-scheme: dark)')
  systemDarkMQ.addEventListener('change', applyTheme)
  applyTheme()
}

function avatarColor(email) {
  return avatarColorFor(email)
}

const avatarKinds = ref({})
const avatarRequested = new Set()
const avatarQueue = new Set()
let avatarFlushTimer = null

function avatarImg(email) {
  const k = (email || '').trim().toLowerCase()
  return k && avatarKinds.value[k] ? api.avatarImgURL(k) : ''
}

function ensureAvatars(emails) {
  for (const e of emails) {
    const k = (e || '').trim().toLowerCase()
    if (!k || avatarRequested.has(k)) continue
    avatarRequested.add(k)
    avatarQueue.add(k)
  }
  if (!avatarQueue.size || avatarFlushTimer) return
  avatarFlushTimer = setTimeout(flushAvatars, 120)
}

async function flushAvatars() {
  avatarFlushTimer = null
  const batch = [...avatarQueue]
  avatarQueue.clear()
  if (!batch.length) return
  try {
    const res = await api.resolveAvatars(batch)
    if (res && res.avatars) avatarKinds.value = { ...avatarKinds.value, ...res.avatars }
  } catch {
  }
}

const trActive = ref(false)
const trBusy = ref(false)
const trData = ref(null)
let trSettingsCache = null

const shownSubject = computed(() =>
  trActive.value && trData.value ? trData.value.subject : selected.value?.subject,
)
const shownHtml = computed(() =>
  trActive.value && trData.value ? trData.value.html : detail.value?.html,
)
const shownText = computed(() =>
  trActive.value && trData.value ? trData.value.text : detail.value?.text,
)

async function effectiveTarget() {
  if (!trSettingsCache) {
    try {
      trSettingsCache = (await api.getTranslateSettings()).settings
    } catch {
      trSettingsCache = null
    }
  }
  const v = trSettingsCache?.target
  return v && v !== 'auto' ? v : lang.value
}

async function toggleTranslate() {
  if (trBusy.value || !selected.value || !detail.value) return
  if (trActive.value) {
    trActive.value = false
    return
  }
  if (trData.value && trData.value.id === selected.value.id) {
    trActive.value = true
    return
  }
  trBusy.value = true
  try {
    const target = await effectiveTarget()
    const res = await api.translateMessage(selected.value.id, target)
    if (res && !res.same_language) {
      trData.value = {
        id: selected.value.id,
        subject: res.subject,
        html: res.html,
        text: res.text,
      }
      trActive.value = true
    }
  } catch {
  }
  trBusy.value = false
}

watch(
  () => selected.value?.id,
  () => {
    trActive.value = false
    trData.value = null
  },
)

async function ensureConvoTranslation(id) {
  if (convoTr.value[id]) return
  convoTr.value = { ...convoTr.value, [id]: { st: 'pending' } }
  try {
    const target = await effectiveTarget()
    const res = await api.translateMessage(id, target)
    if (res && !res.same_language) {
      convoTr.value = { ...convoTr.value, [id]: { st: 'done', subject: res.subject, html: res.html, text: res.text } }
    } else {
      convoTr.value = { ...convoTr.value, [id]: { st: 'same' } }
    }
  } catch {
    convoTr.value = { ...convoTr.value, [id]: { st: 'failed' } }
  }
}

async function toggleCardTranslate(msg) {
  const e = convoTr.value[msg.id]
  if (e && e.st === 'pending') return
  if (e && e.st === 'done' && e.on) {
    convoTr.value = { ...convoTr.value, [msg.id]: { ...e, on: false } }
    return
  }
  if (!e || e.st !== 'done') await ensureConvoTranslation(msg.id)
  const now = convoTr.value[msg.id]
  if (now && now.st === 'done') {
    convoTr.value = { ...convoTr.value, [msg.id]: { ...now, on: true } }
  }
}

function cardTrEntry(id) {
  const e = convoTr.value[id]
  return e && e.st === 'done' && e.on ? e : null
}

watch(
  () => messages.value.map((m) => m.from_email || '').join(','),
  () => ensureAvatars(messages.value.map((m) => m.from_email)),
)

watch(
  () => selected.value?.from_email,
  (e) => {
    if (e) ensureAvatars([e])
  },
)

watch(
  () => (convo.value?.msgs || []).map((m) => m.from_email || '').join(','),
  () => ensureAvatars((convo.value?.msgs || []).map((m) => m.from_email)),
)

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
  const gen = ++loadToken
  const startSeq = inputSeq
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
    journalRows(hit.messages)
    let rows = hit.messages
    if (latestFlags.size) {
      rows = rowsApplyFlags(
        rows,
        [...latestFlags].map(([id, flags]) => ({ id, flags })),
      )
    }
    hit.messages = rows
    baseRows.value = rows
    snapEpoch = hit.epoch
    for (const it of [...intents.value.values()]) {
      if (it.kind === 'restore') settleRestoreIntent(it)
    }
    listOffset = hit.offset
    listHasMore.value = hit.hasMore
    listSnap.value = target
    listStatus.value = 'ready'
    listBusy.value = false
    nextTick(() => {
      const el = scrollEl(listEl.value)
      if (el) el.scrollTop = hit.scroll
    })
    return
  }
  try {
    let batch
    let wm = null
    if (target.kind === 'search') {
      batch = await api.searchMessages(target.query)
    } else if (target.folderId) {
      const r = await api.getFolderMessages(target.folderId, 0, PAGE_SIZE)
      batch = r.items
      wm = r.wm
    } else {
      const r = await api.getMessages(target.scope, 0, PAGE_SIZE)
      batch = r.items
      wm = r.wm
    }
    if (gen !== loadToken) return
    applySnapshot(batch, target, startSeq, false, wm)
    listOffset = batch.length
    listHasMore.value = batch.length === PAGE_SIZE
    listSnap.value = target
    listStatus.value = 'ready'
    listErrorBar.value = ''
  } catch (err) {
    if (gen !== loadToken) return
    if (keepReady) {
      listErrorBar.value = t(err.message)
      listStatus.value = 'ready'
      return
    }
    if (lgAndUp.value && messages.value.length > 0) {
      listErrorBar.value = t(err.message)
      listStatus.value = 'ready'
      return
    }
    baseRows.value = []
    listHasMore.value = false
    listStatus.value = 'error'
    listError.value = t(err.message)
  } finally {
    if (gen === loadToken) listBusy.value = false
  }
}

async function loadMore() {
  if (listStatus.value !== 'ready' || loadingMore.value || !listHasMore.value) return
  loadingMore.value = true
  const gen = loadToken
  const startSeq = inputSeq
  try {
    const batch =
      listSnap.value.kind === 'search'
        ? await api.searchMessages(listSnap.value.query, listOffset, PAGE_SIZE)
        : listSnap.value.folderId
          ? (await api.getFolderMessages(listSnap.value.folderId, listOffset, PAGE_SIZE)).items
          : (await api.getMessages(scope.value, listOffset, PAGE_SIZE)).items
    if (gen !== loadToken) return
    applySnapshot(batch, listSnap.value, startSeq, true)
    listOffset += batch.length
    listHasMore.value = batch.length === PAGE_SIZE
  } catch {
  } finally {
    loadingMore.value = false
  }
}

async function rebuildList() {
  if (searching.value || listStatus.value === 'loading') return
  const gen = ++loadToken
  const startSeq = inputSeq
  try {
    const n = Math.max(PAGE_SIZE, baseRows.value.length)
    const folderId = rangeId.value ? (activeFolder.value?.id ?? -1) : 0
    const r = folderId
      ? await api.getFolderMessages(folderId, 0, n)
      : await api.getMessages(scope.value, 0, n)
    const batch = r.items
    if (gen !== loadToken) return
    const snap = { kind: 'folder', scope: scope.value, folderId }
    applySnapshot(batch, snap, startSeq, false, r.wm)
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

const dividerState = new WeakMap()
function setBarDivider(bar, scrolled) {
  if (!bar || dividerState.get(bar) === scrolled) return
  dividerState.set(bar, scrolled)
  bar.classList.toggle('bar-scrolled', scrolled)
}
let topbarElCache = null
function listTopbarEl() {
  if (!topbarElCache || !topbarElCache.isConnected) {
    topbarElCache = document.querySelector('.app-topbar')
  }
  return topbarElCache
}
function onScrollDivider(e) {
  const t = e.target
  if (!t || t.nodeType !== 1) return
  const list = t.closest('.list-scroll')
  if (list && (t === list || t === scrollEl(list))) {
    setBarDivider(listTopbarEl(), t.scrollTop > 0)
    return
  }
  const reader = t.closest('.reader-scroll')
  if (reader && (t === reader || t === scrollEl(reader))) {
    const main = reader.closest('.reader-main')
    setBarDivider(main?.querySelector('.reader-toolbar, .reader-head'), t.scrollTop > 0)
    return
  }
  const settings = t.closest('.settings-content')
  if (settings && (t === settings || t === scrollEl(settings))) {
    setBarDivider(document.querySelector('.settings-topbar'), t.scrollTop > 0)
    setBarDivider(document.querySelector('.settings-tabs'), t.scrollTop > 0)
  }
}


const SWIPE_COMMIT = 96
const swipeId = ref(0)
const swipeDx = ref(0)
const swipeDragging = ref(false)
let swipeStart = null
let swipeWidth = 0
let suppressClickUntil = 0
let pressTimer = 0

const exitMap = ref(new Map())
const exitAnim = ref(false)
const exitShift = ref(new Map())
const swipeVisualDone = new Map()
const swipePendingSends = new Set()
function flushSwipeSends() {
  for (const p of [...swipePendingSends]) p.flush()
}
const toastText = ref('')
const toastShow = ref(false)
let toastTimer = 0
const cancelDest = { verb: 'cancel', folder_id: 0, special: '', name: '', mixed: false, key: 'cancel' }
const schedDeleteDest = { verb: 'delete', folder_id: 0, special: 'trash', name: '', mixed: false, key: 'sched-delete' }
function folderDest(verb, folder) {
  return {
    verb,
    folder_id: folder ? folder.id : 0,
    special: folder ? folder.special_use || '' : '',
    name: folder ? folder.name : '',
    mixed: false,
    key: verb + '|' + (folder ? (folder.special_use ? 's:' + folder.special_use : 'f:' + folder.id) : 'none'),
  }
}
function specialDest(verb, accountId, special) {
  const f = folders.value.find((x) => x.account_id === accountId && x.special_use === special)
  if (f) return folderDest(verb, f)
  return { verb, folder_id: 0, special, name: '', mixed: false, key: verb + '|s:' + special }
}
function restoreDestForRow(row) {
  const list = folders.value.filter((f) => f.account_id === row.account_id)
  if (row.flags & 16) {
    const d = list.find((f) => f.special_use === 'drafts')
    if (d && d.id !== row.folder_id) return d
  }
  if (row.trash_origin_folder_id) {
    const o = list.find((f) => f.id === row.trash_origin_folder_id)
    if (o && o.id !== row.folder_id) return o
  }
  return list.find((f) => f.special_use === 'inbox') || null
}
function threadDestFor(kind, rep, members) {
  if (kind === 'delete') return specialDest('delete', rep.account_id, 'trash')
  if (kind === 'archive') return specialDest('move', rep.account_id, 'archive')
  if (rep.folder_special === 'trash' || rep.folder_special === 'junk') {
    return restoreGestureDest(members && members.length ? members : [rep])
  }
  return specialDest('move', rep.account_id, 'inbox')
}
function threadGestureFor(kind, anchorSpecial) {
  const inTrash = anchorSpecial === 'trash' || anchorSpecial === 'junk'
  if (kind === 'inbox') return 'inbox'
  if (kind === 'archive') return inTrash ? 'inbox' : 'archive'
  return inTrash ? 'expunge' : 'trash'
}
function threadGestureAction(gesture, mem) {
  const sp = mem.folder_special
  if (gesture === 'archive') return sp === 'inbox' || sp === 'sent' ? 'archive' : null
  if (gesture === 'inbox') {
    if (sp === 'inbox') return null
    return sp === 'trash' ? 'restore' : 'inbox'
  }
  if (gesture === 'trash') {
    if (sp === 'trash') return null
    return sp === 'junk' ? 'trashMove' : 'delete'
  }
  return 'expunge'
}
function bulkArchiveDest(rows) {
  if (rows.length && rows.every((x) => x.folder_special === 'trash' || x.folder_special === 'junk')) {
    return restoreGestureDest(rows)
  }
  if (rows.some((x) => x.folder_special === 'trash' || x.folder_special === 'junk')) {
    return { verb: 'move', folder_id: 0, special: '', name: '', mixed: true, key: 'move|mixed' }
  }
  return specialDest('move', rows[0]?.account_id ?? 0, 'archive')
}
function restoreGestureDest(rows) {
  const dests = rows.map((row) =>
    row.folder_special === 'junk'
      ? folders.value.find((f) => f.account_id === row.account_id && f.special_use === 'inbox')
      : restoreDestForRow(row),
  )
  const first = dests[0]
  const ident = (f) => (f ? (f.special_use ? 's:' + f.special_use : 'f:' + f.id) : 'none')
  if (first && dests.every((d) => ident(d) === ident(first))) return folderDest('restore', first)
  return { verb: 'restore', folder_id: 0, special: '', name: '', mixed: true, key: 'restore|mixed' }
}
function removalText(dest, n) {
  if (dest.verb === 'cancel') return t('已取消 {n} 封定时发送', { n })
  if (dest.mixed) return dest.verb === 'restore' ? t('已恢复 {n} 封邮件', { n }) : t('已移动 {n} 封邮件', { n })
  if (dest.special === 'trash') return t('已删除 {n} 封邮件', { n })
  if (dest.special === 'archive') return t('已归档 {n} 封邮件', { n })
  if (dest.special === 'inbox') return t('已移回收件箱 {n} 封邮件', { n })
  return t('已移至{name} {n} 封邮件', { name: dest.name, n })
}
function removalFailText(dest, n) {
  if (dest.verb === 'cancel') return t('取消发送失败')
  if (dest.mixed) {
    if (dest.verb === 'restore')
      return n > 1 ? t('{n} 封邮件恢复失败，已放回原位', { n }) : t('恢复失败，邮件已放回原位')
    return n > 1 ? t('{n} 封邮件操作失败，已恢复', { n }) : t('操作失败，邮件已恢复')
  }
  if (dest.special === 'trash')
    return n > 1 ? t('{n} 封邮件删除失败，已恢复', { n }) : t('删除失败，邮件已恢复')
  if (dest.special === 'archive')
    return n > 1 ? t('{n} 封邮件归档失败，已恢复', { n }) : t('归档失败，邮件已恢复')
  if (dest.special === 'inbox')
    return n > 1 ? t('{n} 封邮件移回收件箱失败，已恢复', { n }) : t('移回收件箱失败，邮件已恢复')
  return n > 1
    ? t('{n} 封邮件移至{name}失败，已恢复', { n, name: dest.name })
    : t('移至{name}失败，邮件已恢复', { name: dest.name })
}
function showRemovalFailure(dest, n) {
  const text = removalFailText(dest, n)
  if (pendingUndo.value) {
    actionError.value = text
    return
  }
  showToast(text)
}
function onSnackSurfaceClick(e) {
  if (e.target.closest('.v-btn')) return
  if (pendingUndo.value) return
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
let toastDest = null
let toastCount = 0
let toastHideAt = 0
const pendingUndo = ref(null)
const toastTimeout = computed(() => (pendingUndo.value ? 6000 : 3500))
watch(toastShow, (v) => {
  if (!v) {
    pendingUndo.value = null
  }
})
function showCountToast(dest, n, undo = null) {
  const now = Date.now()
  const sameGesture = !!(
    undo &&
    pendingUndo.value &&
    (undo.intentIds ?? []).some((id) => (pendingUndo.value.intentIds ?? []).includes(id))
  )
  const continuing =
    toastDest && toastShow.value && now < toastHideAt && (toastDest.key === dest.key || sameGesture)
  if (continuing) {
    toastCount += n
    toastDest = dest
    if (undo && pendingUndo.value) {
      const liveIntentIds = pendingUndo.value.intentIds ?? []
      if (sameGesture) {
        const byId = new Map(pendingUndo.value.items.map((it) => [it.id, it]))
        for (const it of undo.items) byId.set(it.id, it)
        pendingUndo.value = {
          items: [...byId.values()],
          settled: undo.settled,
          intentIds: [...new Set([...liveIntentIds, ...(undo.intentIds ?? [])])],
          obKeys: [...new Set([...(pendingUndo.value.obKeys ?? []), ...(undo.obKeys ?? [])])],
        }
      } else {
        pendingUndo.value = {
          items: [...pendingUndo.value.items, ...undo.items],
          settled: Promise.all([pendingUndo.value.settled, undo.settled]).then(([a, b]) => [...a, ...b]),
          intentIds: [...liveIntentIds, ...(undo.intentIds ?? [])],
          obKeys: [...new Set([...(pendingUndo.value.obKeys ?? []), ...(undo.obKeys ?? [])])],
        }
      }
    } else if (undo) {
      pendingUndo.value = undo
    }
  } else {
    toastDest = dest
    toastCount = n
    pendingUndo.value = undo
  }
  toastHideAt = now + (pendingUndo.value ? 6000 : 3500)
  showToast(removalText(toastDest, toastCount))
}
function rowBelongsToCurrentView(row) {
  if (searching.value) return false
  if (rangeId.value && row.account_id !== rangeId.value) return false
  if (activeFolder.value?.id) return row.folder_id === activeFolder.value.id
  if (scope.value === 'all') return true
  return row.folder_special === scope.value
}

function undoRemoval() {
  const u = pendingUndo.value
  if (!u) return
  dismissToast()
  for (const k of u.obKeys || []) obVoid(k)
  const hideIntents = (u.intentIds ?? [])
    .map((id) => intents.value.get(id))
    .filter((x) => x && x.kind === 'hide')
  const restore = registerRestoreIntent(u.items, hideIntents)
  for (const it of u.items) {
    clearExitRow(it.id)
    exitNatural.delete(it.id)
  }
  animateRowsIn(u.items.map((it) => it.id))
  u.settled.then((moved) => {
    const movedIds = new Set(moved.map((it) => it.id))
    let shrunk = false
    for (const it of u.items) {
      if (!movedIds.has(it.id) && restore.ids.has(it.id)) {
        restore.ids.delete(it.id)
        restore.rows.delete(it.id)
        shrunk = true
      }
    }
    if (shrunk) touchIntents()
    if (!moved.length) {
      if (!restore.ids.size && intents.value.has(restore.id)) {
        intents.value.delete(restore.id)
        touchIntents()
      }
      return
    }
    ;(async () => {
      for (const it of moved) {
        if (it.op_id) restoreIntentByOp.set(it.op_id, restore.id)
      }
      restore.undoFailures = (
        await Promise.all(
          moved
            .filter((it) => it.op_id)
            .map(async (it) => {
              try {
                await api.undoOp(it.op_id)
                return null
              } catch (err) {
                return { it, err }
              }
            }),
        )
      ).filter(Boolean)
      const withoutOp = moved.filter((it) => !it.op_id)
      if (withoutOp.length) await parkUndoItems(restore, withoutOp, u, moved.length === 1)
      finishUndoRestore(restore)
    })()
  })
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
  for (const id of naturals.keys()) {
    const i = displayMessages.value.findIndex((x) => x.id === id)
    if (i < 0) continue
    const wrap = document.querySelector('[data-rowidx="' + i + '"]')?.closest('.mail-row-wrap')
    if (wrap) void wrap.offsetHeight
  }
  await new Promise((r) => requestAnimationFrame(r))
  enterAnim.value = true
  enterMap.value = naturals
  await sleep(250)
  enterMap.value = new Map()
  enterAnim.value = false
}
function animateRollback(ids) {
  const covered = new Set()
  for (const it of intents.value.values()) {
    if (it.kind === 'restore') for (const id of it.ids) covered.add(id)
  }
  const todo = ids.filter((id) => !covered.has(id))
  if (todo.length) animateRowsIn(todo)
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
  const pinned = new Map(exitMap.value)
  for (const [id, h] of heights) pinned.set(id, h)
  exitMap.value = pinned
  await nextTick()
  await new Promise((r) => requestAnimationFrame(r))
  exitAnim.value = true
  const collapsed = new Map(exitMap.value)
  for (const id of heights.keys()) if (collapsed.has(id)) collapsed.set(id, 0)
  exitMap.value = collapsed
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
  return {}
}

function rowSwipeStyle(m) {
  if (exitShift.value.has(m.id)) {
    return {
      transform: 'translateX(' + exitShift.value.get(m.id) + 'px)',
      transition: 'transform 0.18s ease',
      background: 'rgb(var(--v-theme-surface))',
    }
  }
  if (swipeId.value !== m.id) return {}
  return {
    transform: 'translateX(' + swipeDx.value + 'px)',
    transition: swipeDragging.value ? 'none' : 'transform 0.18s ease',
    background: 'rgb(var(--v-theme-surface))',
  }
}

function rowTouchStart(e, m) {
  if (exitMap.value.has(m.id) || exitShift.value.has(m.id)) return
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
  if (dx <= -SWIPE_COMMIT) {
    settleSwipe(m, scope.value === 'archive' ? 'inbox' : 'archive')
  } else if (dx >= SWIPE_COMMIT) {
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
  let signalVisual
  const visualDone = new Promise((r) => { signalVisual = r })
  swipeVisualDone.set(m.id, signalVisual)
  exitShift.value = new Map(exitShift.value).set(m.id, action === 'delete' ? swipeWidth : -swipeWidth)
  const p = action === 'archive' ? archiveFor(m, true, true) : action === 'inbox' ? inboxFor(m, true, true) : deleteFor(m, true, true)
  p.catch(() => {})
  try {
    await visualDone
  } finally {
    if (swipeVisualDone.get(m.id) === signalVisual) swipeVisualDone.delete(m.id)
    if (swipeId.value === m.id) {
      swipeId.value = 0
      swipeDx.value = 0
    }
  }
}

function onSearchInput() {
  clearTimeout(searchTimer)
  loadToken++
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
    foldersError.value = t(err.message)
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
function onDiscarded({ row_id, kind, op_id }) {
  const row = displayMessages.value.find((x) => x.id === row_id)
  let intent = null
  if (row) {
    intent = registerHideIntent({ ids: [row_id], rows: [row], exiting: [row_id] })
    intent.status = 'confirmed'
    intent.confirmSeq = ++inputSeq
    touchIntents()
  }
  if (kind === 'draft') {
    toastDest = null
    pendingUndo.value = {
      items: [{ id: row_id, special: 'drafts', op_id }],
      settled: Promise.resolve([{ id: row_id, special: 'drafts', op_id }]),
      intentIds: intent ? [intent.id] : [],
    }
    toastHideAt = Date.now() + 6000
    showToast(t('草稿已删除'))
  } else {
    showCountToast(cancelDest, 1, null)
  }
  if (!row || !intent) return
  discardRowId = row_id
  animateRowRemoval(row, intent).then(() => {
    discardRowId = 0
  })
}

async function onComposeDone(info) {
  if (info && info.op_id) {
    trackedSend.value = { op_id: info.op_id, not_before: info.not_before }
  }
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

function rowsInsertFlat(rows, item, belongs) {
  const i = rows.findIndex((r) => r.id === item.id)
  if (i >= 0) {
    const next = [...rows]
    next[i] = { ...rows[i], ...item }
    return next
  }
  if (!belongs(item)) return rows
  return [...rows, item].sort(byDateDesc)
}

function rowsInsertThreaded(rows, item, belongs) {
  if (intentCoversThread(item)) return rows
  const self = rows.findIndex((r) => r.id === item.id)
  if (self >= 0) {
    const cur = rows[self]
    const next = [...rows]
    next[self] = { ...cur, ...item, thread_count: cur.thread_count, thread_unread: cur.thread_unread }
    return next
  }
  const key = item.thread_key || 'id:' + item.id
  const i = rows.findIndex(
    (r) => r.account_id === item.account_id && (r.thread_key || 'id:' + r.id) === key,
  )
  if (i < 0) {
    if (!belongs(item)) return rows
    countedMembers.add(item.id)
    const row = { ...item, thread_key: key, thread_count: 1, thread_unread: item.flags & 1 ? 0 : 1 }
    return [...rows, row].sort(byDateDesc)
  }
  const row = { ...rows[i] }
  if (appendReplay && countedMembers.has(item.id)) {
    const counts = { thread_count: row.thread_count, thread_unread: row.thread_unread }
    if (item.date_ts > row.date_ts || (item.date_ts === row.date_ts && item.id > row.id)) {
      Object.assign(row, item, counts)
    }
    const next = [...rows]
    next[i] = row
    return next.sort(byDateDesc)
  }
  countedMembers.add(item.id)
  row.thread_count = (row.thread_count ?? 1) + 1
  if (!(item.flags & 1)) row.thread_unread = (row.thread_unread ?? 0) + 1
  if (item.date_ts > row.date_ts || (item.date_ts === row.date_ts && item.id > row.id)) {
    const counts = { thread_count: row.thread_count, thread_unread: row.thread_unread }
    Object.assign(row, item, counts)
  }
  const next = [...rows]
  next[i] = row
  return next.sort(byDateDesc)
}

function threadHideIntent(rep) {
  for (const it of intents.value.values()) {
    if (
      it.kind === 'hide' &&
      !it.superseded &&
      it.thread &&
      it.thread.accountId === rep.account_id &&
      it.thread.threadKey === (rep.thread_key || 'id:' + rep.id) &&
      it.ids.has(it.thread.repId)
    ) {
      return it
    }
  }
  return null
}

function intentCoversThread(rep) {
  return threadHideIntent(rep) !== null
}

function rowsApplyRemoved(rows, ev) {
  const ids = new Set(ev.ids ?? [])
  if (!ids.size) return { rows, gap: false }
  if (searching.value) {
    return {
      rows: rows.filter(
        (r) => !(ids.has(r.id) && (ev.folder_id == null || r.folder_id === ev.folder_id)),
      ),
      gap: false,
    }
  }
  let out = rows
  let gap = false
  const folderRelevant = (fid) => {
    if (fid == null) return true
    if (listSnap.value.folderId) return fid === listSnap.value.folderId
    const f = folders.value.find((x) => x.id === fid)
    return f ? f.special_use === listSnap.value.scope : false
  }
  for (const id of ids) {
    const rep = out.find((r) => r.id === id)
    if (rep) {
      if ((rep.thread_count ?? 1) <= 1 || intentCoversThread(rep)) {
        out = out.filter((r) => r.id !== id)
      } else {
        gap = true
      }
      continue
    }
    const th = memberThread.get(id)
    if (!th) {
      if (folderRelevant(ev.folder_id)) gap = true
      continue
    }
    if (appendReplay && !countedMembers.has(id)) continue
    const repRow = out.find(
      (r) => r.account_id === th.accountId && (r.thread_key || 'id:' + r.id) === th.threadKey,
    )
    if (!repRow) continue
    countedMembers.delete(id)
    const prior = memberFlags.get(id)
    const count = (repRow.thread_count ?? 1) - 1
    if (count <= 0) {
      out = out.filter((r) => r.id !== repRow.id)
    } else {
      const unread = Math.max(
        0,
        (repRow.thread_unread ?? 0) - (prior !== undefined && !(prior & 1) ? 1 : 0),
      )
      out = out.map((r) =>
        r.id === repRow.id ? { ...r, thread_count: count, thread_unread: unread } : r,
      )
    }
  }
  return { rows: out, gap }
}

function patchAuxFlags(items) {
  const byId = new Map(items.map((it) => [it.id, it.flags]))
  if (convo.value) {
    convo.value = {
      ...convo.value,
      msgs: convo.value.msgs.map((m) => (byId.has(m.id) ? { ...m, flags: byId.get(m.id) } : m)),
    }
  }
  if (selected.value && byId.has(selected.value.id)) {
    selected.value = { ...selected.value, flags: byId.get(selected.value.id) }
    if (detail.value) detail.value = { ...detail.value, flags: selected.value.flags }
  }
}

function applyFlagsInput(items) {
  if (!items?.length) return
  journalDelta({ type: 'flags', items })
  for (const it of items) latestFlags.set(it.id, it.flags)
  baseRows.value = rowsApplyFlags(baseRows.value, items)
  snapEpoch = listEpoch
  for (const it of items) memberFlags.set(it.id, it.flags)
  patchAuxFlags(items)
}

function applyMessagesInput(items) {
  if (!items?.length) return
  journalDelta({ type: 'msg', items })
  journalRows(items)
  let rows = baseRows.value
  const groupedView = !searching.value
  for (const item of items) {
    rows = groupedView
      ? rowsInsertThreaded(rows, item, rowMatchesScope)
      : rowsInsertFlat(rows, item, rowMatchesScope)
    if (
      convo.value &&
      convo.value.accountId === item.account_id &&
      convo.value.threadKey === (item.thread_key || 'id:' + item.id) &&
      !convo.value.msgs.some((m) => m.id === item.id)
    ) {
      convo.value = {
        ...convo.value,
        msgs: [...convo.value.msgs, item].sort((a, b) => a.date_ts - b.date_ts || a.id - b.id),
      }
    }
  }
  baseRows.value = rows
  snapEpoch = listEpoch
  let retiredAny = false
  for (const it of [...intents.value.values()]) {
    if (it.kind === 'restore') settleRestoreIntent(it)
    else if (it.kind === 'hide' && it.status === 'confirmed') {
      const back = items.some((item) => {
        const r = it.rows.get(item.id)
        return r && item.folder_id === r.folder_id
      })
      if (back) {
        intents.value.delete(it.id)
        retiredAny = true
      }
    }
  }
  if (retiredAny) touchIntents()
}

function applyRemovedInput(ev) {
  journalDelta({ type: 'removed', ev })
  const ids = new Set(ev.ids ?? [])
  const { rows, gap } = rowsApplyRemoved(baseRows.value, ev)
  baseRows.value = rows
  snapEpoch = listEpoch
  if (convo.value && ids.size) {
    convo.value = { ...convo.value, msgs: convo.value.msgs.filter((m) => !ids.has(m.id)) }
  }
  if (gap && !searching.value) rebuildList()
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

let lastEventSeq = 0

function onServerEvent(ev) {
  if (view.value !== 'app') return
  if (typeof ev.seq === 'number') {
    if (lastEventSeq !== 0 && ev.seq > lastEventSeq + 1) {
      loadFolders()
      rebuildList()
      loadStatuses()
      void rebuildSendState()
    }
    lastEventSeq = ev.seq
  }
  if (ev.type === 'messages' || ev.type === 'removed' || ev.type === 'scheduled') listEpoch++
  if (ev.type === 'messages') {
    const beforeCount = messages.value.length
    applyMessagesInput(ev.items ?? [])
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
    applyFlagsInput(ev.items ?? [])
  } else if (ev.type === 'removed') {
    applyRemovedInput(ev)
  } else if (ev.type === 'folder') {
    applyFolderEvent(ev.folder)
  } else if (ev.type === 'ops') {
    if (typeof ev.attention === 'number') attentionCount.value = ev.attention
    if (ev.op_id) {
      lastOpEvent.value = {
        op_id: ev.op_id,
        status: ev.status,
        sent: !!ev.sent,
        undo_ignored: !!ev.undo_ignored,
        undo_failed: !!ev.undo_failed,
        seq: ++opEventSeq,
      }
      if (
        trackedSend.value &&
        ev.op_id === trackedSend.value.op_id &&
        (ev.status === 'done' || ev.status === 'cancelled' || ev.status === 'needs_attention')
      ) {
        trackedSend.value = null
      }
    }
    if (ev.undo_failed && ev.op_id) {
      const intent = intents.value.get(restoreIntentByOp.get(ev.op_id))
      if (intent && intent.kind === 'restore') failRestoreIntent(intent)
      actionError.value = t('撤销失败：{error}', { error: ev.error ? t(ev.error) : t('邮件未恢复') })
      loadFolders()
    }
    if (ev.op_id && ev.status === 'needs_attention') {
      let retired = false
      for (const it of [...intents.value.values()]) {
        if (it.kind === 'hide' && it.status === 'confirmed' && it.opIds.has(ev.op_id)) {
          intents.value.delete(it.id)
          retired = true
        }
      }
      if (retired) {
        touchIntents()
        rebuildList()
      }
    }
    refreshSyncSources()
    void flushUndoKeyWaiters()
  } else if (ev.type === 'scheduled') {
    for (const k of [...scopeCache.keys()]) {
      if (k.startsWith('folder|scheduled|')) scopeCache.delete(k)
    }
    refreshScheduledCount()
    if (scope.value === 'scheduled' && !searching.value) loadMessages(true)
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
      rebuildList()
      loadStatuses()
      obFlush()
      void flushUndoKeyWaiters()
      void rebuildSendState()
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
    if (sessionStorage.getItem('namakani_reloaded_for') === st.version) {
      appVersion.value = st.version
      return
    }
    sessionStorage.setItem('namakani_reloaded_for', st.version)
    location.reload()
  } catch {
  }
}

function onVisibility() {
  if (!document.hidden && view.value === 'app') {
    checkVersion()
    obFlush()
  }
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

let drawerAnimUntil = 0
watch(drawerOpen, (open) => {
  if (open && !lgAndUp.value) drawerAnimUntil = performance.now() + 350
})

let navPress = null
let navManualEl = null
let navManualAt = 0
let navManualInFlight = false

function temporaryDrawerEl() {
  return document.querySelector('.v-navigation-drawer--temporary')
}

function drawerTxOf(d) {
  const m = getComputedStyle(d).transform
  if (!m || m.indexOf('matrix') !== 0) return 0
  return parseFloat(m.split(',')[4])
}

function navItemBySettledPoint(d, x, y) {
  const tx = drawerTxOf(d)
  if (tx >= -2) return null
  for (const item of d.querySelectorAll('.v-list-item')) {
    const r = item.getBoundingClientRect()
    if (x >= r.left - tx && x <= r.right - tx && y >= r.top && y <= r.bottom) return item
  }
  return null
}

function onDrawerPressDown(e) {
  navManualEl = null
  navPress = null
  if (e.isPrimary === false) return
  if (lgAndUp.value || !drawerOpen.value || composeOpen.value) return
  const d = temporaryDrawerEl()
  if (!d) return
  const inDrawer = d.contains(e.target)
  const onScrim = !!(e.target.closest && e.target.closest('.v-navigation-drawer__scrim'))
  if (!inDrawer && !onScrim) return
  if (drawerTxOf(d) >= -2 && performance.now() >= drawerAnimUntil) return
  let item = null
  const hit = e.target.closest ? e.target.closest('.v-list-item') : null
  if (hit && d.contains(hit)) item = hit
  else item = navItemBySettledPoint(d, e.clientX, e.clientY)
  navPress = { x: e.clientX, y: e.clientY, item, nativeItem: null, released: false, consumed: false }
}

function onDrawerPressUp(e) {
  const press = navPress
  if (!press || press.consumed || press.released) return
  const dx = e.clientX - press.x
  const dy = e.clientY - press.y
  if (dx * dx + dy * dy >= 100 || !press.item) {
    press.consumed = true
    return
  }
  press.released = true
  setTimeout(() => {
    press.consumed = true
    if (navPress === press) navPress = null
    if (press.nativeItem === press.item) return
    if (!press.item.isConnected) return
    navManualInFlight = true
    press.item.click()
    navManualInFlight = false
    navManualEl = press.item
    navManualAt = performance.now()
  }, 80)
}

function onDrawerPressCancel() {
  navPress = null
}

function onDrawerPressClick(e) {
  if (navManualInFlight) return
  if (navManualEl && performance.now() - navManualAt < 400) {
    const hit = e.target.closest ? e.target.closest('.v-list-item') : null
    if (hit === navManualEl) {
      e.stopPropagation()
      e.preventDefault()
      navManualEl = null
      return
    }
  }
  const press = navPress
  if (!press || press.consumed) return
  if (Math.abs(e.clientX - press.x) >= 10 || Math.abs(e.clientY - press.y) >= 10) return
  const d = temporaryDrawerEl()
  if (!d) return
  const hit = e.target.closest ? e.target.closest('.v-list-item') : null
  if (hit && d.contains(hit)) press.nativeItem = hit
}

function selectScope(key) {
  flushSwipeSends()
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
      load_error: t('定时邮件加载失败，请关闭后重试') + ' (' + t(err.message) + ')',
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
  applyFlagsInput([{ id, flags }])
}

function bumpFolderUnread(m, delta) {
  if (!m.folder_id) return
  folders.value = folders.value.map((f) =>
    f.id === m.folder_id ? { ...f, unread: Math.max(0, f.unread + delta) } : f,
  )
}

function markSeenImpl(m, seen) {
  const prevFlags = m.flags
  const nextFlags = seen ? prevFlags | 1 : prevFlags & ~1
  actionError.value = ''
  const intent = registerFlagsIntent(m.id, nextFlags)
  patchAuxFlags([{ id: m.id, flags: nextFlags }])
  bumpFolderUnread(m, seen ? -1 : 1)
  const entry = {
    key: api.newClientKey(),
    kind: 'flags',
    ts: Date.now(),
    reqs: [{ op: 'flags', id: m.id, seen }],
    rows: [m],
    undoItems: null,
    destLabel: null,
  }
  try {
    obAdd(entry)
  } catch (e) {
    dropIntent(intent)
    patchAuxFlags([{ id: m.id, flags: prevFlags }])
    bumpFolderUnread(m, seen ? 1 : -1)
    actionError.value = t('操作保存失败：{error}', { error: t(e.message) })
    return
  }
  obDrive(entry, {
    onOk: (out) => {
      dropIntent(intent)
      const it = out.items.find((x) => x.id === m.id)
      if (it && it.flags != null) applyFlagsInput([{ id: m.id, flags: it.flags }])
      loadFolders()
    },
    onRefused: (out) => {
      dropIntent(intent)
      patchAuxFlags([{ id: m.id, flags: prevFlags }])
      bumpFolderUnread(m, seen ? 1 : -1)
      actionError.value = t(seen ? '标为已读失败：{error}' : '标为未读失败：{error}', {
        error: t(out.refusal?.message || ''),
      })
    },
  })
}

async function markSeen(m) {
  if (m.flags & 1) return
  markSeenImpl(m, true)
}

async function markUnseen(m) {
  if (!(m.flags & 1)) return
  markSeenImpl(m, false)
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
    localStorage.setItem('namakani_range', String(route.account))
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
    openSchEditor({ id: m.id, account_id: m.account_id, subject: m.subject || '', send_at: m.date_ts, status: 'pending' })
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
        load_error: t('草稿加载失败，请关闭后重试') + ' (' + t(err.message) + ')',
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
      detailError.value = t(err.message)
      detailState.value = 'error'
    }
  }
}

let convoAnchorMsg = null

async function openConversation(m) {
  convoAnchorMsg = m
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
  cardBodyErrors.value = new Set()
  convoTr.value = {}
  try {
    const allMsgs = await api.getThreadMessagesAll(m.account_id, threadKey)
    journalRows(allMsgs)
    const msgs = allMsgs
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
  convoTr.value = {}
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

const cardBodyErrors = ref(new Set())

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
    if (cardBodyErrors.value.has(id)) {
      const next = new Set(cardBodyErrors.value)
      next.delete(id)
      cardBodyErrors.value = next
    }
  } catch {
    const next = new Set(cardBodyErrors.value)
    next.add(id)
    cardBodyErrors.value = next
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
  if (!(msg.flags & 1)) {
    markSeen(msg)
    return
  }
  markUnseen(msg)
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
  if ((m.thread_count ?? 1) > 1) {
    openConversation(m)
    return
  }
  openMessage(m)
}

async function toggleSelectedSeen() {
  if (!selected.value) return
  if (!(selected.value.flags & 1)) {
    markSeen(selected.value)
    return
  }
  markUnseen(selected.value)
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

function afterRemovalLocal(id) {
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
  loadFolders()
}

function setExitHeight(id, h) {
  const next = new Map(exitMap.value)
  next.set(id, h)
  exitMap.value = next
}

function clearExitRow(id) {
  if (exitMap.value.has(id)) {
    const next = new Map(exitMap.value)
    next.delete(id)
    exitMap.value = next
  }
  if (exitShift.value.has(id)) {
    const next = new Map(exitShift.value)
    next.delete(id)
    exitShift.value = next
  }
  if (!exitMap.value.size) exitAnim.value = false
}

async function animateRowRemoval(m, intent, abortCheck) {
  const idx = displayMessages.value.findIndex((x) => x.id === m.id)
  const wrap = idx >= 0 ? document.querySelector('[data-rowidx="' + idx + '"]')?.closest('.mail-row-wrap') : null
  const h = wrap ? wrap.offsetHeight : 0
  if (convo.value) {
    const left = convo.value.msgs.filter((x) => x.id !== m.id)
    if (left.length === 0) closeConvo()
    else convo.value = { ...convo.value, msgs: left }
  }
  if (selected.value?.id === m.id) clearSelection()
  loadFolders()
  let aborted = false
  if (h > 0) {
    exitAnim.value = false
    setExitHeight(m.id, h)
    await sleep(250)
    if (abortCheck && abortCheck()) {
      aborted = true
    } else {
      await new Promise((r) => requestAnimationFrame(r))
      exitAnim.value = true
      setExitHeight(m.id, 0)
      await sleep(240)
    }
  }
  if (!aborted) exitDoneIntent(intent, m.id)
  clearExitRow(m.id)
  const signalVisual = swipeVisualDone.get(m.id)
  if (signalVisual) {
    swipeVisualDone.delete(m.id)
    signalVisual()
  }
  return aborted ? 'aborted' : 'done'
}

async function actOnThread(m, kind, animate = false, deferSend = false) {
  const gesture = threadGestureFor(kind, m.folder_special)
  let settleResolve
  const settledAll = new Promise((res) => { settleResolve = res })
  const repAction = threadGestureAction(gesture, m)
  const repUndoable = !!(m.folder_id || m.folder_special) && !!repAction && repAction !== 'expunge'
  const intent = registerHideIntent({
    ids: [m.id],
    rows: [m],
    exiting: animate ? [m.id] : [],
    thread: { accountId: m.account_id, threadKey: m.thread_key || 'id:' + m.id, repId: m.id },
  })
  let dest = threadDestFor(kind, m, null)
  const defer = animate && deferSend
  let gateResolve = null
  const sendGate = defer ? new Promise((r) => { gateResolve = r }) : null
  const pending = defer ? { flush: () => gateResolve() } : null
  if (pending) swipePendingSends.add(pending)
  const deferredToastFns = []
  let toastLive = !defer
  let toastDead = false
  const deferOrRun = (fn) => {
    if (toastLive) fn()
    else deferredToastFns.push(fn)
  }
  deferOrRun(() =>
    showCountToast(
      dest,
      m.thread_count || 1,
      repUndoable
        ? {
            items: [{ id: m.id, special: m.folder_special, folder_id: m.folder_id, row: m }],
            settled: settledAll,
            intentIds: [intent.id],
          }
        : null,
    ),
  )
  if (defer) {
    void sendGate.then(() => {
      toastLive = true
      if (toastDead) return
      for (const fn of deferredToastFns.splice(0)) fn()
    })
  }
  if (animate) {
    const animDone = animateRowRemoval(m, intent)
    if (defer) void animDone.then(() => gateResolve())
  } else if (selected.value?.id === m.id) {
    clearSelection()
  }
  let members
  try {
    members = await api.getThreadMessagesAll(m.account_id, m.thread_key || 'id:' + m.id)
    journalRows(members)
  } catch (err) {
    if (pending) swipePendingSends.delete(pending)
    toastDead = true
    failIntent(intent, [m.id])
    showRemovalFailure(dest, m.thread_count || 1)
    animateRollback([m.id])
    settleResolve([])
    return
  }
  if (!members.length) members = [m]
  const ids = members.map((x) => x.id)
  for (const mem of members) {
    intent.ids.add(mem.id)
    if (!intent.rows.has(mem.id)) intent.rows.set(mem.id, mem)
  }
  touchIntents()
  const undoItems = members
    .filter((x) => {
      if (!(x.folder_id || x.folder_special)) return false
      const a = threadGestureAction(gesture, x)
      return !!a && a !== 'expunge'
    })
    .map((x) => ({ id: x.id, special: x.folder_special, folder_id: x.folder_id }))
  const restItems = undoItems.filter((x) => x.id !== m.id)
  dest = threadDestFor(kind, m, members)
  const reqs = []
  for (const mem of members) {
    const action = threadGestureAction(gesture, mem)
    if (!action) continue
    if (action === 'archive') reqs.push({ op: 'move', id: mem.id, to: 'archive' })
    else if (action === 'inbox') reqs.push({ op: 'move', id: mem.id, to: 'inbox' })
    else if (action === 'restore') reqs.push({ op: 'restore', id: mem.id })
    else if (action === 'trashMove') reqs.push({ op: 'move', id: mem.id, to: 'trash' })
    else if (action === 'delete') reqs.push({ op: 'delete', id: mem.id })
    else if (action === 'expunge') reqs.push({ op: 'expunge', id: mem.id })
  }
  const entry = {
    key: api.newClientKey(),
    kind: 'thread',
    ts: Date.now(),
    reqs,
    rows: members,
    undoItems: undoItems.length ? undoItems : null,
    destLabel: dest,
  }
  if (defer) {
    await sendGate
    swipePendingSends.delete(pending)
  }
  try {
    obAdd(entry)
  } catch (e) {
    toastDead = true
    failIntent(intent, [m.id])
    showRemovalFailure(dest, m.thread_count || 1)
    animateRollback([m.id])
    settleResolve([])
    return
  }
  deferOrRun(() => {
    if (
      (reqs.length !== (m.thread_count || 1) || restItems.length) &&
      toastShow.value &&
      Date.now() < toastHideAt &&
      pendingUndo.value &&
      (pendingUndo.value.intentIds ?? []).includes(intent.id)
    ) {
      showCountToast(
        dest,
        reqs.length - (m.thread_count || 1),
        restItems.length ? { items: restItems, settled: settledAll, intentIds: [intent.id], obKeys: [entry.key] } : null,
      )
    }
  })
  deferOrRun(() => {
    if (pendingUndo.value && (pendingUndo.value.intentIds ?? []).includes(intent.id)) {
      pendingUndo.value.obKeys = [entry.key]
    }
  })
  const movedFrom = (out) =>
    undoItems
      .filter((x) => out.okIds.has(x.id))
      .map((x) => {
        const it = out.items.find((y) => y.id === x.id)
        return it?.op_id ? { ...x, op_id: it.op_id } : x
      })
  obDrive(entry, {
    onOk: (out) => {
      const movedItems = movedFrom(out)
      noteIntentOps(intent, movedItems)
      confirmIntent(intent)
      settleResolve(movedItems)
    },
    onRefused: (out) => {
      const failedIds = out.failedIds || []
      const movedItems = movedFrom(out)
      if (failedIds.length) {
        failIntent(intent, failedIds)
        showRemovalFailure(dest, failedIds.length)
        if (failedIds.includes(m.id)) animateRollback([m.id])
      } else {
        noteIntentOps(intent, movedItems)
        confirmIntent(intent)
      }
      settleResolve(movedItems)
    },
  })
}

async function archiveFor(m, animate = false, deferSend = false) {
  if (!m) return
  if (m.folder_special === 'trash' || m.folder_special === 'junk') return inboxFor(m, animate, deferSend)
  if (!searching.value && m.thread_count > 1) return actOnThread(m, 'archive', animate, deferSend)
  if (m.folder_special === 'scheduled') return toDraftsFor(m, animate)
  return moveSingle(m, 'archive', animate, deferSend)
}

function inboxFor(m, animate = false, deferSend = false) {
  if (!searching.value && m.thread_count > 1) return actOnThread(m, 'inbox', animate, deferSend)
  return moveSingle(m, 'inbox', animate, deferSend)
}

async function moveSingle(m, dest, animate = false, deferSend = false) {
  const intent = registerHideIntent({
    ids: [m.id],
    rows: [m],
    exiting: animate ? [m.id] : [],
  })
  const isRestore = dest === 'inbox' && m.folder_special === 'trash'
  const undoable = !!m.folder_special && m.folder_special !== dest
  const gestureDest =
    dest === 'inbox' && (m.folder_special === 'trash' || m.folder_special === 'junk')
      ? restoreGestureDest([m])
      : specialDest('move', m.account_id, dest)
  const entry = {
    key: api.newClientKey(),
    kind: 'single',
    ts: Date.now(),
    reqs: [{ op: isRestore ? 'restore' : 'move', id: m.id, to: dest }],
    rows: [m],
    undoItems: undoable ? [{ id: m.id, special: m.folder_special, folder_id: m.folder_id }] : null,
    destLabel: gestureDest,
  }
  let refusedOut = null
  let animRunning = animate
  let rolledBack = false
  const doRollback = () => {
    if (rolledBack) return
    rolledBack = true
    failIntent(intent, [m.id])
    showRemovalFailure(gestureDest, 1)
  }
  let ctrl = null
  let sendFailed = false
  const send = () => {
    if (ctrl) return true
    if (sendFailed) return false
    try {
      obAdd(entry)
    } catch (e) {
      sendFailed = true
      dropIntent(intent)
      actionError.value = t('操作保存失败：{error}', { error: t(e.message) })
      return false
    }
    ctrl = obDrive(entry, {
      onOk: (out) => {
        noteIntentOps(intent, out.items)
        confirmIntent(intent)
      },
      onRefused: (out) => {
        refusedOut = out
        if (!animRunning) {
          doRollback()
          animateRollback([m.id])
        }
      },
    })
    return true
  }
  const emitToast = () =>
    showCountToast(
      gestureDest,
      1,
      undoable
        ? {
            items: [{ id: m.id, special: m.folder_special, folder_id: m.folder_id, row: m }],
            settled: ctrl.undoPromise,
            intentIds: [intent.id],
            obKeys: [entry.key],
          }
        : null,
    )
  if (!(animate && deferSend)) {
    if (!send()) return
    emitToast()
    if (animate) {
      const r = await animateRowRemoval(m, intent, () => !!refusedOut)
      animRunning = false
      if (r === 'aborted') {
        if (refusedOut) doRollback()
        return
      }
      if (refusedOut) {
        doRollback()
        animateRollback([m.id])
      }
    } else {
      afterRemovalLocal(m.id)
    }
    return
  }
  const pending = {
    flush: () => {
      if (send()) emitToast()
    },
  }
  swipePendingSends.add(pending)
  await animateRowRemoval(m, intent)
  animRunning = false
  swipePendingSends.delete(pending)
  if (ctrl) {
    if (refusedOut) {
      doRollback()
      animateRollback([m.id])
    }
  } else if (send()) {
    emitToast()
  }
}

async function scheduledRowAction(m, animate, purge) {
  const intent = registerHideIntent({
    ids: [m.id],
    rows: [m],
    exiting: animate ? [m.id] : [],
  })
  const dest = purge ? schedDeleteDest : cancelDest
  const p = purge ? api.deleteScheduled(m.id) : api.cancelScheduled(m.id)
  let outcome = null
  const settled = p.then(
    (res) => {
      outcome = { ok: true }
      return [{ id: m.id, special: m.folder_special, folder_id: m.folder_id, op_id: res?.op_id }]
    },
    () => {
      outcome = { ok: false }
      return []
    },
  )
  showCountToast(dest, 1, null)
  if (animate) {
    await animateRowRemoval(m, intent)
  } else {
    afterRemovalLocal(m.id)
  }
  const settledItems = await settled
  if (outcome?.ok) {
    noteIntentOps(intent, settledItems)
    confirmIntent(intent)
  } else {
    failIntent(intent, [m.id])
    showRemovalFailure(dest, 1)
    animateRollback([m.id])
  }
}

function toDraftsFor(m, animate = false) {
  if (!m) return
  return scheduledRowAction(m, animate, false)
}

async function deleteFor(m, animate = false, deferSend = false) {
  if (!m) return
  if (!searching.value && m.thread_count > 1) return actOnThread(m, 'delete', animate, deferSend)
  if (m.folder_special === 'scheduled') return scheduledRowAction(m, animate, true)
  const intent = registerHideIntent({
    ids: [m.id],
    rows: [m],
    exiting: animate ? [m.id] : [],
  })
  const gestureDest = specialDest('delete', m.account_id, 'trash')
  const undoable = !!m.folder_special && m.folder_special !== 'trash' && m.folder_special !== 'junk'
  const entry = {
    key: api.newClientKey(),
    kind: 'single',
    ts: Date.now(),
    reqs: [{ op: 'delete', id: m.id }],
    rows: [m],
    undoItems: undoable ? [{ id: m.id, special: m.folder_special, folder_id: m.folder_id }] : null,
    destLabel: gestureDest,
  }
  let refusedOut = null
  let animRunning = animate
  let rolledBack = false
  const doRollback = () => {
    if (rolledBack) return
    rolledBack = true
    failIntent(intent, [m.id])
    showRemovalFailure(gestureDest, 1)
  }
  let ctrl = null
  let sendFailed = false
  const send = () => {
    if (ctrl) return true
    if (sendFailed) return false
    try {
      obAdd(entry)
    } catch (e) {
      sendFailed = true
      dropIntent(intent)
      actionError.value = t('操作保存失败：{error}', { error: t(e.message) })
      return false
    }
    ctrl = obDrive(entry, {
      onOk: (out) => {
        noteIntentOps(intent, out.items)
        confirmIntent(intent)
      },
      onRefused: (out) => {
        refusedOut = out
        if (!animRunning) {
          doRollback()
          animateRollback([m.id])
        }
      },
    })
    return true
  }
  const emitToast = () =>
    showCountToast(
      gestureDest,
      1,
      undoable
        ? {
            items: [{ id: m.id, special: m.folder_special, folder_id: m.folder_id, row: m }],
            settled: ctrl.undoPromise,
            intentIds: [intent.id],
            obKeys: [entry.key],
          }
        : null,
    )
  if (!(animate && deferSend)) {
    if (!send()) return
    emitToast()
    if (animate) {
      const r = await animateRowRemoval(m, intent, () => !!refusedOut)
      animRunning = false
      if (r === 'aborted') {
        if (refusedOut) doRollback()
        return
      }
      if (refusedOut) {
        doRollback()
        animateRollback([m.id])
      }
    } else {
      afterRemovalLocal(m.id)
    }
    return
  }
  const pending = {
    flush: () => {
      if (send()) emitToast()
    },
  }
  swipePendingSends.add(pending)
  await animateRowRemoval(m, intent)
  animRunning = false
  swipePendingSends.delete(pending)
  if (ctrl) {
    if (refusedOut) {
      doRollback()
      animateRollback([m.id])
    }
  } else if (send()) {
    emitToast()
  }
}

function applyMarkAllReadLocal() {
  baseRows.value = baseRows.value.map((m) => ({
    ...m,
    flags: m.flags | 1,
    ...(m.thread_unread != null ? { thread_unread: 0 } : {}),
  }))
  if (convo.value) {
    convo.value = {
      ...convo.value,
      msgs: convo.value.msgs.map((m) => ({ ...m, flags: m.flags | 1 })),
    }
  }
  if (selected.value) patchAuxFlags([{ id: selected.value.id, flags: selected.value.flags | 1 }])
}

function journalMarkAllRead() {
  const items = baseRows.value.map((m) => ({ id: m.id, flags: m.flags }))
  if (items.length) {
    journalDelta({ type: 'flags', items })
    for (const it of items) latestFlags.set(it.id, it.flags)
  }
  journalRows(baseRows.value)
  if (convo.value) journalRows(convo.value.msgs)
}

async function markAllRead() {
  if (searching.value || rangeId.value || scope.value === 'scheduled') return
  const priorById = new Map()
  const priorTU = new Map()
  for (const m of baseRows.value) {
    priorById.set(m.id, m.flags)
    if (m.thread_unread != null) priorTU.set(m.id, m.thread_unread)
  }
  if (convo.value) {
    for (const m of convo.value.msgs) if (!priorById.has(m.id)) priorById.set(m.id, m.flags)
  }
  if (selected.value && !priorById.has(selected.value.id)) priorById.set(selected.value.id, selected.value.flags)
  const priorItems = [...priorById.entries()].map(([id, flags]) => ({ id, flags }))
  applyMarkAllReadLocal()
  folders.value = folders.value.map((f) => (f.special_use === scope.value ? { ...f, unread: 0 } : f))
  try {
    await api.markAllRead(scope.value)
    journalMarkAllRead()
    loadFolders()
  } catch (err) {
    if (priorItems.length) {
      applyFlagsInput(priorItems)
      if (priorTU.size) {
        baseRows.value = baseRows.value.map((r) =>
          priorTU.has(r.id) && r.thread_unread !== priorTU.get(r.id) ? { ...r, thread_unread: priorTU.get(r.id) } : r,
        )
      }
    }
    loadFolders()
    actionError.value = t('全部标为已读失败：{error}', { error: t(err.message) })
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
    localStorage.setItem('namakani_nav_hidden', navHidden.value ? '1' : '0')
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
  if (atTop && dy > 110 && Math.abs(dx) < 60) {
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
      localStorage.setItem('namakani_range', String(r0.account))
    }
  } else if (r0.type === 'search') {
    searchQuery.value = r0.query
    searching.value = r0.query.trim().length > 0
  }
  const did = deepId || initialDeepId
  if (st && st.app_settings) maxAttachmentMB.value = st.app_settings.max_attachment_mb
  if (st && typeof st.attention === 'number') attentionCount.value = st.attention
  localStorage.removeItem('namakani_threaded')
  const storedUndo = parseInt(localStorage.getItem('namakani_undo_seconds') ?? '', 10)
  if ([0, 5, 10, 20, 30].includes(storedUndo)) undoSeconds.value = storedUndo
  const loaded = refreshAccounts()
  if (did) {
    readerNoAnim.value = true
    history.replaceState(null, '', '#/' + scope.value + (rangeId.value ? '/' + rangeId.value : ''))
    history.pushState(null, '', '#/mail/' + did)
    booted = true
    onHashChange()
  }
  await loaded
  connectEvents()
  refreshScheduledCount()
  obRecover()
  void rebuildSendState()
  loadPending()
  if (!did) {
    booted = true
    onHashChange()
  }
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
          tgAuthError.value = t(err.message)
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
    bootError.value = t(err.message)
    view.value = 'error'
  }
}

function resetToAuth() {
  accounts.value = []
  folders.value = []
  baseRows.value = []
  intents.value = new Map()
  obLive.clear()
  journal.length = 0
  clearSelection()
  settingsOpen.value = false
  authMode.value = 'login'
  view.value = 'auth'
}

async function doLogout() {
  booted = false
  disconnectEvents()
  await api.logout().catch(() => {})
  resetToAuth()
}

function onSessionsRevoked() {
  booted = false
  disconnectEvents()
  resetToAuth()
}

function onAppSettings(s) {
  maxAttachmentMB.value = s.max_attachment_mb
}

function onUndoSecondsChange(v) {
  undoSeconds.value = v
  localStorage.setItem('namakani_undo_seconds', String(v))
}

const online = ref(navigator.onLine)
function onOnlineChange() {
  online.value = navigator.onLine
  if (online.value && view.value === 'app') {
    refreshData()
    obFlush()
  }
}

onMounted(() => {
  window.addEventListener('hashchange', onHashChange)
  window.addEventListener('pagehide', flushSwipeSends)
  window.addEventListener('beforeunload', flushSwipeSends)
  document.addEventListener('visibilitychange', onVisibility)
  window.addEventListener('keydown', onGlobalKey)
  window.addEventListener('online', onOnlineChange)
  window.addEventListener('offline', onOnlineChange)
  document.addEventListener('pointerdown', onDrawerPressDown, true)
  document.addEventListener('pointerup', onDrawerPressUp, true)
  document.addEventListener('pointercancel', onDrawerPressCancel, true)
  document.addEventListener('click', onDrawerPressClick, true)
  window.addEventListener('scroll', onScrollDivider, { capture: true, passive: true })
  initTheme()
  boot()
})

onBeforeUnmount(() => {
  window.removeEventListener('hashchange', onHashChange)
  document.removeEventListener('visibilitychange', onVisibility)
  window.removeEventListener('keydown', onGlobalKey)
  window.removeEventListener('online', onOnlineChange)
  window.removeEventListener('offline', onOnlineChange)
  document.removeEventListener('pointerdown', onDrawerPressDown, true)
  document.removeEventListener('pointerup', onDrawerPressUp, true)
  document.removeEventListener('pointercancel', onDrawerPressCancel, true)
  document.removeEventListener('click', onDrawerPressClick, true)
  window.removeEventListener('scroll', onScrollDivider, { capture: true })
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
          <v-badge v-if="attentionCount > 0" :content="attentionCount" color="error">
            <v-btn icon="mdi-alert-circle" variant="text" color="error" :aria-label="t('有 {n} 项操作没有完成', { n: attentionCount })" @click="openSyncPanel" />
          </v-badge>
          <v-spacer />
          <v-badge v-if="attentionCount === 0 && syncInflightCount > 0" :content="syncInflightCount">
            <v-btn icon="mdi-sync" variant="text" :aria-label="t('有 {n} 项操作正在同步', { n: syncInflightCount })" @click="openSyncPanel" />
          </v-badge>
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
          <v-fade-transition hide-on-leave>
          <div v-if="selectionActive" key="selection" class="d-flex align-center ga-1 mx-n2">
            <v-btn icon="mdi-close" variant="text" :aria-label="t('取消选择')" @click="clearSelectionIds" />
            <v-toolbar-title class="ml-4">{{ selectedIds.size }}</v-toolbar-title>
            <v-btn :icon="scope === 'scheduled' ? 'mdi-file-document-edit-outline' : (scope === 'trash' || scope === 'junk') ? 'mdi-inbox' : 'mdi-archive'" variant="text" :disabled="scope === 'archive'" :aria-label="scope === 'scheduled' ? t('退回草稿箱') : (scope === 'trash' || scope === 'junk') ? t('移回收件箱') : t('归档')" @click="bulkAct('archive')" />
            <v-btn icon="mdi-delete" variant="text" :aria-label="t('删除')" @click="bulkAct('delete')" />
            <v-btn v-if="scope !== 'scheduled'" :icon="selectionHasUnread ? 'mdi-email-open' : 'mdi-email'" variant="text" :aria-label="selectionHasUnread ? t('标为已读') : t('标为未读')" @click="bulkAct(selectionHasUnread ? 'seen' : 'unseen')" />
          </div>
          <div v-else-if="mobileSearch || searching" key="search" class="search-wrap">
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
          <div v-else key="normal" class="d-flex align-center ga-1 mx-n2">
            <v-btn icon="mdi-menu" variant="text" :aria-label="t('展开菜单')" @click="toggleDrawer" />
            <v-toolbar-title class="ml-4">{{ mobileBarTitle }}</v-toolbar-title>
            <v-badge v-if="attentionCount > 0" :content="attentionCount" color="error">
              <v-btn icon="mdi-alert-circle" variant="text" color="error" :aria-label="t('有 {n} 项操作没有完成', { n: attentionCount })" @click="openSyncPanel" />
            </v-badge>
            <v-badge v-else-if="syncInflightCount > 0" :content="syncInflightCount">
              <v-btn icon="mdi-sync" variant="text" :aria-label="t('有 {n} 项操作正在同步', { n: syncInflightCount })" @click="openSyncPanel" />
            </v-badge>
            <v-btn icon="mdi-magnify" variant="text" :aria-label="t('搜索')" @click="openMobileSearch" />
          </div>
          </v-fade-transition>
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
                  <v-btn icon variant="text" size="small" :disabled="scope === 'archive'" @click="bulkAct('archive')">
                    <v-icon :icon="scope === 'scheduled' ? 'mdi-file-document-edit-outline' : (scope === 'trash' || scope === 'junk') ? 'mdi-inbox' : 'mdi-archive'" />
                    <v-tooltip activator="parent" location="bottom">{{ scope === 'scheduled' ? t('退回草稿箱') : (scope === 'trash' || scope === 'junk') ? t('移回收件箱') : t('归档') }}</v-tooltip>
                  </v-btn>
                  <v-btn icon variant="text" size="small" @click="bulkAct('delete')">
                    <v-icon icon="mdi-delete" />
                    <v-tooltip activator="parent" location="bottom">{{ t('删除') }}</v-tooltip>
                  </v-btn>
                  <v-btn v-if="scope !== 'scheduled'" icon variant="text" size="small" @click="bulkAct(selectionHasUnread ? 'seen' : 'unseen')">
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
                  {{ listSnap.kind === 'search' ? t('没有找到匹配的邮件。') : t('{scope}是空的', { scope: snapScopeLabel }) }}
                </p>
              </template>
              <template v-else>
                <template v-for="(m, idx) in displayMessages" :key="listSnap.kind !== 'search' && m.thread_key ? 't' + m.account_id + ':' + m.thread_key : m.id">
                  <div class="mail-row-wrap" :style="rowWrapStyle(m)">
                    <div
                      class="swipe-bg swipe-bg-left swipe-bg-delete"
                      :style="{ opacity: swipeId === m.id && swipeDx > 0 ? 1 : 0 }"
                    >
                      <v-icon icon="mdi-delete" size="20" />
                    </div>
                    <div
                      class="swipe-bg swipe-bg-right swipe-bg-archive"
                      :style="{ opacity: swipeId === m.id && swipeDx < 0 ? 1 : 0 }"
                    >
                      <v-icon :icon="scope === 'scheduled' ? 'mdi-file-document-edit-outline' : (scope === 'archive' || m.folder_special === 'trash' || m.folder_special === 'junk') ? 'mdi-inbox' : 'mdi-archive'" size="20" />
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
                    <v-avatar :color="avatarImg(m.from_email) ? '#FFFFFF' : avatarColor(m.from_email)" :size="listDensity === 'compact' ? 24 : 32" class="mail-avatar-d">
                      <v-img v-if="avatarImg(m.from_email)" :src="avatarImg(m.from_email)" alt="" />
                      <span v-else class="avatar-letter row-avatar-letter">{{ avatarLetter(m) }}</span>
                    </v-avatar>
                    <div class="row-main">
                      <div class="row-l1">
                        <span class="row-sender" :class="{ w700: !(m.flags & 1) }">
                          {{ m.from_name || m.from_email || t('(未知发件人)') }}
                        </span>
                        <span
                          v-if="!searching && (m.thread_count ?? 1) > 1"
                          class="row-thread"
                        >
                          {{ t('共 {n} 封', { n: m.thread_count }) }}<template v-if="m.thread_unread">{{ t('，{n} 未读', { n: m.thread_unread }) }}</template>
                        </span>
                        <v-icon v-if="m.folder_special === 'scheduled'" icon="mdi-clock-outline" size="14" class="row-time-icon" />
                        <span class="row-time">{{ m.op_status === 'in_flight' ? t('正在发送') : fmtListTime(m.date_ts) }}</span>
                      </div>
                      <div class="row-l2" :class="{ w600: !(m.flags & 1) }">
                        <span class="row-subject">{{ m.subject || t('(无主题)') }}</span>
                        <span v-if="listDensity === 'compact' && m.snippet" class="row-l2-snippet">{{ m.snippet }}</span>
                        <v-icon v-if="listDensity === 'compact' && m.has_attach" icon="mdi-paperclip" size="14" class="text-medium-emphasis flex-none" />
                      </div>
                      <div v-if="listDensity === 'standard'" class="row-l3">
                        <span class="row-snippet">{{ m.snippet }}</span>
                        <v-chip v-if="m.has_attach" class="row-chip" size="x-small" variant="outlined" prepend-icon="mdi-paperclip">
                          {{ t('附件') }}
                        </v-chip>
                      </div>
                      <div class="row-acct">
                        {{ m.account_label }}<template v-if="searching">{{ t('，') }}{{ m.folder_name }}</template>
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
                    <v-avatar v-else :color="avatarImg(m.from_email) ? '#FFFFFF' : avatarColor(m.from_email)" size="38" class="mail-avatar">
                      <v-img v-if="avatarImg(m.from_email)" :src="avatarImg(m.from_email)" alt="" />
                      <span v-else class="avatar-letter text-body-2 font-weight-medium">{{ avatarLetter(m) }}</span>
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
                          v-if="!searching && (m.thread_count ?? 1) > 1"
                          class="text-caption text-medium-emphasis text-no-wrap"
                        >
                          {{ t('共 {n} 封', { n: m.thread_count }) }}<template v-if="m.thread_unread">{{ t('，{n} 未读', { n: m.thread_unread }) }}</template>
                        </span>
                        <v-icon v-if="m.has_attach" icon="mdi-paperclip" size="14" class="text-medium-emphasis" />
                        <v-icon v-if="m.folder_special === 'scheduled'" icon="mdi-clock-outline" size="13" class="text-medium-emphasis" />
                          <span class="text-caption text-medium-emphasis text-no-wrap">{{ m.op_status === 'in_flight' ? t('正在发送') : fmtListTime(m.date_ts) }}</span>
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
                <Transition name="bar-title">
                  <span v-if="barTitleShow" class="reader-bar-title text-truncate">{{ barTitleText }}</span>
                </Transition>
                <v-spacer />
              </div>
              <div class="reader-scroll">
                <div data-overlayscrollbars-viewport class="reader-vp">
                  <div data-overlayscrollbars-content class="px-3 pb-3 px-sm-4 pb-sm-4">
                <div class="d-flex align-start ga-1">
                  <h2 :ref="setSubjectEl" class="text-h6 font-weight-bold mb-1 flex-1-1" style="overflow-wrap: anywhere">
                    {{ convo.subject || t('(无主题)') }}
                  </h2>
                </div>
                <p class="text-caption text-medium-emphasis mb-3">{{ t('共 {n} 封', { n: convo.msgs.length }) }}</p>
                <template v-if="convo.status === 'loading'">
                  <v-skeleton-loader type="list-item-three-line@12" class="skel-fixed" />
                </template>
                <v-alert v-else-if="convo.status === 'error'" type="error" variant="tonal" density="compact">
                  {{ t('会话加载失败') }}
                  <div class="mt-2">
                    <v-btn size="small" variant="outlined" @click="convoAnchorMsg && openConversation(convoAnchorMsg)">{{ t('重试') }}</v-btn>
                    <v-btn size="small" variant="text" @click="convo = null">{{ t('关闭') }}</v-btn>
                  </div>
                </v-alert>
                <div v-else class="convo-stack">
                  <div v-for="msg in convo.msgs" :key="msg.id" class="convo-card" :data-convo-card="msg.id">
                    <div v-ripple class="convo-card-head" @click="toggleCard(msg)">
                      <v-avatar :color="avatarImg(msg.from_email) ? '#FFFFFF' : avatarColor(msg.from_email)" size="32" class="flex-none">
                        <v-img v-if="avatarImg(msg.from_email)" :src="avatarImg(msg.from_email)" alt="" />
                        <span v-else class="avatar-letter text-caption font-weight-medium">{{ avatarLetter(msg) }}</span>
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
                          <v-btn icon variant="text" size="small" @click="toggleCardSeen(msg)">
                            <v-icon :icon="(msg.flags & 1) ? 'mdi-email' : 'mdi-email-open'" />
                            <v-tooltip activator="parent" location="bottom">
                              {{ (msg.flags & 1) ? t('标为未读') : t('标为已读') }}
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
                          <v-btn icon variant="text" size="small" :color="cardTrEntry(msg.id) ? 'primary' : undefined" :disabled="convoTr[msg.id]?.st === 'pending'" :aria-label="t('翻译')" @click="toggleCardTranslate(msg)">
                            <v-icon icon="mdi-translate" />
                            <v-tooltip activator="parent" location="bottom">
                              {{ cardTrEntry(msg.id) ? t('显示原文') : t('翻译') }}
                            </v-tooltip>
                          </v-btn>
                          <v-btn
                            v-if="scope !== 'archive'"
                            icon
                            variant="text"
                            size="small"
                           
                            @click="archiveFor(msg)"
                          >
                            <v-icon :icon="(msg.folder_special === 'trash' || msg.folder_special === 'junk') ? 'mdi-inbox' : 'mdi-archive'" />
                            <v-tooltip activator="parent" location="bottom">{{ msg.folder_special === 'junk' ? t('不是垃圾邮件') : msg.folder_special === 'trash' ? t('移回收件箱') : t('归档') }}</v-tooltip>
                          </v-btn>
                          <v-btn icon variant="text" size="small" @click="deleteFor(msg)">
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
                        <p v-if="convoDetails[msg.id].body_truncated" class="pt-1 text-body-2 text-medium-emphasis">
                          {{ t('这封邮件的部分内容无法解析，下面只显示能读取的部分。') }}
                        </p>
                        <template v-if="cardTrEntry(msg.id)">
                          <div v-if="cardTrEntry(msg.id).html" style="overflow: hidden">
                            <iframe
                              :title="t('邮件正文')"
                              class="d-block w-100 border-0"
                              style="height: 320px; background: transparent"
                              sandbox="allow-same-origin"
                              scrolling="no"
                              :srcdoc="frameCss + cardTrEntry(msg.id).html"
                              @load="cardFrameLoaded"
                            />
                          </div>
                          <div
                            v-else-if="cardTrEntry(msg.id).text"
                            class="py-2 text-body-2"
                            :class="{ 'reader-text': lgAndUp }"
                            :style="lgAndUp ? 'white-space: pre-wrap' : 'white-space: pre-wrap; line-height: 1.6'"
                          >
                            {{ cardTrEntry(msg.id).text }}
                          </div>
                          <p v-else class="py-4 text-body-2 text-medium-emphasis">{{ t('这封邮件没有可显示的正文。') }}</p>
                        </template>
                        <template v-else>
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
                      </template>
                      <div v-else-if="cardBodyErrors.has(msg.id)" class="py-2">
                        <p class="text-body-2 text-error">{{ t('正文加载失败') }}</p>
                        <v-btn size="small" variant="outlined" class="mt-1" @click.stop="loadCardBody(msg.id)">{{ t('重试') }}</v-btn>
                      </div>
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
                <Transition name="bar-title">
                  <span v-if="barTitleShow" class="reader-bar-title text-truncate">{{ barTitleText }}</span>
                </Transition>
                <v-spacer />
              </div>
              <div v-if="lgAndUp" class="reader-toolbar">
                <v-btn
                  v-if="scope !== 'archive'"
                  icon
                  variant="text"
                  size="small"
                 
                  @click="archiveFor(selected, true, true)"
                >
                  <v-icon :icon="(selected.folder_special === 'trash' || selected.folder_special === 'junk') ? 'mdi-inbox' : 'mdi-archive'" />
                  <v-tooltip activator="parent" location="bottom">{{ selected.folder_special === 'junk' ? t('不是垃圾邮件') : selected.folder_special === 'trash' ? t('移回收件箱') : t('归档') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" size="small" @click="deleteFor(selected, true, true)">
                  <v-icon icon="mdi-delete" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ scope === 'trash' || scope === 'junk' ? t('彻底删除') : t('删除') }}
                  </v-tooltip>
                </v-btn>
                <v-btn icon variant="text" size="small" @click="toggleSelectedSeen">
                  <v-icon :icon="(selected.flags & 1) ? 'mdi-email' : 'mdi-email-open'" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ (selected.flags & 1) ? t('标为未读') : t('标为已读') }}
                  </v-tooltip>
                </v-btn>
                <v-btn icon variant="text" size="small" :color="trActive ? 'primary' : undefined" :disabled="!detail || trBusy" :aria-label="t('翻译')" @click="toggleTranslate">
                  <v-icon icon="mdi-translate" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ trActive ? t('显示原文') : t('翻译') }}
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
                <Transition name="bar-title">
                  <span v-if="barTitleShow" class="reader-bar-title text-truncate">{{ barTitleText }}</span>
                </Transition>
                <v-spacer />
                <v-btn icon="mdi-printer" variant="text" size="small" :disabled="!detail?.html" :aria-label="t('打印')" @click="printSelected" />
              </div>
              <div class="reader-scroll">
                <div data-overlayscrollbars-viewport class="reader-vp">
                  <div data-overlayscrollbars-content class="px-3 pb-3 px-sm-4 pb-sm-4">
              <template v-if="lgAndUp">
                <div class="reader-head-block">
                  <h2 :ref="setSubjectEl" class="reader-subject">{{ shownSubject || t('(无主题)') }}</h2>
                  <div class="reader-from-row">
                    <v-avatar :color="avatarImg(selected.from_email) ? '#FFFFFF' : avatarColor(selected.from_email)" size="40" class="flex-none">
                      <v-img v-if="avatarImg(selected.from_email)" :src="avatarImg(selected.from_email)" alt="" />
                      <span v-else class="avatar-letter" style="font-size: 16px">{{ avatarLetter(selected) }}</span>
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
                    <v-btn variant="tonal" size="small" prepend-icon="mdi-reply" :disabled="!detail" @click="replySelected">
                      {{ t('回复') }}
                    </v-btn>
                    <v-btn variant="tonal" size="small" prepend-icon="mdi-share" :disabled="!detail" @click="forwardSelected">
                      {{ t('转发') }}
                    </v-btn>
                  </div>
                </div>
              </template>
              <template v-else>
              <h2 :ref="setSubjectEl" class="text-h6 font-weight-bold mb-2" style="overflow-wrap: anywhere">
                {{ shownSubject || t('(无主题)') }}
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
                <v-btn icon variant="text" @click="toggleSelectedSeen">
                  <v-icon :icon="(selected.flags & 1) ? 'mdi-email' : 'mdi-email-open'" />
                  <v-tooltip activator="parent" location="bottom">
                    {{ (selected.flags & 1) ? t('标为未读') : t('标为已读') }}
                  </v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="!detail" @click="replySelected">
                  <v-icon icon="mdi-reply" />
                  <v-tooltip activator="parent" location="bottom">{{ t('回复') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :disabled="!detail" @click="forwardSelected">
                  <v-icon icon="mdi-share" />
                  <v-tooltip activator="parent" location="bottom">{{ t('转发') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" :color="trActive ? 'primary' : undefined" :disabled="!detail || trBusy" :aria-label="t('翻译')" @click="toggleTranslate">
                  <v-icon icon="mdi-translate" />
                  <v-tooltip activator="parent" location="bottom">{{ trActive ? t('显示原文') : t('翻译') }}</v-tooltip>
                </v-btn>
                <v-btn
                  v-if="scope !== 'archive'"
                  icon
                  variant="text"
                 
                  @click="archiveFor(selected, true, true)"
                >
                  <v-icon :icon="(selected.folder_special === 'trash' || selected.folder_special === 'junk') ? 'mdi-inbox' : 'mdi-archive'" />
                  <v-tooltip activator="parent" location="bottom">{{ selected.folder_special === 'junk' ? t('不是垃圾邮件') : selected.folder_special === 'trash' ? t('移回收件箱') : t('归档') }}</v-tooltip>
                </v-btn>
                <v-btn icon variant="text" @click="deleteFor(selected, true, true)">
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
                  <p v-if="detail.body_truncated" class="pt-2 text-body-2 text-medium-emphasis">
                    {{ t('这封邮件的部分内容无法解析，下面只显示能读取的部分。') }}
                  </p>
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
                  <div v-if="shownHtml" style="overflow: hidden">
                    <iframe
                      ref="frameEl"
                      :title="t('邮件正文')"
                      class="mt-2 d-block w-100 border-0"
                      style="height: 480px; background: transparent"
                      sandbox="allow-same-origin"
                      scrolling="no"
                      :srcdoc="frameCss + shownHtml"
                      @load="frameLoaded"
                    />
                  </div>
                  <div
                    v-else-if="shownText"
                    class="py-3 text-body-2"
                    :class="{ 'reader-text': lgAndUp }"
                    :style="lgAndUp ? 'white-space: pre-wrap' : 'white-space: pre-wrap; line-height: 1.6'"
                  >
                    {{ shownText }}
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
          <v-btn color="primary" variant="text" @click="undoRemoval">{{ t('撤销') }}</v-btn>
        </template>
      </v-snackbar>
      <v-dialog v-model="syncOpen" max-width="560" scrollable>
        <v-card>
          <v-card-title>{{ t('同步状态') }}</v-card-title>
          <v-card-text>
            <div v-if="attentionItems === null && pendingItems === null && !attentionError && !pendingError" class="d-flex justify-center pa-6">
              <v-progress-circular indeterminate />
            </div>
            <p v-else-if="attentionError || pendingError" class="text-body-2 text-error">
              {{ t('加载失败：{error}', { error: attentionError || pendingError }) }}
              <v-btn variant="text" size="small" @click="loadSyncPanel">{{ t('重试') }}</v-btn>
            </p>
            <template v-else>
              <p v-if="attentionItems.length === 0 && syncQueueRows.length === 0" class="text-body-2 text-medium-emphasis">
                {{ t('没有未完成或正在同步的操作') }}
              </p>
              <v-list v-if="attentionItems.length" density="comfortable">
                <v-list-item v-for="it in attentionItems" :key="it.op_id">
                  <v-list-item-title>{{ attentionTitle(it) }}</v-list-item-title>
                  <v-list-item-subtitle v-if="attentionDetail(it)">{{ attentionDetail(it) }}</v-list-item-subtitle>
                  <p class="text-body-2 text-error mt-1">{{ t('原因：') }}<RawError :text="it.error" full /></p>
                  <template #append>
                    <div class="d-flex flex-column ga-1">
                      <v-btn size="small" variant="tonal" @click="retryAttention(it)">{{ t('重试') }}</v-btn>
                      <v-btn size="small" variant="text" color="error" @click="discardAttention(it)">{{ t('丢弃') }}</v-btn>
                    </div>
                  </template>
                </v-list-item>
              </v-list>
              <v-divider v-if="attentionItems.length && syncQueueRows.length" />
              <v-list v-if="syncQueueRows.length" density="comfortable">
                <v-list-item v-for="row in syncQueueRows" :key="row.rowKey">
                  <v-list-item-title>{{ syncRowTitle(row) }}</v-list-item-title>
                  <v-list-item-subtitle>{{ syncRowSubtitle(row) }}</v-list-item-subtitle>
                  <template v-if="row.scheduled" #append>
                    <v-btn size="small" variant="text" color="error" @click="undoPendingSend(row)">{{ t('撤销发送') }}</v-btn>
                  </template>
                </v-list-item>
              </v-list>
            </template>
          </v-card-text>
          <v-card-actions>
            <v-spacer />
            <v-btn variant="text" @click="syncOpen = false">{{ t('关闭') }}</v-btn>
          </v-card-actions>
        </v-card>
      </v-dialog>

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
          <v-toolbar color="transparent" class="settings-topbar">
            <v-btn :icon="lgAndUp ? 'mdi-close' : 'mdi-arrow-left'" variant="text" :aria-label="lgAndUp ? t('关闭') : t('返回')" @click="closeSettings()" />
            <v-toolbar-title>{{ t('设置') }}</v-toolbar-title>
          </v-toolbar>
          <v-card-text class="pa-0">
            <Accounts
              :accounts="accounts"
              :accounts-status="accountsStatus"
              :statuses="statuses"
              :theme-mode="themeMode"
              :undo-seconds="undoSeconds"
              :version="appVersion"
              @changed="refreshAccounts"
              @update:theme-mode="setThemeMode"
              @update:undo-seconds="onUndoSecondsChange"
              @app-settings="onAppSettings"
              @revoked="onSessionsRevoked"
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

      <Compose v-model="composeOpen" :z-index="composeZ" :accounts="accounts" :initial="composeInit" :undo-seconds="undoSeconds" :max-attachment-mb="maxAttachmentMB" :op-event="lastOpEvent" :resume-send="resumeSend" @done="onComposeDone" @discarded="onDiscarded" />
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
  border-bottom: 1px solid transparent;
}
.app-topbar.bar-scrolled {
  border-bottom-color: rgb(var(--v-theme-outline-variant));
}
.settings-topbar {
  border-bottom: 1px solid transparent;
}
.settings-topbar.bar-scrolled {
  border-bottom-color: rgb(var(--v-theme-outline-variant));
}
.settings-tabs {
  border-bottom: 1px solid transparent;
}
.settings-tabs.bar-scrolled {
  border-bottom-color: rgb(var(--v-theme-outline-variant));
}
@media (max-width: 1279.98px) {
  .settings-topbar.bar-scrolled {
    border-bottom-color: transparent;
  }
}
.settings-card {
  display: flex;
  flex-direction: column;
}
@media (min-width: 1280px) {
  .v-dialog--scrollable > .v-overlay__content > .settings-card {
    flex: none;
  }
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
  height: 105px;
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
  color: #fff;
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
.row-acct {
  font-size: 12px;
  line-height: 20px;
  color: rgb(var(--v-theme-on-surface-variant));
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.density-compact .mail-row-d {
  height: 85px;
  padding: 8px 12px;
  border-bottom: 1px solid rgb(var(--v-theme-outline-variant));
}
.density-compact .row-avatar-letter {
  font-size: 10px;
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
  .bar-title-enter-active,
  .bar-title-leave-active,
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
  height: 64px;
  flex: none;
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 0 8px;
  border-bottom: 1px solid transparent;
  background: rgb(var(--v-theme-surface));
}
.reader-toolbar.bar-scrolled {
  border-bottom-color: rgb(var(--v-theme-outline-variant));
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
  height: 64px;
  box-sizing: border-box;
  padding: 0 6px;
  border-bottom: 1px solid transparent;
}
.reader-head.bar-scrolled {
  border-bottom-color: rgb(var(--v-theme-outline-variant));
}
.reader-bar-title {
  font-size: 15px;
  font-weight: 500;
  min-width: 0;
  margin-left: 2px;
}
.reader-toolbar .reader-bar-title {
  margin-left: 8px;
}
.bar-title-enter-active {
  transition: opacity 0.18s ease-out, transform 0.18s ease-out;
}
.bar-title-leave-active {
  transition: opacity 0.18s ease-in, transform 0.18s ease-in;
}
.bar-title-enter-from,
.bar-title-leave-to {
  opacity: 0;
  transform: translateY(4px);
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
  justify-content: flex-start;
}
.swipe-bg-right {
  justify-content: flex-end;
}
.swipe-bg-archive {
  background: rgb(var(--v-theme-secondary-container));
  color: rgb(var(--v-theme-on-secondary-container));
}
.swipe-bg-delete {
  background: rgb(var(--v-theme-error-container));
  color: rgb(var(--v-theme-on-error-container));
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
