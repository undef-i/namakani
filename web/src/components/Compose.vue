<script setup>
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useDisplay } from 'vuetify'
import { intlLocale, t } from '../i18n'
import { useEditor, EditorContent } from '@tiptap/vue-3'
import StarterKit from '@tiptap/starter-kit'
import Underline from '@tiptap/extension-underline'
import TextAlign from '@tiptap/extension-text-align'
import { TextStyle } from '@tiptap/extension-text-style'
import Color from '@tiptap/extension-color'
import Link from '@tiptap/extension-link'
import Image from '@tiptap/extension-image'
import * as api from '../api.js'

const maxAttachTotal = computed(() => props.maxAttachmentMB * 1024 * 1024)

const props = defineProps({
  modelValue: { type: Boolean, default: false },
  accounts: { type: Array, default: () => [] },
  initial: { type: Object, default: () => ({}) },
  undoSeconds: { type: Number, default: 5 },
  maxAttachmentMB: { type: Number, default: 20 },
})
const emit = defineEmits(['update:modelValue', 'done'])

const display = useDisplay()
const lgAndUp = computed(() => display.lgAndUp.value)

const open = computed({
  get: () => props.modelValue,
  set: (v) => emit('update:modelValue', v),
})

const accountId = ref('')
const toChips = ref([])
const ccChips = ref([])
const bccChips = ref([])
const toSearch = ref('')
const ccSearch = ref('')
const bccSearch = ref('')

function chipEnter(chips, search) {
  return (e) => {
    if (e.ctrlKey || e.metaKey) return
    const input = e.target
    if (
      input.getAttribute &&
      input.getAttribute('aria-expanded') === 'true' &&
      input.getAttribute('aria-activedescendant')
    ) {
      return
    }
    e.preventDefault()
    e.stopPropagation()
    const s = search.value.trim()
    if (s) {
      if (!chips.value.includes(s)) chips.value = [...chips.value, s]
      search.value = ''
    }
  }
}
const onToEnter = chipEnter(toChips, toSearch)
const onCcEnter = chipEnter(ccChips, ccSearch)
const onBccEnter = chipEnter(bccChips, bccSearch)
const subject = ref('')
const html = ref('')
const plainText = ref('')
const attachments = ref([])
const draftId = ref(0)
const busy = ref(false)
const sending = ref(false)
const error = ref('')
const attachError = ref('')
const warning = ref('')
const done = ref(false)
const savedAt = ref('')
const fileInput = ref(null)
const imageInput = ref(null)
const maximized = ref(false)
const dragOver = ref(false)
const linkMenu = ref(false)
const linkUrl = ref('')
const schedMenu = ref(false)
const schedCustom = ref(false)
const schedWhen = ref('')
const schedInfo = ref('')
const schedInfoShow = ref(false)
const undoShow = ref(false)
const undoLeft = ref(0)
const showCc = ref(false)
const contacts = ref([])
const contactsError = ref(false)
let saveTimer = null
let lastSavedJSON = ''
let lastSigHTML = ''
let initializing = false
let restoring = false
let undoTimer = null
let pendingPayload = null

const title = computed(() =>
  props.initial.draft_id ? t('编辑草稿') : props.initial.reply_to_id ? t('回复邮件') : t('写信'),
)

const contactItems = computed(() =>
  contacts.value.map((c) => (c.name ? c.name + ' <' + c.email + '>' : c.email)),
)

const editor = useEditor({
  extensions: [
    StarterKit,
    Underline,
    TextStyle,
    Color,
    TextAlign.configure({ types: ['heading', 'paragraph'] }),
    Link.configure({ openOnClick: false }),
    Image.configure({ allowBase64: true }),
  ],
  content: '',
  onUpdate: ({ editor: ed }) => {
    html.value = ed.getHTML()
    plainText.value = ed.getText()
  },
})

onBeforeUnmount(() => {
  editor.value?.destroy()
  clearInterval(undoTimer)
})

function escHTML(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

function textToHTML(t) {
  return escHTML(t)
    .split(/\n{2,}/)
    .map((p) => '<p>' + p.replace(/\n/g, '<br>') + '</p>')
    .join('')
}

function sigHTML(sig) {
  if (!sig || !sig.trim()) return ''
  return '<p>' + escHTML(sig).replace(/\n/g, '<br>') + '</p>'
}

function currentSigHTML() {
  const a = props.accounts.find((x) => String(x.id) === String(accountId.value))
  return a ? sigHTML(a.signature || '') : ''
}

function splitAddrStr(s) {
  if (!s) return []
  return s
    .split(',')
    .map((x) => x.trim())
    .filter(Boolean)
}

function allChips(chips, search) {
  const extra = (search || '').trim()
  return extra ? [...chips, extra] : [...chips]
}

function chipEmail(s) {
  const m = /<([^<>]+)>/.exec(s)
  return (m ? m[1] : s).trim()
}

function chipValid(s) {
  return /^[^\s@<>]+@[^\s@<>]+\.[^\s@<>]+$/.test(chipEmail(s))
}

function invalidOf(arr) {
  return arr.filter((c) => !chipValid(c))
}

const toInvalid = computed(() => invalidOf(toChips.value))
const ccInvalid = computed(() => invalidOf(ccChips.value))
const bccInvalid = computed(() => invalidOf(bccChips.value))

function snapshot() {
  return JSON.stringify({
    account_id: Number(accountId.value),
    to: allChips(toChips.value, toSearch.value).join(', '),
    cc: allChips(ccChips.value, ccSearch.value).join(', '),
    bcc: allChips(bccChips.value, bccSearch.value).join(', '),
    subject: subject.value,
    text: plainText.value,
    html: html.value,
    atts: attachments.value.map((a) => a.filename + ':' + a.size),
  })
}

function payload() {
  const ed = editor.value
  const h = ed ? ed.getHTML() : html.value
  const t = ed ? ed.getText() : plainText.value
  return {
    account_id: Number(accountId.value),
    to: allChips(toChips.value, toSearch.value).join(', '),
    cc: allChips(ccChips.value, ccSearch.value).join(', '),
    bcc: allChips(bccChips.value, bccSearch.value).join(', '),
    subject: subject.value,
    text: t,
    html: t.trim() === '' && !/<img/i.test(h) ? '' : h,
    reply_to_id: props.initial.reply_to_id ?? 0,
    draft_id: draftId.value,
    attachments: attachments.value.map((a) => ({
      filename: a.filename,
      content_type: a.content_type,
      data_b64: a.data_b64,
    })),
  }
}

function fmtSize(n) {
  if (n < 1024) return n + ' B'
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
  return (n / 1024 / 1024).toFixed(1) + ' MB'
}

function fmtClock(d) {
  const pad = (n) => String(n).padStart(2, '0')
  return pad(d.getHours()) + ':' + pad(d.getMinutes())
}

function fmtDateTime(d) {
  return new Intl.DateTimeFormat(intlLocale(), {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(d)
}

async function loadContacts() {
  contactsError.value = false
  try {
    contacts.value = await api.getContacts()
  } catch {
    contactsError.value = true
  }
}

watch(open, (isOpen) => {
  if (!isOpen) {
    clearTimeout(saveTimer)
    return
  }
  if (restoring) {
    restoring = false
    return
  }
  if (undoTimer) {
    clearInterval(undoTimer)
    undoTimer = null
    undoShow.value = false
    const p = pendingPayload
    pendingPayload = null
    if (p) realSend(p)
  }
  initializing = true
  const init = props.initial
  accountId.value = String(init.account_id ?? props.accounts[0]?.id ?? '')
  toChips.value = splitAddrStr(init.to ?? '')
  ccChips.value = splitAddrStr(init.cc ?? '')
  bccChips.value = splitAddrStr(init.bcc ?? '')
  showCc.value = ccChips.value.length > 0 || bccChips.value.length > 0
  toSearch.value = ''
  ccSearch.value = ''
  bccSearch.value = ''
  subject.value = init.subject ?? ''
  attachments.value = []
  draftId.value = init.draft_id ?? 0
  error.value = ''
  attachError.value = ''
  warning.value = ''
  done.value = false
  busy.value = false
  sending.value = false
  savedAt.value = ''
  maximized.value = false
  schedCustom.value = false
  let content = ''
  if (init.html) content = init.html
  else if (init.text) content = textToHTML(init.text)
  const sg = currentSigHTML()
  if (init.draft_id) {
    lastSigHTML = sg && content.endsWith(sg) ? sg : ''
  } else if (init.reply_to_id) {
    content = '<p></p>' + sg + content
    lastSigHTML = ''
  } else if (sg) {
    content = content + sg
    lastSigHTML = sg
  } else {
    lastSigHTML = ''
  }
  const ed = editor.value
  if (ed) {
    ed.commands.setContent(content || '<p></p>')
    html.value = ed.getHTML()
    plainText.value = ed.getText()
  } else {
    html.value = content
    plainText.value = ''
  }
  lastSavedJSON = snapshot()
  initializing = false
  loadContacts()
})

watch(accountId, () => {
  if (!open.value || initializing) return
  const ed = editor.value
  if (!ed) return
  const sg = currentSigHTML()
  let content = ed.getHTML()
  if (lastSigHTML) {
    if (content.endsWith(lastSigHTML)) {
      content = content.slice(0, content.length - lastSigHTML.length)
      ed.commands.setContent(content + sg)
      lastSigHTML = sg
    } else {
      lastSigHTML = ''
    }
  } else if (sg && !props.initial.reply_to_id && !props.initial.draft_id) {
    ed.commands.setContent(content + sg)
    lastSigHTML = sg
  }
})

async function addFiles(files) {
  attachError.value = ''
  let total = attachments.value.reduce((s, a) => s + a.size, 0)
  for (const f of files) {
    if (total + f.size > maxAttachTotal.value) {
      attachError.value = t('附件总大小超过 {mb} MB 上限，已跳过 {name}', { mb: props.maxAttachmentMB, name: f.name })
      continue
    }
    const dataUrl = await new Promise((resolve, reject) => {
      const rd = new FileReader()
      rd.onload = () => resolve(rd.result)
      rd.onerror = reject
      rd.readAsDataURL(f)
    })
    attachments.value = [
      ...attachments.value,
      {
        filename: f.name,
        size: f.size,
        content_type: f.type || 'application/octet-stream',
        data_b64: String(dataUrl).split(',')[1] ?? '',
      },
    ]
    total += f.size
  }
}

async function onFilesChosen(e) {
  const files = [...(e.target.files ?? [])]
  e.target.value = ''
  await addFiles(files)
}

function onDrop(e) {
  dragOver.value = false
  const files = [...(e.dataTransfer?.files ?? [])]
  if (files.length > 0) addFiles(files)
}

function onPaste(e) {
  const files = [...(e.clipboardData?.files ?? [])]
  if (files.length > 0) {
    e.preventDefault()
    addFiles(files)
  }
}

function removeAttachment(i) {
  attachments.value = attachments.value.filter((_, j) => j !== i)
}

function onImageChosen(e) {
  const f = e.target.files?.[0]
  e.target.value = ''
  if (!f) return
  const rd = new FileReader()
  rd.onload = () => {
    editor.value?.chain().focus().setImage({ src: String(rd.result) }).run()
  }
  rd.readAsDataURL(f)
}

function setColor(v) {
  editor.value?.chain().focus().setColor(v).run()
}

watch(linkMenu, (v) => {
  if (v) linkUrl.value = editor.value?.getAttributes('link').href || ''
})

function applyLink() {
  const ed = editor.value
  if (!ed) return
  const url = linkUrl.value.trim()
  if (url === '') {
    ed.chain().focus().unsetLink().run()
  } else {
    ed.chain().focus().setLink({ href: url }).run()
  }
  linkMenu.value = false
}

function removeLink() {
  editor.value?.chain().focus().unsetLink().run()
  linkMenu.value = false
}

async function autosave() {
  if (!open.value || done.value || busy.value || sending.value) return
  if (!accountId.value) return
  if (
    toChips.value.length === 0 &&
    ccChips.value.length === 0 &&
    bccChips.value.length === 0 &&
    !subject.value &&
    !plainText.value.trim() &&
    attachments.value.length === 0
  ) {
    return
  }
  const snap = snapshot()
  if (snap === lastSavedJSON) return
  busy.value = true
  try {
    const res = await api.saveDraft(payload())
    draftId.value = res.id
    lastSavedJSON = snapshot()
    savedAt.value = fmtClock(new Date())
    if (res.warning) warning.value = res.warning
  } catch {
  } finally {
    busy.value = false
  }
}

watch([accountId, toChips, ccChips, bccChips, subject, html, attachments], () => {
  if (!open.value) return
  clearTimeout(saveTimer)
  saveTimer = setTimeout(autosave, 2000)
}, { deep: true })

function validateRecipients() {
  const bad = [
    ...invalidOf(allChips(toChips.value, toSearch.value)),
    ...invalidOf(allChips(ccChips.value, ccSearch.value)),
    ...invalidOf(allChips(bccChips.value, bccSearch.value)),
  ]
  if (bad.length > 0) {
    error.value = t('地址格式不对：{list}', { list: bad.join(t('，')) })
    return false
  }
  if (
    toChips.value.length === 0 &&
    ccChips.value.length === 0 &&
    bccChips.value.length === 0 &&
    !toSearch.value.trim() &&
    !ccSearch.value.trim() &&
    !bccSearch.value.trim()
  ) {
    error.value = t('至少填写一个收件人')
    return false
  }
  return true
}

function restoreOpen() {
  restoring = true
  open.value = true
}

async function realSend(p) {
  sending.value = true
  busy.value = true
  try {
    const res = await api.sendMail(p)
    if (res.warning) {
      warning.value = res.warning
      done.value = true
      restoreOpen()
    } else {
      emit('done')
    }
  } catch (e) {
    error.value = e.message
    restoreOpen()
  } finally {
    busy.value = false
    sending.value = false
  }
}

function doSend() {
  error.value = ''
  if (!validateRecipients()) return
  clearTimeout(saveTimer)
  pendingPayload = payload()
  open.value = false
  if (!props.undoSeconds || props.undoSeconds <= 0) {
    const p = pendingPayload
    pendingPayload = null
    if (p) realSend(p)
    return
  }
  undoLeft.value = props.undoSeconds
  undoShow.value = true
  clearInterval(undoTimer)
  undoTimer = setInterval(() => {
    undoLeft.value -= 1
    if (undoLeft.value <= 0) {
      clearInterval(undoTimer)
      undoTimer = null
      undoShow.value = false
      const p = pendingPayload
      pendingPayload = null
      if (p) realSend(p)
    }
  }, 1000)
}

function undoSend() {
  clearInterval(undoTimer)
  undoTimer = null
  undoShow.value = false
  pendingPayload = null
  restoreOpen()
}

async function scheduleAt(date) {
  schedMenu.value = false
  schedCustom.value = false
  error.value = ''
  if (!validateRecipients()) return
  if (!(date.getTime() > Date.now())) {
    error.value = t('定时发送时间必须在将来')
    return
  }
  busy.value = true
  clearTimeout(saveTimer)
  try {
    await api.scheduleSend({ ...payload(), send_at: Math.floor(date.getTime() / 1000) })
    open.value = false
    schedInfo.value = t('已安排在 {time} 发送', { time: fmtDateTime(date) })
    schedInfoShow.value = true
    emit('done')
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}

function scheduleOption(kind) {
  const now = new Date()
  if (kind === 'h1') {
    scheduleAt(new Date(now.getTime() + 3600 * 1000))
  } else if (kind === 'tonight') {
    const d = new Date(now)
    d.setHours(20, 0, 0, 0)
    if (d <= now) d.setDate(d.getDate() + 1)
    scheduleAt(d)
  } else if (kind === 'tomorrow') {
    const d = new Date(now)
    d.setDate(d.getDate() + 1)
    d.setHours(8, 0, 0, 0)
    scheduleAt(d)
  }
}

function scheduleCustomGo() {
  if (!schedWhen.value) return
  scheduleAt(new Date(schedWhen.value))
}

async function discardCompose() {
  clearTimeout(saveTimer)
  if (draftId.value) {
    busy.value = true
    error.value = ''
    try {
      await api.deleteMessage(draftId.value)
    } catch (e) {
      error.value = t('删除草稿失败：{error}', { error: e.message })
      busy.value = false
      return
    }
    busy.value = false
  }
  open.value = false
  emit('done')
}

function closeAfterDone() {
  open.value = false
  emit('done')
}

function composeBlank() {
  return (
    toChips.value.length === 0 &&
    ccChips.value.length === 0 &&
    bccChips.value.length === 0 &&
    !subject.value.trim() &&
    !plainText.value.trim() &&
    attachments.value.length === 0
  )
}

async function closeCompose() {
  if (done.value) {
    closeAfterDone()
    return
  }
  clearTimeout(saveTimer)
  if (!composeBlank() && accountId.value && snapshot() !== lastSavedJSON) {
    busy.value = true
    error.value = ''
    try {
      const res = await api.saveDraft(payload())
      draftId.value = res.id
      lastSavedJSON = snapshot()
    } catch (e) {
      error.value = t('草稿保存失败：{error}', { error: e.message })
      busy.value = false
      return
    }
    busy.value = false
  }
  open.value = false
  emit('done')
}

function onKeydown(e) {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
    e.preventDefault()
    if (!done.value && !busy.value) doSend()
  }
}
</script>

<template>
  <div
    v-show="open"
    class="compose-root"
    :class="{
      'compose-root-mobile': !lgAndUp,
      'compose-max': maximized && lgAndUp,
    }"
    @keydown="onKeydown"
    @dragover.prevent="dragOver = true"
    @dragleave.prevent="dragOver = false"
    @drop.prevent="onDrop"
    @paste="onPaste"
  >
    <v-card class="compose-card">
      <div class="compose-head">
        <span class="text-body-2 font-weight-medium">{{ title }}</span>
        <v-spacer />
        <v-btn
          v-if="lgAndUp"
          icon
          size="small"
          variant="text"
          :aria-label="maximized ? t('还原') : t('全屏')"
          @click.stop="maximized = !maximized"
        >
          <v-icon :icon="maximized ? 'mdi-arrow-collapse' : 'mdi-arrow-expand'" />
        </v-btn>
        <v-btn icon="mdi-close" size="small" variant="text" :aria-label="t('关闭')" :disabled="busy" @click.stop="closeCompose" />
      </div>

      <v-card-text v-if="done">
        <p class="text-body-2" style="overflow-wrap: anywhere">{{ warning }}</p>
      </v-card-text>

      <div v-show="!done" class="compose-main">
        <div class="compose-body">
          <div class="cform">
            <div class="crow">
              <span class="clabel">{{ t('发件人') }}</span>
              <v-select
                v-model="accountId"
                variant="plain"
                density="compact"
                hide-details
                :items="accounts.map((a) => ({ title: (a.from_name || a.label) + ' <' + a.email + '>', value: String(a.id) }))"
                class="cfield"
              />
            </div>
            <div class="crow">
              <span class="clabel">{{ t('收件人') }}</span>
              <v-combobox
                @keydown.capture.enter="onToEnter"
                v-model="toChips"
                v-model:search="toSearch"
                :items="contactItems"
                variant="plain"
                density="compact"
                hide-details
                multiple
                chips
                closable-chips
                :delimiters="[',']"
                :error="toInvalid.length > 0"
                class="cfield"
              />
              <v-btn
                icon="mdi-chevron-down"
                size="x-small"
                variant="text"
                :aria-label="t('抄送和密送')"
                class="chev-btn"
                :class="{ 'chev-open': showCc }"
                @click="showCc = !showCc"
              />
            </div>
            <p v-if="toInvalid.length" class="cerr">{{ t('地址格式不对：{list}', { list: toInvalid.join(t('，')) }) }}</p>
            <p v-if="contactsError" class="cerr">
              {{ t('联系人加载失败，地址联想不可用') }}
              <v-btn variant="text" size="x-small" @click="loadContacts">{{ t('重试') }}</v-btn>
            </p>
            <div v-if="showCc" class="crow">
              <span class="clabel">{{ t('抄送') }}</span>
              <v-combobox
                @keydown.capture.enter="onCcEnter"
                v-model="ccChips"
                v-model:search="ccSearch"
                :items="contactItems"
                variant="plain"
                density="compact"
                hide-details
                multiple
                chips
                closable-chips
                :delimiters="[',']"
                :error="ccInvalid.length > 0"
                class="cfield"
              />
            </div>
            <p v-if="showCc && ccInvalid.length" class="cerr">{{ t('地址格式不对：{list}', { list: ccInvalid.join(t('，')) }) }}</p>
            <div v-if="showCc" class="crow">
              <span class="clabel">{{ t('密送') }}</span>
              <v-combobox
                @keydown.capture.enter="onBccEnter"
                v-model="bccChips"
                v-model:search="bccSearch"
                :items="contactItems"
                variant="plain"
                density="compact"
                hide-details
                multiple
                chips
                closable-chips
                :delimiters="[',']"
                :error="bccInvalid.length > 0"
                class="cfield"
              />
            </div>
            <p v-if="showCc && bccInvalid.length" class="cerr">{{ t('地址格式不对：{list}', { list: bccInvalid.join(t('，')) }) }}</p>
            <div class="crow">
              <v-text-field
                v-model="subject"
                :placeholder="t('主题')"
                variant="plain"
                density="compact"
                hide-details
                class="cfield"
              />
            </div>
          </div>

          <div class="compose-editor">
            <div class="compose-toolbar">
              <v-btn
                icon="mdi-undo"
                size="small"
                variant="text"
                :aria-label="t('撤销')"
                :disabled="!editor?.can().undo()"
                @click="editor?.chain().focus().undo().run()"
              />
              <v-btn
                icon="mdi-redo"
                size="small"
                variant="text"
                :aria-label="t('重做')"
                :disabled="!editor?.can().redo()"
                @click="editor?.chain().focus().redo().run()"
              />
              <v-divider vertical class="mx-1" />
              <v-btn
                icon="mdi-format-bold"
                size="small"
                :variant="editor?.isActive('bold') ? 'tonal' : 'text'"
                :aria-label="t('加粗')"
                @click="editor?.chain().focus().toggleBold().run()"
              />
              <v-btn
                icon="mdi-format-italic"
                size="small"
                :variant="editor?.isActive('italic') ? 'tonal' : 'text'"
                :aria-label="t('斜体')"
                @click="editor?.chain().focus().toggleItalic().run()"
              />
              <v-btn
                icon="mdi-format-underline"
                size="small"
                :variant="editor?.isActive('underline') ? 'tonal' : 'text'"
                :aria-label="t('下划线')"
                @click="editor?.chain().focus().toggleUnderline().run()"
              />
              <v-btn
                icon="mdi-format-strikethrough"
                size="small"
                :variant="editor?.isActive('strike') ? 'tonal' : 'text'"
                :aria-label="t('删除线')"
                @click="editor?.chain().focus().toggleStrike().run()"
              />
              <label class="color-pick" :aria-label="t('文字颜色')">
                <v-icon icon="mdi-format-color-text" size="20" />
                <input type="color" @input="setColor($event.target.value)" />
              </label>
              <v-divider vertical class="mx-1" />
              <v-btn
                icon="mdi-format-align-left"
                size="small"
                :variant="editor?.isActive({ textAlign: 'left' }) ? 'tonal' : 'text'"
                :aria-label="t('左对齐')"
                @click="editor?.chain().focus().setTextAlign('left').run()"
              />
              <v-btn
                icon="mdi-format-align-center"
                size="small"
                :variant="editor?.isActive({ textAlign: 'center' }) ? 'tonal' : 'text'"
                :aria-label="t('居中')"
                @click="editor?.chain().focus().setTextAlign('center').run()"
              />
              <v-btn
                icon="mdi-format-align-right"
                size="small"
                :variant="editor?.isActive({ textAlign: 'right' }) ? 'tonal' : 'text'"
                :aria-label="t('右对齐')"
                @click="editor?.chain().focus().setTextAlign('right').run()"
              />
              <v-divider vertical class="mx-1" />
              <v-btn
                icon="mdi-format-list-bulleted"
                size="small"
                :variant="editor?.isActive('bulletList') ? 'tonal' : 'text'"
                :aria-label="t('无序列表')"
                @click="editor?.chain().focus().toggleBulletList().run()"
              />
              <v-btn
                icon="mdi-format-list-numbered"
                size="small"
                :variant="editor?.isActive('orderedList') ? 'tonal' : 'text'"
                :aria-label="t('有序列表')"
                @click="editor?.chain().focus().toggleOrderedList().run()"
              />
              <v-btn
                icon="mdi-format-quote-close"
                size="small"
                :variant="editor?.isActive('blockquote') ? 'tonal' : 'text'"
                :aria-label="t('引用')"
                @click="editor?.chain().focus().toggleBlockquote().run()"
              />
              <v-menu v-model="linkMenu" :close-on-content-click="false" location="bottom start" z-index="2600">
                <template #activator="{ props: mp }">
                  <v-btn
                    v-bind="mp"
                    icon="mdi-link"
                    size="small"
                    :variant="editor?.isActive('link') ? 'tonal' : 'text'"
                    :aria-label="t('插入链接')"
                  />
                </template>
                <v-card width="300" class="pa-3">
                  <v-text-field
                    v-model="linkUrl"
                    :label="t('链接地址')"
                    density="compact"
                    variant="outlined"
                    hide-details
                    class="mb-2"
                    @keydown.enter.prevent="applyLink"
                  />
                  <div class="d-flex ga-2">
                    <v-btn size="small" color="primary" @click="applyLink">{{ t('确定') }}</v-btn>
                    <v-btn size="small" variant="text" @click="removeLink">{{ t('移除链接') }}</v-btn>
                  </div>
                </v-card>
              </v-menu>
              <v-btn
                icon="mdi-image"
                size="small"
                variant="text"
                :aria-label="t('插入图片')"
                @click="imageInput?.click()"
              />
              <input ref="imageInput" type="file" accept="image/*" class="d-none" @change="onImageChosen" />
              <v-btn
                icon="mdi-format-clear"
                size="small"
                variant="text"
                :aria-label="t('清除格式')"
                @click="editor?.chain().focus().unsetAllMarks().clearNodes().run()"
              />
            </div>
            <EditorContent :editor="editor" class="compose-editarea" />
          </div>

          <div class="compose-pad">
            <div class="d-flex align-center ga-2 mt-2 mb-2">
              <v-btn variant="outlined" size="small" :disabled="busy" @click="fileInput?.click()">
                {{ t('添加附件') }}
              </v-btn>
              <input ref="fileInput" type="file" multiple class="d-none" @change="onFilesChosen" />
              <span class="text-caption text-medium-emphasis">{{ t('也可拖拽文件到窗口，或粘贴图片') }}</span>
            </div>
            <v-list v-if="attachments.length > 0" density="compact" class="mb-2">
              <v-list-item v-for="(a, i) in attachments" :key="a.filename + ':' + i">
                <v-list-item-title class="text-body-2">{{ a.filename }}</v-list-item-title>
                <v-list-item-subtitle>{{ fmtSize(a.size) }}</v-list-item-subtitle>
                <template #append>
                  <v-btn variant="text" size="small" :disabled="busy" @click="removeAttachment(i)">
                    {{ t('移除') }}
                  </v-btn>
                </template>
              </v-list-item>
            </v-list>
            <v-alert v-if="attachError" type="error" variant="tonal" density="compact" class="mb-2">
              {{ attachError }}
            </v-alert>
            <v-alert v-if="error" type="error" variant="tonal" density="compact">
              {{ error }}
            </v-alert>
          </div>
        </div>

        <v-card-actions class="compose-actions">
          <v-btn
            icon="mdi-trash-can-outline"
            variant="text"
            color="error"
            :aria-label="t('删除草稿')"
            :disabled="busy || sending || undoShow"
            @click="discardCompose"
          >
            <v-icon icon="mdi-trash-can-outline" />
            <v-tooltip activator="parent" location="top">{{ t('删除草稿') }}</v-tooltip>
          </v-btn>
          <span v-if="savedAt" class="text-caption text-medium-emphasis ml-1">
            {{ t('草稿已保存 {time}', { time: savedAt }) }}
          </span>
          <v-spacer />
          <v-btn-group color="primary" variant="elevated" density="comfortable" class="ml-2">
            <v-btn :disabled="busy" :loading="sending" @click="doSend">
              {{ sending ? t('发送中') : t('发送') }}
            </v-btn>
            <v-menu v-model="schedMenu" :close-on-content-click="false" location="top end" z-index="2600">
              <template #activator="{ props: mp }">
                <v-btn v-bind="mp" icon="mdi-chevron-down" :disabled="busy" :aria-label="t('定时发送')" />
              </template>
              <v-card width="280" class="pa-2">
                <v-list density="compact">
                  <v-list-item :title="t('1 小时后')" @click="scheduleOption('h1')" />
                  <v-list-item :title="t('今晚 20:00')" @click="scheduleOption('tonight')" />
                  <v-list-item :title="t('明早 08:00')" @click="scheduleOption('tomorrow')" />
                  <v-list-item :title="t('自定义时间')" @click="schedCustom = true" />
                </v-list>
                <template v-if="schedCustom">
                  <v-text-field
                    v-model="schedWhen"
                    type="datetime-local"
                    density="compact"
                    variant="outlined"
                    hide-details
                    class="ma-2"
                  />
                  <v-btn block color="primary" size="small" class="mb-1" @click="scheduleCustomGo">
                    {{ t('安排发送') }}
                  </v-btn>
                </template>
              </v-card>
            </v-menu>
          </v-btn-group>
        </v-card-actions>
      </div>

      <v-card-actions v-if="done">
        <v-spacer />
        <v-btn color="primary" @click="closeAfterDone">{{ t('关闭') }}</v-btn>
      </v-card-actions>

      <div v-if="dragOver" class="compose-drop">{{ t('松开鼠标添加附件') }}</div>
    </v-card>
  </div>

  <v-snackbar v-model="undoShow" :timeout="-1" location="bottom left">
    {{ t('正在发送，{n} 秒后发出', { n: undoLeft }) }}
    <template #actions>
      <v-btn color="warning" variant="text" @click="undoSend">{{ t('撤销') }}</v-btn>
    </template>
  </v-snackbar>

  <v-snackbar v-model="schedInfoShow" :timeout="4000" location="bottom left">
    {{ schedInfo }}
  </v-snackbar>
</template>

<style>
.compose-root {
  position: fixed;
  right: 24px;
  bottom: 0;
  width: 560px;
  max-width: calc(100vw - 48px);
  z-index: 2500;
}
.compose-root-mobile {
  inset: 0;
  width: auto;
  max-width: none;
}
.compose-max {
  inset: 24px;
  width: auto;
  max-width: none;
}
.compose-card {
  position: relative;
  display: flex;
  flex-direction: column;
  height: min(660px, calc(100dvh - 88px));
  border-radius: 16px 16px 0 0;
  box-shadow: 0 10px 36px rgba(0, 0, 0, 0.35);
}
.compose-root-mobile .compose-card {
  height: 100%;
  border-radius: 0;
  box-shadow: none;
}
.compose-max .compose-card {
  height: 100%;
  border-radius: 16px;
}
.compose-head {
  display: flex;
  align-items: center;
  flex: none;
  padding: 6px 8px 6px 16px;
  background: rgba(var(--v-theme-on-surface), 0.06);
  border-radius: inherit;
}
.compose-main {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
}
.compose-body {
  flex: 1;
  overflow-y: auto;
  min-height: 0;
  padding: 12px 0;
}
.compose-pad {
  padding-left: 16px;
  padding-right: 16px;
}
.cform {
  padding: 0 16px;
}
.crow {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 42px;
  border-bottom: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
}
.clabel {
  flex: none;
  width: 44px;
  font-size: 13px;
  color: rgba(var(--v-theme-on-surface), 0.6);
}
.cfield {
  flex: 1;
  min-width: 0;
}
.cfield .v-field__input {
  min-height: 30px;
  padding-top: 3px;
  padding-bottom: 3px;
  font-size: 14px;
}
.cfield .v-chip {
  height: 24px;
  font-size: 12.5px;
}
.cfield .v-field--variant-plain .v-field__append-inner,
.cfield .v-field--variant-plain .v-field__clearable {
  padding-top: 4px;
}
.cerr {
  margin: 4px 0 0;
  font-size: 12px;
  color: rgb(var(--v-theme-error));
}
.chev-btn {
  flex: none;
}
.chev-btn .v-icon {
  transition: transform 0.15s ease;
}
.chev-open .v-icon {
  transform: rotate(180deg);
}
.compose-actions {
  flex: none;
  border-top: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
}
.compose-editor {
  margin: 0;
  border: none;
}
.compose-root-mobile .compose-pad {
  padding-left: 12px;
  padding-right: 12px;
}
.compose-root-mobile .cform {
  padding: 0 12px;
}
.compose-root-mobile .compose-editor {
  margin: 0;
  border-left: none;
  border-right: none;
  border-radius: 0;
}
.compose-toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  padding: 4px 10px;
  border-bottom: 1px solid rgba(var(--v-border-color), var(--v-border-opacity));
}
.compose-editarea .ProseMirror {
  min-height: 180px;
  max-height: 40dvh;
  overflow-y: auto;
  padding: 12px 16px;
  outline: none;
  font-size: 14px;
  line-height: 1.55;
}
.compose-root-mobile .compose-editarea .ProseMirror {
  max-height: none;
}
.compose-editarea .ProseMirror p {
  margin: 0 0 8px;
}
.compose-editarea .ProseMirror ul,
.compose-editarea .ProseMirror ol {
  padding-left: 22px;
  margin: 0 0 8px;
}
.compose-editarea .ProseMirror blockquote {
  margin: 0 0 8px;
  padding-left: 12px;
  border-left: 3px solid rgba(var(--v-border-color), var(--v-border-opacity));
}
.compose-editarea .ProseMirror img {
  max-width: 100%;
  height: auto;
}
.color-pick {
  position: relative;
  width: 34px;
  height: 34px;
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  cursor: pointer;
}
.color-pick:hover {
  background: rgba(var(--v-theme-on-surface), 0.08);
}
.color-pick input {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  opacity: 0;
  cursor: pointer;
}
.compose-drop {
  position: absolute;
  inset: 10px;
  z-index: 5;
  display: flex;
  align-items: center;
  justify-content: center;
  border: 2px dashed rgb(var(--v-theme-primary));
  border-radius: 12px;
  background: rgba(var(--v-theme-surface), 0.92);
  color: rgb(var(--v-theme-primary));
  font-weight: 600;
  pointer-events: none;
}
</style>
