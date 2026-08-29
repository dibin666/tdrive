const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']

/** formatBytes uses binary units, matching how the server splits and how
 *  every file manager reports sizes. */
export function formatBytes(bytes: number, precision?: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—'
  if (bytes < 1024) return `${bytes} B`

  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024
    unit += 1
  }
  const digits = precision ?? (value >= 100 ? 0 : value >= 10 ? 1 : 2)
  return `${value.toFixed(digits)} ${UNITS[unit]}`
}

export function formatSpeed(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return ''
  return `${formatBytes(bytesPerSecond, 1)}/s`
}

/** formatDate shows a relative time for anything recent, since "3 分钟前"
 *  answers the question a timestamp only hints at. */
export function formatDate(ms: number): string {
  if (!ms) return '—'
  const date = new Date(ms)
  const diff = Date.now() - ms

  if (diff < 60_000) return '刚刚'
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`
  if (diff < 7 * 86_400_000) return `${Math.floor(diff / 86_400_000)} 天前`

  return date.toLocaleDateString('zh-CN', {
    year: date.getFullYear() === new Date().getFullYear() ? undefined : 'numeric',
    month: 'short',
    day: 'numeric',
  })
}

export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return ''
  if (seconds < 60) return `${Math.ceil(seconds)} 秒`
  if (seconds < 3600) return `${Math.ceil(seconds / 60)} 分钟`
  return `${(seconds / 3600).toFixed(1)} 小时`
}

/** kindOf classifies a file for icon choice and preview routing. */
export type FileKind = 'image' | 'video' | 'audio' | 'pdf' | 'text' | 'archive' | 'other'

export function kindOf(name: string, mime?: string): FileKind {
  const type = mime ?? ''
  if (type.startsWith('image/')) return 'image'
  if (type.startsWith('video/')) return 'video'
  if (type.startsWith('audio/')) return 'audio'
  if (type === 'application/pdf') return 'pdf'
  if (type.startsWith('text/') || type === 'application/json') return 'text'

  const ext = name.slice(name.lastIndexOf('.') + 1).toLowerCase()
  if (['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'svg'].includes(ext)) return 'image'
  if (['mp4', 'mkv', 'mov', 'webm', 'avi', 'm4v', 'ts'].includes(ext)) return 'video'
  if (['mp3', 'flac', 'wav', 'aac', 'ogg', 'm4a'].includes(ext)) return 'audio'
  if (ext === 'pdf') return 'pdf'
  if (['txt', 'md', 'log', 'json', 'yaml', 'yml', 'toml', 'ini', 'csv', 'xml',
       'go', 'ts', 'tsx', 'js', 'jsx', 'py', 'rs', 'c', 'h', 'cpp', 'sh'].includes(ext)) return 'text'
  if (['zip', 'rar', '7z', 'tar', 'gz', 'bz2', 'xz', 'zst'].includes(ext)) return 'archive'
  return 'other'
}

/** Files above this size are not previewed inline as text, because the whole
 *  body has to be fetched to show any of it. */
export const TEXT_PREVIEW_LIMIT = 512 * 1024
