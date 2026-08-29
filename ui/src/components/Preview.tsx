import { useEffect, useState } from 'react'
import { Download, ExternalLink, FileQuestion } from 'lucide-react'
import { request, type Entry } from '../lib/api'
import { TEXT_PREVIEW_LIMIT, formatBytes, kindOf } from '../lib/format'
import { Button, Spinner } from './primitives'

interface Link {
  url: string
  download: string
}

/**
 * Preview renders a file inline.
 *
 * Video is the case that matters: the browser gets a plain <video src> pointed
 * at the range endpoint, so scrubbing across a segment boundary is an ordinary
 * range request and the player never learns the file is split. Nothing is
 * downloaded up front — the player asks for what it needs.
 */
export function Preview({ entry }: { entry: Entry }) {
  const [link, setLink] = useState<Link | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [text, setText] = useState<string | null>(null)

  const kind = kindOf(entry.name, entry.mime)

  useEffect(() => {
    let cancelled = false
    setLink(null)
    setText(null)
    setError(null)

    // A media link is a short-lived, single-file token: <video> and download
    // anchors cannot send an Authorization header.
    request<Link>(`/files/${entry.id}/link`)
      .then((l) => {
        if (cancelled) return
        setLink(l)
        if (kind === 'text' && entry.size <= TEXT_PREVIEW_LIMIT) {
          return fetch(l.url)
            .then((r) => r.text())
            .then((body) => {
              if (!cancelled) setText(body)
            })
        }
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      })

    return () => {
      cancelled = true
    }
  }, [entry.id, entry.size, kind])

  if (error) {
    return <div className="p-6 text-sm text-[var(--color-danger)]">{error}</div>
  }
  if (!link) {
    return (
      <div className="flex items-center justify-center p-10">
        <Spinner />
      </div>
    )
  }

  const frame = 'w-full rounded-[var(--radius-card)] border border-[var(--line)] bg-[var(--sunk)]'

  switch (kind) {
    case 'image':
      return (
        <img
          src={link.url}
          alt={entry.name}
          className={`${frame} max-h-[70vh] object-contain`}
          loading="lazy"
        />
      )

    case 'video':
      return (
        <video
          key={entry.id}
          src={link.url}
          controls
          preload="metadata"
          playsInline
          className={`${frame} max-h-[70vh] bg-black`}
        />
      )

    case 'audio':
      return <audio key={entry.id} src={link.url} controls preload="metadata" className="w-full" />

    case 'pdf':
      return (
        <iframe
          key={entry.id}
          src={link.url}
          title={entry.name}
          className={`${frame} h-[70vh]`}
        />
      )

    case 'text':
      if (entry.size > TEXT_PREVIEW_LIMIT) {
        return (
          <Unsupported
            entry={entry}
            link={link}
            reason={`文本预览上限 ${formatBytes(TEXT_PREVIEW_LIMIT)}，这个文件是 ${formatBytes(entry.size)}`}
          />
        )
      }
      if (text === null) {
        return (
          <div className="flex items-center justify-center p-10">
            <Spinner />
          </div>
        )
      }
      return (
        <pre
          className={`${frame} max-h-[70vh] overflow-auto p-4 font-[family-name:var(--font-mono)] text-xs leading-relaxed whitespace-pre-wrap`}
        >
          {text}
        </pre>
      )

    default:
      return <Unsupported entry={entry} link={link} reason="这种文件无法在浏览器里预览" />
  }
}

function Unsupported({
  entry,
  link,
  reason,
}: {
  entry: Entry
  link: Link
  reason: string
}) {
  return (
    <div className="surface flex flex-col items-center gap-3 px-6 py-10 text-center">
      <FileQuestion size={28} className="text-[var(--faint)]" />
      <p className="text-sm text-[var(--muted)]">{reason}</p>
      <div className="flex gap-2">
        <Button
          variant="primary"
          icon={<Download size={15} />}
          onClick={() => window.open(link.download, '_blank')}
        >
          下载 {formatBytes(entry.size)}
        </Button>
        <Button icon={<ExternalLink size={15} />} onClick={() => window.open(link.url, '_blank')}>
          新标签打开
        </Button>
      </div>
    </div>
  )
}
