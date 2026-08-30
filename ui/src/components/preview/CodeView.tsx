import { useEffect, useMemo, useState } from 'react'
import clsx from 'clsx'
import { Copy, Check, WrapText } from 'lucide-react'
import type { ViewerProps } from './PreviewModal'
import { TEXT_PREVIEW_LIMIT, formatBytes, kindOf } from '../../lib/format'
import { highlight, languageFor } from '../../lib/highlight'
import { IconButton, Spinner } from '../primitives'

/**
 * Text and source files, highlighted with shiki.
 *
 * Highlighting is a real improvement for the thing this is mostly used for —
 * checking that a config or a subtitle file is the one you thought it was —
 * but it is also the slowest part, so it is skipped entirely above a size
 * where it would lock the tab, and the plain text is shown instead.
 */

const HIGHLIGHT_LIMIT = 256 * 1024

export default function CodeView({ entry, link }: ViewerProps) {
  const [text, setText] = useState<string | null>(null)
  const [html, setHtml] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [wrap, setWrap] = useState(true)
  const [copied, setCopied] = useState(false)

  const language = useMemo(() => languageFor(entry.name), [entry.name])
  const oversize = entry.size > TEXT_PREVIEW_LIMIT

  useEffect(() => {
    if (oversize) return
    let cancelled = false
    setText(null)
    setHtml(null)
    setError(null)

    void fetch(link.url, { credentials: 'same-origin' })
      .then((res) => res.text())
      .then((body) => {
        if (cancelled) return
        setText(body)
        if (!language || body.length > HIGHLIGHT_LIMIT) return

        return highlight(body, language).then((rendered) => {
          if (!cancelled && rendered) setHtml(rendered)
        })
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      })

    return () => {
      cancelled = true
    }
  }, [link.url, language, oversize])

  if (oversize) {
    return (
      <div className="flex h-full items-center justify-center p-8">
        <p className="max-w-sm text-center text-sm text-[var(--muted)]">
          文本预览上限是 {formatBytes(TEXT_PREVIEW_LIMIT)}，这个文件有 {formatBytes(entry.size)}。
          下载后用编辑器打开吧。
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

  if (text === null) {
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner className="size-5" />
      </div>
    )
  }

  const lines = text.split('\n').length

  return (
    <div className="flex h-full flex-col">
      <div className="flex shrink-0 items-center gap-2 border-b border-[var(--line)] px-3 py-1.5">
        <span className="text-xs text-[var(--muted)]">
          {language ? language : kindOf(entry.name, entry.mime) === 'subtitle' ? '字幕' : '纯文本'}
          <span className="ml-2 tabular-nums text-[var(--faint)]">{lines} 行</span>
        </span>
        <div className="ml-auto flex items-center gap-1">
          <IconButton
            label={wrap ? '关闭自动换行' : '开启自动换行'}
            onClick={() => setWrap((v) => !v)}
            className={clsx(wrap && 'bg-[var(--sunk)]')}
          >
            <WrapText size={15} />
          </IconButton>
          <IconButton
            label="复制全文"
            onClick={() => {
              void navigator.clipboard.writeText(text)
              setCopied(true)
              setTimeout(() => setCopied(false), 1500)
            }}
          >
            {copied ? <Check size={15} /> : <Copy size={15} />}
          </IconButton>
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-auto bg-[var(--sunk)]">
        {html ? (
          <div
            className={clsx('shiki-host p-4 text-xs leading-relaxed', wrap && 'shiki-wrap')}
            dangerouslySetInnerHTML={{ __html: html }}
          />
        ) : (
          <pre
            className={clsx(
              'p-4 font-[family-name:var(--font-mono)] text-xs leading-relaxed',
              wrap ? 'whitespace-pre-wrap break-words' : 'whitespace-pre',
            )}
          >
            {text}
          </pre>
        )}
      </div>
    </div>
  )
}
