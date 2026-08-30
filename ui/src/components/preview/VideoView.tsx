import { useCallback, useEffect, useRef, useState } from 'react'
import clsx from 'clsx'
import {
  AlertTriangle,
  Cpu,
  Info,
  Maximize2,
  Pause,
  Play,
  Volume2,
  VolumeX,
  Zap,
} from 'lucide-react'
import type { ViewerProps } from './PreviewModal'
import { describeCodec, probeMedia, type MediaInfo } from '../../lib/media/probe'
import { CanvasPlayer, enableSoftwareDecoding } from '../../lib/media/player'
import { formatDuration } from '../../lib/format'
import { Spinner } from '../primitives'

/**
 * Video playback in three tiers.
 *
 * Tier one is a plain <video>: nothing this code does will beat the browser's
 * own pipeline, so it is always tried first when the container and codec are
 * ones the browser accepts.
 *
 * Tier two exists because of MKV. A Matroska file holding perfectly ordinary
 * H.264 is unplayable in Chrome — the codec is fine, the container is not —
 * and Matroska is what a Telegram drive fills up with. So the file is demuxed
 * here and decoded through WebCodecs onto a canvas. On any machine with
 * hardware decoding this is nearly free, and it also picks up HEVC on Safari
 * and on Chrome builds with HEVC support.
 *
 * Tier three is a WebAssembly decoder for HEVC where the platform has none.
 * It is genuinely slow — a 4K HDR remux will not play smoothly on a laptop —
 * so it says so plainly and offers the download instead of pretending.
 */

type Tier = 'probing' | 'native' | 'canvas' | 'software' | 'failed'

export default function VideoView({ entry, link }: ViewerProps) {
  const [info, setInfo] = useState<MediaInfo | null>(null)
  const [tier, setTier] = useState<Tier>('probing')
  const [error, setError] = useState<string | null>(null)
  const [showDetails, setShowDetails] = useState(false)

  useEffect(() => {
    let cancelled = false
    setTier('probing')
    setError(null)

    void probeMedia(link.url).then(async (probed) => {
      if (cancelled) return
      setInfo(probed)

      switch (probed.strategy) {
        case 'native':
          setTier('native')
          break
        case 'decode':
          setTier('canvas')
          break
        case 'software': {
          const ready = await enableSoftwareDecoding()
          if (cancelled) return
          if (ready) {
            setTier('software')
          } else {
            setTier('failed')
            setError('这个视频用的编码浏览器无法解码，软件解码器也没能加载')
          }
          break
        }
        default:
          // The probe could not make sense of the file. The browser might
          // still know what to do with it, so it gets a turn.
          setTier('native')
      }
    })

    return () => {
      cancelled = true
    }
  }, [link.url])

  // A native attempt that fails on load falls through to the canvas path
  // rather than leaving a broken player: canPlayType is advisory, and it is
  // wrong often enough to be worth catching.
  const onNativeError = useCallback(() => {
    setTier((current) => (current === 'native' ? 'canvas' : current))
  }, [])

  if (tier === 'probing') {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3">
        <Spinner className="size-5" />
        <p className="text-xs text-[var(--muted)]">正在识别视频格式…</p>
      </div>
    )
  }

  return (
    <div className="flex h-full flex-col">
      <div className="relative flex min-h-0 flex-1 items-center justify-center bg-black">
        {tier === 'native' ? (
          <video
            key={entry.id}
            src={link.url}
            controls
            autoPlay
            preload="metadata"
            playsInline
            onError={onNativeError}
            className="max-h-full max-w-full"
          />
        ) : tier === 'failed' ? (
          <div className="max-w-sm p-8 text-center">
            <AlertTriangle size={28} className="mx-auto mb-3 text-[var(--color-warn)]" />
            <p className="text-sm text-white/80">{error}</p>
            <p className="mt-2 text-xs text-white/50">
              下载到本地用 VLC、mpv 或 IINA 播放是最省事的办法。
            </p>
          </div>
        ) : (
          <CanvasVideo url={link.url} software={tier === 'software'} onFail={setError} />
        )}
      </div>

      <div className="shrink-0 border-t border-[var(--line)] px-3 py-2">
        <div className="flex flex-wrap items-center gap-2 text-xs text-[var(--muted)]">
          <TierBadge tier={tier} />
          {info?.video && (
            <span className="tabular-nums">
              {info.video.width}×{info.video.height}
            </span>
          )}
          {info?.duration ? <span>{formatDuration(info.duration)}</span> : null}
          <button
            onClick={() => setShowDetails((v) => !v)}
            className="ml-auto flex items-center gap-1 text-[var(--faint)] transition-colors hover:text-[var(--ink)]"
          >
            <Info size={12} />
            媒体信息
          </button>
        </div>

        {showDetails && info && (
          <dl className="mt-2 grid gap-x-6 gap-y-1 text-xs sm:grid-cols-2">
            <DetailRow label="容器" value={info.container.toUpperCase()} />
            <DetailRow label="视频编码" value={describeCodec(info.video)} />
            <DetailRow label="音频编码" value={describeCodec(info.audio)} />
            <DetailRow
              label="声道 / 采样率"
              value={
                info.audio
                  ? `${info.audio.channels ?? '—'} 声道 · ${info.audio.sampleRate ?? '—'} Hz`
                  : '—'
              }
            />
          </dl>
        )}

        {tier === 'software' && (
          <p className="mt-2 rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs leading-relaxed text-[var(--muted)]">
            浏览器不支持这个编码的硬件解码，正在用 WebAssembly 软解。高码率或 4K 内容可能会卡顿，
            这种情况建议下载到本地播放。
          </p>
        )}
      </div>
    </div>
  )
}

function TierBadge({ tier }: { tier: Tier }) {
  if (tier === 'native') {
    return (
      <span className="chip !border-transparent !bg-green-500/12 !text-green-700 dark:!text-green-400">
        <Zap size={10} />
        原生播放
      </span>
    )
  }
  if (tier === 'canvas') {
    return (
      <span className="chip !border-transparent !bg-blue-500/12 !text-blue-600 dark:!text-blue-400">
        <Cpu size={10} />
        硬件解码 + 画布渲染
      </span>
    )
  }
  if (tier === 'software') {
    return (
      <span className="chip !border-transparent !bg-[var(--clay-soft)] !text-[var(--color-clay)]">
        <Cpu size={10} />
        软件解码
      </span>
    )
  }
  return (
    <span className="chip !border-transparent !bg-[var(--danger-soft)] !text-[var(--color-danger)]">
      <AlertTriangle size={10} />
      无法播放
    </span>
  )
}

function DetailRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-3">
      <dt className="text-[var(--faint)]">{label}</dt>
      <dd className="min-w-0 truncate text-right text-[var(--muted)]" title={value}>
        {value}
      </dd>
    </div>
  )
}

/** CanvasVideo drives the demux-and-decode player and gives it the controls a
 *  <video> element would have provided. */
function CanvasVideo({
  url,
  software,
  onFail,
}: {
  url: string
  software: boolean
  onFail: (message: string) => void
}) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const playerRef = useRef<CanvasPlayer | null>(null)
  const [ready, setReady] = useState(false)
  const [playing, setPlaying] = useState(false)
  const [time, setTime] = useState(0)
  const [duration, setDuration] = useState(0)
  const [volume, setVolume] = useState(1)
  // A ref rather than state: the time callback fires every animation frame and
  // must read the current value without re-subscribing.
  const scrubbing = useRef(false)

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return

    const player = new CanvasPlayer(canvas)
    playerRef.current = player

    player.on('state', (state) => {
      setPlaying(state === 'playing')
      if (state === 'ready') setReady(true)
    })
    // While the user is dragging the scrubber, the player's own time would
    // fight the handle for control of it.
    player.on('time', (seconds) => {
      if (!scrubbing.current) setTime(seconds)
    })
    player.on('duration', setDuration)
    player.on('error', onFail)

    void player.load(url)
    return () => {
      player.destroy()
      playerRef.current = null
    }
  }, [url, onFail])

  const toggle = () => {
    const player = playerRef.current
    if (!player) return
    if (player.playing) player.pause()
    else void player.play()
  }

  return (
    <div className="relative flex h-full w-full flex-col">
      <div className="flex min-h-0 flex-1 items-center justify-center">
        <canvas ref={canvasRef} className="max-h-full max-w-full" />
        {!ready && (
          <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-black/60">
            <Spinner className="size-5 text-white/70" />
            <p className="text-xs text-white/60">
              {software ? '正在启动软件解码器…' : '正在解析视频…'}
            </p>
          </div>
        )}
      </div>

      <div className="flex shrink-0 items-center gap-3 bg-black/70 px-3 py-2 backdrop-blur">
        <button
          onClick={toggle}
          aria-label={playing ? '暂停' : '播放'}
          className="shrink-0 rounded-full p-1.5 text-white transition-colors hover:bg-white/10"
        >
          {playing ? <Pause size={17} /> : <Play size={17} />}
        </button>

        <span className="shrink-0 font-[family-name:var(--font-mono)] text-[11px] tabular-nums text-white/70">
          {clock(time)} / {clock(duration)}
        </span>

        <input
          type="range"
          min={0}
          max={duration || 0}
          step={0.1}
          value={time}
          onChange={(e) => setTime(Number(e.target.value))}
          onPointerDown={() => {
            scrubbing.current = true
          }}
          onPointerUp={(e) => {
            scrubbing.current = false
            void playerRef.current?.seek(Number((e.target as HTMLInputElement).value))
          }}
          className="h-1 min-w-0 flex-1 cursor-pointer appearance-none rounded-full bg-white/20 accent-[var(--color-clay)]"
        />

        <button
          onClick={() => {
            const next = volume > 0 ? 0 : 1
            setVolume(next)
            playerRef.current?.setVolume(next)
          }}
          aria-label={volume > 0 ? '静音' : '取消静音'}
          className="shrink-0 rounded-full p-1.5 text-white transition-colors hover:bg-white/10"
        >
          {volume > 0 ? <Volume2 size={16} /> : <VolumeX size={16} />}
        </button>

        <button
          onClick={() => canvasRef.current?.parentElement?.parentElement?.requestFullscreen?.()}
          aria-label="全屏"
          className={clsx('shrink-0 rounded-full p-1.5 text-white transition-colors hover:bg-white/10')}
        >
          <Maximize2 size={15} />
        </button>
      </div>
    </div>
  )
}

function clock(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '0:00'
  const total = Math.floor(seconds)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  if (h > 0) return `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
  return `${m}:${String(s).padStart(2, '0')}`
}
