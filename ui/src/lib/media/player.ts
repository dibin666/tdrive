import {
  ALL_FORMATS,
  AudioBufferSink,
  CanvasSink,
  Input,
  UrlSource,
  type InputAudioTrack,
  type InputVideoTrack,
} from 'mediabunny'

/**
 * A player for files the browser will not play itself.
 *
 * mediabunny demuxes the container and decodes the tracks; this class is the
 * part that turns a stream of decoded frames and audio buffers back into
 * something that looks like a video element: a clock, a render loop, a seek
 * that does not leak the loops it replaces.
 *
 * The audio context is the clock whenever there is audio, because audio
 * hardware is the one thing on the page that cannot be told to wait — a video
 * frame arriving late is a dropped frame, an audio buffer arriving late is an
 * audible gap. With no audio track, wall time takes over.
 */

export type PlayerState = 'idle' | 'loading' | 'ready' | 'playing' | 'paused' | 'ended' | 'error'

export interface PlayerEvents {
  state: (state: PlayerState) => void
  time: (seconds: number) => void
  duration: (seconds: number) => void
  error: (message: string) => void
}

/** How far ahead audio is scheduled. Long enough to survive a slow decode,
 *  short enough that a seek does not have to cancel a second of sound. */
const AUDIO_LOOKAHEAD = 0.8

export class CanvasPlayer {
  private input: Input | null = null
  private videoTrack: InputVideoTrack | null = null
  private audioTrack: InputAudioTrack | null = null
  private videoSink: CanvasSink | null = null
  private audioSink: AudioBufferSink | null = null

  private audioContext: AudioContext | null = null
  private gain: GainNode | null = null
  private scheduled = new Set<AudioBufferSourceNode>()

  /** Media time of the current playback run's start, and the clock reading it
   *  was anchored to. */
  private anchorMedia = 0
  private anchorClock = 0

  private runToken = 0
  private state: PlayerState = 'idle'
  private durationSeconds = 0
  private currentSeconds = 0
  private listeners: Partial<PlayerEvents> = {}
  private frameHandle: number | null = null

  constructor(private canvas: HTMLCanvasElement) {}

  on<K extends keyof PlayerEvents>(event: K, handler: PlayerEvents[K]) {
    this.listeners[event] = handler
  }

  get duration() {
    return this.durationSeconds
  }

  get currentTime() {
    return this.currentSeconds
  }

  get playing() {
    return this.state === 'playing'
  }

  private setState(next: PlayerState) {
    this.state = next
    this.listeners.state?.(next)
  }

  async load(url: string) {
    this.setState('loading')
    try {
      this.input = new Input({ formats: ALL_FORMATS, source: new UrlSource(url) })

      this.videoTrack = await this.input.getPrimaryVideoTrack()
      this.audioTrack = await this.input.getPrimaryAudioTrack()

      if (!this.videoTrack && !this.audioTrack) {
        throw new Error('这个文件里没有可播放的音视频轨道')
      }

      if (this.videoTrack) {
        if (!(await this.videoTrack.canDecode())) {
          throw new Error('浏览器无法解码这个视频编码')
        }
        // Rendering at the display size rather than the coded size keeps a 4K
        // source from allocating 4K canvases per frame on a laptop.
        this.videoSink = new CanvasSink(this.videoTrack, {
          width: Math.min(this.videoTrack.displayWidth, 1920),
          fit: 'contain',
          poolSize: 2,
        })
        this.canvas.width = this.videoTrack.displayWidth
        this.canvas.height = this.videoTrack.displayHeight
      }

      if (this.audioTrack) {
        if (await this.audioTrack.canDecode()) {
          this.audioSink = new AudioBufferSink(this.audioTrack)
        } else {
          // Playing the picture with no sound beats refusing outright, and the
          // caller surfaces the reason.
          this.audioTrack = null
        }
      }

      this.durationSeconds = (await this.input.getDurationFromMetadata()) ?? 0
      if (!this.durationSeconds) {
        this.durationSeconds = await this.input.computeDuration().catch(() => 0)
      }
      this.listeners.duration?.(this.durationSeconds)

      // A first frame right away, so the player does not sit on a black
      // rectangle until someone presses play.
      await this.renderFrameAt(0)
      this.setState('ready')
    } catch (err) {
      this.setState('error')
      this.listeners.error?.(err instanceof Error ? err.message : String(err))
    }
  }

  private clockTime(): number {
    if (this.audioContext) {
      return this.anchorMedia + (this.audioContext.currentTime - this.anchorClock)
    }
    return this.anchorMedia + (performance.now() / 1000 - this.anchorClock)
  }

  async play() {
    if (this.state === 'playing') return
    if (this.state === 'ended') {
      await this.seek(0)
    }

    if (this.audioSink && !this.audioContext) {
      this.audioContext = new AudioContext()
      this.gain = this.audioContext.createGain()
      this.gain.connect(this.audioContext.destination)
    }
    await this.audioContext?.resume()

    this.anchorMedia = this.currentSeconds
    this.anchorClock = this.audioContext ? this.audioContext.currentTime : performance.now() / 1000
    this.setState('playing')

    const token = ++this.runToken
    if (this.audioSink) void this.runAudio(token)
    if (this.videoSink) void this.runVideo(token)
    this.tick(token)
  }

  pause() {
    if (this.state !== 'playing') return
    // Bumping the token is what stops every loop: they all check it after each
    // await, so nothing has to be individually cancelled.
    this.runToken++
    this.currentSeconds = this.clockTime()
    this.stopAudio()
    this.setState('paused')
  }

  async seek(seconds: number) {
    const wasPlaying = this.state === 'playing'
    this.runToken++
    this.stopAudio()

    this.currentSeconds = Math.max(0, Math.min(this.durationSeconds || seconds, seconds))
    this.listeners.time?.(this.currentSeconds)
    await this.renderFrameAt(this.currentSeconds)

    if (wasPlaying) {
      await this.play()
    } else {
      this.setState('paused')
    }
  }

  setVolume(value: number) {
    if (this.gain) this.gain.gain.value = value
  }

  destroy() {
    this.runToken++
    this.stopAudio()
    if (this.frameHandle !== null) cancelAnimationFrame(this.frameHandle)
    void this.audioContext?.close()
    this.audioContext = null
    try {
      this.input?.dispose()
    } catch {
      /* already gone */
    }
    this.input = null
    this.setState('idle')
  }

  private stopAudio() {
    for (const node of this.scheduled) {
      try {
        node.stop()
      } catch {
        /* already stopped */
      }
    }
    this.scheduled.clear()
  }

  /** tick publishes the current time to the UI at animation rate. */
  private tick(token: number) {
    const step = () => {
      if (token !== this.runToken) return
      this.currentSeconds = this.clockTime()
      this.listeners.time?.(this.currentSeconds)

      if (this.durationSeconds > 0 && this.currentSeconds >= this.durationSeconds) {
        this.runToken++
        this.stopAudio()
        this.currentSeconds = this.durationSeconds
        this.setState('ended')
        return
      }
      this.frameHandle = requestAnimationFrame(step)
    }
    this.frameHandle = requestAnimationFrame(step)
  }

  private async renderFrameAt(seconds: number) {
    if (!this.videoSink) return
    const frame = await this.videoSink.getCanvas(seconds)
    if (frame) this.draw(frame.canvas)
  }

  private draw(source: HTMLCanvasElement | OffscreenCanvas) {
    const ctx = this.canvas.getContext('2d')
    if (!ctx) return
    if (this.canvas.width !== source.width || this.canvas.height !== source.height) {
      this.canvas.width = source.width
      this.canvas.height = source.height
    }
    ctx.drawImage(source as CanvasImageSource, 0, 0)
  }

  private async runVideo(token: number) {
    if (!this.videoSink) return
    try {
      for await (const frame of this.videoSink.canvases(this.currentSeconds)) {
        if (token !== this.runToken) return

        // Wait until this frame is due. A frame that is already late is drawn
        // immediately rather than dropped: with software decoding, being a
        // little behind is normal and skipping would make it worse.
        for (;;) {
          if (token !== this.runToken) return
          const delta = frame.timestamp - this.clockTime()
          if (delta <= 0) break
          await sleep(Math.min(delta * 1000, 50))
        }
        this.draw(frame.canvas)
      }
    } catch (err) {
      if (token === this.runToken) {
        this.listeners.error?.(err instanceof Error ? err.message : String(err))
      }
    }
  }

  private async runAudio(token: number) {
    const sink = this.audioSink
    const context = this.audioContext
    if (!sink || !context || !this.gain) return

    try {
      for await (const chunk of sink.buffers(this.currentSeconds)) {
        if (token !== this.runToken) return

        // Backpressure: decode only a little ahead of the clock, so a seek
        // does not have to throw away seconds of already-decoded audio.
        while (chunk.timestamp - this.clockTime() > AUDIO_LOOKAHEAD) {
          if (token !== this.runToken) return
          await sleep(50)
        }

        const when = this.anchorClock + (chunk.timestamp - this.anchorMedia)
        // A buffer whose slot has already passed is dropped; playing it now
        // would put it out of sync with the picture.
        if (when < context.currentTime - 0.05) continue

        const node = context.createBufferSource()
        node.buffer = chunk.buffer
        node.connect(this.gain)
        node.onended = () => this.scheduled.delete(node)
        node.start(Math.max(when, context.currentTime))
        this.scheduled.add(node)
      }
    } catch (err) {
      if (token === this.runToken) {
        this.listeners.error?.(err instanceof Error ? err.message : String(err))
      }
    }
  }
}

function sleep(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

/**
 * enableSoftwareDecoding registers a WASM decoder for the codecs the platform
 * cannot handle itself, and reports whether it worked.
 *
 * It is loaded lazily and only when needed: the decoder is several megabytes,
 * and the overwhelming majority of files never reach this tier.
 */
let softwareDecodingReady: Promise<boolean> | null = null

export function enableSoftwareDecoding(): Promise<boolean> {
  if (!softwareDecodingReady) {
    softwareDecodingReady = (async () => {
      try {
        const [{ registerDecoder, CustomVideoDecoder, CustomAudioDecoder, VideoSample, AudioSample }, polyfill, libav] =
          await Promise.all([
            import('mediabunny'),
            import('libavjs-webcodecs-polyfill'),
            import('@libav.js/variant-webcodecs'),
          ])

        // The WASM is served from this app's own origin rather than the
        // upstream CDN default: a self-hosted drive should not need a second
        // network dependency to play a file it is already storing.
        const LibAV = libav.default as { base?: string }
        LibAV.base = `${window.location.origin}/libav`

        await polyfill.load({
          polyfill: false,
          LibAV: LibAV as never,
          // Threading needs cross-origin isolation headers this server does
          // not send, and asking for it would make the loader fetch a build
          // that then fails to start.
          libavOptions: { nothreads: true },
        })

        class SoftwareVideoDecoder extends CustomVideoDecoder {
          private decoder: InstanceType<typeof polyfill.VideoDecoder> | null = null

          // This decoder is only ever registered as a last resort, so it
          // claims everything: mediabunny prefers the native decoder whenever
          // one is available.
          static supports() {
            return true
          }

          async init() {
            this.decoder = new polyfill.VideoDecoder({
              output: (frame) => this.onSample(new VideoSample(frame as unknown as VideoFrame)),
              error: (error) => this.onError(error),
            })
            await this.decoder.configure(this.config as never)
          }

          async decode(packet: { toEncodedVideoChunk(): EncodedVideoChunk }) {
            this.decoder?.decode(packet.toEncodedVideoChunk() as never)
          }

          async flush() {
            await this.decoder?.flush()
          }

          async close() {
            this.decoder?.close()
            this.decoder = null
          }
        }

        class SoftwareAudioDecoder extends CustomAudioDecoder {
          private decoder: InstanceType<typeof polyfill.AudioDecoder> | null = null

          static supports() {
            return true
          }

          async init() {
            this.decoder = new polyfill.AudioDecoder({
              output: (data) => this.onSample(new AudioSample(data as unknown as AudioData)),
              error: (error) => this.onError(error),
            })
            await this.decoder.configure(this.config as never)
          }

          async decode(packet: { toEncodedAudioChunk(): EncodedAudioChunk }) {
            this.decoder?.decode(packet.toEncodedAudioChunk() as never)
          }

          async flush() {
            await this.decoder?.flush()
          }

          async close() {
            this.decoder?.close()
            this.decoder = null
          }
        }

        registerDecoder(SoftwareVideoDecoder as never)
        registerDecoder(SoftwareAudioDecoder as never)
        return true
      } catch {
        // The WASM decoder is fetched from a CDN, so an offline or firewalled
        // deployment lands here. The player says so rather than spinning.
        softwareDecodingReady = null
        return false
      }
    })()
  }
  return softwareDecodingReady
}
