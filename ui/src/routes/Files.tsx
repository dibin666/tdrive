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
  ClipboardCopy,
  CloudDownload,
  Download,
  Eye,
  FolderInput,
  FolderOpen,
  FolderPlus,
  Grid2x2,
  Home,
  Info,
  Layers,
  Link2,
  List,
  Pencil,
  RefreshCw,
  Rows3,
  Trash2,
  Upload,
  X,
} from 'lucide-react'
import {
  api,
  can,
  type Entry,
  type Listing,
  type LocalEntry,
  type LocalListing,
} from '../lib/api'
import { useApp } from '../app/context'
import { events } from '../lib/events'
import { COPY_FAILED, copyText } from '../lib/clipboard'
import { formatBytes, formatDate, isPreviewable, kindOf, naturalCompare } from '../lib/format'
import { uploads } from '../lib/uploads'
import { downloads } from '../lib/downloads'
import { useSelection, useMarquee } from '../lib/selection'
import { useEdgeSwipe, useLongPress, usePullToRefresh } from '../lib/gestures'
import {
  Button,
  EmptyState,
  Field,
  IconButton,
  Input,
  Modal,
  Spinner,
  toast,
} from '../components/primitives'
import { EntryIcon } from '../components/icons'
import { MovePicker } from '../components/MovePicker'
import { BatchRename } from '../components/BatchRename'
import { DownloadDialog } from '../components/DownloadDialog'
import { PreviewModal } from '../components/preview/PreviewModal'
import { ContextMenu, useContextMenu, type MenuItem } from '../components/ContextMenu'

type View = 'list' | 'grid'
type SortField = 'name' | 'size' | 'time'
type SortOrder = 'asc' | 'desc'

export function Files({ path, onNavigate }: { path: string; onNavigate: (to: string) => void }) {
  const { status, user } = useApp()
  const [listing, setListing] = useState<Listing | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [detail, setDetail] = useState<Entry | null>(null)
  const [previewIndex, setPreviewIndex] = useState<number | null>(null)

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
  const scrollRef = useRef<HTMLDivElement>(null)

  const [newFolderOpen, setNewFolderOpen] = useState(false)
  const [renaming, setRenaming] = useState<Entry | null>(null)
  const [batchRenaming, setBatchRenaming] = useState<Entry[] | null>(null)
  const [downloadTarget, setDownloadTarget] = useState<Entry | null>(null)
  const [remoteOpen, setRemoteOpen] = useState(false)
  const [uploadOpen, setUploadOpen] = useState(false)
  const [moving, setMoving] = useState<string[] | null>(null)

  const { menu, openMenu, closeMenu } = useContextMenu()

  const load = useCallback(async () => {
    setError(null)
    try {
      const data = await api.list(path)
      setListing(data)
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
    setPreviewIndex(null)
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

  const sortedEntries = useMemo(() => {
    if (!listing?.entries) return []
    return [...listing.entries].sort((a, b) => {
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
      let cmp = 0
      if (sortField === 'name') cmp = naturalCompare(a.name, b.name)
      else if (sortField === 'size') cmp = a.size - b.size
      else if (sortField === 'time') cmp = a.modifiedAt - b.modifiedAt
      return sortOrder === 'asc' ? cmp : -cmp
    })
  }, [listing?.entries, sortField, sortOrder])

  const keyOf = useCallback((entry: Entry) => entry.path, [])
  const selection = useSelection(sortedEntries, keyOf)
  const { selected, selectedItems } = selection

  // Only files can be previewed, and the arrow keys inside the previewer walk
  // this list rather than the raw listing.
  const previewable = useMemo(
    () => sortedEntries.filter((e) => !e.isDir && isPreviewable(kindOf(e.name, e.mime))),
    [sortedEntries],
  )

  const canRead = can(user, 'read')
  const canWrite = can(user, 'upload')
  const canDelete = can(user, 'delete')
  const canRename = can(user, 'rename')
  const canMove = can(user, 'move')
  const canMkdir = can(user, 'mkdir')
  const canDownload = can(user, 'download')
  const canShare = can(user, 'share')
  const canRemote = can(user, 'remoteFetch')

  const openEntry = useCallback(
    (entry: Entry) => {
      if (entry.isDir) {
        onNavigate(`/files${entry.path}`)
        return
      }
      const index = previewable.findIndex((e) => e.path === entry.path)
      if (index >= 0) setPreviewIndex(index)
      else setDetail(entry)
    },
    [onNavigate, previewable],
  )

  const startUpload = useCallback(
    async (files: FileList | File[]) => {
      const list = Array.from(files)
      if (list.length === 0) return

      toast(
        list.length === 1
          ? `已添加 "${list[0].name}" 到上传队列`
          : `已添加 ${list.length} 个文件到上传队列`,
        'info',
      )

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

  const remove = useCallback(
    async (targets: Entry[]) => {
      if (targets.length === 0) return
      const names = targets.length === 1 ? `"${targets[0].name}"` : `这 ${targets.length} 项`
      if (!confirm(`确定删除${names}？Telegram 上的对应消息也会一并删除，无法撤销。`)) return
      try {
        await api.remove(targets.map((t) => t.path))
        toast(`已删除 ${targets.length} 项`, 'success')
        if (detail && targets.some((t) => t.path === detail.path)) setDetail(null)
        selection.clear()
        await load()
      } catch (err) {
        toast(err instanceof Error ? err.message : String(err), 'error')
      }
    },
    [detail, load, selection],
  )

  const copyPaths = useCallback(async (targets: Entry[]) => {
    const ok = await copyText(targets.map((t) => t.path).join('\n'))
    if (!ok) {
      toast(COPY_FAILED, 'error')
      return
    }
    toast(targets.length === 1 ? '路径已复制' : `已复制 ${targets.length} 条路径`, 'success')
  }, [])

  const copyLink = useCallback(async (entry: Entry) => {
    try {
      const link = await api.share(entry.id, {})
      const ok = await copyText(link.file.url)
      toast(ok ? '下载直链已复制，可直接粘贴到下载工具' : `直链已生成：${link.file.url}`, ok ? 'success' : 'info')
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }, [])

  const quickDownload = useCallback(
    async (entry: Entry) => {
      // One small single-segment file does not need the dialog: the browser's
      // own downloader is already the right answer.
      if ((entry.segmentCount ?? 1) <= 1 && entry.size < 512 * 1024 * 1024) {
        try {
          // Use the same tracked native-download path as the dialog. It gets
          // a short-lived media token, records history, and never requires the
          // share permission just to download a file.
          await downloads.start({
            fileId: entry.id,
            name: entry.name,
            size: entry.size,
            mode: 'direct',
          })
          return
        } catch (err) {
          // A download the user cancelled is finished business; only a real
          // failure is worth escalating to the dialog.
          if (err instanceof DOMException && err.name === 'AbortError') return
          /* fall through to the dialog, which can explain what went wrong */
        }
      }
      setDownloadTarget(entry)
    },
    [],
  )

  /** Menu contents depend on what is under the cursor and what the account is
   *  allowed to do. Actions it cannot perform are omitted rather than greyed
   *  out — a permanently disabled item is just noise. */
  const buildMenu = useCallback(
    (targets: Entry[]): MenuItem[] => {
      if (targets.length === 0) {
        return [
          {
            id: 'upload',
            label: '上传文件',
            icon: <Upload size={14} />,
            onSelect: () => setUploadOpen(true),
            hidden: !canWrite,
          },
          {
            id: 'new-folder',
            label: '新建文件夹',
            icon: <FolderPlus size={14} />,
            onSelect: () => setNewFolderOpen(true),
            hidden: !canMkdir,
          },
          {
            id: 'remote',
            label: '从链接下载',
            icon: <CloudDownload size={14} />,
            onSelect: () => setRemoteOpen(true),
            hidden: !canRemote,
          },
          {
            id: 'refresh',
            label: '刷新',
            icon: <RefreshCw size={14} />,
            hint: 'F5',
            separated: true,
            onSelect: () => void load(),
          },
          {
            id: 'select-all',
            label: '全选',
            icon: <Check size={14} />,
            hint: 'Ctrl+A',
            onSelect: selection.selectAll,
          },
        ]
      }

      const single = targets.length === 1 ? targets[0] : null
      const files = targets.filter((t) => !t.isDir)

      return [
        {
          id: 'open',
          label: single?.isDir ? '打开' : '预览',
          icon: single?.isDir ? <FolderOpen size={14} /> : <Eye size={14} />,
          hint: 'Enter',
          onSelect: () => single && openEntry(single),
          hidden: !single || !canRead,
        },
        {
          id: 'download',
          label: files.length > 1 ? `下载 ${files.length} 个文件` : '下载…',
          icon: <Download size={14} />,
          onSelect: () => {
            if (files.length === 1) void quickDownload(files[0])
            else files.forEach((file) => void quickDownload(file))
          },
          hidden: files.length === 0 || !canDownload,
        },
        {
          id: 'copy-link',
          label: '复制下载直链',
          icon: <Link2 size={14} />,
          onSelect: () => single && void copyLink(single),
          hidden: !single || single.isDir || !canShare,
        },
        {
          id: 'rename',
          label: '重命名',
          icon: <Pencil size={14} />,
          hint: 'F2',
          separated: true,
          onSelect: () => single && setRenaming(single),
          hidden: !single || !canRename,
        },
        {
          id: 'batch-rename',
          label: `批量重命名 ${targets.length} 项`,
          icon: <Rows3 size={14} />,
          onSelect: () => setBatchRenaming(targets),
          hidden: targets.length < 2 || !canRename,
        },
        {
          id: 'move',
          label: '移动到…',
          icon: <FolderInput size={14} />,
          onSelect: () => setMoving(targets.map((t) => t.path)),
          hidden: !canMove,
        },
        {
          id: 'copy-path',
          label: '复制路径',
          icon: <ClipboardCopy size={14} />,
          onSelect: () => void copyPaths(targets),
        },
        {
          id: 'info',
          label: '详细信息',
          icon: <Info size={14} />,
          onSelect: () => single && setDetail(single),
          hidden: !single || single.isDir,
        },
        {
          id: 'delete',
          label: targets.length > 1 ? `删除 ${targets.length} 项` : '删除',
          icon: <Trash2 size={14} />,
          hint: 'Del',
          separated: true,
          danger: true,
          onSelect: () => void remove(targets),
          hidden: !canDelete,
        },
      ]
    },
    [
      canDelete, canDownload, canMkdir, canMove, canRead, canRemote, canRename, canShare, canWrite,
      copyLink, copyPaths, load, openEntry, quickDownload, remove, selection,
    ],
  )

  const onEntryContextMenu = useCallback(
    (entry: Entry, position: { x: number; y: number }) => {
      // Right-clicking outside the current selection selects that entry
      // first, which is what every file manager does and what stops an action
      // from silently applying to something else.
      const targets = selected.has(entry.path) ? selectedItems : [entry]
      if (!selected.has(entry.path)) selection.only(entry.path)
      openMenu(position, buildMenu(targets), targets.length > 1 ? `已选 ${targets.length} 项` : entry.name)
    },
    [buildMenu, openMenu, selected, selectedItems, selection],
  )

  const onEmptyContextMenu = useCallback(
    (position: { x: number; y: number }) => {
      selection.clear()
      openMenu(position, buildMenu([]), listing?.path)
    },
    [buildMenu, listing?.path, openMenu, selection],
  )

  // Keyboard shortcuts are scoped to the listing: typing in a dialog must not
  // delete the files behind it.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null
      if (target && ['INPUT', 'TEXTAREA', 'SELECT'].includes(target.tagName)) return
      if (previewIndex !== null || newFolderOpen || renaming || batchRenaming || uploadOpen) return

      if ((e.ctrlKey || e.metaKey) && e.key === 'a') {
        e.preventDefault()
        selection.selectAll()
      } else if (e.key === 'Escape') {
        selection.clear()
      } else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault()
        selection.moveCursor(e.key === 'ArrowDown' ? 1 : -1, e.shiftKey)
      } else if (e.key === 'Home' || e.key === 'End') {
        e.preventDefault()
        selection.moveCursor(e.key === 'Home' ? -sortedEntries.length : sortedEntries.length, e.shiftKey)
      } else if (e.key === 'Enter' && selectedItems.length === 1) {
        e.preventDefault()
        openEntry(selectedItems[0])
      } else if (e.key === 'F2' && selectedItems.length === 1 && canRename) {
        e.preventDefault()
        setRenaming(selectedItems[0])
      } else if (e.key === 'Delete' && selectedItems.length > 0 && canDelete) {
        e.preventDefault()
        void remove(selectedItems)
      } else if (e.key === 'F5') {
        e.preventDefault()
        void load()
      }
    }

    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [
    batchRenaming, canDelete, canRename, load, newFolderOpen, openEntry, previewIndex, remove,
    renaming, selectedItems, selection, sortedEntries.length, uploadOpen,
  ])

  const onMarquee = useCallback(
    (keys: string[], additive: boolean) => {
      if (additive) selection.add(keys)
      else selection.set(keys)
    },
    [selection],
  )
  const { marquee, onPointerDown: onMarqueePointerDown } = useMarquee(scrollRef, onMarquee)

  const { pull, refreshing, threshold } = usePullToRefresh(scrollRef, load)
  useEdgeSwipe(() => {
    if (path === '/') return
    const parent = path.slice(0, path.lastIndexOf('/')) || '/'
    onNavigate(`/files${parent === '/' ? '' : parent}`)
  })

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
        e.clientX <= 0 || e.clientY <= 0 ||
        e.clientX >= window.innerWidth || e.clientY >= window.innerHeight ||
        !e.relatedTarget
      ) {
        resetDrag()
      }
    }
    window.addEventListener('dragend', resetDrag)
    window.addEventListener('drop', resetDrag)
    window.addEventListener('dragleave', handleWindowDragLeave)
    window.addEventListener('blur', resetDrag)
    return () => {
      window.removeEventListener('dragend', resetDrag)
      window.removeEventListener('drop', resetDrag)
      window.removeEventListener('dragleave', handleWindowDragLeave)
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

  const handleDragEnter = (e: React.DragEvent) => {
    e.preventDefault()
    if (e.dataTransfer.types && !Array.from(e.dataTransfer.types).includes('Files')) return
    dragCounterRef.current += 1
    if (dragCounterRef.current === 1) setDragging(true)
  }
  const handleDragOver = (e: React.DragEvent) => {
    e.preventDefault()
    if (e.dataTransfer.types && !Array.from(e.dataTransfer.types).includes('Files')) return
    e.dataTransfer.dropEffect = 'copy'
    if (!dragging) setDragging(true)
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
    if (e.dataTransfer.files?.length) void startUpload(e.dataTransfer.files)
  }

  const rowProps = {
    selection,
    onOpen: openEntry,
    onContextMenu: onEntryContextMenu,
    onInfo: setDetail,
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
          canUpload={canWrite}
          canMkdir={canMkdir}
          canRemote={canRemote}
        />

        {(pull > 0 || refreshing) && (
          <div
            className="flex shrink-0 items-center justify-center overflow-hidden text-xs text-[var(--muted)]"
            style={{ height: refreshing ? threshold * 0.6 : pull }}
          >
            {refreshing ? (
              <Spinner />
            ) : (
              <span>{pull >= threshold ? '松手刷新' : '下拉刷新'}</span>
            )}
          </div>
        )}

        <div
          ref={scrollRef}
          className="relative min-h-0 flex-1 overflow-y-auto px-3 pb-24 sm:px-5 sm:pb-6"
          onPointerDown={onMarqueePointerDown}
          onContextMenu={(e) => {
            const target = e.target as HTMLElement
            if (target.closest('[data-selectable]')) return
            e.preventDefault()
            onEmptyContextMenu({ x: e.clientX, y: e.clientY })
          }}
        >
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
                canWrite ? (
                  <Button variant="primary" icon={<Upload size={15} />} onClick={() => setUploadOpen(true)}>
                    上传文件
                  </Button>
                ) : undefined
              }
            />
          ) : view === 'list' ? (
            <ListView
              entries={sortedEntries}
              sortField={sortField}
              sortOrder={sortOrder}
              onSort={handleSort}
              onDownload={quickDownload}
              onDelete={(entry) => void remove([entry])}
              canDownload={canDownload}
              canDelete={canDelete}
              {...rowProps}
            />
          ) : (
            <GridView entries={sortedEntries} {...rowProps} />
          )}

          {marquee.rect && marquee.active && (
            <div
              className="pointer-events-none absolute z-10 rounded-sm border border-[var(--color-clay)] bg-[var(--color-clay)]/10"
              style={{
                left: marquee.rect.left,
                top: marquee.rect.top,
                width: marquee.rect.width,
                height: marquee.rect.height,
              }}
            />
          )}
        </div>

        {dragging && (
          <div className="pointer-events-none absolute inset-0 z-30 flex items-center justify-center bg-[var(--bg)]/80 p-4 backdrop-blur-[2px] fade-in">
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

      {detail && <DetailPanel entry={detail} onClose={() => setDetail(null)} onDownload={quickDownload} />}

      {selection.size > 0 && (
        <SelectionBar
          count={selection.size}
          allSelected={selection.size === sortedEntries.length}
          entries={selectedItems}
          onSelectAll={() => (selection.size === sortedEntries.length ? selection.clear() : selection.selectAll())}
          onClear={selection.clear}
          onRename={() => setRenaming(selectedItems[0] ?? null)}
          onBatchRename={() => setBatchRenaming(selectedItems)}
          onMove={() => setMoving([...selected])}
          onDownload={() => selectedItems.filter((e) => !e.isDir).forEach((e) => void quickDownload(e))}
          onDelete={() => void remove(selectedItems)}
          canRename={canRename}
          canMove={canMove}
          canDownload={canDownload}
          canDelete={canDelete}
        />
      )}

      <ContextMenu state={menu} onClose={closeMenu} />

      {previewIndex !== null && previewable.length > 0 && (
        <PreviewModal
          entries={previewable}
          index={Math.min(previewIndex, previewable.length - 1)}
          onIndexChange={setPreviewIndex}
          onClose={() => setPreviewIndex(null)}
          onDownload={quickDownload}
        />
      )}

      <DownloadDialog entry={downloadTarget} onClose={() => setDownloadTarget(null)} />

      <NewFolderModal
        open={newFolderOpen}
        parent={path}
        onClose={() => setNewFolderOpen(false)}
        onCreated={() => void load()}
      />
      <RenameModal entry={renaming} onClose={() => setRenaming(null)} onRenamed={() => void load()} />
      <BatchRename
        open={batchRenaming !== null}
        entries={batchRenaming ?? []}
        onClose={() => setBatchRenaming(null)}
        onDone={() => void load()}
      />
      <RemoteModal open={remoteOpen} path={path} onClose={() => setRemoteOpen(false)} />
      <UploadModal
        open={uploadOpen}
        destinationPath={path}
        localEnabled={(status?.localEnabled ?? false) && can(user, 'uploadLocal')}
        onClose={() => setUploadOpen(false)}
        onBrowserFiles={(files) => void startUpload(files)}
        onLocalStarted={() => void load()}
      />
      <MovePicker
        open={moving !== null}
        sources={moving ?? []}
        onClose={() => setMoving(null)}
        onMoved={() => {
          toast('已移动', 'success')
          selection.clear()
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
  canUpload,
  canMkdir,
  canRemote,
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
  canUpload: boolean
  canMkdir: boolean
  canRemote: boolean
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

          {canMkdir && (
            <IconButton label="新建文件夹" onClick={onNewFolder}>
              <FolderPlus size={16} />
            </IconButton>
          )}
          {canRemote && (
            <IconButton label="从链接下载" onClick={onRemote}>
              <CloudDownload size={16} />
            </IconButton>
          )}
          {canUpload && (
            <Button variant="primary" icon={<Upload size={15} />} onClick={onUpload}>
              <span className="hidden sm:inline">上传</span>
            </Button>
          )}
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
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setOpen(false)
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

type RowShared = {
  selection: ReturnType<typeof useSelection<Entry>>
  onOpen: (entry: Entry) => void
  onContextMenu: (entry: Entry, position: { x: number; y: number }) => void
  onInfo: (entry: Entry) => void
}

function ListView({
  entries,
  selection,
  onOpen,
  onContextMenu,
  onInfo,
  onDownload,
  onDelete,
  sortField,
  sortOrder,
  onSort,
  canDownload,
  canDelete,
}: RowShared & {
  entries: Entry[]
  sortField: SortField
  sortOrder: SortOrder
  onSort: (field: SortField) => void
  onDownload: (entry: Entry) => void
  onDelete: (entry: Entry) => void
  canDownload: boolean
  canDelete: boolean
}) {
  return (
    <div className="pt-2">
      <div className="hidden select-none px-3 pb-1.5 text-[11px] font-medium tracking-wide text-[var(--faint)] sm:grid sm:grid-cols-[1fr_7rem_9rem_2rem] sm:gap-3">
        <SortHeader label="名称" field="name" sortField={sortField} sortOrder={sortOrder} onSort={onSort} />
        <SortHeader label="大小" field="size" sortField={sortField} sortOrder={sortOrder} onSort={onSort} align="end" />
        <SortHeader label="修改时间" field="time" sortField={sortField} sortOrder={sortOrder} onSort={onSort} />
        <span />
      </div>
      <div className="space-y-0.5">
        {entries.map((entry) => (
          <FileRow
            key={entry.path}
            entry={entry}
            selection={selection}
            onOpen={onOpen}
            onContextMenu={onContextMenu}
            onInfo={onInfo}
            onDownload={onDownload}
            onDelete={onDelete}
            canDownload={canDownload}
            canDelete={canDelete}
          />
        ))}
      </div>
    </div>
  )
}

function SortHeader({
  label,
  field,
  sortField,
  sortOrder,
  onSort,
  align,
}: {
  label: string
  field: SortField
  sortField: SortField
  sortOrder: SortOrder
  onSort: (field: SortField) => void
  align?: 'end'
}) {
  return (
    <button
      onClick={() => onSort(field)}
      className={clsx(
        'flex cursor-pointer items-center gap-1 transition-colors hover:text-[var(--ink)]',
        align === 'end' ? 'justify-end text-right' : 'text-left',
      )}
    >
      <span>{label}</span>
      {sortField === field &&
        (sortOrder === 'asc' ? (
          <ArrowUp size={12} className="text-[var(--color-clay)]" />
        ) : (
          <ArrowDown size={12} className="text-[var(--color-clay)]" />
        ))}
    </button>
  )
}

/** FileRow carries the whole interaction surface for one entry: click
 *  selection, double-click open, right-click menu, long-press menu and a
 *  left-swipe action drawer on touch. */
function FileRow({
  entry,
  selection,
  onOpen,
  onContextMenu,
  onInfo,
  onDownload,
  onDelete,
  canDownload,
  canDelete,
}: RowShared & {
  entry: Entry
  onDownload: (entry: Entry) => void
  onDelete: (entry: Entry) => void
  canDownload: boolean
  canDelete: boolean
}) {
  const longPress = useLongPress({
    onLongPress: ({ clientX, clientY }) => onContextMenu(entry, { x: clientX, y: clientY }),
  })

  const selected = selection.isSelected(entry.path)
  const isCursor = selection.cursor === entry.path

  return (
    <div className="relative overflow-hidden rounded-[var(--radius-control)]">
      <div
        data-selectable={entry.path}
        data-selected={selected}
        tabIndex={0}
        className={clsx(
          'row relative cursor-pointer sm:grid sm:grid-cols-[1fr_7rem_9rem_2rem] sm:items-center sm:gap-3',
          isCursor && !selected && 'ring-1 ring-inset ring-[var(--line-strong)]',
        )}
        onClick={(e) => selection.click(entry.path, e)}
        onDoubleClick={() => onOpen(entry)}
        onContextMenu={(e) => {
          e.preventDefault()
          onContextMenu(entry, { x: e.clientX, y: e.clientY })
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') onOpen(entry)
        }}
        {...longPress}
      >
        <div className="flex min-w-0 items-center gap-2.5">
          <EntryIcon name={entry.name} mime={entry.mime} isDir={entry.isDir} />
          <span className="truncate text-sm">{entry.name}</span>
          <SegmentBadge entry={entry} />
          <BrokenBadge entry={entry} />
        </div>
        <span
          className="hidden text-right text-xs tabular-nums text-[var(--muted)] sm:block"
          title={entry.isDir ? '文件夹内所有文件的总大小' : undefined}
        >
          {formatBytes(entry.size)}
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
          {entry.isDir ? `文件夹 · ${formatBytes(entry.size)}` : formatBytes(entry.size)} ·{' '}
          {formatDate(entry.modifiedAt)}
        </span>
      </div>

      {/* Touch-only quick actions, revealed by a left swipe. */}
      <div className="pointer-events-none absolute inset-y-0 right-0 flex items-stretch sm:hidden">
        {!entry.isDir && canDownload && (
          <button
            className="pointer-events-auto flex w-14 items-center justify-center bg-[var(--sunk)] text-[var(--muted)]"
            onClick={() => onDownload(entry)}
            aria-label="下载"
          >
            <Download size={16} />
          </button>
        )}
        {canDelete && (
          <button
            className="pointer-events-auto flex w-14 items-center justify-center bg-[var(--color-danger)] text-white"
            onClick={() => onDelete(entry)}
            aria-label="删除"
          >
            <Trash2 size={16} />
          </button>
        )}
      </div>
    </div>
  )
}

function GridView({ entries, selection, onOpen, onContextMenu }: RowShared & { entries: Entry[] }) {
  return (
    <div className="grid grid-cols-2 gap-2 pt-3 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-5">
      {entries.map((entry) => (
        <GridTile
          key={entry.path}
          entry={entry}
          selection={selection}
          onOpen={onOpen}
          onContextMenu={onContextMenu}
        />
      ))}
    </div>
  )
}

function GridTile({
  entry,
  selection,
  onOpen,
  onContextMenu,
}: {
  entry: Entry
  selection: ReturnType<typeof useSelection<Entry>>
  onOpen: (entry: Entry) => void
  onContextMenu: (entry: Entry, position: { x: number; y: number }) => void
}) {
  const longPress = useLongPress({
    onLongPress: ({ clientX, clientY }) => onContextMenu(entry, { x: clientX, y: clientY }),
  })

  return (
    <button
      data-selectable={entry.path}
      data-selected={selection.isSelected(entry.path)}
      onClick={(e) => selection.click(entry.path, e)}
      onDoubleClick={() => onOpen(entry)}
      onContextMenu={(e) => {
        e.preventDefault()
        onContextMenu(entry, { x: e.clientX, y: e.clientY })
      }}
      {...longPress}
      className="surface flex flex-col items-start gap-2 p-3 text-left transition-colors hover:border-[var(--line-strong)] data-[selected=true]:border-[var(--color-clay)] data-[selected=true]:bg-[var(--clay-soft)]"
    >
      <EntryIcon name={entry.name} mime={entry.mime} isDir={entry.isDir} size={22} />
      <span className="line-clamp-2 w-full break-all text-sm leading-snug">{entry.name}</span>
      <span className="flex items-center gap-1.5 text-[11px] text-[var(--faint)]">
        {entry.isDir ? `文件夹 · ${formatBytes(entry.size)}` : formatBytes(entry.size)}
        <SegmentBadge entry={entry} />
      </span>
    </button>
  )
}

function SelectionBar({
  count,
  allSelected,
  entries,
  onSelectAll,
  onClear,
  onRename,
  onBatchRename,
  onMove,
  onDownload,
  onDelete,
  canRename,
  canMove,
  canDownload,
  canDelete,
}: {
  count: number
  allSelected: boolean
  entries: Entry[]
  onSelectAll: () => void
  onClear: () => void
  onRename: () => void
  onBatchRename: () => void
  onMove: () => void
  onDownload: () => void
  onDelete: () => void
  canRename: boolean
  canMove: boolean
  canDownload: boolean
  canDelete: boolean
}) {
  const files = entries.filter((e) => !e.isDir)
  return (
    <div className="fixed inset-x-0 bottom-6 z-30 mx-auto flex w-fit max-w-[92vw] items-center gap-1.5 rounded-full border border-[var(--line-strong)] bg-[var(--surface)]/95 px-4 py-2 shadow-lg backdrop-blur-md rise-in">
      <span className="mr-1 shrink-0 text-xs font-medium text-[var(--ink)]">已选 {count} 项</span>
      <div className="h-4 w-px bg-[var(--line)]" />
      <button onClick={onSelectAll} className="btn btn-ghost !px-2 !py-1 text-xs">
        {allSelected ? '取消全选' : '全选'}
      </button>
      {canRename && count === 1 && (
        <button onClick={onRename} className="btn btn-ghost !px-2 !py-1 text-xs" title="重命名">
          <Pencil size={14} className="text-[var(--muted)]" />
          <span className="hidden sm:inline">重命名</span>
        </button>
      )}
      {canRename && count > 1 && (
        <button onClick={onBatchRename} className="btn btn-ghost !px-2 !py-1 text-xs" title="批量重命名">
          <Rows3 size={14} className="text-[var(--muted)]" />
          <span className="hidden sm:inline">批量重命名</span>
        </button>
      )}
      {canMove && (
        <button onClick={onMove} className="btn btn-ghost !px-2 !py-1 text-xs" title="移动到...">
          <FolderInput size={14} className="text-[var(--muted)]" />
          <span className="hidden sm:inline">移动</span>
        </button>
      )}
      {canDownload && files.length > 0 && (
        <button onClick={onDownload} className="btn btn-ghost !px-2 !py-1 text-xs" title="下载">
          <Download size={14} className="text-[var(--muted)]" />
          <span className="hidden sm:inline">下载</span>
        </button>
      )}
      {canDelete && (
        <button onClick={onDelete} className="btn btn-danger !px-2 !py-1 text-xs" title="删除">
          <Trash2 size={14} />
          <span className="hidden sm:inline">删除</span>
        </button>
      )}
      <div className="h-4 w-px bg-[var(--line)]" />
      <IconButton
        label="取消选择"
        onClick={onClear}
        className="!p-1 text-[var(--faint)] hover:text-[var(--ink)]"
      >
        <X size={15} />
      </IconButton>
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

function DetailPanel({
  entry,
  onClose,
  onDownload,
}: {
  entry: Entry
  onClose: () => void
  onDownload: (entry: Entry) => void
}) {
  const [segments, setSegments] = useState<{ index: number; size: number; messageId: number }[]>([])

  useEffect(() => {
    if (entry.isDir) return
    void api
      .segments(entry.id)
      .then((r) => setSegments(r.segments))
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
        <Button
          variant="primary"
          className="w-full"
          icon={<Download size={15} />}
          onClick={() => onDownload(entry)}
        >
          下载 {formatBytes(entry.size)}
        </Button>

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
              尚未配置 VPS 文件目录，或当前账号没有这个权限。管理员可以前往「设置 → 存储」填写服务器目录（Docker
              下通常为 <span className="mx-1 font-[family-name:var(--font-mono)]">/vps</span>）。
            </p>
          ) : (
            <div className="rounded-[var(--radius-card)] border border-[var(--line)]">
              <div className="flex items-center gap-1 overflow-x-auto border-b border-[var(--line)] px-2 py-1.5">
                <IconButton
                  label="返回上一级"
                  disabled={localPath === '/'}
                  onClick={() => void browse(parentPath)}
                >
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
                      {selected.has(entry.path) && (
                        <Check size={14} className="shrink-0 text-[var(--color-clay)]" />
                      )}
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
