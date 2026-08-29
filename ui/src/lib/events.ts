// The server-sent event stream.
//
// EventSource cannot set an Authorization header, and the access token is
// deliberately not in a cookie, so the stream is consumed with fetch and a
// ReadableStream instead. That also lets a 401 trigger the normal token
// refresh rather than silently reconnecting forever.

import { currentToken, api } from './api'

export type EventType = 'upload' | 'index' | 'telegram' | 'tree'

export interface ServerEvent {
  type: EventType
  data: unknown
  at: number
}

type Handler = (event: ServerEvent) => void

class EventStream {
  private handlers = new Set<Handler>()
  private controller: AbortController | null = null
  private retryDelay = 1000
  private running = false

  subscribe(fn: Handler) {
    this.handlers.add(fn)
    if (!this.running) this.start()
    return () => {
      this.handlers.delete(fn)
      if (this.handlers.size === 0) this.stop()
    }
  }

  private emit(event: ServerEvent) {
    this.handlers.forEach((fn) => {
      try {
        fn(event)
      } catch (err) {
        console.error('event handler failed', err)
      }
    })
  }

  stop() {
    this.running = false
    this.controller?.abort()
    this.controller = null
  }

  private start() {
    this.running = true
    void this.loop()
  }

  private async loop() {
    while (this.running) {
      try {
        await this.connect()
        // A clean end means the server closed the stream; reconnect promptly.
        this.retryDelay = 1000
      } catch (err) {
        if (!this.running) return
        if ((err as Error)?.name === 'AbortError') return
      }
      if (!this.running) return

      await sleep(this.retryDelay)
      // Back off to 30s so a server that is down does not get hammered by
      // every open tab.
      this.retryDelay = Math.min(this.retryDelay * 2, 30_000)
    }
  }

  private async connect() {
    const controller = new AbortController()
    this.controller = controller

    const headers = new Headers({ Accept: 'text/event-stream' })
    const token = currentToken()
    if (token) headers.set('Authorization', `Bearer ${token}`)

    const res = await fetch('/api/events', {
      headers,
      credentials: 'same-origin',
      signal: controller.signal,
    })

    if (res.status === 401) {
      // The token expired while the stream was open. Refresh, then let the
      // loop reconnect with the new one.
      await api.refresh()
      throw new Error('unauthorized')
    }
    if (!res.ok || !res.body) {
      throw new Error(`event stream failed: ${res.status}`)
    }

    this.retryDelay = 1000
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    let buffer = ''

    for (;;) {
      const { done, value } = await reader.read()
      if (done) return

      buffer += decoder.decode(value, { stream: true })

      // Events are separated by a blank line; a partial event stays in the
      // buffer until the rest arrives.
      let boundary = buffer.indexOf('\n\n')
      while (boundary !== -1) {
        const raw = buffer.slice(0, boundary)
        buffer = buffer.slice(boundary + 2)
        this.handleChunk(raw)
        boundary = buffer.indexOf('\n\n')
      }
    }
  }

  private handleChunk(raw: string) {
    for (const line of raw.split('\n')) {
      if (!line.startsWith('data:')) continue // ": keepalive" and friends
      const payload = line.slice(5).trim()
      if (!payload) continue
      try {
        this.emit(JSON.parse(payload) as ServerEvent)
      } catch {
        /* a malformed frame is not worth tearing the stream down for */
      }
    }
  }
}

function sleep(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

export const events = new EventStream()
