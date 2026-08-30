import { ALL_FORMATS, Input, UrlSource, type InputAudioTrack } from 'mediabunny'

/**
 * Reading an audio file's header.
 *
 * The audio player is a plain <audio> element, which handles MP3, AAC, FLAC,
 * Opus and WAV everywhere. The probe exists for the two things the element
 * cannot supply: the codec details shown beside the player, and advance notice
 * that the browser has no decoder for this file — APE and older WMA being the
 * usual culprits — so the UI can say so instead of presenting a dead player.
 *
 * Only the header is read, which the range-capable byte endpoint makes cheap:
 * a few hundred kilobytes rather than a download.
 */

export type PlaybackStrategy = 'native' | 'unsupported'

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
  audio?: TrackInfo & { channels?: number; sampleRate?: number }
  /** Set when the probe itself failed, in which case native playback is still
   *  worth attempting — the browser may know something we do not. */
  error?: string
}

function containerName(formatName: string): string {
  const lower = formatName.toLowerCase()
  if (lower.includes('quicktime')) return 'quicktime'
  if (lower.includes('mp4') || lower.includes('isobmff')) return 'mp4'
  if (lower.includes('webm')) return 'webm'
  if (lower.includes('matroska')) return 'matroska'
  if (lower.includes('ogg')) return 'ogg'
  if (lower.includes('wave')) return 'wave'
  if (lower.includes('mp3')) return 'mp3'
  if (lower.includes('flac')) return 'flac'
  if (lower.includes('adts')) return 'aac'
  return lower
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
 *  spinner is the one outcome worse than guessing. */
const PROBE_TIMEOUT_MS = 30_000

async function describeInput(input: Input): Promise<MediaInfo> {
  const format = await input.getFormat()
  const container = containerName(format.name)

  const audioTrack = await input.getPrimaryAudioTrack()
  const [audio, duration] = await Promise.all([
    audioTrack ? describeAudio(audioTrack) : Promise.resolve(undefined),
    input.getDurationFromMetadata().catch(() => 0),
  ])

  return {
    // No track at all means the probe found nothing to play; a track the
    // platform cannot decode is the case worth warning about.
    strategy: audio?.canDecode ? 'native' : 'unsupported',
    container,
    duration: duration ?? 0,
    audio,
  }
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
      timer = setTimeout(() => reject(new Error('识别音频格式超时')), PROBE_TIMEOUT_MS)
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

/** describeCodec renders a codec id for the media-info panel. */
export function describeCodec(info?: TrackInfo | null): string {
  if (!info) return '—'
  const name = (info.codec ?? '未知').toUpperCase()
  return info.codecString ? `${name} (${info.codecString})` : name
}
