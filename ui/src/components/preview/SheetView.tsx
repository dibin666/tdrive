import { useEffect, useState } from 'react'
import clsx from 'clsx'
import type { ViewerProps } from './PreviewModal'
import { DOC_PREVIEW_LIMIT, formatBytes } from '../../lib/format'
import { Spinner } from '../primitives'

/**
 * Spreadsheets, rendered as a table.
 *
 * The whole workbook has to be parsed in the browser, so this is capped by
 * size and by row count: the point is to answer "is this the right file and
 * what is in it", not to be a spreadsheet application.
 */

const MAX_ROWS = 500
const MAX_COLS = 40

interface SheetData {
  names: string[]
  rows: string[][]
  truncatedRows: boolean
  truncatedCols: boolean
}

export default function SheetView({ entry, link }: ViewerProps) {
  const [data, setData] = useState<SheetData | null>(null)
  const [sheet, setSheet] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [workbook, setWorkbook] = useState<unknown>(null)

  const oversize = entry.size > DOC_PREVIEW_LIMIT

  useEffect(() => {
    if (oversize) return
    let cancelled = false
    setData(null)
    setError(null)
    setSheet(0)

    void (async () => {
      try {
        const buffer = await fetch(link.url, { credentials: 'same-origin' }).then((r) =>
          r.arrayBuffer(),
        )
        if (cancelled) return
        const XLSX = await import('xlsx')
        const book = XLSX.read(buffer, { type: 'array' })
        if (cancelled) return
        setWorkbook(book)
        setData(extract(XLSX, book, 0))
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      }
    })()

    return () => {
      cancelled = true
    }
  }, [link.url, oversize])

  useEffect(() => {
    if (!workbook) return
    void import('xlsx').then((XLSX) => {
      setData(extract(XLSX, workbook as never, sheet))
    })
  }, [sheet, workbook])

  if (oversize) {
    return (
      <Message>
        表格预览上限是 {formatBytes(DOC_PREVIEW_LIMIT)}，这个文件有 {formatBytes(entry.size)}。
      </Message>
    )
  }
  if (error) return <Message tone="danger">{error}</Message>
  if (!data) {
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner className="size-5" />
      </div>
    )
  }

  return (
    <div className="flex h-full flex-col">
      {data.names.length > 1 && (
        <div className="flex shrink-0 gap-1 overflow-x-auto border-b border-[var(--line)] px-3 py-1.5">
          {data.names.map((name, index) => (
            <button
              key={name}
              onClick={() => setSheet(index)}
              className={clsx(
                'shrink-0 rounded-md px-2.5 py-1 text-xs transition-colors',
                index === sheet
                  ? 'bg-[var(--sunk)] font-medium text-[var(--ink)]'
                  : 'text-[var(--muted)] hover:text-[var(--ink)]',
              )}
            >
              {name}
            </button>
          ))}
        </div>
      )}

      <div className="min-h-0 flex-1 overflow-auto">
        <table className="min-w-full border-collapse text-xs">
          <tbody>
            {data.rows.map((row, y) => (
              <tr key={y} className={y === 0 ? 'sticky top-0 bg-[var(--surface)]' : ''}>
                <td className="sticky left-0 border border-[var(--line)] bg-[var(--sunk)] px-2 py-1 text-right tabular-nums text-[var(--faint)]">
                  {y + 1}
                </td>
                {row.map((cell, x) => (
                  <td
                    key={x}
                    className={clsx(
                      'max-w-[16rem] truncate border border-[var(--line)] px-2 py-1',
                      y === 0 && 'font-medium',
                    )}
                    title={cell}
                  >
                    {cell}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {(data.truncatedRows || data.truncatedCols) && (
        <p className="shrink-0 border-t border-[var(--line)] px-3 py-1.5 text-xs text-[var(--faint)]">
          只显示前 {MAX_ROWS} 行 / {MAX_COLS} 列，完整内容请下载后查看。
        </p>
      )}
    </div>
  )
}

function extract(
  XLSX: typeof import('xlsx'),
  book: import('xlsx').WorkBook,
  index: number,
): SheetData {
  const names = book.SheetNames
  const worksheet = book.Sheets[names[index]]
  const all = XLSX.utils.sheet_to_json<string[]>(worksheet, {
    header: 1,
    blankrows: false,
    defval: '',
    raw: false,
  })

  const rows = all.slice(0, MAX_ROWS).map((row) => row.slice(0, MAX_COLS).map((c) => String(c ?? '')))
  return {
    names,
    rows,
    truncatedRows: all.length > MAX_ROWS,
    truncatedCols: all.some((row) => row.length > MAX_COLS),
  }
}

function Message({ children, tone }: { children: React.ReactNode; tone?: 'danger' }) {
  return (
    <div className="flex h-full items-center justify-center p-8">
      <p
        className={clsx(
          'max-w-sm text-center text-sm',
          tone === 'danger' ? 'text-[var(--color-danger)]' : 'text-[var(--muted)]',
        )}
      >
        {children}
      </p>
    </div>
  )
}
