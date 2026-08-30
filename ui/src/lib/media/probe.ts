import {
  ALL_FORMATS,
  Input,
  UrlSource,
  type InputAudioTrack,
  type InputVideoTrack,
} from 'mediabunny'

/**
 * Deciding how to play a file.
 *
 * A Telegram drive collects whatever people upload, which in practice means a
 * lot of MKV. Browsers cannot play an MKV at all — not even one containing
 * plain H.264 that the same browser decodes happily inside an MP4 — because
 * the container is unsupported, not the codec. And a good share of the rest is
 * HEVC, which only Safari and some hardware-accelerated Chrome builds will
 * touch natively.
 *
 * So "can this play?" has three separate answers, and the player picks the
 * cheapest one that works:
 *
 *   native    the browser plays the URL directly; nothing beats this
 *   decode    the browser can decode the codecs but not read the container, so
 *             mediabunny demuxes and WebCodecs decodes onto a canvas
 *   software  nothing native can decode the codec, so a WASM decoder does it
 *
 * The probe only reads the file header, which the range-capable byte endpoint
 * makes cheap: a few hundred kilobytes rather than a download.
 */

export type PlaybackStrategy = 'native' | 'decode' | 'software' | 'unsupported'

export interface TrackInfo {
  codec: string | null
  codecString: string | null
  canDecode: boolean
  language?: string
}

export interface MediaInfo {
  strategy: PlaybackStrategy
  container: string
  duration: number
  video?: TrackInfo & { width: number; height: number }
  audio?: TrackInfo & { channels?: number; sampleRate?: number }
  /** Set when the probe itself failed, in which case native playback is still
   *  worth attempting — the browser may know something we do not. */
  error?: string
}

/** nativeMimeFor maps what we learned back onto a MIME type the browser can
 *  be asked about. A null result means "not something <video> will take". */
function nativeMimeFor(container: string, video: string | null, audio: string | null): string | null {
  const codecs = [video, audio].filter(Boolean).join(', ')
  switch (container) {
    case 'mp4':
    case 'quicktime':
      return codecs ? `video/mp4; codecs="${codecs}"` : 'video/mp4'
    case 'webm':
      return codecs ? `video/webm; codecs="${codecs}"` : 'video/webm'
    case 'ogg':
      return 'video/ogg'
    default:
      // Matroska, MPEG-TS, FLV and friends: no browser plays these, whatever
      // is inside them.
      return null
  }
}

function containerName(formatName: string): string {
  const lower = formatName.toLowerCase()
  if (lower.includes('quicktime')) return 'quicktime'
  if (lower.includes('mp4') || lower.includes('isobmff')) return 'mp4'
  if (lower.includes('webm')) return 'webm'
  if (lower.includes('matroska')) return 'matroska'
  if (lower.includes('mpeg-ts') || lower.includes('mpegts')) return 'mpegts'
  if (lower.includes('ogg')) return 'ogg'
  if (lower.includes('wave')) return 'wave'
  if (lower.includes('mp3')) return 'mp3'
  if (lower.includes('flac')) return 'flac'
  if (lower.includes('adts')) return 'aac'
  if (lower.includes('hls')) return 'hls'
  return lower
}

async function describeVideo(track: InputVideoTrack) {
  const [codecString, canDecode] = await Promise.all([
    track.getCodecParameterString().catch(() => null),
    track.canDecode().catch(() => false),
  ])
  return {
    codec: track.codec,
    codecString,
    canDecode,
    width: track.displayWidth,
    height: track.displayHeight,
    language: track.languageCode,
  }
}

async function describeAudio(track: InputAudioTrack) {
  const [codecString, canDecode] = await Promise.all([
    track.getCodecParameterString().catch(() => null),
    track.canDecode().catch(() => false),
  ])
  return {
    codec: track.codec,
    codecString,
    canDecode,
    channels: track.numberOfChannels,
    sampleRate: track.sampleRate,
    language: track.languageCode,
  }
}

/** A header probe is a few hundred kilobytes of range requests. Taking longer
 *  than this means something upstream is stalled, and waiting forever on a
 *  spinner that says "identifying format" is the one outcome worse than
 *  guessing. */
const PROBE_TIMEOUT_MS = 30_000

async function describeInput(input: Input): Promise<MediaInfo> {
  const format = await input.getFormat()
  const container = containerName(format.name)

  const [videoTrack, audioTrack] = await Promise.all([
    input.getPrimaryVideoTrack(),
    input.getPrimaryAudioTrack(),
  ])

  const [video, audio, duration] = await Promise.all([
    videoTrack ? describeVideo(videoTrack) : Promise.resolve(undefined),
    audioTrack ? describeAudio(audioTrack) : Promise.resolve(undefined),
    input.getDurationFromMetadata().catch(() => 0),
  ])

  const mime = nativeMimeFor(container, video?.codecString ?? null, audio?.codecString ?? null)
  let strategy: PlaybackStrategy

  if (mime && canPlayNatively(mime)) {
    strategy = 'native'
  } else if (video?.canDecode || (!video && audio?.canDecode)) {
    // The container is the problem, not the codec: demux here, decode with
    // WebCodecs, draw to a canvas.
    strategy = 'decode'
  } else if (video || audio) {
    strategy = 'software'
  } else {
    strategy = 'unsupported'
  }

  return { strategy, container, duration: duration ?? 0, video, audio }
}

export async function probeMedia(url: string): Promise<MediaInfo> {
  // A probe that fails must not stop playback: the browser is perfectly
  // capable of playing a file this code could not parse, and refusing on that
  // basis would be worse than the problem being solved.
  const fallback: MediaInfo = { strategy: 'native', container: 'unknown', duration: 0 }

  let input: Input | null = null
  let timer: ReturnType<typeof setTimeout> | undefined
  try {
    input = new Input({ formats: ALL_FORMATS, source: new UrlSource(url) })

    const expired = new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error('识别视频格式超时')), PROBE_TIMEOUT_MS)
    })
    return await Promise.race([describeInput(input), expired])
  } catch (err) {
    return { ...fallback, error: err instanceof Error ? err.message : String(err) }
  } finally {
    clearTimeout(timer)
    // The probe holds an open source; leaving it around would keep buffered
    // header bytes alive for every file the user glanced at — and after a
    // timeout it is also what cancels the requests still in flight.
    try {
      input?.dispose?.()
    } catch {
      /* dispose is best-effort */
    }
  }
}

/** canPlayNatively asks the browser, which is the only authority on this. */
export function canPlayNatively(mime: string): boolean {
  if (typeof document === 'undefined') return false
  const probe = document.createElement('video')
  const answer = probe.canPlayType(mime)
  // "maybe" is the browser hedging about the codecs string; treating it as a
  // yes is right, because the fallback is one failed load event away.
  return answer === 'probably' || answer === 'maybe'
}

/** describeCodec renders a codec id for the media-info panel. */
export function describeCodec(info?: TrackInfo | null): string {
  if (!info) return '—'
  const name = (info.codec ?? '未知').toUpperCase()
  return info.codecString ? `${name} (${info.codecString})` : name
}
