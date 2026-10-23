export const AVATAR_COLORS = [
  '#F6AEA9', '#F8D7A0', '#FDE49C', '#BFE3C6', '#A8DAB5', '#A1E3D8',
  '#A9D6E5', '#AECBFA', '#C5CAE9', '#D7BDE2', '#F3C6D3', '#E6C9A8',
  '#D5D8DC', '#B2DFDB', '#FFCCBC', '#DCEDC8',
]

export function avatarColorFor(email) {
  const s = email || ''
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0
  return AVATAR_COLORS[h % AVATAR_COLORS.length]
}

export function avatarLetterFor(name, email) {
  const s = (name || email || '?').trim()
  return s ? s.charAt(0).toUpperCase() : '?'
}
