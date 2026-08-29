import { useEffect, useState } from 'react'
import clsx from 'clsx'
import { ChevronRight, FolderPlus, Home } from 'lucide-react'
import { api, type Entry } from '../lib/api'
import { Button, Field, Input, Modal, Spinner } from './primitives'

/**
 * A destination picker for moving files and folders.
 *
 * It walks the tree one level at a time rather than showing the whole thing:
 * a drive with thousands of folders would be unusable as a flat list, and
 * fetching only what is opened keeps the dialog instant on a large drive.
 */
export function MovePicker({
  open,
  sources,
  onClose,
  onMoved,
}: {
  open: boolean
  /** Paths being moved. Used to grey out invalid destinations. */
  sources: string[]
  onClose: () => void
  onMoved: () => void
}) {
  const [path, setPath] = useState('/')
  const [dirs, setDirs] = useState<Entry[] | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [newFolder, setNewFolder] = useState('')

  useEffect(() => {
    if (open) {
      setPath('/')
      setError(null)
      setNewFolder('')
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    setDirs(null)
    void api
      .list(path)
      .then((r) => setDirs(r.entries.filter((e) => e.isDir)))
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
  }, [open, path])

  // Moving something into itself or its own subtree would detach it from the
  // root; the server refuses, and the dialog should not offer it.
  const invalid = (candidate: string) =>
    sources.some((src) => candidate === src || candidate.startsWith(src + '/'))

  const sameParent = sources.every((src) => {
    const parent = src.slice(0, src.lastIndexOf('/')) || '/'
    return parent === path
  })

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      for (const src of sources) {
        await api.move(src, path)
      }
      onMoved()
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const createFolder = async () => {
    if (!newFolder.trim()) return
    setBusy(true)
    setError(null)
    try {
      const created = await api.mkdir(`${path === '/' ? '' : path}/${newFolder.trim()}`)
      setNewFolder('')
      setPath(created.path)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const crumbs = ['/', ...cumulative(path)]

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={sources.length === 1 ? '移动到' : `移动 ${sources.length} 项到`}
      description="Telegram 上对应消息的标签会同步更新。"
      width="max-w-lg"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button
            variant="primary"
            loading={busy}
            disabled={invalid(path) || sameParent}
            onClick={() => void submit()}
          >
            移动到 {path === '/' ? '根目录' : path.slice(path.lastIndexOf('/') + 1)}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <nav className="flex flex-wrap items-center gap-0.5 text-sm">
          {crumbs.map((crumb, i) => (
            <span key={crumb} className="flex items-center gap-0.5">
              {i > 0 && <ChevronRight size={13} className="text-[var(--faint)]" />}
              <button
                onClick={() => setPath(crumb)}
                className={clsx(
                  'rounded px-1.5 py-1 transition-colors hover:bg-[var(--sunk)]',
                  i === crumbs.length - 1 ? 'font-medium' : 'text-[var(--muted)]',
                )}
              >
                {i === 0 ? <Home size={14} /> : crumb.slice(crumb.lastIndexOf('/') + 1)}
              </button>
            </span>
          ))}
        </nav>

        <div className="h-56 overflow-y-auto rounded-[var(--radius-control)] border border-[var(--line)] p-1">
          {dirs === null ? (
            <div className="flex h-full items-center justify-center">
              <Spinner />
            </div>
          ) : dirs.length === 0 ? (
            <p className="flex h-full items-center justify-center text-sm text-[var(--muted)]">
              这里没有子文件夹
            </p>
          ) : (
            dirs.map((dir) => (
              <button
                key={dir.path}
                disabled={invalid(dir.path)}
                onClick={() => setPath(dir.path)}
                className="row w-full !justify-start text-sm disabled:cursor-not-allowed disabled:opacity-40"
              >
                <FolderPlus size={15} className="shrink-0 text-[var(--color-clay)]" />
                <span className="truncate">{dir.name}</span>
                <ChevronRight size={13} className="ml-auto shrink-0 text-[var(--faint)]" />
              </button>
            ))
          )}
        </div>

        <Field label="或在这里新建一个文件夹" error={error ?? undefined}>
          <div className="flex gap-2">
            <Input
              value={newFolder}
              onChange={(e) => setNewFolder(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && void createFolder()}
              placeholder="文件夹名称"
            />
            <Button onClick={() => void createFolder()} disabled={!newFolder.trim() || busy}>
              新建
            </Button>
          </div>
        </Field>

        {sameParent && (
          <p className="text-xs text-[var(--faint)]">这些项目已经在这个目录里了。</p>
        )}
      </div>
    </Modal>
  )
}

/** cumulative turns "/a/b/c" into ["/a", "/a/b", "/a/b/c"]. */
function cumulative(path: string): string[] {
  const parts = path.split('/').filter(Boolean)
  const out: string[] = []
  let acc = ''
  for (const part of parts) {
    acc += '/' + part
    out.push(acc)
  }
  return out
}
