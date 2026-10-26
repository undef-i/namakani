let flushHook = null
export function setFlushHook(fn) {
  flushHook = fn
}
function noteRoundTrip() {
  if (flushHook) queueMicrotask(() => flushHook())
}

async function req(path, opts = {}) {
  let res
  try {
    res = await fetch(path, {
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      ...opts,
    })
  } catch {
    const err = new Error('网络连接失败，请检查网络后重试')
    err.status = 0
    throw err
  }
  noteRoundTrip()
  if (!res.ok) {
    let msg = res.status + ' ' + res.statusText
    try {
      const j = await res.json()
      if (j && j.error) msg = j.error
    } catch {
    }
    const err = new Error(
      res.status === 401 && path !== '/api/login' ? '登录已过期，请重新登录' : msg,
    )
    err.status = res.status
    err.rawMessage = msg
    throw err
  }
  if (res.status === 204) return null
  return res.json()
}

export function describeRawError(raw) {
  const s = String(raw || '').toLowerCase()
  if (!s) return ''
  if (s.includes('authenticationfailed') || s.includes('authentication failed') || s.includes('invalid credentials') || s.includes('login failed'))
    return '账号登录被拒绝'
  if (s.includes('timeout') || s.includes('deadline exceeded')) return '连接超时'
  if (s.includes('connection refused') || s.includes('no such host') || s.includes('dial tcp') || s.includes('network is unreachable'))
    return '无法连接服务器'
  if (s.includes('certificate') || s.includes('tls')) return 'TLS 证书校验失败'
  if (s.includes('quota')) return '空间或配额不足'
  if (s.includes('rate limit') || s.includes('too many requests')) return '服务器限流'
  return ''
}

async function reqList(path) {
  let res
  try {
    res = await fetch(path, {
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
    })
  } catch {
    const err = new Error('网络连接失败，请检查网络后重试')
    err.status = 0
    throw err
  }
  noteRoundTrip()
  if (!res.ok) {
    let msg = res.status + ' ' + res.statusText
    try {
      const j = await res.json()
      if (j && j.error) msg = j.error
    } catch {
    }
    const err = new Error(res.status === 401 ? '登录已过期，请重新登录' : msg)
    err.status = res.status
    err.rawMessage = msg
    throw err
  }
  const items = await res.json()
  const n = Number(res.headers.get('X-WM-Ver'))
  return { items, wm: Number.isFinite(n) ? n : null }
}

const post = (path, body) => req(path, { method: 'POST', body: JSON.stringify(body ?? {}) })

export const getState = () => req('/api/state')
export const setup = (password) => post('/api/setup', { password })
export const login = (password) => post('/api/login', { password })
export const logout = () => post('/api/logout')
export const listAccounts = () => req('/api/accounts')
export const createAccount = (a) => post('/api/accounts', a)
export const updateAccount = (id, a) =>
  req('/api/accounts/' + id, { method: 'PUT', body: JSON.stringify(a) })
export const deleteAccount = (id) => req('/api/accounts/' + id, { method: 'DELETE' })
export const testAccountForm = (a) => post('/api/accounts/test', a)
export const testAccount = (id) => post('/api/accounts/' + id + '/test', {})
export const getSyncStatus = (id) => req('/api/accounts/' + id + '/sync')
export const reconnectAccount = (id, payload) => post('/api/accounts/' + id + '/reconnect', payload)
export const getAppSettings = () => req('/api/settings/app')
export const saveAppSettings = (b) =>
  req('/api/settings/app', { method: 'PUT', body: JSON.stringify(b) })
export const getTelegram = () => req('/api/settings/telegram')
export const saveTelegram = (b) =>
  req('/api/settings/telegram', { method: 'PUT', body: JSON.stringify(b) })
export const discoverTelegram = (bot_token) => post('/api/settings/telegram/discover', { bot_token })
export const testTelegram = (b) => post('/api/settings/telegram/test', b)
export const telegramWebAppAuth = (initData) => post('/api/telegram/webapp-auth', { init_data: initData })
export const getWebPush = () => req('/api/settings/webpush')
export const testWebPush = () => post('/api/settings/webpush/test')
export const subscribePush = (sub) => post('/api/push/subscribe', sub)
export const unsubscribePush = (endpoint) => post('/api/push/unsubscribe', { endpoint })
export const getFolders = () => req('/api/folders')
export const purgeFolder = (id) => post('/api/folders/' + id + '/purge')
export const getMessages = (special, offset = 0, limit = 60, threaded = false) =>
  reqList(
    '/api/messages?special=' +
      encodeURIComponent(special) +
      '&limit=' +
      limit +
      '&offset=' +
      offset +
      (threaded ? '&threaded=1' : ''),
  )
export const getFolderMessages = (folderId, offset = 0, limit = 60, threaded = false) =>
  reqList(
    '/api/messages?folder_id=' +
      folderId +
      '&limit=' +
      limit +
      '&offset=' +
      offset +
      (threaded ? '&threaded=1' : ''),
  )
export const getThreadMessagesAll = (accountId, threadKey) =>
  req(
    '/api/messages?thread_all=1&account_id=' +
      accountId +
      '&thread_key=' +
      encodeURIComponent(threadKey),
  )
export const moveMessageToFolder = (id, folderId, clientKey) =>
  post('/api/messages/' + id + '/move', { to_folder_id: folderId, client_key: clientKey || undefined })
let keySeq = 0
export const newClientKey = () =>
  'g' + Date.now().toString(36) + (keySeq++).toString(36) + Math.random().toString(36).slice(2, 8)
export const getThreadMessages = (special, accountId, threadKey) =>
  req(
    '/api/messages?special=' +
      encodeURIComponent(special) +
      '&account_id=' +
      accountId +
      '&thread_key=' +
      encodeURIComponent(threadKey),
  )
export const markAllRead = (special) => post('/api/messages/mark-all-read', { special })
export const getMessage = (id) => req('/api/messages/' + id)
export const setFlags = (id, body) => post('/api/messages/' + id + '/flags', body)
export const batchSetFlags = (ids, body) => post('/api/messages/flags', { ids, ...body })
export const moveMessage = (id, to, clientKey) =>
  post('/api/messages/' + id + '/move', { to, client_key: clientKey || undefined })

export const restoreMessage = (id, clientKey) =>
  post('/api/messages/' + id + '/restore', { client_key: clientKey || undefined })
export const deleteMessage = (id, clientKey) =>
  post('/api/messages/' + id + '/delete', { client_key: clientKey || undefined })
export const expungeMessage = (id, clientKey) =>
  post('/api/messages/' + id + '/delete', { permanent: true, client_key: clientKey || undefined })
export const undoOp = (opId) => post('/api/ops/' + opId + '/undo')
export const retryOp = (opId) => post('/api/ops/' + opId + '/retry')
export const discardOp = (opId) => req('/api/ops/' + opId, { method: 'DELETE' })
export const sendNowOp = (opId) => post('/api/ops/' + opId + '/send-now')
export const listAttention = () => req('/api/ops/attention')
export const listPending = () => req('/api/ops/pending')
export const opsByKeys = (keys) => post('/api/ops/by-keys', { keys })
export const attachmentURL = (id, attId) => '/api/messages/' + id + '/attachments/' + attId
export const sendMail = (body) => post('/api/send', body)
export const scheduleSend = (body) => post('/api/send/schedule', body)
export const listScheduled = () => req('/api/scheduled')
export const cancelScheduled = (id) => req('/api/scheduled/' + id, { method: 'DELETE' })
export const getScheduled = (id) => req('/api/scheduled/' + id)
export const updateScheduled = (id, body) =>
  req('/api/scheduled/' + id, { method: 'PUT', body: JSON.stringify(body) })
export const getTranslateSettings = () => req('/api/settings/translate')
export const saveTranslateSettings = (b) =>
  req('/api/settings/translate', { method: 'PUT', body: JSON.stringify(b) })
export const translateMessage = (id, target) => post('/api/messages/' + id + '/translate', { target })
export const fetchLLMModels = (b) => post('/api/translate/llm/models', b)
export const getTranslateModels = () => req('/api/translate/models')
export const resolveAvatars = (emails) => post('/api/avatars', { emails })
export const avatarImgURL = (email) => '/api/avatar/img?e=' + encodeURIComponent(email)
export const getContacts = () => req('/api/contacts')
export const saveDraft = (body) => post('/api/drafts', body)
export const searchMessages = (q, offset = 0, limit = 60) =>
  req('/api/search?q=' + encodeURIComponent(q) + '&limit=' + limit + '&offset=' + offset)
