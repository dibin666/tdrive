// Browser-side downloading.
//
// The server can already serve any byte range of any file, so the interesting
// work here is deciding how to ask for those ranges and where to put them.
//
// Three shapes, matching the three modes the server describes:
//
//   direct    N parallel range requests against the whole logical file
//   staged    ask the server to assemble the file on its disk first, then pull
//             that with the same N parallel requests — much safer for a file
//             spread across a dozen Telegram objects
//   segments  N parallel requests, but each one is a whole stored segment, and
//             the pieces are joined locally
//
// Parallelism needs somewhere to write out-of-order bytes. With the File
// System Access API that is a real file on disk and a 40 GB download costs a
// few megabytes of memory. Without it — Firefox, Safari — the only option is
// to assemble in memory, so those browsers get a single-connection stream
// straight to the downloads folder instead, and a many-segment file is offered
// as separate parts plus a join script.

import { api, currentToken, segmentRawUrl, type DownloadMode, type SegmentBound } from './api'

export type DownloadState =
  | 'queued'
  | 'preparing'
  | 'downloading'
  | 'merging'
  | 'complete'
  | 'failed'
  | 'cancelled'

export interface LocalDownload {
  id: string
  fileId: string
  name: string
  size: number
  received: number
  speed: number
  state: DownloadState
  mode: DownloadMode
  error?: string
  /** Server-side job id, for staged downloads and for progress reporting. */
  jobId?: string
  /** How many connections this transfer is using right now. */
  connections: number
  startedAt: number
  finishedAt?: number
  /** Set when the browser could not write to disk and fell back to parts. */
  note?: string
}

type Listener = (downloads: LocalDownload[]) => void

/** One range request asks for this much. Small enough that a failure costs
 *  little and progress moves visibly; large enough that the per-request
 *  overhead stays irrelevant. */
const CHUNK_SIZE = 8 * 1024 * 1024

/** A chunk is retried a few times before the transfer gives up, since the
 *  usual cause is a transient network blip or a Telegram hiccup the server
 *  will recover from on its own. */
const CHUNK_RETRIES = 3

/** Above this size, an in-memory assembly is refused rather than attempted:
 *  it would either fail outright or take the tab down with it. */
const MEMORY_LIMIT = 1.5 * 1024 * 1024 * 1024

export function supportsDiskWrites(): boolean {
  return typeof window !== 'undefined' && 'showSaveFilePicker' in window
}

interface FileSink {
  write(position: number, data: ArrayBuffer): Promise<void>
  close(): Promise<void>
  abort(): Promise<void>
}

/** diskSink streams to a real file, seeking to each chunk's position. */
async function diskSink(handle: FileSystemFileHandle, size: number): Promise<FileSink> {
  const writable = await handle.createWritable()
  // Truncating up front means every later write is a seek into an existing
  // file rather than an append, which is what allows chunks to arrive in any
  // order.
  await writable.truncate(size)
  return {
    async write(position, data) {
      await writable.write({ type: 'write', position, data })
    },
    async close() {
      await writable.close()
    },
    async abort() {
      try {
        await writable.abort()
      } catch {
        /* already closed */
      }
    },
  }
}

/** memorySink assembles into one buffer, for browsers with no disk access. */
function memorySink(size: number, onDone: (blob: Blob) => void): FileSink {
  const buffer = new Uint8Array(size)
  return {
    async write(position, data) {
      buffer.set(new Uint8Array(data), position)
    },
    async close() {
      onDone(new Blob([buffer]))
    },
    async abort() {
      /* nothing to release; the buffer is garbage collected */
    },
  }
}

export interface StartOptions {
  fileId: string
  name: string
  size: number
  mode: DownloadMode
  /** Parallel connections, bounded by the server's own limit. */
  connections: number
  segmentBounds?: SegmentBound[]
  /** Pre-opened save target. It has to be created inside the click handler
   *  that started the download, because the picker requires user activation. */
  saveHandle?: FileSystemFileHandle | null
  /** Called once the download finishes successfully. */
  onDone?: () => void
}

class DownloadManager {
  private downloads = new Map<string, LocalDownload>()
  private listeners = new Set<Listener>()
  private aborts = new Map<string, AbortController>()

  subscribe(fn: Listener) {
    this.listeners.add(fn)
    fn(this.snapshot())
    return () => {
      this.listeners.delete(fn)
    }
  }

  snapshot(): LocalDownload[] {
    return [...this.downloads.values()]
  }

  private emit() {
    const snap = this.snapshot()
    this.listeners.forEach((fn) => fn(snap))
  }

  private update(id: string, patch: Partial<LocalDownload>) {
    const current = this.downloads.get(id)
    if (!current) return
    this.downloads.set(id, { ...current, ...patch })
    this.emit()
  }

  clearFinished() {
    for (const [id, d] of this.downloads) {
      if (d.state === 'complete' || d.state === 'cancelled' || d.state === 'failed') {
        this.downloads.delete(id)
      }
    }
    this.emit()
  }

  cancel(id: string) {
    this.aborts.get(id)?.abort()
    const current = this.downloads.get(id)
    if (current && current.state !== 'complete') {
      this.update(id, { state: 'cancelled', speed: 0 })
      if (current.jobId) {
        if (current.mode === 'staged') {
          // Reporting a client-side status is not enough for a detached
          // staged worker: the server must cancel its context and remove the
          // partial cache file as well.
          void api.cancelDownload(current.jobId).catch(() => {})
        } else {
          void api.reportDownload(current.jobId, { status: 'cancelled' }).catch(() => {})
        }
      }
    }
  }

  /**
   * openSaveTarget must be called synchronously from the click that starts a
   * download: the file picker only opens while the browser still considers the
   * page to be handling a user gesture.
   */
  async openSaveTarget(name: string): Promise<FileSystemFileHandle | null> {
    const picker = window.showSaveFilePicker
    if (!picker) return null
    try {
      return await picker.call(window, {
        suggestedName: name,
        // A generic accept entry keeps the picker from rewriting the
        // extension of names it does not recognise.
        types: [{ description: '文件', accept: { 'application/octet-stream': ['.' + (name.split('.').pop() ?? 'bin')] } }],
      })
    } catch {
      // The user dismissed the picker, which is a decision rather than a
      // failure; the caller falls back to a plain browser download.
      return null
    }
  }

  async start(options: StartOptions): Promise<void> {
    const id = `dl-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
    const entry: LocalDownload = {
      id,
      fileId: options.fileId,
      name: options.name,
      size: options.size,
      received: 0,
      speed: 0,
      state: 'preparing',
      mode: options.mode,
      connections: 0,
      startedAt: Date.now(),
    }
    this.downloads.set(id, entry)
    this.emit()

    const controller = new AbortController()
    this.aborts.set(id, controller)

    try {
      // Every download gets a server-side record so it appears in the transfer
      // history next to the uploads, with the same date filtering and the same
      // average-speed reporting.
      const job = await api.startDownload(options.fileId, options.mode)
      this.update(id, { jobId: job.id })
      if (controller.signal.aborted) {
        // Cancellation can happen while the start request is in flight, before
        // the job id was available to cancel(). Do not leave a staged worker
        // running after that race.
        if (options.mode === 'staged') {
          await api.cancelDownload(job.id).catch(() => {})
        } else {
          await api.reportDownload(job.id, { status: 'cancelled' }).catch(() => {})
        }
        throw new DOMException('aborted', 'AbortError')
      }

      let sourceUrl = `/api/files/${options.fileId}/raw?download=1`
      let sourceSize = options.size

      if (options.mode === 'staged') {
        const ready = await this.awaitStaged(id, job.id, controller.signal)
        sourceUrl = ready.url ?? `/api/downloads/${job.id}/file`
        sourceSize = ready.totalSize || options.size
      }

      this.update(id, { state: 'downloading' })

      if (!options.saveHandle && options.connections <= 1 &&
        (options.mode === 'direct' || options.mode === 'staged')) {
        // A native browser download streams to the downloads folder without
        // buffering the file in JavaScript. The media token also works with the
        // raw endpoint's staged-copy preference, so staged mode still uses the
        // assembled file when it is ready.
        const link = await api.mediaLink(options.fileId)
        if (controller.signal.aborted) throw new DOMException('aborted', 'AbortError')
        simpleDownload(link.download, options.name)
        this.update(id, {
          state: 'complete',
          received: options.size,
          speed: 0,
          finishedAt: Date.now(),
          connections: 0,
        })
        void api
          .reportDownload(job.id, { downloaded: options.size, status: 'complete' })
          .catch(() => {})
        options.onDone?.()
        return
      }

      if (options.mode === 'segments' && options.segmentBounds?.length) {
        await this.runSegments(id, options, controller.signal)
      } else {
        await this.runRanges(id, options, sourceUrl, sourceSize, controller.signal)
      }

      this.update(id, {
        state: 'complete',
        received: options.size,
        speed: 0,
        finishedAt: Date.now(),
        connections: 0,
      })
      if (this.downloads.get(id)?.jobId) {
        void api
          .reportDownload(job.id, { downloaded: options.size, status: 'complete' })
          .catch(() => {})
      }
      options.onDone?.()
    } catch (err) {
      if (controller.signal.aborted) {
        this.update(id, { state: 'cancelled', speed: 0, connections: 0 })
        return
      }
      const message = err instanceof Error ? err.message : String(err)
      this.update(id, { state: 'failed', error: message, speed: 0, connections: 0 })
      const jobId = this.downloads.get(id)?.jobId
      if (jobId) {
        void api.reportDownload(jobId, { status: 'failed', error: message }).catch(() => {})
      }
      throw err
    } finally {
      this.aborts.delete(id)
    }
  }

  /** awaitStaged waits for the server to finish assembling the file, showing
   *  its progress as this download's progress so the wait is not a blank bar. */
  private async awaitStaged(id: string, jobId: string, signal: AbortSignal) {
    for (;;) {
      if (signal.aborted) throw new DOMException('aborted', 'AbortError')
      const job = await api.download(jobId)

      if (job.status === 'ready' || job.status === 'complete') {
        this.update(id, { received: 0, speed: 0 })
        return job
      }
      if (job.status === 'failed' || job.status === 'cancelled' || job.status === 'expired') {
        throw new Error(job.error || '服务器暂存失败')
      }

      // While staging, the meaningful number is how much the server has
      // fetched, so it is shown in place of local progress.
      this.update(id, {
        state: 'preparing',
        received: job.downloadedBytes,
        connections: 1,
      })
      await new Promise((resolve) => setTimeout(resolve, 1000))
    }
  }

  /** runRanges downloads one contiguous object with N parallel connections. */
  private async runRanges(
    id: string,
    options: StartOptions,
    url: string,
    size: number,
    signal: AbortSignal,
  ) {
    const sink = await this.openSink(id, options, size)

    const chunks: { start: number; end: number }[] = []
    for (let offset = 0; offset < size; offset += CHUNK_SIZE) {
      chunks.push({ start: offset, end: Math.min(offset + CHUNK_SIZE, size) - 1 })
    }
    // A zero-byte file still has to produce a file on disk.
    if (chunks.length === 0) {
      await sink.close()
      return
    }

    const workers = Math.max(1, Math.min(options.connections, chunks.length))
    await this.pump(id, chunks.length, workers, signal, sink, async (index) => {
      const chunk = chunks[index]
      const body = await this.fetchRange(url, chunk.start, chunk.end, signal)
      await sink.write(chunk.start, body)
      return body.byteLength
    })

    await sink.close()
  }

  /** runSegments downloads each stored segment and joins them locally. */
  private async runSegments(id: string, options: StartOptions, signal: AbortSignal) {
    const bounds = options.segmentBounds ?? []

    // Without disk access there is nowhere to join the parts, so each one is
    // handed to the browser separately and the user joins them afterwards.
    if (!options.saveHandle) {
      await this.downloadSeparateParts(id, options, bounds, signal)
      return
    }

    const sink = await this.openSink(id, options, options.size)
    const workers = Math.max(1, Math.min(options.connections, bounds.length))

    await this.pump(id, bounds.length, workers, signal, sink, async (index) => {
      const bound = bounds[index]
      const url = `/api/files/${options.fileId}/segments/${bound.index}/raw`
      const body = await this.fetchRange(url, 0, bound.size - 1, signal, bound.size)
      await sink.write(bound.start, body)
      return body.byteLength
    })

    this.update(id, { state: 'merging' })
    await sink.close()
  }

  /**
   * downloadSeparateParts is the fallback for browsers that cannot write to
   * disk. Each segment is fetched by the browser as its own download, and a
   * join script is produced alongside them, because telling someone to run
   * `cat` is a much better answer than silently producing an unusable pile of
   * files.
   */
  private async downloadSeparateParts(
    id: string,
    options: StartOptions,
    bounds: SegmentBound[],
    signal: AbortSignal,
  ) {
    this.update(id, {
      note: '浏览器不支持直接写入磁盘，已改为逐卷下载并附带合并脚本',
      connections: 1,
    })
    const media = await api.mediaLink(options.fileId)
    const token = new URL(media.download, window.location.origin).searchParams.get('t')
    if (!token) throw new Error('无法生成分卷下载凭证')

    for (const bound of bounds) {
      if (signal.aborted) throw new DOMException('aborted', 'AbortError')
      const link = document.createElement('a')
      link.href = segmentRawUrl(options.fileId, bound.index, token)
      link.download = `${options.name}.part${String(bound.index).padStart(3, '0')}`
      link.style.display = 'none'
      document.body.appendChild(link)
      link.click()
      link.remove()

      this.update(id, { received: bound.start + bound.size })
      // Browsers throttle or drop a burst of programmatic downloads, so they
      // are spaced out rather than fired all at once.
      await delay(400)
    }

    if (signal.aborted) throw new DOMException('aborted', 'AbortError')
    downloadMergeScript(options.name, bounds.length)
  }

  /** pump runs `workers` concurrent tasks over an index range, keeping the
   *  progress and speed numbers up to date as they land. */
  private async pump(
    id: string,
    total: number,
    workers: number,
    signal: AbortSignal,
    sink: FileSink,
    task: (index: number) => Promise<number>,
  ) {
    let next = 0
    let received = 0
    let lastTime = performance.now()
    let lastBytes = 0
    let speed = 0

    this.update(id, { connections: workers })

    const report = () => {
      const now = performance.now()
      const dt = (now - lastTime) / 1000
      if (dt >= 0.25) {
        const instant = Math.max(0, received - lastBytes) / dt
        // Exponential smoothing: a raw instantaneous figure jumps around too
        // much to read while chunks land in bursts.
        speed = speed === 0 ? instant : speed * 0.7 + instant * 0.3
        lastTime = now
        lastBytes = received
      }
      this.update(id, { received, speed })
    }

    const run = async () => {
      for (;;) {
        if (signal.aborted) throw new DOMException('aborted', 'AbortError')
        const index = next++
        if (index >= total) return
        received += await task(index)
        report()
      }
    }

    try {
      await Promise.all(Array.from({ length: workers }, run))
    } catch (err) {
      await sink.abort()
      throw err
    }
  }

  private async openSink(id: string, options: StartOptions, size: number): Promise<FileSink> {
    if (options.saveHandle) {
      return diskSink(options.saveHandle, size)
    }
    if (size > MEMORY_LIMIT) {
      throw new Error(
        '这个浏览器不支持直接写入磁盘，而文件太大无法在内存中拼接。请改用单线程下载，或换用 Chrome / Edge。',
      )
    }
    this.update(id, { note: '浏览器不支持写入磁盘，正在内存中拼接' })
    return memorySink(size, (blob) => {
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = options.name
      link.style.display = 'none'
      document.body.appendChild(link)
      link.click()
      link.remove()
      setTimeout(() => URL.revokeObjectURL(url), 60_000)
    })
  }

  /** fetchRange asks for one byte range, retrying transient failures. */
  private async fetchRange(
    url: string,
    start: number,
    end: number,
    signal: AbortSignal,
    expected?: number,
  ): Promise<ArrayBuffer> {
    let lastError: unknown
    for (let attempt = 0; attempt <= CHUNK_RETRIES; attempt++) {
      if (signal.aborted) throw new DOMException('aborted', 'AbortError')
      try {
        const headers = new Headers({ Range: `bytes=${start}-${end}` })
        // The token is read per request, so a download that outlives one
        // access token picks up the refreshed one rather than failing.
        const token = currentToken()
        if (token) headers.set('Authorization', `Bearer ${token}`)

        const res = await fetch(url, { headers, credentials: 'same-origin', signal })
        if (res.status === 429) {
          // The server is telling us this transfer has too many connections
          // open. Backing off is exactly the right response.
          await delay(1000 * (attempt + 1))
          continue
        }
        if (!res.ok && res.status !== 206) {
          throw new Error(`HTTP ${res.status}`)
        }
        const body = await res.arrayBuffer()
        const want = expected ?? end - start + 1
        if (body.byteLength !== want) {
          throw new Error(`分片长度不对：期望 ${want} 字节，实际 ${body.byteLength} 字节`)
        }
        return body
      } catch (err) {
        if (signal.aborted) throw err
        lastError = err
        await delay(500 * (attempt + 1))
      }
    }
    throw lastError instanceof Error ? lastError : new Error(String(lastError))
  }
}

function delay(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/**
 * downloadMergeScript hands the user a script that joins the parts.
 *
 * Both flavours are produced in one file rather than guessing the platform:
 * the browser knows very little about the machine it is on, and a two-line
 * comment is cheaper than being wrong.
 */
export function downloadMergeScript(name: string, parts: number) {
  const partNames = Array.from({ length: parts }, (_, i) => `${name}.part${String(i + 1).padStart(3, '0')}`)

  const sh = [
    '#!/bin/sh',
    '# macOS / Linux：把这个文件和所有分卷放在同一个目录，然后执行 sh merge.sh',
    `cat ${partNames.map(shellQuote).join(' ')} > ${shellQuote(name)}`,
    `printf '%s\\n' ${shellQuote(`已合并为 ${name}`)}`,
    '',
  ].join('\n')

  // cmd.exe's quoting rules are not a portable variant of POSIX quoting:
  // &, %, !, ^ and embedded quotes all have their own expansion rules. Keep
  // the .bat command itself limited to a base64 alphabet and let PowerShell
  // open each path with LiteralPath semantics, using a streaming copy so
  // untrusted filenames can never become commands.
  const powershell = [
    '$ErrorActionPreference = "Stop"',
    `$output = ${powerShellQuote(name)}`,
    `$count = ${Math.max(0, Math.floor(parts))}`,
    '$parts = if ($count -gt 0) { 1..$count | ForEach-Object { \'{0}.part{1:D3}\' -f $output, $_ } } else { @() }',
    '$buffer = New-Object byte[] 1048576',
    '$destination = [IO.File]::Open($output, [IO.FileMode]::Create, [IO.FileAccess]::Write, [IO.FileShare]::None)',
    'try {',
    '  foreach ($part in $parts) {',
    '    $source = [IO.File]::OpenRead($part)',
    '    try {',
    '      while (($read = $source.Read($buffer, 0, $buffer.Length)) -gt 0) {',
    '        $destination.Write($buffer, 0, $read)',
    '      }',
    '    } finally { $source.Dispose() }',
    '  }',
    '} finally { $destination.Dispose() }',
    `Write-Host ('已合并为 ' + $output)`,
    '',
  ].join('\n')

  const bat = [
    '@echo off',
    'REM Windows：把这个文件和所有分卷放在同一个目录，然后双击运行',
    `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand ${encodeUtf16Base64(powershell)}`,
    'pause',
    '',
  ].join('\r\n')

  saveText(`merge-${name}.sh`, sh)
  // A short gap so the browser treats these as two downloads rather than
  // suppressing the second one.
  setTimeout(() => saveText(`merge-${name}.bat`, bat), 300)
}

function shellQuote(value: string) {
  return `'${value.replace(/'/g, "'\\''")}'`
}

function powerShellQuote(value: string) {
  return `'${value.replace(/'/g, "''")}'`
}

function encodeUtf16Base64(value: string) {
  const bytes = new Uint8Array(value.length * 2)
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i)
    bytes[i * 2] = code & 0xff
    bytes[i * 2 + 1] = code >>> 8
  }

  let binary = ''
  const chunkSize = 0x8000
  for (let offset = 0; offset < bytes.length; offset += chunkSize) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + chunkSize))
  }
  return btoa(binary)
}

function saveText(filename: string, content: string) {
  const blob = new Blob([content], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  link.style.display = 'none'
  document.body.appendChild(link)
  link.click()
  link.remove()
  setTimeout(() => URL.revokeObjectURL(url), 10_000)
}

/** simpleDownload is the single-connection path: hand the URL to the browser
 *  and let it do what it already does well. */
export function simpleDownload(url: string, name?: string) {
  const link = document.createElement('a')
  link.href = url
  if (name) link.download = name
  link.style.display = 'none'
  document.body.appendChild(link)
  link.click()
  link.remove()
}

export const downloads = new DownloadManager()
