import { useEffect, useState } from 'react'
import clsx from 'clsx'
import { AlertTriangle, Ban, Check, CloudDownload, HardDrive, Inbox, Layers } from 'lucide-react'
import { api, type UploadJob } from '../lib/api'
import { events } from '../lib/events'
import { formatBytes, formatDate } from '../lib/format'
import { uploads, type Transfer } from '../lib/uploads'
import { Button, EmptyState, IconButton, Progress } from '../components/primitives'

/**
 * The transfer panel merges two sources: uploads this browser is driving, held
 * in memory by the upload manager, and server-side jobs (remote URL fetches,
 * WebDAV writes, other sessions) that arrive over the event stream.
 */
export function Transfers() {
  const [local, setLocal] = useState<Transfer[]>([])
  const [jobs, setJobs] = useState<UploadJob[]>([])
  const [remoteSpeeds, setRemoteSpeeds] = useState<Record<string, number>>({})

  useEffect(() => uploads.subscribe(setLocal), [])

  useEffect(() => {
    const history = new Map<string, { bytes: number; time: number; speed: number }>()
    let knownJobs: UploadJob[] = []

    const terminal = (status: UploadJob['status']) =>
      status === 'complete' || status === 'failed' || status === 'cancelled'

    // The jobs endpoint is backed by completed-segment state and can therefore
    // lag behind the SSE stream. Keep progress monotonic when those two sources
    // cross in flight, otherwise a refresh briefly produces a fake speed spike.
    const mergeJob = (current: UploadJob | undefined, incoming: UploadJob): UploadJob => {
      if (!current) return incoming

      let status = incoming.status
      if (terminal(current.status) && !terminal(incoming.status)) {
        status = current.status
      } else if (!terminal(current.status) && !terminal(incoming.status)) {
        status = current.status === 'running' || incoming.status === 'running' ? 'running' : 'pending'
      }

      return {
        ...current,
        ...incoming,
        uploadedBytes: Math.max(current.uploadedBytes, incoming.uploadedBytes),
        totalSize: incoming.totalSize || current.totalSize,
        segmentSize: incoming.segmentSize || current.segmentSize,
        segmentCount: incoming.segmentCount || current.segmentCount,
        status,
      }
    }

    const calculateSpeed = (id: string, uploadedBytes: number, status: UploadJob['status']) => {
      const now = performance.now()
      if (status !== 'running' && status !== 'pending') {
        history.delete(id)
        return 0
      }
      const prev = history.get(id)
      if (prev) {
        const dt = (now - prev.time) / 1000
        if (dt > 5.0) {
          history.set(id, { bytes: uploadedBytes, time: now, speed: 0 })
          return 0
        } else if (dt >= 0.25) {
          const db = Math.max(0, uploadedBytes - prev.bytes)
          const instantSpeed = db / dt
          const smooth = prev.speed === 0 ? instantSpeed : prev.speed * 0.7 + instantSpeed * 0.3
          history.set(id, { bytes: uploadedBytes, time: now, speed: smooth })
          return smooth
        }
        return prev.speed
      } else {
        history.set(id, { bytes: uploadedBytes, time: now, speed: 0 })
        return 0
      }
    }

    const publishJobs = (next: UploadJob[]) => {
      knownJobs = next
      const currentIds = new Set(knownJobs.map((j) => j.id))
      for (const id of history.keys()) {
        if (!currentIds.has(id)) history.delete(id)
      }
      const speeds: Record<string, number> = {}
      for (const j of knownJobs) {
        speeds[j.id] = calculateSpeed(j.id, j.uploadedBytes, j.status)
      }
      setJobs(knownJobs)
      setRemoteSpeeds(speeds)
    }

    const updateJobs = (next: UploadJob[]) => {
      const incomingIds = new Set(next.map((j) => j.id))
      const merged = next.map((j) => mergeJob(knownJobs.find((current) => current.id === j.id), j))
      // Do not let a stale list response discard an event that arrived before it.
      merged.push(...knownJobs.filter((j) => !incomingIds.has(j.id)))
      publishJobs(merged)
    }

    const updateJob = (incoming: UploadJob) => {
      const idx = knownJobs.findIndex((j) => j.id === incoming.id)
      if (idx < 0) {
        publishJobs([incoming, ...knownJobs])
        return
      }
      const next = [...knownJobs]
      next[idx] = mergeJob(next[idx], incoming)
      publishJobs(next)
    }

    const load = () => void api.jobs().then(updateJobs).catch(() => {})

    // Subscribe first so an upload event cannot arrive between the initial
    // snapshot request and the subscription being established.
    const unsubscribe = events.subscribe((event) => {
      if (event.type === 'upload') {
        const data = event.data as {
          jobId: string
          fileId?: string
          name: string
          uploaded: number
          total: number
          segmentCount: number
          status: UploadJob['status']
          error?: string
          source?: string
          sourceUrl?: string
        }
        if (!data || !data.jobId) {
          load()
          return
        }

        const current = knownJobs.find((j) => j.id === data.jobId)
        const now = new Date().toISOString()
        updateJob({
          id: data.jobId,
          fileId: data.fileId || current?.fileId,
          dirId: current?.dirId,
          name: data.name || current?.name || '上传',
          totalSize: data.total || current?.totalSize || 0,
          segmentSize: current?.segmentSize || 0,
          segmentCount: data.segmentCount || current?.segmentCount || 0,
          uploadedBytes: Math.max(0, data.uploaded || 0),
          status: data.status,
          error: data.error,
          source: data.source || current?.source,
          sourceUrl: data.sourceUrl || current?.sourceUrl,
          createdAt: current?.createdAt || now,
          updatedAt: now,
        })
      }
    })

    load()
    return unsubscribe
  }, [])
  // A job this browser is already showing locally would otherwise appear
  // twice, once with live progress and once with the server's last snapshot.
  const localIds = new Set(local.map((t) => t.id))
  const remote = jobs.filter((j) => !localIds.has(j.id))

  const empty = local.length === 0 && remote.length === 0

  return (
    <div className="h-full min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto w-full max-w-3xl px-4 py-6 sm:px-6">
        <header className="mb-5 flex items-center justify-between">
          <div>
            <h1 className="display text-xl">传输</h1>
            <p className="mt-1 text-sm text-[var(--muted)]">
              大文件会拆成多个分卷分别上传，失败时只重传缺失的那一卷。
            </p>
          </div>
          {local.some((t) => t.state === 'complete' || t.state === 'cancelled') && (
            <Button onClick={() => uploads.clearFinished()}>清除已完成</Button>
          )}
        </header>

        {empty ? (
          <EmptyState
            icon={<Inbox size={30} />}
            title="没有正在进行的传输"
            description="上传或从链接下载时，进度会显示在这里。"
          />
        ) : (
          <div className="space-y-2">
            {local.map((t) => (
              <LocalRow key={t.id} transfer={t} />
            ))}
            {remote.map((j) => (
              <RemoteRow key={j.id} job={j} speed={remoteSpeeds[j.id] ?? 0} />
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

function LocalRow({ transfer }: { transfer: Transfer }) {
  const pct = transfer.size > 0 ? (transfer.uploaded / transfer.size) * 100 : 0
  const active = transfer.state === 'uploading'

  return (
    <div className="surface p-3.5">
      <div className="flex items-start gap-3">
        <StateDot state={transfer.state} />
        <div className="min-w-0 flex-1">
          <div className="flex items-baseline justify-between gap-3">
            <span className="truncate text-sm font-medium">{transfer.name}</span>
            <div className="flex shrink-0 items-center gap-2">
              {active && transfer.speed !== undefined && transfer.speed > 0 && (
                <span className="text-xs font-mono font-medium text-[var(--color-clay)] tabular-nums">
                  {formatBytes(transfer.speed)}/s
                </span>
              )}
              <span className="text-xs tabular-nums text-[var(--muted)]">
                {active
                  ? `${formatBytes(transfer.uploaded)} / ${formatBytes(transfer.size)}`
                  : formatBytes(transfer.size)}
              </span>
            </div>
          </div>

          <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-[var(--muted)]">
            <span className="chip shrink-0 !bg-[var(--clay-soft)] !text-[var(--color-clay)] !border-transparent">
              WebUI
            </span>
            <span className="truncate">{transfer.path}</span>
            {transfer.segmentCount > 1 && (
              <span className="chip shrink-0" title="这个文件被拆成多个分卷">
                <Layers size={10} />
                {transfer.segmentsDone}/{transfer.segmentCount} 卷
              </span>
            )}
          </div>

          {active && <Progress value={pct} className="mt-2.5" />}

          {transfer.error && (
            <p className="mt-2 text-xs text-[var(--color-danger)]">{transfer.error}</p>
          )}
        </div>

        {active && (
          <IconButton label="取消" onClick={() => uploads.cancel(transfer.id)}>
            <Ban size={15} />
          </IconButton>
        )}
      </div>
    </div>
  )
}

function RemoteRow({ job, speed }: { job: UploadJob; speed: number }) {
  const pct = job.totalSize > 0 ? (job.uploadedBytes / job.totalSize) * 100 : 0
  const active = job.status === 'running' || job.status === 'pending'

  const isWebdav = job.source === 'webdav'
  const isLocal = job.source === 'local'
  const isRemote = job.source === 'remote' || Boolean(job.sourceUrl?.startsWith('http://') || job.sourceUrl?.startsWith('https://'))

  return (
    <div className="surface p-3.5">
      <div className="flex items-start gap-3">
        <StateDot state={job.status === 'complete' ? 'complete' : job.status === 'failed' ? 'failed' : 'uploading'} />
        <div className="min-w-0 flex-1">
          <div className="flex items-baseline justify-between gap-3">
            <span className="flex min-w-0 items-center gap-1.5 truncate text-sm font-medium">
              {isRemote && <CloudDownload size={13} className="shrink-0 text-[var(--faint)]" />}
              {isLocal && <HardDrive size={13} className="shrink-0 text-[var(--faint)]" />}
              {job.name}
            </span>
            <div className="flex shrink-0 items-center gap-2">
              {active && speed > 0 && (
                <span className="text-xs font-mono font-medium text-[var(--color-clay)] tabular-nums">
                  {formatBytes(speed)}/s
                </span>
              )}
              <span className="text-xs tabular-nums text-[var(--muted)]">
                {active && job.uploadedBytes > 0
                  ? `${formatBytes(job.uploadedBytes)} / ${formatBytes(job.totalSize)}`
                  : formatBytes(job.totalSize)}
              </span>
            </div>
          </div>

          <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-[var(--muted)]">
            {isWebdav ? (
              <span className="chip shrink-0 !bg-blue-500/12 !text-blue-600 dark:!text-blue-400 !border-transparent">
                WebDAV
              </span>
            ) : isLocal ? (
              <span className="chip shrink-0 !bg-green-500/12 !text-green-700 dark:!text-green-400 !border-transparent">
                VPS 本地
              </span>
            ) : isRemote ? (
              <span className="chip shrink-0 !bg-purple-500/12 !text-purple-600 dark:!text-purple-400 !border-transparent">
                离线下载
              </span>
            ) : (
              <span className="chip shrink-0 !bg-[var(--clay-soft)] !text-[var(--color-clay)] !border-transparent">
                WebUI
              </span>
            )}
            <span>{formatDate(new Date(job.updatedAt).getTime())}</span>
            {job.segmentCount > 1 && (
              <span className="chip shrink-0">
                <Layers size={10} />
                {job.segmentCount} 卷
              </span>
            )}
          </div>

          {active && <Progress value={pct} className="mt-2.5" />}
          {job.error && <p className="mt-2 text-xs text-[var(--color-danger)]">{job.error}</p>}
        </div>
      </div>
    </div>
  )
}

function StateDot({ state }: { state: Transfer['state'] }) {
  if (state === 'complete') {
    return (
      <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-[var(--color-success)]/12">
        <Check size={12} className="text-[var(--color-success)]" />
      </span>
    )
  }
  if (state === 'failed') {
    return (
      <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-[var(--color-danger)]/12">
        <AlertTriangle size={11} className="text-[var(--color-danger)]" />
      </span>
    )
  }
  if (state === 'cancelled') {
    return (
      <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-[var(--sunk)]">
        <Ban size={11} className="text-[var(--faint)]" />
      </span>
    )
  }
  return (
    <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center">
      <span className={clsx('size-2 rounded-full bg-[var(--color-clay)]', 'animate-pulse')} />
    </span>
  )
}
