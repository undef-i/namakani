async function req(path, opts = {}) {
  const res = await fetch(path, {
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  })
  if (!res.ok) {
    let msg = res.status + ' ' + res.statusText
    try {
      const j = await res.json()
      if (j && j.error) msg = j.error
    } catch {
    }
    throw new Error(msg)
  }
  if (res.status === 204) return null
  return res.json()
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
export const getSyncStatus = (id) => req('/api/accounts/' + id + '/sync')
export const getAppSettings = () => req('/api/settings/app')
export const saveAppSettings = (b) =>
  req('/api/settings/app', { method: 'PUT', body: JSON.stringify(b) })
export const getTelegram = () => req('/api/settings/telegram')
export const saveTelegram = (b) =>
  req('/api/settings/telegram', { method: 'PUT', body: JSON.stringify(b) })
export const discoverTelegram = (bot_token) => post('/api/settings/telegram/discover', { bot_token })
export const testTelegram = (b) => post('/api/settings/telegram/test', b)
export const getWebPush = () => req('/api/settings/webpush')
export const testWebPush = () => post('/api/settings/webpush/test')
export const subscribePush = (sub) => post('/api/push/subscribe', sub)
export const unsubscribePush = (endpoint) => post('/api/push/unsubscribe', { endpoint })
export const getFolders = () => req('/api/folders')
export const getMessages = (special, offset = 0, limit = 60, threaded = false) =>
  req(
    '/api/messages?special=' +
      encodeURIComponent(special) +
      '&limit=' +
      limit +
      '&offset=' +
      offset +
      (threaded ? '&threaded=1' : ''),
  )
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
export const moveMessage = (id, to) => post('/api/messages/' + id + '/move', { to })
export const deleteMessage = (id) => post('/api/messages/' + id + '/delete')
export const attachmentURL = (id, attId) => '/api/messages/' + id + '/attachments/' + attId
export const sendMail = (body) => post('/api/send', body)
export const scheduleSend = (body) => post('/api/send/schedule', body)
export const listScheduled = () => req('/api/scheduled')
export const cancelScheduled = (id) => req('/api/scheduled/' + id, { method: 'DELETE' })
export const getContacts = () => req('/api/contacts')
export const saveDraft = (body) => post('/api/drafts', body)
export const searchMessages = (q, limit = 60) =>
  req('/api/search?q=' + encodeURIComponent(q) + '&limit=' + limit)
