export const AVATAR_COLORS = [
  '#E53935', '#D81B60', '#8E24AA', '#5E35B1',
  '#3949AB', '#1E88E5', '#039BE5', '#00ACC1',
  '#00897B', '#43A047', '#7CB342', '#C0CA33',
  '#FB8C00', '#F4511E', '#6D4C41', '#546E7A',
]

export function avatarColorFor(email) {
  const s = email || ''
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0
  return AVATAR_COLORS[h % AVATAR_COLORS.length]
}

export function avatarLetterFor(name, email) {
  const raw = (name || '').trim() || (email || '').split('@')[0].trim()
  if (!raw) return '?'
  const parts = raw.split(/[\s._\-]+/).filter(Boolean)
  if (parts.length >= 2) {
    const first = Array.from(parts[0])[0] || ''
    const last = Array.from(parts[parts.length - 1])[0] || ''
    return (first + last).toUpperCase()
  }
  return (Array.from(parts[0] || '?')[0] || '?').toUpperCase()
}
