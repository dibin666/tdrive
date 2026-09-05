import { useEffect, useState } from 'react'
import { Music } from 'lucide-react'
import type { ViewerProps } from './PreviewModal'
import { describeCodec, probeMedia, type MediaInfo } from '../../lib/media/probe'
import { formatDuration } from '../../lib/format'
import { EntryIcon } from '../icons'

/**
 * Audio gets the native element wherever it works, which is nearly always —
 * MP3, AAC, FLAC, Opus and WAV are all handled by every current browser. The
 * probe is here for the metadata panel and for the cases it does not handle
 * (APE, older WMA), where saying so beats a silent dead player.
 */
export default function AudioView({ entry, link }: ViewerProps) {
  const [info, setInfo] = useState<MediaInfo | null>(null)

  useEffect(() => {
    let cancelled = false
    setInfo(null)
    void probeMedia(link.url).then((probed) => {
      if (!cancelled) setInfo(probed)
    })
    return () => {
      cancelled = true
    }
  }, [link.url])

  const unsupported = info?.strategy === 'unsupported'

  return (
    <div className="flex h-full flex-col items-center justify-center gap-6 p-8">
      <div className="flex size-24 items-center justify-center rounded-[var(--radius-panel)] bg-[var(--sunk)]">
        <EntryIcon name={entry.name} mime={entry.mime} isDir={false} size={38} />
      </div>

      <div className="w-full max-w-lg text-center">
        <h3 className="truncate text-sm font-medium">{entry.name}</h3>
        {info && (
          <p className="mt-1 flex flex-wrap items-center justify-center gap-x-3 gap-y-1 text-xs text-[var(--muted)]">
            <span>{info.container.toUpperCase()}</span>
            <span>{describeCodec(info.audio)}</span>
            {info.audio?.sampleRate ? <span>{info.audio.sampleRate} Hz</span> : null}
            {info.audio?.channels ? <span>{info.audio.channels} 声道</span> : null}
            {info.duration ? <span>{formatDuration(info.duration)}</span> : null}
          </p>
        )}
      </div>

      {unsupported ? (
        <div className="max-w-sm rounded-[var(--radius-card)] bg-[var(--sunk)] px-4 py-3 text-center">
          <Music size={20} className="mx-auto mb-2 text-[var(--faint)]" />
          <p className="text-xs leading-relaxed text-[var(--muted)]">
            浏览器不支持此音频编码，请下载后在本地播放。
          </p>
        </div>
      ) : (
        <audio key={entry.id} src={link.url} controls autoPlay className="w-full max-w-lg" />
      )}
    </div>
  )
}
