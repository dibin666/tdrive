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

/** formatDateTime is the absolute form, for tooltips and log tables where
 *  "3 天前" is not precise enough to act on. */
export function formatDateTime(ms: number): string {
  if (!ms) return '—'
  return new Date(ms).toLocaleString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return ''
  if (seconds < 60) return `${Math.ceil(seconds)} 秒`
  if (seconds < 3600) return `${Math.ceil(seconds / 60)} 分钟`
  return `${(seconds / 3600).toFixed(1)} 小时`
}

/** formatEta turns a remaining byte count and a speed into "还需 3 分钟". */
export function formatEta(remaining: number, bytesPerSecond: number): string {
  if (!Number.isFinite(remaining) || remaining <= 0) return ''
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return ''
  return formatDuration(remaining / bytesPerSecond)
}

/** kindOf classifies a file for icon choice and preview routing. */
export type FileKind =
  | 'image'
  | 'video'
  | 'audio'
  | 'pdf'
  | 'text'
  | 'code'
  | 'markdown'
  | 'sheet'
  | 'doc'
  | 'slides'
  | 'archive'
  | 'subtitle'
  | 'font'
  | 'ebook'
  | 'other'

const IMAGE_EXT = ['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'svg', 'ico', 'heic', 'heif', 'jxl', 'tif', 'tiff']
const VIDEO_EXT = ['mp4', 'mkv', 'mov', 'webm', 'avi', 'm4v', 'ts', 'm2ts', 'flv', 'wmv', 'mpg', 'mpeg', '3gp', 'ogv', 'rmvb']
const AUDIO_EXT = ['mp3', 'flac', 'wav', 'aac', 'ogg', 'oga', 'm4a', 'opus', 'wma', 'ape', 'alac', 'aiff']
const CODE_EXT = [
  'go', 'ts', 'tsx', 'js', 'jsx', 'mjs', 'cjs', 'py', 'rs', 'c', 'h', 'cpp', 'hpp', 'cc',
  'java', 'kt', 'swift', 'rb', 'php', 'cs', 'sh', 'bash', 'zsh', 'fish', 'sql', 'lua',
  'pl', 'r', 'scala', 'dart', 'vue', 'svelte', 'css', 'scss', 'less', 'html', 'htm',
  'json', 'yaml', 'yml', 'toml', 'ini', 'xml', 'gradle', 'dockerfile', 'makefile',
]
const TEXT_EXT = ['txt', 'log', 'csv', 'tsv', 'env', 'conf', 'cfg', 'properties', 'nfo']
const ARCHIVE_EXT = ['zip', 'rar', '7z', 'tar', 'gz', 'bz2', 'xz', 'zst', 'tgz', 'iso', 'cab']
const SUBTITLE_EXT = ['srt', 'ass', 'ssa', 'vtt', 'sub', 'lrc']

export function kindOf(name: string, mime?: string): FileKind {
  const ext = name.slice(name.lastIndexOf('.') + 1).toLowerCase()

  // The extension is consulted before the MIME type on purpose. Telegram
  // stores whatever was declared at upload time, and a file that arrived as
  // application/octet-stream is extremely common; the name is the more
  // reliable signal in practice.
  if (SUBTITLE_EXT.includes(ext)) return 'subtitle'
  if (ext === 'md' || ext === 'markdown' || ext === 'mdx') return 'markdown'
  if (['xlsx', 'xls', 'xlsm', 'ods'].includes(ext)) return 'sheet'
  if (['docx', 'doc', 'odt', 'rtf'].includes(ext)) return 'doc'
  if (['pptx', 'ppt', 'odp'].includes(ext)) return 'slides'
  if (['epub', 'mobi', 'azw3', 'fb2'].includes(ext)) return 'ebook'
  if (['ttf', 'otf', 'woff', 'woff2'].includes(ext)) return 'font'
  if (IMAGE_EXT.includes(ext)) return 'image'
  if (VIDEO_EXT.includes(ext)) return 'video'
  if (AUDIO_EXT.includes(ext)) return 'audio'
  if (ext === 'pdf') return 'pdf'
  if (ARCHIVE_EXT.includes(ext)) return 'archive'
  if (CODE_EXT.includes(ext)) return 'code'
  if (TEXT_EXT.includes(ext)) return 'text'

  const type = mime ?? ''
  if (type.startsWith('image/')) return 'image'
  if (type.startsWith('video/')) return 'video'
  if (type.startsWith('audio/')) return 'audio'
  if (type === 'application/pdf') return 'pdf'
  if (type === 'application/json') return 'code'
  if (type.startsWith('text/')) return 'text'
  return 'other'
}

/** previewable reports whether the preview modal has anything to show, so a
 *  double-click on an opaque binary opens the details rather than a spinner
 *  followed by a shrug. */
export function isPreviewable(kind: FileKind): boolean {
  return kind !== 'other' && kind !== 'font'
}

/** Files above this size are not previewed inline as text, because the whole
 *  body has to be fetched to show any of it. */
export const TEXT_PREVIEW_LIMIT = 512 * 1024

/** Office and archive previews parse the whole file in the browser, so they
 *  need a tighter ceiling than plain text does. */
export const DOC_PREVIEW_LIMIT = 32 * 1024 * 1024

/** naturalCompare sorts "第2集" before "第10集", which a plain string compare
 *  gets backwards and which is the single most noticeable sorting bug in a
 *  media library. */
export function naturalCompare(a: string, b: string): number {
  return a.localeCompare(b, 'zh-CN', { numeric: true, sensitivity: 'base' })
}
