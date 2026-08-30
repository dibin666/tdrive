import { useEffect, useRef, useState } from 'react'
import { ChevronLeft, ChevronRight, Minus, Plus } from 'lucide-react'
import type { ViewerProps } from './PreviewModal'
import { IconButton, Spinner } from '../primitives'

/**
 * PDF rendering through pdf.js rather than an <iframe>.
 *
 * An iframe hands the file to whatever viewer the browser happens to ship,
 * which cannot be styled, does not exist on some mobile browsers, and — the
 * reason that actually matters here — downloads the whole document before
 * showing anything. pdf.js fetches by range, so the first page of a 300 MB
 * scan appears in a moment instead of after the whole file arrives.
 */
export default function PdfView({ entry, link }: ViewerProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const docRef = useRef<{ numPages: number; getPage: (n: number) => Promise<PdfPage> } | null>(null)
  const renderTask = useRef<{ cancel: () => void } | null>(null)

  const [page, setPage] = useState(1)
  const [pages, setPages] = useState(0)
  const [scale, setScale] = useState(1.2)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError(null)
    setPage(1)

    void (async () => {
      try {
        const pdfjs = await import('pdfjs-dist')
        // The worker is bundled by Vite rather than fetched from a CDN, so the
        // viewer works on a deployment with no outbound internet access.
        const worker = await import('pdfjs-dist/build/pdf.worker.mjs?url')
        pdfjs.GlobalWorkerOptions.workerSrc = worker.default

        const task = pdfjs.getDocument({ url: link.url, withCredentials: true })
        const doc = await task.promise
        if (cancelled) {
          // The loading task owns the worker; destroying it releases both.
          void task.destroy()
          return
        }
        docRef.current = doc as unknown as typeof docRef.current
        setPages(doc.numPages)
        setLoading(false)
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err))
          setLoading(false)
        }
      }
    })()

    return () => {
      cancelled = true
      renderTask.current?.cancel()
    }
  }, [link.url, entry.id])

  useEffect(() => {
    const doc = docRef.current
    const canvas = canvasRef.current
    if (!doc || !canvas || loading) return

    let cancelled = false
    void (async () => {
      try {
        renderTask.current?.cancel()
        const pdfPage = await doc.getPage(page)
        if (cancelled) return

        // Rendering at device pixel ratio keeps text crisp on a retina screen,
        // which is most of the point of rendering it ourselves.
        const dpr = Math.min(window.devicePixelRatio || 1, 2)
        const viewport = pdfPage.getViewport({ scale: scale * dpr })
        const context = canvas.getContext('2d')
        if (!context) return

        canvas.width = viewport.width
        canvas.height = viewport.height
        canvas.style.width = `${viewport.width / dpr}px`
        canvas.style.height = `${viewport.height / dpr}px`

        const task = pdfPage.render({ canvas, canvasContext: context, viewport })
        renderTask.current = task
        await task.promise
      } catch (err) {
        // A cancelled render is the normal result of paging quickly.
        if (!cancelled && err instanceof Error && !err.message.includes('cancel')) {
          setError(err.message)
        }
      }
    })()

    return () => {
      cancelled = true
    }
  }, [page, scale, loading])

  if (error) {
    return (
      <div className="flex h-full items-center justify-center p-8">
        <p className="max-w-sm text-center text-sm text-[var(--color-danger)]">{error}</p>
      </div>
    )
  }

  return (
    <div className="flex h-full flex-col">
      <div ref={containerRef} className="min-h-0 flex-1 overflow-auto bg-[var(--sunk)] p-4">
        {loading ? (
          <div className="flex h-full items-center justify-center">
            <Spinner className="size-5" />
          </div>
        ) : (
          <canvas ref={canvasRef} className="mx-auto shadow-md" />
        )}
      </div>

      <div className="flex shrink-0 items-center gap-1 border-t border-[var(--line)] px-3 py-1.5">
        <IconButton label="上一页" disabled={page <= 1} onClick={() => setPage((p) => Math.max(1, p - 1))}>
          <ChevronLeft size={15} />
        </IconButton>
        <span className="min-w-16 text-center text-xs tabular-nums text-[var(--muted)]">
          {page} / {pages || '—'}
        </span>
        <IconButton
          label="下一页"
          disabled={page >= pages}
          onClick={() => setPage((p) => Math.min(pages, p + 1))}
        >
          <ChevronRight size={15} />
        </IconButton>

        <div className="ml-auto flex items-center gap-1">
          <IconButton label="缩小" onClick={() => setScale((s) => Math.max(0.4, s - 0.2))}>
            <Minus size={15} />
          </IconButton>
          <span className="w-12 text-center text-xs tabular-nums text-[var(--muted)]">
            {Math.round(scale * 100)}%
          </span>
          <IconButton label="放大" onClick={() => setScale((s) => Math.min(4, s + 0.2))}>
            <Plus size={15} />
          </IconButton>
        </div>
      </div>
    </div>
  )
}

interface PdfPage {
  getViewport(options: { scale: number }): { width: number; height: number }
  render(options: {
    canvas: HTMLCanvasElement
    canvasContext: CanvasRenderingContext2D
    viewport: { width: number; height: number }
  }): { promise: Promise<void>; cancel: () => void }
}
