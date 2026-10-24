import { OverlayScrollbars } from 'overlayscrollbars'
import 'overlayscrollbars/overlayscrollbars.css'

const SELECTOR = [
  '.list-scroll',
  '.v-navigation-drawer__content',
  '.settings-nav',
  '.settings-content',
  '.compose-body',
  '.compose-attach-chips',
  '.v-menu > .v-overlay__content',
  '.v-select__content',
  '.v-autocomplete__content',
  '.v-combobox__content',
  '.v-dialog--scrollable > .v-overlay__content > .v-card > .v-card-text',
  '.v-table__wrapper',
  '.v-virtual-scroll',
].join(',')

const done = new WeakSet()

function initEl(el) {
  if (done.has(el)) return
  done.add(el)
  const options = { scrollbars: { autoHide: 'leave', autoHideDelay: 600 } }
  if (el.classList.contains('v-table__wrapper')) {
    options.overflow = { x: 'scroll', y: 'scroll' }
  }
  try {
    OverlayScrollbars(el, options)
  } catch {}
}

export function scrollEl(el) {
  if (!el) return el
  const inst = OverlayScrollbars(el)
  return inst ? inst.elements().viewport : el
}

export function initScrollbars() {
  const scan = (root) => {
    if (!root || root.nodeType !== 1) return
    if (root.matches(SELECTOR)) initEl(root)
    root.querySelectorAll(SELECTOR).forEach(initEl)
  }
  scan(document.body)
  const mo = new MutationObserver((muts) => {
    for (const m of muts) {
      for (const n of m.addedNodes) scan(n)
    }
  })
  mo.observe(document.body, { childList: true, subtree: true })
}
