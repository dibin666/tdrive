import { useEffect, useState } from 'react'
import type { ViewerProps } from './PreviewModal'
import { TEXT_PREVIEW_LIMIT, formatBytes } from '../../lib/format'
import { Spinner } from '../primitives'

/**
 * Markdown, rendered.
 *
 * The output is sanitised before it goes anywhere near the DOM. Markdown
 * permits raw HTML, and these files arrive from whoever uploaded them, so
 * rendering one is running someone else's markup inside an authenticated
 * session — exactly the shape of an XSS if the sanitiser is skipped.
 */
export default function MarkdownView({ entry, link }: ViewerProps) {
  const [html, setHtml] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const oversize = entry.size > TEXT_PREVIEW_LIMIT

  useEffect(() => {
    if (oversize) return
    let cancelled = false
    setHtml(null)
    setError(null)

    void (async () => {
      try {
        const body = await fetch(link.url, { credentials: 'same-origin' }).then((r) => r.text())
        if (cancelled) return

        const [{ marked }, DOMPurify] = await Promise.all([import('marked'), import('dompurify')])
        const rendered = await marked.parse(body, { gfm: true, breaks: false })
        const clean = DOMPurify.default.sanitize(rendered, {
          // Links are kept but forced to open elsewhere; scripts and event
          // handlers are dropped by the default profile.
          ADD_ATTR: ['target', 'rel'],
        })
        if (!cancelled) setHtml(clean)
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      }
    })()

    return () => {
      cancelled = true
    }
  }, [link.url, oversize])

  if (oversize) {
    return (
      <div className="flex h-full items-center justify-center p-8">
        <p className="max-w-sm text-center text-sm text-[var(--muted)]">
          文档超出预览大小上限（{formatBytes(TEXT_PREVIEW_LIMIT)}，当前 {formatBytes(entry.size)}）。请下载后查看。
        </p>
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex h-full items-center justify-center p-8">
        <p className="text-sm text-[var(--color-danger)]">{error}</p>
      </div>
    )
  }

  if (html === null) {
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner className="size-5" />
      </div>
    )
  }

  return (
    <div className="h-full overflow-auto">
      <article
        className="markdown-body mx-auto max-w-3xl px-6 py-8"
        // The content passed through DOMPurify immediately above.
        dangerouslySetInnerHTML={{ __html: html }}
      />
    </div>
  )
}
