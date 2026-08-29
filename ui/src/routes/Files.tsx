import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import clsx from 'clsx'
import {
  AlertTriangle,
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  Check,
  ChevronLeft,
  ChevronRight,
  CloudDownload,
  Download,
  FolderOpen,
  FolderPlus,
  Grid2x2,
  Home,
  Info,
  FolderInput,
  Layers,
  List,
  Pencil,
  Trash2,
  Upload,
  X,
} from 'lucide-react'
import { api, request, type Entry, type Listing, type LocalEntry, type LocalListing } from '../lib/api'
import { useApp } from '../app/context'
import { events } from '../lib/events'
import { formatBytes, formatDate } from '../lib/format'
import { uploads } from '../lib/uploads'
import { Button, EmptyState, Field, IconButton, Input, Modal, Spinner, toast } from '../components/primitives'
import { EntryIcon } from '../components/icons'
import { Preview } from '../components/Preview'
import { MovePicker } from '../components/MovePicker'

type View = 'list' | 'grid'
type SortField = 'name' | 'size' | 'time'
type SortOrder = 'asc' | 'desc'
export function Files({ path, onNavigate }: { path: string; onNavigate: (to: string) => void }) {
  const { status } = useApp()
  const [listing, setListing] = useState<Listing | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [detail, setDetail] = useState<Entry | null>(null)
  const [view, setView] = useState<View>(
    () => (localStorage.getItem('tdrive.view') as View) || 'list',
  )
  const [sortField, setSortField] = useState<SortField>(
    () => (localStorage.getItem('tdrive.sortField') as SortField) || 'name',
  )
  const [sortOrder, setSortOrder] = useState<SortOrder>(
    () => (localStorage.getItem('tdrive.sortOrder') as SortOrder) || 'asc',
  )
  const [dragging, setDragging] = useState(false)
  const dragCounterRef = useRef(0)

  const [newFolderOpen, setNewFolderOpen] = useState(false)
  const [renaming, setRenaming] = useState<Entry | null>(null)
  const [remoteOpen, setRemoteOpen] = useState(false)
  const [uploadOpen, setUploadOpen] = useState(false)
  const [moving, setMoving] = useState<string[] | null>(null)

  const load = useCallback(async () => {
    setError(null)
    try {
      const data = await api.list(path)
      setListing(data)
      setSelected(new Set())
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setListing(null)
    } finally {
      setLoading(false)
    }
  }, [path])

  useEffect(() => {
    setLoading(true)
    setDetail(null)
    void load()
  }, [load])

  // The tree changes from other tabs, from WebDAV clients and from finished
  // server-side transfers, so the listing follows the event stream rather
  // than only reloading on navigation.
  useEffect(() => {
    return events.subscribe((event) => {
      if (event.type !== 'tree') return
      const changed = (event.data as { path: string }).path
      if (changed === path || changed === '/' || path.startsWith(changed)) {
        void load()
      }
    })
  }, [path, load])

  useEffect(() => {
    dragCounterRef.current = 0
    setDragging(false)
  }, [path])

  useEffect(() => {
    const resetDrag = () => {
      dragCounterRef.current = 0
      setDragging(false)
    }

    const handleWindowDragLeave = (e: DragEvent) => {
      if (
        e.clientX <= 0 ||
        e.clientY <= 0 ||
        e.clientX >= window.innerWidth ||
        e.clientY >= window.innerHeight ||
        !e.relatedTarget
      ) {
        resetDrag()
      }
    }

    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        resetDrag()
      }
    }

    window.addEventListener('dragend', resetDrag)
    window.addEventListener('drop', resetDrag)
    window.addEventListener('dragleave', handleWindowDragLeave)
    window.addEventListener('keydown', handleKeyDown)
    window.addEventListener('blur', resetDrag)

    return () => {
      window.removeEventListener('dragend', resetDrag)
      window.removeEventListener('drop', resetDrag)
      window.removeEventListener('dragleave', handleWindowDragLeave)
      window.removeEventListener('keydown', handleKeyDown)
      window.removeEventListener('blur', resetDrag)
    }
  }, [])

  const setViewMode = (next: View) => {
    setView(next)
    localStorage.setItem('tdrive.view', next)
  }

  const handleSort = (field: SortField) => {
    if (sortField === field) {
      const nextOrder = sortOrder === 'asc' ? 'desc' : 'asc'
      setSortOrder(nextOrder)
      localStorage.setItem('tdrive.sortOrder', nextOrder)
    } else {
      setSortField(field)
      const nextOrder = field === 'name' ? 'asc' : 'desc'
      setSortOrder(nextOrder)
      localStorage.setItem('tdrive.sortField', field)
      localStorage.setItem('tdrive.sortOrder', nextOrder)
    }
  }

  const toggle = (entry: Entry, additive: boolean) => {
    setSelected((prev) => {
      const next = additive ? new Set(prev) : new Set<string>()
      if (prev.has(entry.path) && additive) next.delete(entry.path)
      else next.add(entry.path)
      return next
    })
  }

  const open = (entry: Entry) => {
    if (entry.isDir) {
      onNavigate(`/files${entry.path}`)
    } else {
      setDetail(entry)
    }
  }

  const startUpload = useCallback(
    async (files: FileList | File[]) => {
      const list = Array.from(files)
      if (list.length === 0) return

      if (list.length === 1) {
        toast(`已添加 "${list[0].name}" 到上传队列`, 'info')
      } else {
        toast(`已添加 ${list.length} 个文件到上传队列`, 'info')
      }

      await Promise.all(
        list.map(async (file) => {
          try {
            await uploads.upload(file, path, () => {
              toast(`"${file.name}" 上传完成`, 'success')
              void load()
            })
          } catch (err) {
            toast(
              `${file.name} 上传失败：${err instanceof Error ? err.message : String(err)}`,
              'error',
            )
          }
        }),
      )
    },
    [path, load],
  )

  const remove = async () => {
    const paths = [...selected]
    if (paths.length === 0) return
    if (!confirm(`确定删除这 ${paths.length} 项？Telegram 上的对应消息也会一并删除，无法撤销。`)) {
      return
    }
    try {
      await api.remove(paths)
      toast(`已删除 ${paths.length} 项`, 'success')
      if (detail && paths.includes(detail.path)) setDetail(null)
      await load()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  const sortedEntries = useMemo(() => {
    if (!listing?.entries) return []
    return [...listing.entries].sort((a, b) => {
      if (a.isDir !== b.isDir) {
        return a.isDir ? -1 : 1
      }
      let cmp = 0
      if (sortField === 'name') {
        cmp = a.name.localeCompare(b.name, 'zh-CN', { numeric: true, sensitivity: 'base' })
      } else if (sortField === 'size') {
        cmp = a.size - b.size
      } else if (sortField === 'time') {
        cmp = a.modifiedAt - b.modifiedAt
      }
      return sortOrder === 'asc' ? cmp : -cmp
    })
  }, [listing?.entries, sortField, sortOrder])

  const selectedEntries = useMemo(
    () => sortedEntries.filter((e) => selected.has(e.path)),
    [sortedEntries, selected],
  )

  const handleDragEnter = (e: React.DragEvent) => {
    e.preventDefault()
    if (e.dataTransfer.types && !Array.from(e.dataTransfer.types).includes('Files')) {
      return
    }
    dragCounterRef.current += 1
    if (dragCounterRef.current === 1) {
      setDragging(true)
    }
  }

  const handleDragOver = (e: React.DragEvent) => {
    e.preventDefault()
    if (e.dataTransfer.types && !Array.from(e.dataTransfer.types).includes('Files')) {
      return
    }
    e.dataTransfer.dropEffect = 'copy'
    if (!dragging) {
      setDragging(true)
    }
  }

  const handleDragLeave = (e: React.DragEvent) => {
    e.preventDefault()
    dragCounterRef.current -= 1
    if (dragCounterRef.current <= 0) {
      dragCounterRef.current = 0
      setDragging(false)
    }
  }

  const handleDrop = (e: React.DragEvent) => {
    e.preventDefault()
    dragCounterRef.current = 0
    setDragging(false)
    if (e.dataTransfer.files && e.dataTransfer.files.length > 0) {
      void startUpload(e.dataTransfer.files)
    }
  }

  return (
    <div className="flex h-full min-h-0">
      <div
        className="relative flex min-w-0 flex-1 flex-col"
        onDragEnter={handleDragEnter}
        onDragOver={handleDragOver}
        onDragLeave={handleDragLeave}
        onDrop={handleDrop}
      >
        <Toolbar
          breadcrumbs={listing?.breadcrumbs ?? []}
          onNavigate={onNavigate}
          view={view}
          setView={setViewMode}
          sortField={sortField}
          sortOrder={sortOrder}
          onSort={handleSort}
          onUpload={() => setUploadOpen(true)}
          onNewFolder={() => setNewFolderOpen(true)}
          onRemote={() => setRemoteOpen(true)}
        />

        <div className="min-h-0 flex-1 overflow-y-auto px-3 pb-24 sm:px-5 sm:pb-6">
          {loading ? (
            <div className="flex justify-center py-20">
              <Spinner />
            </div>
          ) : error ? (
            <EmptyState
              icon={<AlertTriangle size={30} />}
              title="打不开这个目录"
              description={error}
              action={<Button onClick={() => void load()}>重试</Button>}
            />
          ) : !listing || listing.entries.length === 0 ? (
            <EmptyState
              icon={<Layers size={30} />}
              title="这里还什么都没有"
              description="把文件拖到这里，或者用上传按钮。超过 2 GB 的文件会自动分卷，在这里仍然显示为一个文件。"
              action={
                <Button variant="primary" icon={<Upload size={15} />} onClick={() => setUploadOpen(true)}>
                  上传文件
                </Button>
              }
            />
          ) : view === 'list' ? (
            <ListView
              entries={sortedEntries}
              selected={selected}
              onToggle={toggle}
              onOpen={open}
              onInfo={setDetail}
              sortField={sortField}
              sortOrder={sortOrder}
              onSort={handleSort}
            />
          ) : (
            <GridView
              entries={sortedEntries}
              selected={selected}
              onToggle={toggle}
              onOpen={open}
            />
          )}
        </div>

        {dragging && (
          <div className="pointer-events-none absolute inset-0 z-30 flex items-center justify-center bg-[var(--bg)]/80 backdrop-blur-[2px] p-4 fade-in">
            <div className="flex h-full w-full flex-col items-center justify-center rounded-2xl border-2 border-dashed border-[var(--color-clay)] bg-[var(--clay-soft)]/20 p-6 text-center">
              <div className="panel px-6 py-5 text-center shadow-lg">
                <Upload size={28} className="mx-auto mb-2 text-[var(--color-clay)]" />
                <p className="display text-base font-medium">松手即可上传到 {path}</p>
                <p className="mt-1 text-xs text-[var(--muted)]">支持多文件上传，超过 2 GB 将自动分卷</p>
              </div>
            </div>
          </div>
        )}
      </div>

      {detail && (
        <DetailPanel entry={detail} onClose={() => setDetail(null)} />
      )}

      {/* Floating bottom action bar */}
      {selected.size > 0 && (
        <div className="fixed bottom-6 inset-x-0 z-30 mx-auto flex w-fit max-w-[92vw] items-center gap-1.5 rounded-full border border-[var(--line-strong)] bg-[var(--surface)]/95 px-4 py-2 shadow-lg backdrop-blur-md rise-in">
          <span className="text-xs font-medium text-[var(--ink)] shrink-0 mr-1">
            已选 {selected.size} 项
          </span>
          <div className="h-4 w-px bg-[var(--line)]" />
          <button
            onClick={() => {
              if (selected.size === sortedEntries.length) {
                setSelected(new Set())
              } else {
                setSelected(new Set(sortedEntries.map((e) => e.path)))
              }
            }}
            className="btn btn-ghost !px-2 !py-1 text-xs"
          >
            {selected.size === sortedEntries.length ? '取消全选' : '全选'}
          </button>
          {selected.size === 1 && (
            <button
              onClick={() => setRenaming(selectedEntries[0] ?? null)}
              className="btn btn-ghost !px-2 !py-1 text-xs"
              title="重命名"
            >
              <Pencil size={14} className="text-[var(--muted)]" />
              <span className="hidden sm:inline">重命名</span>
            </button>
          )}
          <button
            onClick={() => setMoving([...selected])}
            className="btn btn-ghost !px-2 !py-1 text-xs"
            title="移动到..."
          >
            <FolderInput size={14} className="text-[var(--muted)]" />
            <span className="hidden sm:inline">移动</span>
          </button>
          {selected.size === 1 && !selectedEntries[0]?.isDir && (
            <a
              href={`/api/download${selectedEntries[0]?.path}`}
              download={selectedEntries[0]?.name}
              className="btn btn-ghost !px-2 !py-1 text-xs"
              title="下载"
            >
              <Download size={14} className="text-[var(--muted)]" />
              <span className="hidden sm:inline">下载</span>
            </a>
          )}
          <button
            onClick={remove}
            className="btn btn-danger !px-2 !py-1 text-xs"
            title="删除"
          >
            <Trash2 size={14} />
            <span className="hidden sm:inline">删除</span>
          </button>
          <div className="h-4 w-px bg-[var(--line)]" />
          <IconButton
            label="取消选择"
            onClick={() => setSelected(new Set())}
            className="!p-1 text-[var(--faint)] hover:text-[var(--ink)]"
          >
            <X size={15} />
          </IconButton>
        </div>
      )}

      <NewFolderModal
        open={newFolderOpen}
        parent={path}
        onClose={() => setNewFolderOpen(false)}
        onCreated={() => void load()}
      />
      <RenameModal
        entry={renaming}
        onClose={() => setRenaming(null)}
        onRenamed={() => void load()}
      />
      <RemoteModal
        open={remoteOpen}
        path={path}
        onClose={() => setRemoteOpen(false)}
      />
      <UploadModal
        open={uploadOpen}
        destinationPath={path}
        localEnabled={status?.localEnabled ?? false}
        onClose={() => setUploadOpen(false)}
        onBrowserFiles={(files) => {
          void startUpload(files)
        }}
        onLocalStarted={() => void load()}
      />
      <MovePicker
        open={moving !== null}
        sources={moving ?? []}
        onClose={() => setMoving(null)}
        onMoved={() => {
          toast('已移动', 'success')
          void load()
        }}
      />
    </div>
  )
}

function Toolbar({
  breadcrumbs,
  onNavigate,
  view,
  setView,
  sortField,
  sortOrder,
  onSort,
  onUpload,
  onNewFolder,
  onRemote,
}: {
  breadcrumbs: { name: string; path: string }[]
  onNavigate: (to: string) => void
  view: View
  setView: (v: View) => void
  sortField: SortField
  sortOrder: SortOrder
  onSort: (field: SortField) => void
  onUpload: () => void
  onNewFolder: () => void
  onRemote: () => void
}) {
  return (
    <div className="sticky top-0 z-20 border-b border-[var(--line)] bg-[var(--bg)]/85 px-3 py-2.5 backdrop-blur sm:px-5">
      <div className="flex items-center gap-2">
        <nav className="flex min-w-0 flex-1 items-center gap-0.5 overflow-x-auto text-sm">
          {breadcrumbs.map((crumb, i) => (
            <span key={crumb.path} className="flex shrink-0 items-center gap-0.5">
              {i > 0 && <ChevronRight size={13} className="text-[var(--faint)]" />}
              <button
                onClick={() => onNavigate(`/files${crumb.path === '/' ? '' : crumb.path}`)}
                className={clsx(
                  'rounded px-1.5 py-1 transition-colors hover:bg-[var(--sunk)]',
                  i === breadcrumbs.length - 1
                    ? 'font-medium text-[var(--ink)]'
                    : 'text-[var(--muted)]',
                )}
              >
                {i === 0 ? <Home size={14} /> : crumb.name}
              </button>
            </span>
          ))}
        </nav>

        <div className="flex shrink-0 items-center gap-1">
          <SortMenu sortField={sortField} sortOrder={sortOrder} onSort={onSort} />

          <div className="hidden items-center rounded-[var(--radius-control)] border border-[var(--line)] p-0.5 sm:flex">
            <button
              onClick={() => setView('list')}
              aria-label="列表视图"
              className={clsx(
                'rounded-md p-1.5 transition-colors',
                view === 'list' ? 'bg-[var(--sunk)] text-[var(--ink)]' : 'text-[var(--faint)]',
              )}
            >
              <List size={15} />
            </button>
            <button
              onClick={() => setView('grid')}
              aria-label="网格视图"
              className={clsx(
                'rounded-md p-1.5 transition-colors',
                view === 'grid' ? 'bg-[var(--sunk)] text-[var(--ink)]' : 'text-[var(--faint)]',
              )}
            >
              <Grid2x2 size={15} />
            </button>
          </div>

          <IconButton label="新建文件夹" onClick={onNewFolder}>
            <FolderPlus size={16} />
          </IconButton>
          <IconButton label="从链接下载" onClick={onRemote}>
            <CloudDownload size={16} />
          </IconButton>
          <Button variant="primary" icon={<Upload size={15} />} onClick={onUpload}>
            <span className="hidden sm:inline">上传</span>
          </Button>
        </div>
      </div>
    </div>
  )
}

function SortMenu({
  sortField,
  sortOrder,
  onSort,
}: {
  sortField: SortField
  sortOrder: SortOrder
  onSort: (field: SortField) => void
}) {
  const [open, setOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const handleClick = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setOpen(false)
      }
    }
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', handleClick)
    document.addEventListener('keydown', handleKeyDown)
    return () => {
      document.removeEventListener('mousedown', handleClick)
      document.removeEventListener('keydown', handleKeyDown)
    }
  }, [open])

  const fields: { id: SortField; label: string }[] = [
    { id: 'name', label: '名称' },
    { id: 'size', label: '大小' },
    { id: 'time', label: '修改时间' },
  ]

  return (
    <div className="relative" ref={menuRef}>
      <IconButton
        label="排序方式"
        onClick={() => setOpen((prev) => !prev)}
        className={clsx(open && 'bg-[var(--sunk)]')}
      >
        <ArrowUpDown size={15} />
      </IconButton>

      {open && (
        <div className="surface absolute right-0 top-full z-40 mt-1 w-40 p-1 shadow-lg fade-in">
          <div className="px-2.5 py-1 text-[11px] font-medium text-[var(--faint)]">排序方式</div>
          {fields.map((f) => (
            <button
              key={f.id}
              onClick={() => {
                onSort(f.id)
                setOpen(false)
              }}
              className="row w-full !justify-between rounded px-2.5 py-1.5 text-xs text-[var(--ink)] hover:bg-[var(--sunk)]"
            >
              <span>{f.label}</span>
              {sortField === f.id && (
                <span className="flex items-center gap-1 text-[var(--color-clay)]">
                  {sortOrder === 'asc' ? <ArrowUp size={13} /> : <ArrowDown size={13} />}
                </span>
              )}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

function ListView({
  entries,
  selected,
  onToggle,
  onOpen,
  onInfo,
  sortField,
  sortOrder,
  onSort,
}: {
  entries: Entry[]
  selected: Set<string>
  onToggle: (e: Entry, additive: boolean) => void
  onOpen: (e: Entry) => void
  onInfo: (e: Entry) => void
  sortField: SortField
  sortOrder: SortOrder
  onSort: (field: SortField) => void
}) {
  return (
    <div className="pt-2">
      <div className="hidden px-3 pb-1.5 text-[11px] font-medium tracking-wide text-[var(--faint)] sm:grid sm:grid-cols-[1fr_7rem_9rem_2rem] sm:gap-3 select-none">
        <button
          onClick={() => onSort('name')}
          className="flex items-center gap-1 text-left transition-colors hover:text-[var(--ink)] cursor-pointer"
        >
          <span>名称</span>
          {sortField === 'name' && (
            sortOrder === 'asc' ? <ArrowUp size={12} className="text-[var(--color-clay)]" /> : <ArrowDown size={12} className="text-[var(--color-clay)]" />
          )}
        </button>
        <button
          onClick={() => onSort('size')}
          className="flex items-center justify-end gap-1 text-right transition-colors hover:text-[var(--ink)] cursor-pointer"
        >
          <span>大小</span>
          {sortField === 'size' && (
            sortOrder === 'asc' ? <ArrowUp size={12} className="text-[var(--color-clay)]" /> : <ArrowDown size={12} className="text-[var(--color-clay)]" />
          )}
        </button>
        <button
          onClick={() => onSort('time')}
          className="flex items-center gap-1 text-left transition-colors hover:text-[var(--ink)] cursor-pointer"
        >
          <span>修改时间</span>
          {sortField === 'time' && (
            sortOrder === 'asc' ? <ArrowUp size={12} className="text-[var(--color-clay)]" /> : <ArrowDown size={12} className="text-[var(--color-clay)]" />
          )}
        </button>
        <span />
      </div>
      <div className="space-y-0.5">
        {entries.map((entry) => (
          <div
            key={entry.path}
            data-selected={selected.has(entry.path)}
            className="row cursor-pointer sm:grid sm:grid-cols-[1fr_7rem_9rem_2rem] sm:items-center sm:gap-3"
            onClick={(e) => onToggle(entry, e.metaKey || e.ctrlKey)}
            onDoubleClick={() => onOpen(entry)}
            tabIndex={0}
            onKeyDown={(e) => {
              if (e.key === 'Enter') onOpen(entry)
            }}
          >
            <div className="flex min-w-0 items-center gap-2.5">
              <EntryIcon name={entry.name} mime={entry.mime} isDir={entry.isDir} />
              <span className="truncate text-sm">{entry.name}</span>
              <SegmentBadge entry={entry} />
              <BrokenBadge entry={entry} />
            </div>
            <span className="hidden text-right text-xs tabular-nums text-[var(--muted)] sm:block">
              {entry.isDir ? '—' : formatBytes(entry.size)}
            </span>
            <span className="hidden text-xs text-[var(--muted)] sm:block">
              {formatDate(entry.modifiedAt)}
            </span>
            <div className="hidden justify-end sm:flex">
              {!entry.isDir && (
                <IconButton
                  label="详情"
                  onClick={(e) => {
                    e.stopPropagation()
                    onInfo(entry)
                  }}
                >
                  <Info size={14} />
                </IconButton>
              )}
            </div>
            <span className="text-xs text-[var(--faint)] sm:hidden">
              {entry.isDir ? '文件夹' : formatBytes(entry.size)} · {formatDate(entry.modifiedAt)}
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}

function GridView({
  entries,
  selected,
  onToggle,
  onOpen,
}: {
  entries: Entry[]
  selected: Set<string>
  onToggle: (e: Entry, additive: boolean) => void
  onOpen: (e: Entry) => void
}) {
  return (
    <div className="grid grid-cols-2 gap-2 pt-3 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5">
      {entries.map((entry) => (
        <button
          key={entry.path}
          data-selected={selected.has(entry.path)}
          onClick={(e) => onToggle(entry, e.metaKey || e.ctrlKey)}
          onDoubleClick={() => onOpen(entry)}
          className="surface flex flex-col items-start gap-2 p-3 text-left transition-colors hover:border-[var(--line-strong)] data-[selected=true]:border-[var(--color-clay)] data-[selected=true]:bg-[var(--clay-soft)]"
        >
          <EntryIcon name={entry.name} mime={entry.mime} isDir={entry.isDir} size={22} />
          <span className="line-clamp-2 w-full text-sm leading-snug break-all">{entry.name}</span>
          <span className="flex items-center gap-1.5 text-[11px] text-[var(--faint)]">
            {entry.isDir ? '文件夹' : formatBytes(entry.size)}
            <SegmentBadge entry={entry} />
          </span>
        </button>
      ))}
    </div>
  )
}

/**
 * SegmentBadge is the only place segmentation is ever visible, and even here
 * it is an aside rather than a property of the file. The name, the size and
 * everything else describe one file.
 */
function SegmentBadge({ entry }: { entry: Entry }) {
  if (entry.isDir || !entry.segmentCount || entry.segmentCount < 2) return null
  return (
    <span className="chip shrink-0" title={`存储为 ${entry.segmentCount} 个分卷，下载时自动合并`}>
      <Layers size={10} />
      {entry.segmentCount}
    </span>
  )
}

function BrokenBadge({ entry }: { entry: Entry }) {
  if (entry.status !== 'broken') return null
  return (
    <span
      className="chip shrink-0 !border-[var(--color-danger)]/40 !text-[var(--color-danger)]"
      title="部分分卷在 Telegram 上找不到了，这个文件无法完整下载"
    >
      <AlertTriangle size={10} />
      缺卷
    </span>
  )
}

function DetailPanel({ entry, onClose }: { entry: Entry; onClose: () => void }) {
  const [segments, setSegments] = useState<{ index: number; size: number; messageId: number }[]>([])
  const [link, setLink] = useState<string | null>(null)

  useEffect(() => {
    if (entry.isDir) return
    void api.segments(entry.id).then((r) => setSegments(r.segments)).catch(() => {})
    void request<{ download: string }>(`/files/${entry.id}/link`)
      .then((l) => setLink(l.download))
      .catch(() => {})
  }, [entry.id, entry.isDir])

  return (
    <aside className="fixed inset-0 z-40 flex flex-col border-l border-[var(--line)] bg-[var(--bg)] lg:static lg:z-auto lg:w-[24rem] lg:shrink-0">
      <div className="flex items-center justify-between border-b border-[var(--line)] px-4 py-3">
        <h2 className="display truncate text-base">{entry.name}</h2>
        <IconButton label="关闭" onClick={onClose}>
          <X size={16} />
        </IconButton>
      </div>

      <div className="min-h-0 flex-1 space-y-5 overflow-y-auto p-4">
        <Preview entry={entry} />

        {link && (
          <Button
            variant="primary"
            className="w-full"
            icon={<Download size={15} />}
            onClick={() => window.open(link, '_blank')}
          >
            下载 {formatBytes(entry.size)}
          </Button>
        )}

        <dl className="space-y-2 text-sm">
          <Row label="大小" value={formatBytes(entry.size)} />
          <Row label="类型" value={entry.mime || '未知'} />
          <Row label="修改" value={formatDate(entry.modifiedAt)} />
          <Row label="创建" value={formatDate(entry.createdAt)} />
        </dl>

        {segments.length > 1 && (
          <div>
            <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-[var(--faint)]">
              存储分卷
            </h3>
            <p className="mb-2.5 text-xs text-[var(--muted)]">
              Telegram 单个文件上限 2 GB，这个文件被拆成 {segments.length} 条消息存储。下载、播放和
              WebDAV 都会自动拼接，你不需要关心。
            </p>
            <div className="space-y-1">
              {segments.map((seg) => (
                <div
                  key={seg.index}
                  className="flex items-center justify-between rounded-[var(--radius-control)] bg-[var(--sunk)] px-2.5 py-1.5 text-xs"
                >
                  <span className="font-[family-name:var(--font-mono)] text-[var(--muted)]">
                    #{String(seg.index).padStart(2, '0')}
                  </span>
                  <span className="tabular-nums text-[var(--muted)]">{formatBytes(seg.size)}</span>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </aside>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-4">
      <dt className="shrink-0 text-[var(--muted)]">{label}</dt>
      <dd className="truncate text-right">{value}</dd>
    </div>
  )
}

function NewFolderModal({
  open,
  parent,
  onClose,
  onCreated,
}: {
  open: boolean
  parent: string
  onClose: () => void
  onCreated: () => void
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (open) {
      setName('')
      setError(null)
    }
  }, [open])

  const submit = async () => {
    if (!name.trim()) return
    setBusy(true)
    setError(null)
    try {
      await api.mkdir(`${parent === '/' ? '' : parent}/${name.trim()}`)
      onCreated()
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="新建文件夹"
      description="文件夹会作为一条带标签的消息记录到 Telegram，索引丢失后可以还原。"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" loading={busy} onClick={() => void submit()}>
            创建
          </Button>
        </>
      }
    >
      <Field label="名称" error={error ?? undefined}>
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && void submit()}
          placeholder="例如 电影"
          autoFocus
        />
      </Field>
    </Modal>
  )
}

function RenameModal({
  entry,
  onClose,
  onRenamed,
}: {
  entry: Entry | null
  onClose: () => void
  onRenamed: () => void
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (entry) {
      setName(entry.name)
      setError(null)
    }
  }, [entry])

  const submit = async () => {
    if (!entry || !name.trim() || name === entry.name) return onClose()
    setBusy(true)
    setError(null)
    try {
      await api.rename(entry.path, name.trim())
      onRenamed()
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={entry !== null}
      onClose={onClose}
      title="重命名"
      description="Telegram 上每个分卷的标题也会同步更新。"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" loading={busy} onClick={() => void submit()}>
            保存
          </Button>
        </>
      }
    >
      <Field label="新名称" error={error ?? undefined}>
        <Input
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && void submit()}
          autoFocus
        />
      </Field>
    </Modal>
  )
}

function UploadModal({
  open,
  destinationPath,
  localEnabled,
  onClose,
  onBrowserFiles,
  onLocalStarted,
}: {
  open: boolean
  destinationPath: string
  localEnabled: boolean
  onClose: () => void
  onBrowserFiles: (files: FileList) => void
  onLocalStarted: () => void
}) {
  const browserInput = useRef<HTMLInputElement>(null)
  const [listing, setListing] = useState<LocalListing | null>(null)
  const [localPath, setLocalPath] = useState('/')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [localLoading, setLocalLoading] = useState(false)
  const [localBusy, setLocalBusy] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)

  useEffect(() => {
    if (!open) return
    setListing(null)
    setLocalPath('/')
    setSelected(new Set())
    setLocalError(null)
    if (!localEnabled) return

    let cancelled = false
    setLocalLoading(true)
    void api
      .localList('/')
      .then((next) => {
        if (!cancelled) setListing(next)
      })
      .catch((err) => {
        if (!cancelled) setLocalError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => {
        if (!cancelled) setLocalLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [open, localEnabled])

  const browse = async (nextPath: string) => {
    setLocalLoading(true)
    setLocalError(null)
    try {
      const next = await api.localList(nextPath)
      setListing(next)
      setLocalPath(next.path)
      setSelected(new Set())
    } catch (err) {
      setLocalError(err instanceof Error ? err.message : String(err))
    } finally {
      setLocalLoading(false)
    }
  }

  const toggle = (entry: LocalEntry) => {
    if (entry.isDir) {
      void browse(entry.path)
      return
    }
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(entry.path)) next.delete(entry.path)
      else next.add(entry.path)
      return next
    })
  }

  const startLocalUploads = async () => {
    const entries = listing?.entries.filter((entry) => selected.has(entry.path) && !entry.isDir) ?? []
    if (entries.length === 0) return

    setLocalBusy(true)
    setLocalError(null)
    let started = 0
    let failed = 0
    let firstError = ''
    try {
      for (const entry of entries) {
        try {
          await api.localUpload({ sourcePath: entry.path, path: destinationPath })
          started += 1
        } catch (err) {
          failed += 1
          if (!firstError) firstError = err instanceof Error ? err.message : String(err)
        }
      }
      if (started > 0) {
        onLocalStarted()
        toast(`已添加 ${started} 个 VPS 文件到上传队列`, 'success')
      }
      if (failed > 0) {
        setLocalError(`${failed} 个文件添加失败${firstError ? `：${firstError}` : ''}`)
      } else {
        onClose()
      }
    } finally {
      setLocalBusy(false)
    }
  }

  const parentPath = localPath === '/' ? '/' : localPath.slice(0, localPath.lastIndexOf('/')) || '/'
  const selectedCount = selected.size

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="上传文件"
      description={`上传到 ${destinationPath}`}
      width="max-w-2xl"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button
            variant="primary"
            loading={localBusy}
            disabled={!localEnabled || selectedCount === 0}
            onClick={() => void startLocalUploads()}
          >
            上传已选 {selectedCount} 个 VPS 文件
          </Button>
        </>
      }
    >
      <div className="space-y-5">
        <section>
          <div className="mb-2 flex items-center gap-2">
            <Upload size={15} className="text-[var(--color-clay)]" />
            <h3 className="text-sm font-medium">浏览器文件</h3>
          </div>
          <div className="flex flex-col gap-3 rounded-[var(--radius-card)] border border-dashed border-[var(--line-strong)] bg-[var(--sunk)]/45 p-4 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-xs leading-relaxed text-[var(--muted)]">
              选择当前设备上的文件，文件会从浏览器分片上传到 TDrive。
            </p>
            <Button variant="outline" onClick={() => browserInput.current?.click()}>
              选择文件
            </Button>
            <input
              ref={browserInput}
              type="file"
              multiple
              className="hidden"
              onChange={(event) => {
                if (event.target.files?.length) {
                  onBrowserFiles(event.target.files)
                  onClose()
                }
                event.target.value = ''
              }}
            />
          </div>
        </section>

        <section className="border-t border-[var(--line)] pt-5">
          <div className="mb-2 flex items-center gap-2">
            <FolderOpen size={15} className="text-[var(--color-clay)]" />
            <div>
              <h3 className="text-sm font-medium">VPS 本地文件</h3>
              <p className="text-xs text-[var(--muted)]">服务器从挂载目录直接读取，不经过浏览器。</p>
            </div>
          </div>

          {!localEnabled ? (
            <p className="rounded-[var(--radius-card)] bg-[var(--sunk)] p-4 text-xs leading-relaxed text-[var(--muted)]">
              尚未配置 VPS 文件目录。请管理员前往“设置 → 运行参数”填写服务器目录（Docker 下通常为
              <span className="mx-1 font-[family-name:var(--font-mono)]">/vps</span>），并确保该目录已挂载且可读。
            </p>
          ) : (
            <div className="rounded-[var(--radius-card)] border border-[var(--line)]">
              <div className="flex items-center gap-1 overflow-x-auto border-b border-[var(--line)] px-2 py-1.5">
                <IconButton label="返回上一级" disabled={localPath === '/'} onClick={() => void browse(parentPath)}>
                  <ChevronLeft size={15} />
                </IconButton>
                {listing?.breadcrumbs.map((crumb, index) => (
                  <span key={crumb.path} className="flex shrink-0 items-center gap-0.5">
                    {index > 0 && <ChevronRight size={12} className="text-[var(--faint)]" />}
                    <button
                      className={clsx(
                        'rounded px-1.5 py-1 text-xs transition-colors hover:bg-[var(--sunk)]',
                        index === listing.breadcrumbs.length - 1
                          ? 'font-medium text-[var(--ink)]'
                          : 'text-[var(--muted)]',
                      )}
                      onClick={() => void browse(crumb.path)}
                    >
                      {index === 0 ? 'VPS' : crumb.name}
                    </button>
                  </span>
                ))}
              </div>

              {localError && <p className="px-3 pt-3 text-xs text-[var(--color-danger)]">{localError}</p>}
              {localLoading ? (
                <div className="flex justify-center py-10">
                  <Spinner />
                </div>
              ) : listing && listing.entries.length > 0 ? (
                <div className="max-h-64 overflow-y-auto p-1.5">
                  {listing.entries.map((entry) => (
                    <button
                      key={entry.path}
                      className="row w-full !justify-between text-left data-[selected=true]:bg-[var(--clay-soft)]"
                      data-selected={selected.has(entry.path)}
                      onClick={() => toggle(entry)}
                    >
                      <span className="flex min-w-0 items-center gap-2">
                        <EntryIcon name={entry.name} isDir={entry.isDir} size={16} />
                        <span className="truncate text-sm">{entry.name}</span>
                      </span>
                      <span className="shrink-0 text-xs tabular-nums text-[var(--muted)]">
                        {entry.isDir ? '文件夹' : formatBytes(entry.size)}
                      </span>
                      {selected.has(entry.path) && <Check size={14} className="shrink-0 text-[var(--color-clay)]" />}
                    </button>
                  ))}
                </div>
              ) : (
                <p className="py-10 text-center text-xs text-[var(--muted)]">这个目录为空</p>
              )}
            </div>
          )}
        </section>
      </div>
    </Modal>
  )
}

function RemoteModal({
  open,
  path,
  onClose,
}: {
  open: boolean
  path: string
  onClose: () => void
}) {
  const [url, setUrl] = useState('')
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (open) {
      setUrl('')
      setName('')
      setError(null)
    }
  }, [open])

  const submit = async () => {
    if (!url.trim()) return
    setBusy(true)
    setError(null)
    try {
      await api.remoteUpload({ url: url.trim(), path, name: name.trim() || undefined })
      toast('已开始下载，进度会显示在传输列表里', 'success')
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="从链接下载"
      description="服务器直接抓取并存进 Telegram，不经过你的浏览器。"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" loading={busy} onClick={() => void submit()}>
            开始
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <Field label="链接" error={error ?? undefined}>
          <Input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://example.com/file.mkv"
            autoFocus
          />
        </Field>
        <Field label="文件名" hint="留空则使用链接里的名字">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="可选" />
        </Field>
      </div>
    </Modal>
  )
}
