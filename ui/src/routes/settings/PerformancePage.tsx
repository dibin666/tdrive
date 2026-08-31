import { useEffect, useMemo, useState } from 'react'
import clsx from 'clsx'
import { Feather, Gauge, RotateCcw, Rocket, Scale, SlidersHorizontal } from 'lucide-react'
import { api, type RuntimeSettings } from '../../lib/api'
import { formatBytes } from '../../lib/format'
import { Button, Field, Select, Slider, Spinner, toast } from '../../components/primitives'
import { Section } from './shared'

/**
 * Telegram tuning.
 *
 * The old form was eight number boxes and a save button that answered
 * "分片大小必须是 Telegram 上传分片的整数倍" after the fact. Every constraint
 * here is a real one — Telegram accepts at most 4000 parts per object, and the
 * part size has to divide 512 KiB — so the fix is not better error messages
 * but controls that cannot express the invalid combinations in the first
 * place: the part size is a list of the legal divisors, and the segment slider
 * derives its ceiling and its step from whatever that list currently says.
 *
 * The presets exist because most people do not want to understand any of this;
 * they want "my link is slow" or "I have a good VPS" to be an answer.
 */

const MAX_UPLOAD_PARTS = 4000
const KIB = 1024
const MIB = 1024 * 1024

/** The divisors of 512 KiB, which is exactly the set Telegram accepts. */
const PART_SIZES = [16, 32, 64, 128, 256, 512].map((kib) => kib * KIB)

interface Preset {
  id: string
  label: string
  icon: typeof Feather
  blurb: string
  values: Partial<RuntimeSettings>
}

const PRESETS: Preset[] = [
  {
    id: 'safe',
    label: '保守',
    icon: Feather,
    blurb: '弱网、家宽或经常被限流时用。请求更稀疏，出错更少。',
    values: {
      uploadPartSize: 256 * KIB,
      segmentSize: 1024 * MIB,
      rateLimitMs: 300,
      poolSize: 4,
      uploadThreads: 4,
      streamConcurrency: 3,
      uploadConcurrency: 1,
      downloadConcurrency: 1,
      maxDownloadConns: 4,
    },
  },
  {
    id: 'balanced',
    label: '均衡',
    icon: Scale,
    blurb: '默认配置。绝大多数 VPS 直接用这一档就好。',
    values: {
      uploadPartSize: 512 * KIB,
      segmentSize: 1900 * MIB,
      rateLimitMs: 100,
      poolSize: 8,
      uploadThreads: 8,
      streamConcurrency: 6,
      uploadConcurrency: 2,
      downloadConcurrency: 2,
      maxDownloadConns: 8,
    },
  },
  {
    id: 'fast',
    label: '极速',
    icon: Rocket,
    blurb: '带宽充足的独立服务器。吞吐最高，也最容易撞上 Telegram 限流。',
    values: {
      uploadPartSize: 512 * KIB,
      segmentSize: 1900 * MIB,
      rateLimitMs: 30,
      poolSize: 16,
      uploadThreads: 16,
      streamConcurrency: 12,
      uploadConcurrency: 4,
      downloadConcurrency: 4,
      maxDownloadConns: 16,
    },
  },
]

export function PerformancePage() {
  const [settings, setSettings] = useState<RuntimeSettings | null>(null)
  const [draft, setDraft] = useState<RuntimeSettings | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [custom, setCustom] = useState(false)

  useEffect(() => {
    void api
      .settings()
      .then((value) => {
        setSettings(value)
        setDraft(value)
      })
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
  }, [])

  const patch = (next: Partial<RuntimeSettings>) => {
    setDraft((prev) => {
      if (!prev) return prev
      const merged = { ...prev, ...next }
      // Changing the part size can invalidate the current segment size, so the
      // segment is snapped back into range in the same update rather than
      // being rejected at save time.
      return { ...merged, segmentSize: clampSegment(merged.segmentSize, merged.uploadPartSize) }
    })
  }

  const activePreset = useMemo(() => {
    if (!draft) return null
    return (
      PRESETS.find((preset) =>
        Object.entries(preset.values).every(
          ([key, value]) => draft[key as keyof RuntimeSettings] === value,
        ),
      )?.id ?? null
    )
  }, [draft])

  const dirty = useMemo(
    () => Boolean(settings && draft && JSON.stringify(settings) !== JSON.stringify(draft)),
    [draft, settings],
  )

  const save = async () => {
    if (!draft) return
    setBusy(true)
    setError(null)
    try {
      const saved = await api.updateSettings({
        segmentSize: draft.segmentSize,
        uploadPartSize: draft.uploadPartSize,
        rateLimitMs: draft.rateLimitMs,
        poolSize: draft.poolSize,
        uploadThreads: draft.uploadThreads,
        streamConcurrency: draft.streamConcurrency,
        uploadConcurrency: draft.uploadConcurrency,
        downloadConcurrency: draft.downloadConcurrency,
        maxDownloadConns: draft.maxDownloadConns,
        downloadGraceMs: draft.downloadGraceMs,
      })
      setSettings(saved)
      setDraft(saved)
      toast('参数已保存并立即生效', 'success')
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  if (!draft) {
    return (
      <Section icon={<Gauge size={16} />} title="性能参数">
        {error ? <p className="text-sm text-[var(--color-danger)]">{error}</p> : <Spinner />}
      </Section>
    )
  }

  const maxSegmentMiB = (draft.uploadPartSize * MAX_UPLOAD_PARTS) / MIB
  const segmentMiB = draft.segmentSize / MIB
  const stepMiB = draft.uploadPartSize / MIB
  const partsPerSegment = Math.round(draft.segmentSize / draft.uploadPartSize)
  // Pool size and thread count are per account: every login opens its own
  // pool and runs its own uploads, so the drive-wide peak is this times the
  // number of accounts.
  const accounts = Math.max(1, draft.accountCount ?? 1)
  const peakConnections = Number(draft.poolSize) * draft.uploadThreads

  return (
    <div className="space-y-4">
      <Section
        icon={<Gauge size={16} />}
        title="性能预设"
        description="先按网络条件挑一档，需要再逐项微调。所有改动保存后立即生效，不需要重启。"
      >
        <div className="grid gap-2 sm:grid-cols-3">
          {PRESETS.map((preset) => {
            const Icon = preset.icon
            const active = activePreset === preset.id
            return (
              <button
                key={preset.id}
                onClick={() => {
                  patch(preset.values)
                  setCustom(false)
                }}
                className={clsx(
                  'rounded-[var(--radius-card)] border p-3 text-left transition-colors',
                  active
                    ? 'border-[var(--color-clay)] bg-[var(--clay-soft)]'
                    : 'border-[var(--line)] hover:border-[var(--line-strong)]',
                )}
              >
                <div className="flex items-center gap-2">
                  <Icon
                    size={15}
                    className={active ? 'text-[var(--color-clay)]' : 'text-[var(--faint)]'}
                  />
                  <span className="text-sm font-medium">{preset.label}</span>
                </div>
                <p className="mt-1.5 text-xs leading-relaxed text-[var(--muted)]">{preset.blurb}</p>
              </button>
            )
          })}
        </div>

        {activePreset === null && (
          <p className="mt-3 text-xs text-[var(--muted)]">
            当前是自定义配置，不完全匹配任何预设。
          </p>
        )}

        <button
          onClick={() => setCustom((v) => !v)}
          className="mt-3 flex items-center gap-1.5 text-xs text-[var(--muted)] transition-colors hover:text-[var(--ink)]"
        >
          <SlidersHorizontal size={13} />
          {custom ? '收起自定义参数' : '展开自定义参数'}
        </button>
      </Section>

      {custom && (
        <>
          <Section
            icon={<SlidersHorizontal size={16} />}
            title="分片"
            description="决定一个文件怎么被切开存进 Telegram。只影响之后新上传的文件。"
          >
            <div className="space-y-5">
              <Field
                label="Telegram 上传分片"
                hint="Telegram 只接受能整除 512 KiB 的大小，所以这里是一个固定的选项列表"
              >
                <Select
                  value={draft.uploadPartSize}
                  onChange={(e) => patch({ uploadPartSize: Number(e.target.value) })}
                >
                  {PART_SIZES.map((size) => (
                    <option key={size} value={size}>
                      {size / KIB} KiB
                      {size === 512 * KIB ? '（默认，最快）' : ''}
                    </option>
                  ))}
                </Select>
              </Field>

              <Field label="存储分片大小">
                <Slider
                  value={Number(segmentMiB.toFixed(2))}
                  min={stepMiB}
                  max={Math.min(2000, maxSegmentMiB)}
                  step={stepMiB}
                  suffix="MiB"
                  onChange={(value) => patch({ segmentSize: Math.round(value * MIB) })}
                  format={() => (
                    <>
                      {formatBytes(draft.segmentSize)} ＝ {partsPerSegment.toLocaleString()} ×{' '}
                      {draft.uploadPartSize / KIB} KiB 分片；当前上传分片下最大可设到{' '}
                      {Math.min(2000, maxSegmentMiB)} MiB。一个 40 GB 的文件会被拆成{' '}
                      {Math.ceil((40 * 1024) / segmentMiB)} 卷。
                    </>
                  )}
                />
              </Field>
            </div>
          </Section>

          <Section
            icon={<SlidersHorizontal size={16} />}
            title="连接与并发"
            description="连接池和线程决定单个传输能跑多快；任务数决定同时能有几个传输。"
          >
            <div className="space-y-5">
              <Field label="请求间隔">
                <Slider
                  value={draft.rateLimitMs}
                  min={10}
                  max={1000}
                  step={10}
                  suffix="ms"
                  onChange={(value) => patch({ rateLimitMs: value })}
                  format={(value) => (
                    <span
                      className={clsx(
                        value < 50
                          ? 'text-[var(--color-danger)]'
                          : value < 100
                            ? 'text-[var(--color-warn)]'
                            : undefined,
                      )}
                    >
                      每秒最多约 {Math.round(1000 / value)} 个请求。
                      {value < 50
                        ? ' 很容易触发 Telegram 限流（FLOOD_WAIT）。'
                        : value < 100
                          ? ' 偏激进，网络不稳时可能限流。'
                          : ' 这个区间是安全的。'}
                    </span>
                  )}
                />
              </Field>

              <Field label="Telegram 连接池">
                <Slider
                  value={Number(draft.poolSize)}
                  min={1}
                  max={32}
                  onChange={(value) => patch({ poolSize: value })}
                  format={() =>
                    accounts > 1
                      ? `每个账号保持 ${draft.poolSize} 条 MTProto 连接，${accounts} 个账号共 ${
                          Number(draft.poolSize) * accounts
                        } 条。`
                      : `与 Telegram 数据中心保持 ${draft.poolSize} 条 MTProto 连接。`
                  }
                />
              </Field>

              <Field label="单个上传的线程数">
                <Slider
                  value={draft.uploadThreads}
                  min={1}
                  max={32}
                  onChange={(value) => patch({ uploadThreads: value })}
                  format={() =>
                    `一个分卷内同时发出 ${draft.uploadThreads} 个分片请求；预计峰值并发约 ${peakConnections} 个请求。`
                  }
                />
              </Field>

              <Field label="单个下载的并发块数">
                <Slider
                  value={draft.streamConcurrency}
                  min={1}
                  max={32}
                  onChange={(value) => patch({ streamConcurrency: value })}
                  format={() => `读取时同时预取 ${draft.streamConcurrency} 个 1 MiB 数据块。`}
                />
              </Field>
            </div>
          </Section>

          <Section
            icon={<SlidersHorizontal size={16} />}
            title="任务队列"
            description={
              accounts > 1
                ? `这两个额度是「每个 Telegram 账号」的。当前有 ${accounts} 个账号可用，实际额度是设置值的 ${accounts} 倍；每个传输固定走一个账号，不会跨账号拆分。`
                : '这两个额度由 WebUI、VPS 上传、离线下载和 WebDAV 共同占用。超出后排队等待，不会失败。额度按 Telegram 账号计算，再加一个账号就能翻倍。'
            }
          >
            <div className="space-y-5">
              <Field label="同时进行的上传任务">
                <Slider
                  value={draft.uploadConcurrency}
                  min={1}
                  max={16}
                  suffix="个"
                  onChange={(value) => patch({ uploadConcurrency: value })}
                  format={() =>
                    accounts > 1
                      ? `每账号 ${draft.uploadConcurrency} 个 × ${accounts} 个账号 ＝ 实际最多 ${
                          draft.uploadConcurrency * accounts
                        } 个文件同时上传。浏览器上传的多个分卷算作一个任务。`
                      : `最多 ${draft.uploadConcurrency} 个文件同时上传。浏览器上传的多个分卷算作一个任务。`
                  }
                />
              </Field>

              <Field label="同时进行的下载任务">
                <Slider
                  value={draft.downloadConcurrency}
                  min={1}
                  max={16}
                  suffix="个"
                  onChange={(value) => patch({ downloadConcurrency: value })}
                  format={() =>
                    accounts > 1
                      ? `每账号 ${draft.downloadConcurrency} 个 × ${accounts} 个账号 ＝ 实际最多 ${
                          draft.downloadConcurrency * accounts
                        } 个文件同时读取。一个下载开的多条连接只算一个任务。`
                      : `最多 ${draft.downloadConcurrency} 个文件同时从 Telegram 读取。一个下载开的多条连接只算一个任务。`
                  }
                />
              </Field>

              <Field label="单个下载允许的连接数">
                <Slider
                  value={draft.maxDownloadConns}
                  min={1}
                  max={32}
                  suffix="条"
                  onChange={(value) => patch({ maxDownloadConns: value })}
                  format={() =>
                    `多线程下载最多开 ${draft.maxDownloadConns} 条连接，超出的请求会收到 429 并自动退避。`
                  }
                />
              </Field>
            </div>
          </Section>
        </>
      )}

      {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}

      <div className="flex flex-wrap items-center gap-2">
        <Button variant="primary" loading={busy} disabled={!dirty} onClick={() => void save()}>
          {dirty ? '保存并生效' : '已是最新'}
        </Button>
        {dirty && (
          <Button icon={<RotateCcw size={14} />} onClick={() => setDraft(settings)}>
            撤销修改
          </Button>
        )}
      </div>
    </div>
  )
}

/** clampSegment keeps the segment size a legal multiple of the part size and
 *  inside the 4000-part ceiling, which is what makes the two controls
 *  impossible to put into an invalid combination. */
function clampSegment(segment: number, partSize: number): number {
  const max = Math.min(2000 * MIB, partSize * MAX_UPLOAD_PARTS)
  const snapped = Math.round(segment / partSize) * partSize
  return Math.max(partSize, Math.min(max, snapped))
}
