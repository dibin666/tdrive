// Browser-side segmented upload.
//
// The server tells us its segment size; we slice the local File on exactly
// those boundaries and PUT each slice, so one request maps to one Telegram
// object. Slicing here rather than streaming one long body is what makes a
// 40 GB upload workable from a browser: a dropped connection costs one segment
// instead of everything, two segments can be in flight at once, and the server
// never has to spool anything to disk.

import { api, ApiError, currentToken, type UploadPlan } from './api'

export type TransferState = 'queued' | 'uploading' | 'complete' | 'failed' | 'cancelled'

export interface Transfer {
  id: string
  name: string
  path: string
  size: number
  uploaded: number
  speed?: number
  segmentCount: number
  segmentsDone: number
  state: TransferState
  error?: string
}

type Listener = (transfers: Transfer[]) => void

/** How many segments of one file upload at once. Two keeps the link busy
 *  through the gaps between requests without making progress unreadable or
 *  multiplying peak memory. */
const SEGMENT_CONCURRENCY = 2

/** A failed segment is retried a few times before the whole transfer gives up,
 *  since the usual cause is a transient network blip rather than bad input. */
const SEGMENT_RETRIES = 3

class UploadManager {
  private transfers = new Map<string, Transfer>()
  private listeners = new Set<Listener>()
  private aborts = new Map<string, AbortController>()

  subscribe(fn: Listener) {
    this.listeners.add(fn)
    fn(this.snapshot())
    return () => {
      this.listeners.delete(fn)
    }
  }

  snapshot(): Transfer[] {
    return [...this.transfers.values()]
  }

  private emit() {
    const snap = this.snapshot()
    this.listeners.forEach((fn) => fn(snap))
  }

  private update(id: string, patch: Partial<Transfer>) {
    const current = this.transfers.get(id)
    if (!current) return
    this.transfers.set(id, { ...current, ...patch })
    this.emit()
  }

  /** clearFinished removes completed and cancelled rows from the panel. */
  clearFinished() {
    for (const [id, t] of this.transfers) {
      if (t.state === 'complete' || t.state === 'cancelled') this.transfers.delete(id)
    }
    this.emit()
  }

  /**
   * cancel stops a transfer this browser is driving.
   *
   * Both halves are needed. Aborting the request only stops what has not been
   * sent yet: the server is still pushing whatever it already holds into
   * Telegram, so a transfer cancelled here used to keep running there and the
   * two views of it disagreed until it finished. The server call is awaited
   * rather than fired and forgotten, because a cancellation that silently fails
   * leaves a row that can never be stopped or deleted.
   */
  async cancel(id: string): Promise<void> {
    const transfer = this.transfers.get(id)
    if (!transfer || transfer.state === 'complete' || transfer.state === 'cancelled') return

    this.aborts.get(id)?.abort()
    this.update(id, { state: 'cancelled', speed: 0 })

    // A transfer whose job was never created only exists in this panel.
    if (id.startsWith('local-')) return

    try {
      await api.cancelUpload(id)
    } catch (err) {
      // The one refusal worth acting on: it finished between the click and the
      // request. Leaving it marked cancelled would be the same disagreement the
      // cancellation was supposed to remove.
      if (err instanceof ApiError && err.code === 'transfer_finished') {
        this.update(id, { state: 'complete', uploaded: transfer.size, speed: 0 })
        return
      }
      throw err
    }
  }

  /**
   * upload stores one file and resolves when every segment has landed.
   * onDone fires once so the caller can refresh the listing.
   */
  async upload(file: File, path: string, onDone?: () => void): Promise<void> {
    let plan: UploadPlan
    try {
      plan = await api.beginUpload({
        path,
        name: file.name,
        size: file.size,
        mime: file.type || undefined,
        overwrite: false,
      })
    } catch (err) {
      // A local id keeps the failure visible in the panel even though the
      // server never created a job.
      const localId = `local-${Date.now()}-${file.name}`
      this.transfers.set(localId, {
        id: localId,
        name: file.name,
        path,
        size: file.size,
        uploaded: 0,
        segmentCount: 0,
        segmentsDone: 0,
        state: 'failed',
        error: err instanceof Error ? err.message : String(err),
      })
      this.emit()
      throw err
    }

    const id = plan.job.id
    this.transfers.set(id, {
      id,
      name: file.name,
      path,
      size: file.size,
      uploaded: 0,
      speed: 0,
      segmentCount: plan.segmentBounds.length,
      segmentsDone: 0,
      state: 'uploading',
      error: undefined,
    })
    this.emit()

    const controller = new AbortController()
    this.aborts.set(id, controller)

    // Per-segment byte counters, summed for the overall figure. Segments
    // finish out of order, so a single running total would jump around.
    const progress = new Map<number, number>()
    let lastTime = performance.now()
    let lastBytes = 0
    let currentSpeed = 0

    const reportProgress = () => {
      let total = 0
      for (const n of progress.values()) total += n
      const now = performance.now()
      const timeDelta = (now - lastTime) / 1000
      if (timeDelta >= 0.25) {
        const bytesDelta = total - lastBytes
        const instantSpeed = Math.max(0, bytesDelta / timeDelta)
        currentSpeed = currentSpeed === 0 ? instantSpeed : currentSpeed * 0.7 + instantSpeed * 0.3
        lastTime = now
        lastBytes = total
      }
      this.update(id, { uploaded: total, speed: currentSpeed })
    }

    try {
      const queue = [...plan.pending]
      let done = 0

      const worker = async () => {
        for (;;) {
          const index = queue.shift()
          if (index === undefined) return

          const bound = plan.segmentBounds.find((b) => b.index === index)
          if (!bound) continue

          const slice = file.slice(bound.start, bound.start + bound.size)
          await this.putSegment(id, index, slice, controller.signal, (sent) => {
            progress.set(index, sent)
            reportProgress()
          })

          progress.set(index, bound.size)
          done += 1
          this.update(id, { segmentsDone: done })
          reportProgress()
        }
      }

      await Promise.all(
        Array.from({ length: Math.min(SEGMENT_CONCURRENCY, queue.length || 1) }, worker),
      )

      await api.completeUpload(id)
      this.update(id, { state: 'complete', uploaded: file.size, speed: 0, segmentsDone: plan.segmentBounds.length })
      onDone?.()
    } catch (err) {
      if (controller.signal.aborted) {
        this.update(id, { state: 'cancelled', speed: 0 })
        return
      }
      // The server refuses further segments once a transfer has been stopped,
      // which happens when it was cancelled from the transfer panel or another
      // tab rather than from here. That is a cancellation, and reporting it as
      // an upload failure was the visible half of the two sides disagreeing.
      if (err instanceof ApiError && err.code === 'transfer_finished') {
        this.update(id, { state: 'cancelled', speed: 0 })
        return
      }
      const message = err instanceof Error ? err.message : String(err)
      this.update(id, { state: 'failed', speed: 0, error: message })
      throw err
    } finally {
      this.aborts.delete(id)
    }
  }

  /**
   * putSegment sends one slice, retrying transient failures.
   *
   * XMLHttpRequest is used rather than fetch because it is still the only way
   * to observe upload progress in every browser; fetch request streams are
   * not universally available and require HTTP/2 to boot.
   */
  private putSegment(
    jobId: string,
    index: number,
    blob: Blob,
    signal: AbortSignal,
    onProgress: (sent: number) => void,
  ): Promise<void> {
    const attempt = (remaining: number): Promise<void> =>
      new Promise<void>((resolve, reject) => {
        if (signal.aborted) return reject(new DOMException('aborted', 'AbortError'))

        const xhr = new XMLHttpRequest()
        xhr.open('PUT', `/api/uploads/${jobId}/segments/${index}`)
        const token = currentToken()
        if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)
        xhr.setRequestHeader('Content-Type', 'application/octet-stream')
        xhr.withCredentials = true

        const onAbort = () => xhr.abort()
        signal.addEventListener('abort', onAbort, { once: true })
        const cleanup = () => signal.removeEventListener('abort', onAbort)

        xhr.upload.onprogress = (e) => onProgress(e.loaded)

        xhr.onload = () => {
          cleanup()
          if (xhr.status >= 200 && xhr.status < 300) return resolve()

          let message = xhr.statusText
          let code: string | undefined
          try {
            const body = JSON.parse(xhr.responseText)
            if (body?.error) message = body.error
            code = body?.code
          } catch {
            /* leave the status text */
          }
          // A rejected segment is not worth retrying: the request itself is
          // wrong, and re-sending it would fail identically.
          if (xhr.status >= 400 && xhr.status < 500 && xhr.status !== 429) {
            return reject(new ApiError(xhr.status, message, code))
          }
          if (remaining > 0) {
            const backoff = (SEGMENT_RETRIES - remaining + 1) * 1000
            setTimeout(() => attempt(remaining - 1).then(resolve, reject), backoff)
            return
          }
          reject(new ApiError(xhr.status, message, code))
        }

        xhr.onerror = () => {
          cleanup()
          if (remaining > 0) {
            const backoff = (SEGMENT_RETRIES - remaining + 1) * 1000
            setTimeout(() => attempt(remaining - 1).then(resolve, reject), backoff)
            return
          }
          reject(new Error('the connection dropped while sending this segment'))
        }

        xhr.onabort = () => {
          cleanup()
          reject(new DOMException('aborted', 'AbortError'))
        }

        xhr.send(blob)
      })

    return attempt(SEGMENT_RETRIES)
  }
}

export const uploads = new UploadManager()
