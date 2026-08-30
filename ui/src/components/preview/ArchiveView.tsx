import { useEffect, useMemo, useState } from 'react'
import clsx from 'clsx'
import { ChevronRight, FileArchive, Folder } from 'lucide-react'
import type { ViewerProps } from './PreviewModal'
import { formatBytes, formatDate, naturalCompare } from '../../lib/format'
import { EntryIcon } from '../icons'
import { Spinner } from '../primitives'

/**
 * Listing the contents of a zip without downloading it.
 *
 * A zip's index lives at the end of the file, so reading it needs exactly two
 * range requests — one for the end-of-central-directory record, one for the
 * directory itself — and the byte endpoint already serves ranges. Listing a
 * 20 GB archive therefore costs a few kilobytes, which is the difference
 * between this being useful and being a download with extra steps.
 *
 * Only zip is handled. RAR and 7z put their indexes in formats no
 * browser-side library reads well enough to be worth the bundle.
 */

interface ArchiveEntry {
  name: string
  directory: boolean
  size: number
  compressedSize: number
  date?: Date
}

export default function ArchiveView({ entry, link }: ViewerProps) {
  const [items, setItems] = useState<ArchiveEntry[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [path, setPath] = useState<string[]>([])

  const zipLike = entry.name.toLowerCase().endsWith('.zip')

  useEffect(() => {
    if (!zipLike) return
    let cancelled = false
    setItems(null)
    setError(null)
    setPath([])

    void (async () => {
      try {
        const zip = await import('@zip.js/zip.js')
        // HttpRangeReader is the whole reason this is cheap: it fetches only
        // the bytes the central directory occupies.
        const reader = new zip.ZipReader(new zip.HttpRangeReader(link.url, { useXHR: false }))
        const listed = await reader.getEntries()
        if (cancelled) return

        setItems(
          listed.map((item) => ({
            name: item.filename,
            directory: item.directory,
            size: item.uncompressedSize ?? 0,
            compressedSize: item.compressedSize ?? 0,
            date: item.lastModDate,
          })),
        )
        await reader.close()
      } catch (err) {
        if (!cancelled) {
          setError(
            err instanceof Error
              ? `无法读取压缩包目录：${err.message}`
              : '无法读取压缩包目录',
          )
        }
      }
    })()

    return () => {
      cancelled = true
    }
  }, [link.url, zipLike])

  const prefix = path.length > 0 ? path.join('/') + '/' : ''

  // The zip index is a flat list of full paths; the folder view is derived
  // from it rather than stored, which is also how the format itself works.
  const visible = useMemo(() => {
    if (!items) return []
    const folders = new Map<string, { size: number; count: number }>()
    const files: ArchiveEntry[] = []

    for (const item of items) {
      if (!item.name.startsWith(prefix)) continue
      const rest = item.name.slice(prefix.length).replace(/\/$/, '')
      if (!rest) continue

      const slash = rest.indexOf('/')
      if (slash < 0) {
        if (!item.directory) files.push({ ...item, name: rest })
      } else {
        const folder = rest.slice(0, slash)
        const current = folders.get(folder) ?? { size: 0, count: 0 }
        folders.set(folder, { size: current.size + item.size, count: current.count + 1 })
      }
    }

    const folderRows: ArchiveEntry[] = [...folders.entries()].map(([name, stats]) => ({
      name,
      directory: true,
      size: stats.size,
      compressedSize: 0,
    }))

    folderRows.sort((a, b) => naturalCompare(a.name, b.name))
    files.sort((a, b) => naturalCompare(a.name, b.name))
    return [...folderRows, ...files]
  }, [items, prefix])

  if (!zipLike) {
    return (
      <Message>
        目前只能在线浏览 zip 压缩包的目录。RAR / 7z 需要下载后用本地工具打开。
      </Message>
    )
  }
  if (error) return <Message tone="danger">{error}</Message>
  if (!items) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3">
        <Spinner className="size-5" />
        <p className="text-xs text-[var(--muted)]">正在读取压缩包目录…</p>
      </div>
    )
  }

  const total = items.filter((i) => !i.directory)
  const uncompressed = total.reduce((sum, i) => sum + i.size, 0)

  return (
    <div className="flex h-full flex-col">
      <div className="flex shrink-0 flex-wrap items-center gap-1 border-b border-[var(--line)] px-3 py-2 text-xs">
        <button
          onClick={() => setPath([])}
          className="flex items-center gap-1 rounded px-1.5 py-1 transition-colors hover:bg-[var(--sunk)]"
        >
          <FileArchive size={13} className="text-[var(--faint)]" />
          {entry.name}
        </button>
        {path.map((part, index) => (
          <span key={index} className="flex items-center gap-0.5">
            <ChevronRight size={12} className="text-[var(--faint)]" />
            <button
              onClick={() => setPath(path.slice(0, index + 1))}
              className="rounded px-1.5 py-1 transition-colors hover:bg-[var(--sunk)]"
            >
              {part}
            </button>
          </span>
        ))}
        <span className="ml-auto text-[var(--faint)]">
          {total.length} 个文件 · 解压后 {formatBytes(uncompressed)}
        </span>
      </div>

      <div className="min-h-0 flex-1 overflow-auto p-2">
        {visible.length === 0 ? (
          <p className="py-12 text-center text-xs text-[var(--muted)]">这个目录是空的</p>
        ) : (
          visible.map((item) => (
            <button
              key={item.name}
              disabled={!item.directory}
              onClick={() => item.directory && setPath([...path, item.name])}
              className={clsx(
                'row w-full !justify-between text-left',
                !item.directory && 'cursor-default hover:bg-transparent',
              )}
            >
              <span className="flex min-w-0 items-center gap-2.5">
                {item.directory ? (
                  <Folder size={16} className="shrink-0 text-[var(--color-clay)]" />
                ) : (
                  <EntryIcon name={item.name} isDir={false} size={16} />
                )}
                <span className="truncate text-sm">{item.name}</span>
              </span>
              <span className="flex shrink-0 items-center gap-3 text-xs text-[var(--muted)]">
                {item.date && <span className="hidden sm:inline">{formatDate(item.date.getTime())}</span>}
                <span className="tabular-nums">{formatBytes(item.size)}</span>
              </span>
            </button>
          ))
        )}
      </div>
    </div>
  )
}

function Message({ children, tone }: { children: React.ReactNode; tone?: 'danger' }) {
  return (
    <div className="flex h-full items-center justify-center p-8">
      <p
        className={clsx(
          'max-w-sm text-center text-sm leading-relaxed',
          tone === 'danger' ? 'text-[var(--color-danger)]' : 'text-[var(--muted)]',
        )}
      >
        {children}
      </p>
    </div>
  )
}
