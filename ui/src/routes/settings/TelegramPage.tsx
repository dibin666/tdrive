import { useEffect, useRef, useState, type ChangeEvent, type ReactNode } from 'react'
import clsx from 'clsx'
import {
  Check,
  ChevronDown,
  Download,
  Eye,
  EyeOff,
  ExternalLink,
  KeyRound,
  LogOut,
  RefreshCw,
  Send,
  Upload,
  Radio,
} from 'lucide-react'
import { api, type RuntimeSettings, type TelegramAccountExport } from '../../lib/api'
import { useApp } from '../../app/context'
import { Button, Field, Input, Modal, Spinner, toast } from '../../components/primitives'
import { ChannelStep, LoginStep } from '../Setup'
import { AccountsSection } from './AccountsSection'
import { Line, Section, StatusDot } from './shared'

/**
 * The Telegram page, arranged as the three things that actually have to happen
 * in order: give the server API credentials, sign the account in, pick a
 * channel to store into.
 *
 * The previous version put all three on one flat page with no indication of
 * which one was blocking. That is fine once everything works and useless while
 * setting it up — which is exactly when someone is reading this page.
 */

type StepState = 'done' | 'current' | 'todo'

export function TelegramPage({ onChanged }: { onChanged: () => Promise<void> }) {
  const { status } = useApp()
  const tg = status?.telegram
  const [settings, setSettings] = useState<RuntimeSettings | null>(null)
  const [showChannel, setShowChannel] = useState(false)
  const [reconnecting, setReconnecting] = useState(false)

  const reload = () => {
    void api.settings().then(setSettings).catch(() => {})
  }
  useEffect(reload, [])

  if (!tg) return <Spinner />

  const hasCredentials = Boolean(settings?.appId)
  const signedIn = tg.state === 'ready'
  const hasChannel = status?.hasChannel ?? false

  const credentialState: StepState = hasCredentials ? 'done' : 'current'
  const loginState: StepState = signedIn ? 'done' : hasCredentials ? 'current' : 'todo'
  const channelState: StepState = hasChannel ? 'done' : signedIn ? 'current' : 'todo'
  const channelSummary = hasChannel
    ? status?.channelTitle
      ? `「${status.channelTitle}」`
      : '已指定存储频道'
    : signedIn
      ? '未指定存储频道'
      : '须先登录账号'

  return (
    <div className="space-y-4">
      <ConnectionCard
        state={tg.state}
        error={tg.error}
        name={tg.firstName || tg.username || (tg.userId ? String(tg.userId) : '')}
        phone={tg.phone}
        dc={tg.dc}
        premium={tg.premium}
        busy={reconnecting}
        onReconnect={async () => {
          setReconnecting(true)
          try {
            if (settings?.appId && settings.appHash) {
              await api.configureTelegram(settings.appId, settings.appHash)
            }
            await onChanged()
            toast('已重新建立 Telegram 连接', 'success')
          } catch (err) {
            toast(err instanceof Error ? err.message : String(err), 'error')
          } finally {
            setReconnecting(false)
          }
        }}
        onLogout={async () => {
          if (!confirm('确定退出当前 Telegram 账号？已存文件保留，但重新登录前不可访问。')) return
          await api.telegramLogout()
          await onChanged()
        }}
      />

      <Step index={1} state={credentialState} title="API 凭据" summary={hasCredentials ? `api_id ${settings?.appId}` : '未填写'}>
        <CredentialsForm
          settings={settings}
          onSaved={async () => {
            reload()
            await onChanged()
          }}
        />
      </Step>

      <Step
        index={2}
        state={loginState}
        title="账号登录"
        summary={signedIn ? tg.phone || tg.firstName || '已登录' : hasCredentials ? '等待输入验证码' : '需先配置凭据'}
        disabled={!hasCredentials}
      >
        {signedIn ? (
          <p className="text-xs leading-relaxed text-[var(--muted)]">
            当前账号已登录。如需更换，请在上方卡片中退出登录。
          </p>
        ) : (
          <LoginStep onDone={onChanged} />
        )}
      </Step>

      <Step
        index={3}
        state={channelState}
        title="存储频道"
        summary={channelSummary}
        disabled={!signedIn}
      >
        {hasChannel ? (
          <div className="space-y-3">
            <p className="text-xs leading-relaxed text-[var(--muted)]">
              新上传文件将写入当前频道
              {status?.channelTitle ? `「${status.channelTitle}」` : ''}。更换频道后已有文件依然保留在原频道且可正常读取。
            </p>
            <Button icon={<Radio size={14} />} onClick={() => setShowChannel(true)}>
              更换存储频道
            </Button>
          </div>
        ) : (
          <ChannelStep onDone={onChanged} />
        )}
      </Step>

      <AccountsSection hasChannel={hasChannel} onChanged={onChanged} />

      <AdvancedSection ready={signedIn} onChanged={onChanged} />

      <Modal
        open={showChannel}
        onClose={() => setShowChannel(false)}
        title="更换存储频道"
        description="已有文件保留在原频道并支持继续读取；后续上传将写入新频道。"
        width="max-w-lg"
      >
        <ChannelStep
          onDone={async () => {
            await onChanged()
            setShowChannel(false)
          }}
        />
      </Modal>
    </div>
  )
}

function ConnectionCard({
  state,
  error,
  name,
  phone,
  dc,
  premium,
  busy,
  onReconnect,
  onLogout,
}: {
  state: string
  error?: string
  name: string
  phone?: string
  dc?: number
  premium: boolean
  busy: boolean
  onReconnect: () => void | Promise<void>
  onLogout: () => void | Promise<void>
}) {
  const meta: Record<string, { tone: 'ok' | 'warn' | 'error' | 'idle' | 'busy'; label: string; hint: string }> = {
    ready: { tone: 'ok', label: '已连接', hint: '存储服务正常，支持文件读写' },
    connecting: { tone: 'busy', label: '连接中', hint: '正在建立 MTProto 会话' },
    unauthorized: { tone: 'warn', label: '未登录', hint: 'API 凭据已就绪，请登录 Telegram 账号' },
    unconfigured: { tone: 'idle', label: '未配置', hint: '尚未配置 api_id 与 api_hash' },
    error: { tone: 'error', label: '连接失败', hint: error || '请检查网络或 API 凭据' },
  }
  const current = meta[state] ?? meta.error

  return (
    <section className="panel p-5">
      <div className="flex flex-wrap items-start gap-4">
        <div className="flex min-w-0 flex-1 items-start gap-3">
          <span className="mt-1.5">
            <StatusDot tone={current.tone} />
          </span>
          <div className="min-w-0">
            <h2 className="display text-base">Telegram {current.label}</h2>
            <p className="mt-0.5 text-xs leading-relaxed text-[var(--muted)]">{current.hint}</p>

            {state === 'ready' && (
              <div className="mt-3 flex flex-wrap gap-x-5 gap-y-1 text-xs">
                <span className="text-[var(--muted)]">
                  账号 <span className="text-[var(--ink)]">{name || '—'}</span>
                </span>
                {phone && (
                  <span className="text-[var(--muted)]">
                    手机号 <span className="text-[var(--ink)]">{phone}</span>
                  </span>
                )}
                <span className="text-[var(--muted)]">
                  数据中心 <span className="text-[var(--ink)]">{dc ? `DC${dc}` : '—'}</span>
                </span>
                <span className="text-[var(--muted)]">
                  Premium <span className="text-[var(--ink)]">{premium ? '是' : '否'}</span>
                </span>
              </div>
            )}
          </div>
        </div>

        <div className="flex shrink-0 gap-2">
          <Button icon={<RefreshCw size={14} />} loading={busy} onClick={() => void onReconnect()}>
            重新连接
          </Button>
          {state === 'ready' && (
            <Button variant="danger" icon={<LogOut size={14} />} onClick={() => void onLogout()}>
              退出
            </Button>
          )}
        </div>
      </div>
    </section>
  )
}

function Step({
  index,
  state,
  title,
  summary,
  disabled,
  children,
}: {
  index: number
  state: StepState
  title: string
  summary: string
  disabled?: boolean
  children: ReactNode
}) {
  // A completed step collapses but stays openable: redoing one is a normal
  // thing to want, and hiding it behind a different page would be worse.
  const [open, setOpen] = useState(state === 'current')

  useEffect(() => {
    setOpen(state === 'current')
  }, [state])

  return (
    <section className={clsx('panel overflow-hidden', disabled && 'opacity-60')}>
      <button
        disabled={disabled}
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-3 px-5 py-4 text-left disabled:cursor-not-allowed"
      >
        <span
          className={clsx(
            'flex size-6 shrink-0 items-center justify-center rounded-full text-xs font-medium',
            state === 'done'
              ? 'bg-[var(--color-success)]/15 text-[var(--color-success)]'
              : state === 'current'
                ? 'bg-[var(--color-clay)] text-white'
                : 'bg-[var(--sunk)] text-[var(--faint)]',
          )}
        >
          {state === 'done' ? <Check size={13} /> : index}
        </span>
        <span className="min-w-0 flex-1">
          <span className="block text-sm font-medium">{title}</span>
          <span className="block truncate text-xs text-[var(--muted)]">{summary}</span>
        </span>
        <ChevronDown
          size={15}
          className={clsx('shrink-0 text-[var(--faint)] transition-transform', open && 'rotate-180')}
        />
      </button>
      {open && !disabled && <div className="border-t border-[var(--line)] px-5 py-4">{children}</div>}
    </section>
  )
}

function CredentialsForm({
  settings,
  onSaved,
}: {
  settings: RuntimeSettings | null
  onSaved: () => Promise<void>
}) {
  const [appId, setAppId] = useState('')
  const [appHash, setAppHash] = useState('')
  const [reveal, setReveal] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!settings) return
    setAppId(settings.appId ? String(settings.appId) : '')
    setAppHash(settings.appHash)
  }, [settings])

  /**
   * People paste the whole block from my.telegram.org rather than two fields.
   * Pulling the numbers out of it is two lines here and saves an error message
   * that would otherwise be entirely the interface's fault.
   */
  const absorbPaste = (text: string) => {
    const id = text.match(/\b(\d{5,10})\b/)
    const hash = text.match(/\b([a-f0-9]{32})\b/i)
    if (id && hash) {
      setAppId(id[1])
      setAppHash(hash[1])
      toast('已从剪贴板内容自动解析 api_id 与 api_hash', 'success')
      return true
    }
    return false
  }

  const save = async () => {
    const id = Number(appId.trim())
    if (!Number.isInteger(id) || id <= 0) {
      setError('api_id 必须为正整数')
      return
    }
    if (!/^[a-f0-9]{32}$/i.test(appHash.trim())) {
      setError('api_hash 必须为 32 位十六进制字符')
      return
    }

    setBusy(true)
    setError(null)
    try {
      await api.updateSettings({ appId: id, appHash: appHash.trim() })
      await onSaved()
      toast('凭据已保存，正在建立连接', 'success')
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  if (!settings) return <Spinner />

  return (
    <div className="space-y-4">
      <div className="rounded-[var(--radius-card)] bg-[var(--sunk)] p-3 text-xs leading-relaxed text-[var(--muted)]">
        <p className="mb-1.5 font-medium text-[var(--ink)]">获取 API 凭据</p>
        <ol className="list-decimal space-y-0.5 pl-4">
          <li>
            打开{' '}
            <a
              href="https://my.telegram.org/apps"
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-0.5 text-[var(--color-clay)] underline underline-offset-2"
            >
              my.telegram.org/apps
              <ExternalLink size={10} />
            </a>{' '}
            并使用手机号登录
          </li>
          <li>填写应用信息（平台选择 Desktop）并提交</li>
          <li>将生成的 api_id 与 api_hash 填入下方输入框</li>
        </ol>
        <p className="mt-1.5">凭据仅保存在本地服务器，不向外部泄露。</p>
      </div>

      <Field label="api_id" hint="亦可直接粘贴完整网页内容，系统将自动识别">
        <Input
          value={appId}
          inputMode="numeric"
          onChange={(e) => setAppId(e.target.value)}
          onPaste={(e) => {
            if (absorbPaste(e.clipboardData.getData('text'))) e.preventDefault()
          }}
          placeholder="1234567"
        />
      </Field>

      <Field label="api_hash" error={error ?? undefined}>
        <div className="relative">
          <Input
            value={appHash}
            type={reveal ? 'text' : 'password'}
            onChange={(e) => setAppHash(e.target.value)}
            onPaste={(e) => {
              if (absorbPaste(e.clipboardData.getData('text'))) e.preventDefault()
            }}
            className="pr-10 font-[family-name:var(--font-mono)] text-xs"
            placeholder="0123456789abcdef0123456789abcdef"
          />
          <button
            type="button"
            onClick={() => setReveal((v) => !v)}
            aria-label={reveal ? '隐藏' : '显示'}
            className="absolute right-2 top-1/2 -translate-y-1/2 p-1 text-[var(--faint)] transition-colors hover:text-[var(--ink)]"
          >
            {reveal ? <EyeOff size={14} /> : <Eye size={14} />}
          </button>
        </div>
      </Field>

      <Button variant="primary" icon={<Send size={14} />} loading={busy} onClick={() => void save()}>
        保存并建立连接
      </Button>
    </div>
  )
}

function AdvancedSection({ ready, onChanged }: { ready: boolean; onChanged: () => Promise<void> }) {
  const [open, setOpen] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const [busy, setBusy] = useState(false)

  const exportAccount = async () => {
    setBusy(true)
    try {
      const account = await api.exportTelegramAccount()
      const blob = new Blob([JSON.stringify(account, null, 2)], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = 'tdrive-telegram-account.json'
      link.style.display = 'none'
      document.body.appendChild(link)
      link.click()
      link.remove()
      window.setTimeout(() => URL.revokeObjectURL(url), 0)
      toast('Telegram 凭据已导出，请妥善保管配置文件', 'success')
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    } finally {
      setBusy(false)
    }
  }

  const importAccount = async (event: ChangeEvent<HTMLInputElement>) => {
    const input = event.currentTarget
    const file = input.files?.[0]
    input.value = ''
    if (!file) return
    if (!confirm('导入新配置将覆盖当前 Telegram 登录状态，确定继续？')) return

    setBusy(true)
    try {
      const account = JSON.parse(await file.text()) as TelegramAccountExport
      await api.importTelegramAccount(account)
      await onChanged()
      toast('Telegram 账号配置已导入', 'success')
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={<KeyRound size={16} />}
      title="账号迁移"
      description="导出登录会话与 API 凭据，换机部署时直接导入，无需重新验证手机号。"
      actions={
        <button
          onClick={() => setOpen((v) => !v)}
          className="flex items-center gap-1 text-xs text-[var(--muted)] transition-colors hover:text-[var(--ink)]"
        >
          {open ? '收起' : '展开'}
          <ChevronDown size={13} className={clsx('transition-transform', open && 'rotate-180')} />
        </button>
      }
    >
      {open && (
        <div className="space-y-3">
          <p className="rounded-[var(--radius-control)] bg-[var(--danger-soft)] px-3 py-2 text-xs leading-relaxed text-[var(--color-danger)]">
            导出文件包含 Telegram 登录凭据，持有者可读取存储数据。请勿泄露。
          </p>
          <div className="flex flex-wrap gap-2">
            <Button
              icon={<Download size={14} />}
              loading={busy}
              disabled={!ready}
              onClick={() => void exportAccount()}
            >
              导出账号
            </Button>
            <Button icon={<Upload size={14} />} loading={busy} onClick={() => fileInput.current?.click()}>
              导入账号
            </Button>
            <input
              ref={fileInput}
              type="file"
              accept="application/json,.json"
              className="hidden"
              onChange={(event) => void importAccount(event)}
            />
          </div>
          <Line label="当前状态" value={ready ? '已就绪，支持导出' : '未登录，仅支持导入'} />
        </div>
      )}
    </Section>
  )
}
