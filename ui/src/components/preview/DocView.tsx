import { useEffect, useState } from 'react'
import type { ViewerProps } from './PreviewModal'
import { DOC_PREVIEW_LIMIT, formatBytes } from '../../lib/format'
import { Spinner } from '../primitives'

/**
 * Word documents, converted to HTML by mammoth.
 *
 * mammoth handles .docx only — the modern zip-based format — which covers
 * essentially everything written this decade. Older .doc is a completely
 * different binary format that no browser-side library reads, so it is
 * reported as such rather than shown as garbage.
 */
export default function DocView({ entry, link }: ViewerProps) {
  const [html, setHtml] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [warnings, setWarnings] = useState<string[]>([])

  const legacy = entry.name.toLowerCase().endsWith('.doc')
  const oversize = entry.size > DOC_PREVIEW_LIMIT

  useEffect(() => {
    if (legacy || oversize) return
    let cancelled = false
    setHtml(null)
    setError(null)

    void (async () => {
      try {
        const buffer = await fetch(link.url, { credentials: 'same-origin' }).then((r) =>
          r.arrayBuffer(),
        )
        if (cancelled) return

        const [mammoth, DOMPurify] = await Promise.all([import('mammoth'), import('dompurify')])
        const result = await mammoth.convertToHtml({ arrayBuffer: buffer })
        if (cancelled) return

        // mammoth's output is derived from the document, but the document came
        // from a user, so it is sanitised like any other untrusted markup.
        setHtml(DOMPurify.default.sanitize(result.value))
        setWarnings(result.messages.slice(0, 3).map((m) => m.message))
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      }
    })()

    return () => {
      cancelled = true
    }
  }, [link.url, legacy, oversize])

  if (legacy) {
    return (
      <Message>
        这是旧版 .doc 格式，浏览器里没法解析。转成 .docx 或下载后用 Word / WPS 打开。
      </Message>
    )
  }
  if (oversize) {
    return (
      <Message>
        文档预览上限是 {formatBytes(DOC_PREVIEW_LIMIT)}，这个文件有 {formatBytes(entry.size)}。
      </Message>
    )
  }
  if (error) {
    return (
      <div className="flex h-full items-center justify-center p-8">
        <p className="max-w-sm text-center text-sm text-[var(--color-danger)]">{error}</p>
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
      {warnings.length > 0 && (
        <div className="mx-auto max-w-3xl px-6 pt-4">
          <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs text-[var(--muted)]">
            部分格式没能完整还原：{warnings.join('；')}
          </p>
        </div>
      )}
      <article
        className="markdown-body mx-auto max-w-3xl px-6 py-8"
        dangerouslySetInnerHTML={{ __html: html }}
      />
    </div>
  )
}

function Message({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex h-full items-center justify-center p-8">
      <p className="max-w-sm text-center text-sm leading-relaxed text-[var(--muted)]">{children}</p>
    </div>
  )
}
