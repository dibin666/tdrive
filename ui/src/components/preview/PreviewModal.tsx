import { Suspense, lazy, useCallback, useEffect, useMemo, useState } from 'react'
import clsx from 'clsx'
import {
  ChevronLeft,
  ChevronRight,
  Download,
  ExternalLink,
  Info,
  Layers,
  X,
} from 'lucide-react'
import { request, type Entry } from '../../lib/api'
import { formatBytes, formatDate, isPreviewable, kindOf, type FileKind } from '../../lib/format'
import { EntryIcon } from '../icons'
import { IconButton, Spinner } from '../primitives'

/**
 * A full-screen previewer rather than a panel.
 *
 * The old inline preview had to share a 24rem sidebar with the file's
 * metadata, which meant a video played at postage-stamp size and a PDF was
 * unreadable. Making the preview the whole screen also makes arrow-key
 * navigation between files worth having, which turns "look at this file" into
 * "look through this folder" — the thing people actually do with a media
 * library.
 */

export interface PreviewLink {
  url: string
  download: string
}

/** Each renderer is loaded only when a file of its kind is opened. Between
 *  pdf.js, shiki and SheetJS there is far too much here to put in the initial
 *  bundle.
 *
 *  Video is deliberately absent. Browsers cannot play the containers a
 *  Telegram drive fills up with, and the demux-and-decode player that tried to
 *  work around that never played reliably enough to be worth its weight, so a
 *  video file offers the download instead of a player that fails. */
const VIEWERS: Partial<Record<FileKind, ReturnType<typeof lazy>>> = {
  image: lazy(() => import('./ImageView')),
  audio: lazy(() => import('./AudioView')),
  pdf: lazy(() => import('./PdfView')),
  text: lazy(() => import('./CodeView')),
  code: lazy(() => import('./CodeView')),
  subtitle: lazy(() => import('./CodeView')),
  markdown: lazy(() => import('./MarkdownView')),
  sheet: lazy(() => import('./SheetView')),
  doc: lazy(() => import('./DocView')),
  archive: lazy(() => import('./ArchiveView')),
}

export interface ViewerProps {
  entry: Entry
  link: PreviewLink
}

export function PreviewModal({
  entries,
  index,
  onIndexChange,
  onClose,
  onDownload,
}: {
  entries: Entry[]
  index: number
  onIndexChange: (next: number) => void
  onClose: () => void
  onDownload?: (entry: Entry) => void
}) {
  const entry = entries[index]
  const [link, setLink] = useState<PreviewLink | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [showInfo, setShowInfo] = useState(false)

  const kind = useMemo(() => (entry ? kindOf(entry.name, entry.mime) : 'other'), [entry])

  useEffect(() => {
    if (!entry) return
    let cancelled = false
    setLink(null)
    setError(null)

    // A media link is a short-lived, single-file token: <video>, <img> and a
    // download anchor cannot send an Authorization header.
    request<PreviewLink>(`/files/${entry.id}/link`)
      .then((next) => {
        if (!cancelled) setLink(next)
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      })

    return () => {
      cancelled = true
    }
  }, [entry?.id])

  const step = useCallback(
    (delta: number) => {
      if (entries.length < 2) return
      const next = (index + delta + entries.length) % entries.length
      onIndexChange(next)
    },
    [entries.length, index, onIndexChange],
  )

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // Typing in the code viewer's search box should not page through files.
      const target = e.target as HTMLElement | null
      if (target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA')) return

      if (e.key === 'Escape') onClose()
      else if (e.key === 'ArrowLeft') step(-1)
      else if (e.key === 'ArrowRight') step(1)
      else if (e.key === 'i' || e.key === 'I') setShowInfo((v) => !v)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose, step])

  if (!entry) return null

  const Viewer = VIEWERS[kind]

  return (
    <div className="fixed inset-0 z-[60] flex flex-col bg-[var(--bg)] fade-in">
      <header className="flex shrink-0 items-center gap-2 border-b border-[var(--line)] px-3 py-2 sm:px-4">
        <EntryIcon name={entry.name} mime={entry.mime} isDir={false} size={17} />
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-sm font-medium">{entry.name}</h2>
          <p className="truncate text-[11px] text-[var(--muted)]">
            {formatBytes(entry.size)} · {formatDate(entry.modifiedAt)}
            {entry.segmentCount && entry.segmentCount > 1 ? ` · ${entry.segmentCount} 卷` : ''}
          </p>
        </div>

        {entries.length > 1 && (
          <span className="hidden shrink-0 text-xs tabular-nums text-[var(--faint)] sm:block">
            {index + 1} / {entries.length}
          </span>
        )}
        <IconButton
          label="文件信息"
          onClick={() => setShowInfo((v) => !v)}
          className={clsx(showInfo && 'bg-[var(--sunk)]')}
        >
          <Info size={16} />
        </IconButton>
        {link && (
          <IconButton label="新标签打开" onClick={() => window.open(link.url, '_blank')}>
            <ExternalLink size={16} />
          </IconButton>
        )}
        <IconButton
          label="下载"
          onClick={() => (onDownload ? onDownload(entry) : window.open(link?.download, '_blank'))}
        >
          <Download size={16} />
        </IconButton>
        <IconButton label="关闭" onClick={onClose}>
          <X size={17} />
        </IconButton>
      </header>

      <div className="relative flex min-h-0 flex-1">
        {entries.length > 1 && (
          <>
            <NavButton side="left" onClick={() => step(-1)} />
            <NavButton side="right" onClick={() => step(1)} />
          </>
        )}

        <div className="min-h-0 min-w-0 flex-1 overflow-auto">
          {error ? (
            <Centered>
              <p className="text-sm text-[var(--color-danger)]">{error}</p>
            </Centered>
          ) : !link ? (
            <Centered>
              <Spinner className="size-5" />
            </Centered>
          ) : !Viewer || !isPreviewable(kind) ? (
            <Unsupported entry={entry} link={link} onDownload={onDownload} />
          ) : (
            <Suspense
              fallback={
                <Centered>
                  <Spinner className="size-5" />
                </Centered>
              }
            >
              <Viewer entry={entry} link={link} />
            </Suspense>
          )}
        </div>

        {showInfo && <InfoPanel entry={entry} />}
      </div>
    </div>
  )
}

function NavButton({ side, onClick }: { side: 'left' | 'right'; onClick: () => void }) {
  const Icon = side === 'left' ? ChevronLeft : ChevronRight
  return (
    <button
      onClick={onClick}
      aria-label={side === 'left' ? '上一个' : '下一个'}
      className={clsx(
        'absolute top-1/2 z-10 hidden -translate-y-1/2 rounded-full border border-[var(--line)] bg-[var(--surface)]/90 p-2 shadow-sm backdrop-blur transition-opacity hover:bg-[var(--sunk)] sm:block',
        side === 'left' ? 'left-3' : 'right-3',
      )}
    >
      <Icon size={18} />
    </button>
  )
}

function Centered({ children }: { children: React.ReactNode }) {
  return <div className="flex h-full items-center justify-center p-8">{children}</div>
}

function InfoPanel({ entry }: { entry: Entry }) {
  const [segments, setSegments] = useState<{ index: number; size: number; messageId: number }[]>([])

  useEffect(() => {
    void request<{ segments: { index: number; size: number; messageId: number }[] }>(
      `/files/${entry.id}/segments`,
    )
      .then((r) => setSegments(r.segments))
      .catch(() => setSegments([]))
  }, [entry.id])

  return (
    <aside className="hidden w-72 shrink-0 overflow-y-auto border-l border-[var(--line)] p-4 lg:block">
      <h3 className="mb-3 text-xs font-medium uppercase tracking-wide text-[var(--faint)]">
        文件信息
      </h3>
      <dl className="space-y-2 text-sm">
        <InfoRow label="大小" value={formatBytes(entry.size)} />
        <InfoRow label="类型" value={entry.mime || '未知'} />
        <InfoRow label="修改" value={formatDate(entry.modifiedAt)} />
        <InfoRow label="创建" value={formatDate(entry.createdAt)} />
        <InfoRow label="路径" value={entry.path} />
      </dl>

      {segments.length > 1 && (
        <div className="mt-5">
          <h3 className="mb-2 flex items-center gap-1.5 text-xs font-medium uppercase tracking-wide text-[var(--faint)]">
            <Layers size={12} />
            存储分卷 ({segments.length})
          </h3>
          <p className="mb-2 text-xs leading-relaxed text-[var(--muted)]">
            Telegram 单个对象上限 2 GB，这个文件被拆成 {segments.length} 条消息。播放和下载都会自动拼接。
          </p>
          <div className="space-y-1">
            {segments.map((segment) => (
              <div
                key={segment.index}
                className="flex items-center justify-between rounded-[var(--radius-control)] bg-[var(--sunk)] px-2.5 py-1.5 text-xs"
              >
                <span className="font-[family-name:var(--font-mono)] text-[var(--muted)]">
                  #{String(segment.index).padStart(2, '0')}
                </span>
                <span className="tabular-nums text-[var(--muted)]">{formatBytes(segment.size)}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </aside>
  )
}

function InfoRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="shrink-0 text-[var(--muted)]">{label}</dt>
      <dd className="min-w-0 truncate text-right" title={value}>
        {value}
      </dd>
    </div>
  )
}

function Unsupported({
  entry,
  link,
  onDownload,
}: {
  entry: Entry
  link: PreviewLink
  onDownload?: (entry: Entry) => void
}) {
  return (
    <Centered>
      <div className="max-w-sm text-center">
        <EntryIcon name={entry.name} mime={entry.mime} isDir={false} size={32} />
        <p className="mt-4 text-sm text-[var(--muted)]">这种文件没法在浏览器里预览</p>
        <div className="mt-5 flex justify-center gap-2">
          <button
            className="btn btn-primary"
            onClick={() => (onDownload ? onDownload(entry) : window.open(link.download, '_blank'))}
          >
            <Download size={15} />
            下载 {formatBytes(entry.size)}
          </button>
        </div>
      </div>
    </Centered>
  )
}
