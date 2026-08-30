import { useCallback, useEffect, useState } from 'react'
import clsx from 'clsx'
import {
  Check,
  Copy,
  Gauge,
  HardDriveDownload,
  Layers,
  Link2,
  Loader2,
  Server,
  Zap,
} from 'lucide-react'
import {
  api,
  type DownloadMode,
  type DownloadOptions,
  type Entry,
  type ShareResponse,
} from '../lib/api'
import { COPY_FAILED, copyText } from '../lib/clipboard'
import { downloads, supportsDiskWrites } from '../lib/downloads'
import { formatBytes } from '../lib/format'
import { Button, Modal, Slider, Spinner, toast } from './primitives'

/**
 * The download dialog exists because for a segmented file the choice actually
 * matters, and the server is the only party that knows why.
 *
 * It also carries the reusable link, which is the answer for anyone who would
 * rather use aria2 or IDM than a browser tab — and for a 40 GB file, that is
 * usually the right instinct.
 */

const MODE_META: Record<
  DownloadMode,
  { title: string; icon: typeof Zap; blurb: string }
> = {
  direct: {
    title: '直接下载',
    icon: Zap,
    blurb: '浏览器直接向服务器请求字节，服务器边从 Telegram 读边发。',
  },
  staged: {
    title: '先暂存到服务器',
    icon: Server,
    blurb: '服务器先把整个文件拼好放到本地磁盘，然后你从磁盘高速取走。',
  },
  segments: {
    title: '分卷下载后合并',
    icon: Layers,
    blurb: '每个分卷单独下载，再在本地合并成完整文件，不占服务器磁盘。',
  },
}

export function DownloadDialog({
  entry,
  onClose,
}: {
  entry: Entry | null
  onClose: () => void
}) {
  const [options, setOptions] = useState<DownloadOptions | null>(null)
  const [mode, setMode] = useState<DownloadMode>('direct')
  const [connections, setConnections] = useState(4)
  const [error, setError] = useState<string | null>(null)
  const [share, setShare] = useState<ShareResponse | null>(null)
  const [sharing, setSharing] = useState(false)
  const [starting, setStarting] = useState(false)

  const open = entry !== null

  useEffect(() => {
    if (!entry) return
    let cancelled = false
    setOptions(null)
    setShare(null)
    setError(null)

    void api
      .downloadOptions(entry.id)
      .then((next) => {
        if (cancelled) return
        setOptions(next)
        const recommended = next.modes.find((m) => m.recommended && m.available)
        setMode(recommended?.mode ?? 'direct')
        // Without disk access every extra connection buys a copy of the file in
        // RAM, while a single connection is handed straight to the browser's own
        // downloader. Defaulting to parallel there is how a routine download
        // turned into a tab-sized memory buffer.
        setConnections(supportsDiskWrites() ? Math.min(4, next.maxConnections) : 1)
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      })

    return () => {
      cancelled = true
    }
  }, [entry])

  const start = useCallback(async () => {
    if (!entry || !options) return
    setStarting(true)
    try {
      // The save picker has to open while the click is still considered a user
      // gesture, so it goes first — before any await that could outlive it.
      const parallel = connections > 1
      const wantsDisk = parallel || mode === 'segments'
      const handle = wantsDisk ? await downloads.openSaveTarget(entry.name) : null

      const running = downloads.start({
        fileId: entry.id,
        name: entry.name,
        size: entry.size,
        mode,
        connections,
        segmentBounds: options.segmentBounds,
        saveHandle: handle,
      })

      // The transfer belongs to the transfer panel from here on. Awaiting it in
      // the dialog left the button spinning for the whole of a multi-gigabyte
      // download — and for staging, for the whole server-side assembly — which
      // reads as "nothing happened" even though the task was already running.
      let settled = false
      running
        .then(() => {
          settled = true
          toast(`"${entry.name}" 下载完成`, 'success')
        })
        .catch((err) => {
          settled = true
          // A cancellation is the user's own decision, not something to report.
          if (err instanceof DOMException && err.name === 'AbortError') return
          toast(err instanceof Error ? err.message : String(err), 'error')
        })

      onClose()
      // Closing the dialog on its own looks like the click did nothing, so a
      // transfer that is still going says where to watch it. One that finished
      // in the meantime — a small file handed straight to the browser — says
      // nothing, because its own result is about to arrive.
      setTimeout(() => {
        if (!settled) toast(`"${entry.name}" 已开始下载，进度在「传输」里`, 'info')
      }, 600)
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') return
      toast(err instanceof Error ? err.message : String(err), 'error')
    } finally {
      setStarting(false)
    }
  }, [connections, entry, mode, onClose, options])

  const mintShare = async (withSegments: boolean) => {
    if (!entry) return
    setSharing(true)
    try {
      const link = await api.share(entry.id, { segments: withSegments })
      setShare(link)
      const copied = await copyText(link.file.url)
      toast(copied ? '下载直链已生成并复制到剪贴板' : '下载直链已生成，请手动复制', copied ? 'success' : 'info')
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    } finally {
      setSharing(false)
    }
  }

  if (!entry) return null

  const selected = options?.modes.find((m) => m.mode === mode)
  const canStart = Boolean(options) && (selected?.available ?? false)

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="下载"
      description={`${entry.name} · ${formatBytes(entry.size)}${
        options && options.segmentCount > 1 ? ` · ${options.segmentCount} 卷` : ''
      }`}
      width="max-w-2xl"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button
            variant="primary"
            loading={starting}
            disabled={!canStart}
            icon={<HardDriveDownload size={15} />}
            onClick={() => void start()}
          >
            开始下载
          </Button>
        </>
      }
    >
      {error ? (
        <p className="text-sm text-[var(--color-danger)]">{error}</p>
      ) : !options ? (
        <div className="flex justify-center py-10">
          <Spinner />
        </div>
      ) : (
        <div className="space-y-5">
          <section className="space-y-2">
            {options.modes.map((info) => {
              const meta = MODE_META[info.mode]
              const Icon = meta.icon
              const active = mode === info.mode
              return (
                <button
                  key={info.mode}
                  disabled={!info.available}
                  onClick={() => setMode(info.mode)}
                  className={clsx(
                    'flex w-full items-start gap-3 rounded-[var(--radius-card)] border p-3 text-left transition-colors',
                    active
                      ? 'border-[var(--color-clay)] bg-[var(--clay-soft)]'
                      : 'border-[var(--line)] hover:border-[var(--line-strong)]',
                    !info.available && 'cursor-not-allowed opacity-45',
                  )}
                >
                  <Icon
                    size={17}
                    className={clsx('mt-0.5 shrink-0', active ? 'text-[var(--color-clay)]' : 'text-[var(--faint)]')}
                  />
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{meta.title}</span>
                      {info.recommended && info.available && (
                        <span className="chip !border-transparent !bg-[var(--clay-soft)] !text-[var(--color-clay)]">
                          推荐
                        </span>
                      )}
                      {!info.available && <span className="chip">不可用</span>}
                    </div>
                    <p className="mt-1 text-xs leading-relaxed text-[var(--muted)]">{meta.blurb}</p>
                    {info.reason && (
                      <p className="mt-1 text-xs leading-relaxed text-[var(--faint)]">{info.reason}</p>
                    )}
                  </div>
                  {active && <Check size={15} className="mt-0.5 shrink-0 text-[var(--color-clay)]" />}
                </button>
              )
            })}
          </section>

          <section className="space-y-2 border-t border-[var(--line)] pt-4">
            <div className="flex items-center gap-2">
              <Gauge size={15} className="text-[var(--faint)]" />
              <h3 className="text-sm font-medium">并发连接数</h3>
            </div>
            <Slider
              value={connections}
              min={1}
              max={options.maxConnections}
              onChange={setConnections}
              suffix="条"
              format={(value) =>
                value === 1
                  ? '单线程：交给浏览器自己下载，最省事'
                  : `${value} 条连接并行下载；服务器把它们算作一个下载任务`
              }
            />
            {!supportsDiskWrites() && connections > 1 && (
              <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs leading-relaxed text-[var(--muted)]">
                当前浏览器不支持直接写入磁盘（需要 Chrome 或 Edge）。多线程下载会先在内存中拼接，
                超大文件建议改用单线程，或复制下面的直链交给 aria2 / IDM。
              </p>
            )}
          </section>

          <section className="space-y-2 border-t border-[var(--line)] pt-4">
            <div className="flex items-center gap-2">
              <Link2 size={15} className="text-[var(--faint)]" />
              <h3 className="text-sm font-medium">可复用直链</h3>
            </div>
            <p className="text-xs leading-relaxed text-[var(--muted)]">
              生成一条带独立令牌的完整下载地址，可以直接粘贴到 aria2、IDM、迅雷或另一台设备上，
              支持多线程和断点续传。链接可以随时在「设置 → 安全」里撤销。
            </p>

            {share ? (
              <div className="space-y-2">
                <LinkRow url={share.file.url} label="完整文件" />
                {share.segments?.map((segment) => (
                  <LinkRow key={segment.id} url={segment.url} label={`第 ${segment.index} 卷`} />
                ))}
              </div>
            ) : (
              <div className="flex flex-wrap gap-2">
                <Button loading={sharing} icon={<Link2 size={14} />} onClick={() => void mintShare(false)}>
                  生成完整文件直链
                </Button>
                {options.segmentCount > 1 && (
                  <Button loading={sharing} icon={<Layers size={14} />} onClick={() => void mintShare(true)}>
                    生成每个分卷的直链
                  </Button>
                )}
              </div>
            )}
          </section>

          {options.cache.limit > 0 && (
            <p className="text-xs text-[var(--faint)]">
              服务器暂存空间：已用 {formatBytes(options.cache.used)} / {formatBytes(options.cache.limit)}
              {options.staged && '（这个文件已有暂存副本，可以直接高速下载）'}
            </p>
          )}
        </div>
      )}
    </Modal>
  )
}

function LinkRow({ url, label }: { url: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="flex items-center gap-2">
      <span className="w-20 shrink-0 text-xs text-[var(--muted)]">{label}</span>
      <input
        readOnly
        value={url}
        onFocus={(e) => e.currentTarget.select()}
        className="input min-w-0 flex-1 font-[family-name:var(--font-mono)] !py-1.5 text-xs"
      />
      <Button
        className="shrink-0"
        icon={copied ? <Check size={14} /> : <Copy size={14} />}
        onClick={() => {
          void copyText(url).then((ok) => {
            if (!ok) {
              toast(COPY_FAILED, 'error')
              return
            }
            setCopied(true)
            setTimeout(() => setCopied(false), 1500)
          })
        }}
      >
        {copied ? '已复制' : '复制'}
      </Button>
    </div>
  )
}

/** DownloadSpinner is the inline indicator the file list shows while a
 *  download is being prepared. */
export function DownloadSpinner() {
  return <Loader2 size={14} className="animate-spin text-[var(--color-clay)]" />
}
