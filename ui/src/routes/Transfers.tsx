import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import clsx from 'clsx'
import {
  AlertTriangle,
  Ban,
  Check,
  ChevronDown,
  CloudDownload,
  Download,
  HardDrive,
  Inbox,
  Layers,
  Link2,
  RotateCw,
  Rows2,
  Rows3,
  Search,
  Server,
  Trash2,
  Upload as UploadIcon,
  X,
} from 'lucide-react'
import { api, type TransferRow } from '../lib/api'
import { events, type ServerEvent } from '../lib/events'
import { formatBytes, formatDate, formatDateTime, formatDuration, formatSpeed } from '../lib/format'
import { uploads, type Transfer } from '../lib/uploads'
import { downloads, type LocalDownload } from '../lib/downloads'
import { Button, Chip, EmptyState, IconButton, Segmented, Select, Spinner, toast } from '../components/primitives'

/**
 * The transfer panel.
 *
 * Three sources have to look like one list: uploads this browser is driving,
 * downloads this browser is driving, and the server's own record of both —
 * remote fetches, WebDAV writes, staged downloads, and everything anyone did
 * yesterday. The live ones win where they overlap, because only the browser
 * knows the byte count between two server-side checkpoints.
 *
 * The layout is deliberately dense. A transfer list is something you scan, and
 * the previous three-line-per-row card meant four transfers filled the screen.
 */

type KindFilter = 'all' | 'upload' | 'download'
type Density = 'compact' | 'comfortable'
type DatePreset = 'any' | 'today' | 'week' | 'month' | 'custom'

interface Row {
  id: string
  kind: 'upload' | 'download'
  name: string
  status: string
  /** 'running' covers pending too: both mean "not finished". */
  active: boolean
  total: number
  done: number
  speed: number
  avgSpeed: number
  createdAt: number
  finishedAt?: number
  startedAt?: number
  source: string
  segmentCount: number
  error?: string
  note?: string
  /** Set for transfers this browser is driving, which can be cancelled. */
  localId?: string
  fileId?: string
  mode?: string
}

const SOURCE_META: Record<string, { label: string; tone: 'clay' | 'blue' | 'green' | 'purple' | 'neutral' }> = {
  webui: { label: 'WebUI', tone: 'clay' },
  webdav: { label: 'WebDAV', tone: 'blue' },
  local: { label: 'VPS 本地', tone: 'green' },
  remote: { label: '离线下载', tone: 'purple' },
  direct: { label: '直接下载', tone: 'clay' },
  staged: { label: '服务器暂存', tone: 'blue' },
  segments: { label: '分卷下载', tone: 'purple' },
  // Plugins set their own source string. Listing the known ones here only
  // affects the label; an unknown source still renders through the fallback
  // below.
  aliyunpan: { label: '阿里云盘', tone: 'blue' },
}

const STATUS_LABEL: Record<string, string> = {
  pending: '排队中',
  running: '进行中',
  ready: '暂存完成',
  complete: '已完成',
  failed: '失败',
  cancelled: '已取消',
  expired: '已过期',
}

/**
 * What the event stream last said about a server-driven transfer.
 *
 * The transfer list is a snapshot, and refetching it is the slow way to learn
 * that four more megabytes landed. Progress events already carry the byte
 * count and the rate, so they are applied straight to the row and the refetch
 * is left to catch the things an event does not carry — a row appearing, a
 * status settling, a record being deleted.
 */
interface LiveSnapshot {
  done: number
  total: number
  speed: number
  /** When the event arrived, so a stalled transfer stops claiming a speed. */
  at: number
}

/** How long an event's speed is still a description of what is happening. */
const LIVE_SPEED_TTL = 5000
/** Refetch at most this often while events are streaming in. */
const RELOAD_INTERVAL = 1200
/** And poll this often while anything is moving, in case the stream is not. */
const ACTIVE_POLL_INTERVAL = 2000

function liveFromEvent(event: ServerEvent): { key: string; value: LiveSnapshot } | null {
  if (event.type === 'upload') {
    const data = event.data as { jobId?: string; uploaded?: number; total?: number; speed?: number }
    if (!data?.jobId) return null
    return {
      key: `upload:${data.jobId}`,
      value: { done: data.uploaded ?? 0, total: data.total ?? 0, speed: data.speed ?? 0, at: Date.now() },
    }
  }
  if (event.type === 'download') {
    const data = event.data as { jobId?: string; downloaded?: number; total?: number; speed?: number }
    if (!data?.jobId) return null
    return {
      key: `download:${data.jobId}`,
      value: { done: data.downloaded ?? 0, total: data.total ?? 0, speed: data.speed ?? 0, at: Date.now() },
    }
  }
  return null
}

function liveSpeed(snapshot?: LiveSnapshot): number {
  if (!snapshot) return 0
  return Date.now() - snapshot.at < LIVE_SPEED_TTL ? snapshot.speed : 0
}

export function Transfers() {
  const [local, setLocal] = useState<Transfer[]>([])
  const [localDownloads, setLocalDownloads] = useState<LocalDownload[]>([])
  const [remote, setRemote] = useState<TransferRow[]>([])
  const [live, setLive] = useState<Map<string, LiveSnapshot>>(() => new Map())
  const [loading, setLoading] = useState(true)

  const [kind, setKind] = useState<KindFilter>('all')
  const [statuses, setStatuses] = useState<Set<string>>(new Set())
  const [sources, setSources] = useState<Set<string>>(new Set())
  const [datePreset, setDatePreset] = useState<DatePreset>('any')
  const [customFrom, setCustomFrom] = useState('')
  const [customTo, setCustomTo] = useState('')
  const [search, setSearch] = useState('')
  const [density, setDensity] = useState<Density>(
    () => (localStorage.getItem('tdrive.transferDensity') as Density) || 'compact',
  )
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [expanded, setExpanded] = useState<Set<string>>(new Set())

  useEffect(() => uploads.subscribe(setLocal), [])
  useEffect(() => downloads.subscribe(setLocalDownloads), [])

  const range = useMemo(() => dateRange(datePreset, customFrom, customTo), [datePreset, customFrom, customTo])

  const reload = useCallback(async () => {
    try {
      const result = await api.transfers({
        kind: kind === 'all' ? undefined : kind,
        status: [...statuses],
        source: [...sources],
        from: range.from,
        to: range.to,
        q: search.trim() || undefined,
        limit: 400,
      })
      setRemote(result.transfers)
      // Drop the event-stream overlay for anything the list no longer has, so
      // the map does not accumulate a snapshot per transfer for the session.
      setLive((prev) => {
        if (prev.size === 0) return prev
        const alive = new Set(result.transfers.map((row) => `${row.kind}:${row.id}`))
        const next = new Map([...prev].filter(([key]) => alive.has(key)))
        return next.size === prev.size ? prev : next
      })
    } catch {
      /* a failed refresh leaves the previous list rather than blanking it */
    } finally {
      setLoading(false)
    }
  }, [kind, range.from, range.to, search, sources, statuses])

  useEffect(() => {
    void reload()
  }, [reload])

  // The subscription below must not be torn down and rebuilt every time a
  // filter changes, so it reaches the current reload through a ref.
  const reloadRef = useRef(reload)
  reloadRef.current = reload

  useEffect(() => {
    // Coalesce the refetch; do not debounce it. Progress events arrive several
    // times a second for as long as a transfer runs, so a debounce cancelled
    // itself on every event and the list only refreshed once everything had
    // stopped — which is why this page appeared frozen until it was refreshed
    // by hand. The first event schedules the refetch and the rest ride along.
    let timer: number | undefined
    const scheduleReload = () => {
      if (timer !== undefined) return
      timer = window.setTimeout(() => {
        timer = undefined
        void reloadRef.current()
      }, RELOAD_INTERVAL)
    }

    const unsubscribe = events.subscribe((event) => {
      const update = liveFromEvent(event)
      if (!update) return
      setLive((prev) => {
        const next = new Map(prev)
        const previous = next.get(update.key)
        next.set(update.key, {
          ...update.value,
          // Upload callbacks for parts in flight can arrive out of order, and
          // a byte count that goes backwards reads as a stall.
          done: Math.max(update.value.done, previous?.done ?? 0),
        })
        return next
      })
      scheduleReload()
    })

    return () => {
      unsubscribe()
      window.clearTimeout(timer)
    }
  }, [])

  const setDensityMode = (next: Density) => {
    setDensity(next)
    localStorage.setItem('tdrive.transferDensity', next)
  }

  const rows = useMemo(
    () => mergeRows(local, localDownloads, remote, live),
    [local, localDownloads, remote, live],
  )

  const hasActive = rows.some((row) => row.active)

  // A poll on top of the stream. Events are one long-lived HTTP response, and
  // a proxy that buffers it or a machine that slept leaves the page silently
  // stale; two seconds of polling while something is moving costs nothing and
  // means "in progress" is never a lie for long.
  useEffect(() => {
    if (!hasActive) return
    const timer = window.setInterval(() => void reloadRef.current(), ACTIVE_POLL_INTERVAL)
    return () => window.clearInterval(timer)
  }, [hasActive])

  // The server already applied the filters to its own rows; the live local
  // ones have to be filtered here so the two halves agree.
  const visible = useMemo(() => {
    return rows.filter((row) => {
      if (kind !== 'all' && row.kind !== kind) return false
      if (statuses.size > 0 && !statuses.has(normalizeStatus(row.status))) return false
      if (sources.size > 0 && !sources.has(row.source)) return false
      if (range.from && row.createdAt < range.from) return false
      if (range.to && row.createdAt > range.to) return false
      if (search.trim() && !row.name.toLowerCase().includes(search.trim().toLowerCase())) return false
      return true
    })
  }, [kind, range.from, range.to, rows, search, sources, statuses])

  const stats = useMemo(() => {
    const active = visible.filter((r) => r.active)
    const up = active.filter((r) => r.kind === 'upload')
    const down = active.filter((r) => r.kind === 'download')
    const todayStart = startOfDay(Date.now())
    const doneToday = rows.filter((r) => !r.active && (r.finishedAt ?? r.createdAt) >= todayStart)
    return {
      active: active.length,
      upSpeed: up.reduce((sum, r) => sum + r.speed, 0),
      downSpeed: down.reduce((sum, r) => sum + r.speed, 0),
      doneToday: doneToday.length,
      bytesToday: doneToday.reduce((sum, r) => sum + r.total, 0),
    }
  }, [rows, visible])

  const finished = visible.filter((r) => !r.active)
  const selectedRows = visible.filter((r) => selected.has(r.id))

  const toggleFilter = (set: Set<string>, apply: (next: Set<string>) => void, value: string) => {
    const next = new Set(set)
    if (next.has(value)) next.delete(value)
    else next.add(value)
    apply(next)
    setSelected(new Set())
  }

  const deleteSelected = async () => {
    const removable = selectedRows.filter((r) => !r.active)
    if (removable.length === 0) {
      // Deleting a row out from under a running transfer would orphan it, so
      // say what to do instead of doing nothing.
      toast('选中的传输任务仍在进行，请先取消再删除', 'info')
      return
    }
    if (!confirm(`确定删除 ${removable.length} 条传输记录？服务器上的暂存文件将一并清除。`)) return
    try {
      await api.deleteTransfers({ ids: removable.map((r) => r.id) })
      toast(`已删除 ${removable.length} 条记录`, 'success')
      setSelected(new Set())
      uploads.clearFinished()
      downloads.clearFinished()
      await reload()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  const clearFinished = async (scope: 'filtered' | 'all') => {
    const message =
      scope === 'all'
        ? '确定清空所有已结束的传输记录？服务器上的暂存文件将一并清除。'
        : `确定清空当前筛选出的 ${finished.length} 条已结束记录？`
    if (!confirm(message)) return
    try {
      const result = await api.deleteTransfers(
        scope === 'all'
          ? {}
          : {
              ids: finished.map((r) => r.id),
            },
      )
      toast(`已清除 ${result.removed} 条记录`, 'success')
      uploads.clearFinished()
      downloads.clearFinished()
      setSelected(new Set())
      await reload()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  const removeOne = async (row: Row) => {
    try {
      if (row.localId) {
        uploads.clearFinished()
        downloads.clearFinished()
      }
      await api.deleteTransfer(row.kind, row.id)
      await reload()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  const cancel = async (row: Row) => {
    // A cancellation that quietly fails is how a stuck transfer becomes a
    // permanent one: it stays "in progress", so the list offers no delete
    // button either. Report what went wrong instead of swallowing it.
    try {
      if (row.kind === 'upload') {
        // Awaited: the manager stops the request here and the transfer on the
        // server, and a refusal from either has to reach the toast below rather
        // than leaving a row that says cancelled while the upload continues.
        if (row.localId) await uploads.cancel(row.localId)
        else await api.cancelUpload(row.id)
      } else {
        if (row.localId) downloads.cancel(row.localId)
        else await api.cancelDownload(row.id)
      }
      await reload()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  const filtersActive =
    statuses.size > 0 || sources.size > 0 || datePreset !== 'any' || search.trim() !== ''

  return (
    <div className="flex h-full min-h-0 flex-1 flex-col">
      <div className="sticky top-0 z-20 shrink-0 border-b border-[var(--line)] bg-[var(--bg)]/90 backdrop-blur">
        <div className="mx-auto w-full max-w-5xl px-4 pt-4 sm:px-6">
          <div className="mb-3 flex flex-wrap items-center gap-3">
            <h1 className="display text-lg">传输</h1>
            <SummaryBar {...stats} />
            <div className="ml-auto flex items-center gap-1">
              <IconButton
                label={density === 'compact' ? '切换为舒适密度' : '切换为紧凑密度'}
                onClick={() => setDensityMode(density === 'compact' ? 'comfortable' : 'compact')}
              >
                {density === 'compact' ? <Rows3 size={15} /> : <Rows2 size={15} />}
              </IconButton>
              <IconButton label="刷新" onClick={() => void reload()}>
                <RotateCw size={15} />
              </IconButton>
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-2 pb-3">
            <Segmented
              value={kind}
              onChange={(next) => {
                setKind(next)
                setSelected(new Set())
              }}
              options={[
                { value: 'all', label: '全部' },
                { value: 'upload', label: '上传' },
                { value: 'download', label: '下载' },
              ]}
            />

            <div className="flex flex-wrap items-center gap-1.5">
              {(['running', 'complete', 'failed', 'cancelled'] as const).map((status) => (
                <Chip
                  key={status}
                  active={statuses.has(status)}
                  tone="neutral"
                  onClick={() => toggleFilter(statuses, setStatuses, status)}
                >
                  {STATUS_LABEL[status]}
                </Chip>
              ))}
            </div>

            <div className="flex flex-wrap items-center gap-1.5">
              {Object.entries(SOURCE_META)
                .filter(([key]) => ['webui', 'webdav', 'local', 'remote', 'aliyunpan', 'staged'].includes(key))
                .map(([key, meta]) => (
                  <Chip
                    key={key}
                    active={sources.has(key)}
                    tone={meta.tone}
                    onClick={() => toggleFilter(sources, setSources, key)}
                  >
                    {meta.label}
                  </Chip>
                ))}
            </div>

            <Select
              className="!w-auto !py-1.5 text-xs"
              value={datePreset}
              onChange={(e) => setDatePreset(e.target.value as DatePreset)}
            >
              <option value="any">全部时间</option>
              <option value="today">今天</option>
              <option value="week">近 7 天</option>
              <option value="month">近 30 天</option>
              <option value="custom">自定义…</option>
            </Select>

            {datePreset === 'custom' && (
              <div className="flex items-center gap-1.5">
                <input
                  type="date"
                  value={customFrom}
                  onChange={(e) => setCustomFrom(e.target.value)}
                  className="input !w-auto !py-1.5 text-xs"
                />
                <span className="text-xs text-[var(--faint)]">至</span>
                <input
                  type="date"
                  value={customTo}
                  onChange={(e) => setCustomTo(e.target.value)}
                  className="input !w-auto !py-1.5 text-xs"
                />
              </div>
            )}

            <div className="relative ml-auto min-w-40 flex-1 sm:max-w-56 sm:flex-none">
              <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--faint)]" />
              <input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="搜索文件名"
                className="input !py-1.5 !pl-7 text-xs"
              />
            </div>

            {filtersActive && (
              <IconButton
                label="清除筛选"
                onClick={() => {
                  setStatuses(new Set())
                  setSources(new Set())
                  setDatePreset('any')
                  setSearch('')
                }}
              >
                <X size={14} />
              </IconButton>
            )}
          </div>

          {(selected.size > 0 || finished.length > 0) && (
            <div className="flex flex-wrap items-center gap-2 border-t border-[var(--line)] py-2 text-xs">
              {selected.size > 0 ? (
                <>
                  <span className="text-[var(--muted)]">已选 {selected.size} 条</span>
                  <button
                    className="btn btn-danger !px-2 !py-1 text-xs"
                    onClick={() => void deleteSelected()}
                  >
                    <Trash2 size={13} />
                    删除所选
                  </button>
                  <button className="btn btn-ghost !px-2 !py-1 text-xs" onClick={() => setSelected(new Set())}>
                    取消选择
                  </button>
                </>
              ) : (
                <>
                  <span className="text-[var(--faint)]">{finished.length} 条已结束</span>
                  <button
                    className="btn btn-ghost !px-2 !py-1 text-xs"
                    onClick={() => void clearFinished('filtered')}
                  >
                    清除当前筛选
                  </button>
                  <button
                    className="btn btn-ghost !px-2 !py-1 text-xs text-[var(--color-danger)]"
                    onClick={() => void clearFinished('all')}
                  >
                    清空全部记录
                  </button>
                </>
              )}
            </div>
          )}
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto w-full max-w-5xl px-4 py-3 sm:px-6">
          {loading ? (
            <div className="flex justify-center py-20">
              <Spinner />
            </div>
          ) : visible.length === 0 ? (
            <EmptyState
              icon={<Inbox size={30} />}
              title={filtersActive ? '无符合条件的传输任务' : '暂无传输记录'}
              description={
                filtersActive
                  ? '尝试调整或清除筛选条件以查看全部记录。'
                  : '文件上传、下载与离线下载进度均在此展示。'
              }
              action={
                filtersActive ? (
                  <Button
                    onClick={() => {
                      setStatuses(new Set())
                      setSources(new Set())
                      setDatePreset('any')
                      setSearch('')
                      setKind('all')
                    }}
                  >
                    清除筛选
                  </Button>
                ) : undefined
              }
            />
          ) : (
            <div className={clsx('divide-y divide-[var(--line)]')}>
              {visible.map((row) => (
                <TransferRowView
                  key={`${row.kind}:${row.id}`}
                  row={row}
                  density={density}
                  selected={selected.has(row.id)}
                  expanded={expanded.has(row.id)}
                  onToggleSelect={() =>
                    setSelected((prev) => {
                      const next = new Set(prev)
                      if (next.has(row.id)) next.delete(row.id)
                      else next.add(row.id)
                      return next
                    })
                  }
                  onToggleExpand={() =>
                    setExpanded((prev) => {
                      const next = new Set(prev)
                      if (next.has(row.id)) next.delete(row.id)
                      else next.add(row.id)
                      return next
                    })
                  }
                  onCancel={() => void cancel(row)}
                  onDelete={() => void removeOne(row)}
                />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function SummaryBar({
  active,
  upSpeed,
  downSpeed,
  doneToday,
  bytesToday,
}: {
  active: number
  upSpeed: number
  downSpeed: number
  doneToday: number
  bytesToday: number
}) {
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-[var(--muted)]">
      {active > 0 ? (
        <span className="flex items-center gap-1.5">
          <span className="size-1.5 animate-pulse rounded-full bg-[var(--color-clay)]" />
          进行中 {active}
        </span>
      ) : (
        <span className="text-[var(--faint)]">空闲</span>
      )}
      {upSpeed > 0 && (
        <span className="flex items-center gap-1 tabular-nums">
          <UploadIcon size={11} />
          {formatSpeed(upSpeed)}
        </span>
      )}
      {downSpeed > 0 && (
        <span className="flex items-center gap-1 tabular-nums">
          <Download size={11} />
          {formatSpeed(downSpeed)}
        </span>
      )}
      <span className="text-[var(--faint)]">
        今日完成 {doneToday} 个 · {formatBytes(bytesToday)}
      </span>
    </div>
  )
}

function TransferRowView({
  row,
  density,
  selected,
  expanded,
  onToggleSelect,
  onToggleExpand,
  onCancel,
  onDelete,
}: {
  row: Row
  density: Density
  selected: boolean
  expanded: boolean
  onToggleSelect: () => void
  onToggleExpand: () => void
  onCancel: () => void
  onDelete: () => void
}) {
  const pct = row.total > 0 ? Math.min(100, (row.done / row.total) * 100) : 0
  const source = SOURCE_META[row.source] ?? SOURCE_META.webui

  return (
    <div
      className={clsx(
        'group relative',
        density === 'compact' ? 'py-1.5' : 'py-3',
        selected && 'bg-[var(--clay-soft)]/40',
      )}
    >
      {/* Progress is the row's own background rather than a separate bar: it
          saves a line of height per row without losing the information. */}
      {row.active && (
        <div
          className="pointer-events-none absolute inset-y-0 left-0 bg-[var(--clay-soft)]/60 transition-[width] duration-500"
          style={{ width: `${pct}%` }}
        />
      )}

      <div className="relative flex items-center gap-2.5 px-2">
        <button
          onClick={onToggleSelect}
          aria-label="选择"
          className={clsx(
            'flex size-4 shrink-0 items-center justify-center rounded border transition-colors',
            selected
              ? 'border-[var(--color-clay)] bg-[var(--color-clay)] text-white'
              : 'border-[var(--line-strong)] opacity-0 group-hover:opacity-100',
          )}
        >
          {selected && <Check size={11} />}
        </button>

        <StateIcon row={row} />

        <button onClick={onToggleExpand} className="flex min-w-0 flex-1 items-center gap-2 text-left">
          <span className="truncate text-sm">{row.name}</span>
          {row.segmentCount > 1 && (
            <span className="chip shrink-0" title={`${row.segmentCount} 个分卷`}>
              <Layers size={10} />
              {row.segmentCount}
            </span>
          )}
          {density === 'comfortable' && (
            <span className={clsx('chip shrink-0 !border-transparent', toneClass(source.tone))}>
              {source.label}
            </span>
          )}
        </button>

        <div className="flex shrink-0 items-center gap-3 text-xs tabular-nums text-[var(--muted)]">
          {row.active ? (
            <>
              {row.speed > 0 && (
                <span className="hidden font-medium text-[var(--color-clay)] sm:inline">
                  {formatSpeed(row.speed)}
                </span>
              )}
              <span className="hidden sm:inline">
                {formatBytes(row.done)} / {formatBytes(row.total)}
              </span>
              <span className="w-9 text-right sm:hidden">{Math.round(pct)}%</span>
            </>
          ) : (
            <>
              {row.avgSpeed > 0 && (
                <span className="hidden text-[var(--faint)] sm:inline" title="平均速度">
                  均 {formatSpeed(row.avgSpeed)}
                </span>
              )}
              <span>{formatBytes(row.total)}</span>
              <span className="hidden text-[var(--faint)] lg:inline" title={formatDateTime(row.createdAt)}>
                {formatDate(row.finishedAt ?? row.createdAt)}
              </span>
            </>
          )}
        </div>

        <div className="flex shrink-0 items-center">
          {row.active ? (
            <IconButton label="取消" onClick={onCancel} className="!p-1.5">
              <Ban size={14} />
            </IconButton>
          ) : (
            <IconButton
              label="删除记录"
              onClick={onDelete}
              className="!p-1.5 opacity-0 group-hover:opacity-100"
            >
              <Trash2 size={14} />
            </IconButton>
          )}
          <ChevronDown
            size={14}
            className={clsx(
              'shrink-0 text-[var(--faint)] transition-transform',
              expanded && 'rotate-180',
            )}
          />
        </div>
      </div>

      {(expanded || row.error) && (
        <div className="relative mt-1.5 space-y-1 px-2 pl-12 text-xs text-[var(--muted)]">
          {row.error && <p className="text-[var(--color-danger)]">{row.error}</p>}
          {row.note && <p className="text-[var(--faint)]">{row.note}</p>}
          {expanded && (
            <dl className="grid gap-x-6 gap-y-0.5 sm:grid-cols-2">
              <Detail label="来源" value={source.label} />
              <Detail label="状态" value={STATUS_LABEL[normalizeStatus(row.status)] ?? row.status} />
              <Detail label="开始" value={row.startedAt ? formatDateTime(row.startedAt) : '—'} />
              <Detail label="用时" value={elapsedLabel(row)} />
              <Detail label="平均速度" value={row.avgSpeed > 0 ? formatSpeed(row.avgSpeed) : '—'} />
              <Detail label="创建" value={formatDateTime(row.createdAt)} />
            </dl>
          )}
        </div>
      )}
    </div>
  )
}

function Detail({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="text-[var(--faint)]">{label}</dt>
      <dd className="truncate">{value}</dd>
    </div>
  )
}

/** How long a transfer has been going, counted to now while it is still
 *  running. Waiting for a finish time meant a transfer showed a dash for its
 *  entire run, which is precisely when the number is worth reading. */
function elapsedLabel(row: Row): string {
  if (!row.startedAt) return '—'
  const until = row.finishedAt ?? (row.active ? Date.now() : undefined)
  if (!until || until <= row.startedAt) return '—'
  return formatDuration((until - row.startedAt) / 1000) || '不到 1 秒'
}

function StateIcon({ row }: { row: Row }) {
  const status = normalizeStatus(row.status)
  if (status === 'complete' || status === 'ready') {
    return (
      <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-[var(--color-success)]/12">
        <Check size={12} className="text-[var(--color-success)]" />
      </span>
    )
  }
  if (status === 'failed') {
    return (
      <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-[var(--color-danger)]/12">
        <AlertTriangle size={11} className="text-[var(--color-danger)]" />
      </span>
    )
  }
  if (status === 'cancelled' || status === 'expired') {
    return (
      <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-[var(--sunk)]">
        <Ban size={11} className="text-[var(--faint)]" />
      </span>
    )
  }

  const Icon =
    row.kind === 'download'
      ? row.mode === 'staged'
        ? Server
        : row.mode === 'webdav'
          ? Link2
          : Download
      : row.source === 'remote'
        ? CloudDownload
        : row.source === 'local'
          ? HardDrive
          : row.source === 'webdav'
            ? Link2
            : UploadIcon

  return (
    <span className="flex size-5 shrink-0 items-center justify-center">
      <Icon size={13} className="text-[var(--color-clay)]" />
    </span>
  )
}

function toneClass(tone: string): string {
  switch (tone) {
    case 'blue':
      return '!bg-blue-500/12 !text-blue-600 dark:!text-blue-400'
    case 'green':
      return '!bg-green-500/12 !text-green-700 dark:!text-green-400'
    case 'purple':
      return '!bg-purple-500/12 !text-purple-600 dark:!text-purple-400'
    case 'neutral':
      return '!bg-[var(--sunk)] !text-[var(--muted)]'
    default:
      return '!bg-[var(--clay-soft)] !text-[var(--color-clay)]'
  }
}

function normalizeStatus(status: string): string {
  return status === 'pending' || status === 'uploading' || status === 'downloading' || status === 'preparing' || status === 'merging'
    ? 'running'
    : status
}

/**
 * mergeRows folds the sources into one list.
 *
 * A transfer this browser is driving appears in both the live map and the
 * server's list, and the live one is authoritative: the server only learns of
 * progress at segment boundaries, so its number lags by up to a whole segment.
 *
 * For a transfer the server drives — a WebDAV write, a VPS-local upload, a
 * remote fetch, a staged download — there is no browser to ask, so the event
 * stream plays that role and its snapshot is laid over the fetched row.
 */
function mergeRows(
  localUploads: Transfer[],
  localDownloads: LocalDownload[],
  remote: TransferRow[],
  live: Map<string, LiveSnapshot>,
): Row[] {
  const rows = new Map<string, Row>()

  for (const row of remote) {
    if (row.upload) {
      const job = row.upload
      const key = `upload:${row.id}`
      const snapshot = live.get(key)
      rows.set(key, {
        id: row.id,
        kind: 'upload',
        name: job.name,
        status: job.status,
        active: job.status === 'running' || job.status === 'pending',
        total: Math.max(job.totalSize, snapshot?.total ?? 0),
        done: Math.max(job.uploadedBytes, snapshot?.done ?? 0),
        speed: liveSpeed(snapshot) || job.speed || 0,
        avgSpeed: job.avgSpeed ?? 0,
        createdAt: typeof job.createdAt === 'number' ? job.createdAt : Date.parse(String(job.createdAt)),
        startedAt: job.startedAt,
        finishedAt: job.finishedAt,
        source: job.source ?? 'webui',
        segmentCount: job.segmentCount,
        error: job.error,
        fileId: job.fileId,
      })
    } else if (row.download) {
      const job = row.download
      const key = `download:${row.id}`
      const snapshot = live.get(key)
      rows.set(key, {
        id: row.id,
        kind: 'download',
        name: job.name,
        status: job.status,
        active: job.status === 'running' || job.status === 'pending',
        total: Math.max(job.totalSize, snapshot?.total ?? 0),
        done: Math.max(job.downloadedBytes, snapshot?.done ?? 0),
        speed: liveSpeed(snapshot) || job.speed || 0,
        avgSpeed: job.avgSpeed ?? 0,
        createdAt: job.createdAt,
        startedAt: job.startedAt,
        finishedAt: job.finishedAt,
        source: job.mode,
        segmentCount: 0,
        error: job.error,
        fileId: job.fileId,
        mode: job.mode,
      })
    }
  }

  for (const transfer of localUploads) {
    const key = `upload:${transfer.id}`
    const existing = rows.get(key)
    rows.set(key, {
      id: transfer.id,
      kind: 'upload',
      name: transfer.name,
      status: transfer.state === 'uploading' ? 'running' : transfer.state,
      active: transfer.state === 'uploading' || transfer.state === 'queued',
      total: transfer.size,
      // The live counter can only move forward; a stale server snapshot must
      // not drag it back.
      done: Math.max(transfer.uploaded, existing?.done ?? 0),
      speed: transfer.speed ?? existing?.speed ?? 0,
      avgSpeed: existing?.avgSpeed ?? 0,
      createdAt: existing?.createdAt ?? Date.now(),
      startedAt: existing?.startedAt,
      finishedAt: existing?.finishedAt,
      source: existing?.source ?? 'webui',
      segmentCount: transfer.segmentCount,
      error: transfer.error,
      localId: transfer.id,
    })
  }

  for (const download of localDownloads) {
    const key = download.jobId ? `download:${download.jobId}` : `download:${download.id}`
    const existing = rows.get(key)
    rows.set(key, {
      id: download.jobId ?? download.id,
      kind: 'download',
      name: download.name,
      status:
        download.state === 'downloading' || download.state === 'preparing' || download.state === 'merging'
          ? 'running'
          : download.state,
      active: ['queued', 'preparing', 'downloading', 'merging'].includes(download.state),
      total: download.size,
      done: Math.max(download.received, existing?.done ?? 0),
      speed: download.speed || existing?.speed || 0,
      avgSpeed:
        download.finishedAt && download.startedAt && download.finishedAt > download.startedAt
          ? (download.size / (download.finishedAt - download.startedAt)) * 1000
          : (existing?.avgSpeed ?? 0),
      createdAt: download.startedAt,
      startedAt: download.startedAt,
      finishedAt: download.finishedAt,
      source: download.mode,
      segmentCount: 0,
      error: download.error,
      note: download.note,
      localId: download.id,
      fileId: download.fileId,
      mode: download.mode,
    })
  }

  return [...rows.values()].sort((a, b) => {
    // Anything moving goes to the top; the rest is newest-first history.
    if (a.active !== b.active) return a.active ? -1 : 1
    return b.createdAt - a.createdAt
  })
}

function startOfDay(ms: number): number {
  const date = new Date(ms)
  date.setHours(0, 0, 0, 0)
  return date.getTime()
}

function dateRange(preset: DatePreset, from: string, to: string): { from?: number; to?: number } {
  const now = Date.now()
  switch (preset) {
    case 'today':
      return { from: startOfDay(now) }
    case 'week':
      return { from: startOfDay(now - 6 * 86_400_000) }
    case 'month':
      return { from: startOfDay(now - 29 * 86_400_000) }
    case 'custom': {
      const parsedFrom = from ? new Date(`${from}T00:00:00`).getTime() : undefined
      // The end of the chosen day, not its start: picking the same date for
      // both bounds must include that whole day.
      const parsedTo = to ? new Date(`${to}T23:59:59.999`).getTime() : undefined
      return { from: parsedFrom, to: parsedTo }
    }
    default:
      return {}
  }
}
