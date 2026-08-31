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
            toast('已重新连接 Telegram', 'success')
          } catch (err) {
            toast(err instanceof Error ? err.message : String(err), 'error')
          } finally {
            setReconnecting(false)
          }
        }}
        onLogout={async () => {
          if (!confirm('退出 Telegram 登录？已上传的文件不会被删除，但在重新登录前无法访问。')) return
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
        summary={signedIn ? tg.phone || tg.firstName || '已登录' : hasCredentials ? '等待验证码登录' : '需要先填写凭据'}
        disabled={!hasCredentials}
      >
        {signedIn ? (
          <p className="text-xs leading-relaxed text-[var(--muted)]">
            账号已登录。要更换账号，请先在上方的连接卡片里退出登录。
          </p>
        ) : (
          <LoginStep onDone={onChanged} />
        )}
      </Step>

      <Step
        index={3}
        state={channelState}
        title="存储频道"
        summary={hasChannel ? '已选择存储频道' : signedIn ? '尚未选择频道' : '需要先登录账号'}
        disabled={!signedIn}
      >
        {hasChannel ? (
          <div className="space-y-3">
            <p className="text-xs leading-relaxed text-[var(--muted)]">
              新上传的文件会进入当前频道。更换频道后，已有文件仍留在原频道并可以正常读取。
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
        description="已有文件仍然留在原频道，可以正常读取；新上传会进入新频道。"
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
    ready: { tone: 'ok', label: '已连接', hint: '文件可以正常上传和读取' },
    connecting: { tone: 'busy', label: '连接中', hint: '正在建立 MTProto 连接' },
    unauthorized: { tone: 'warn', label: '未登录', hint: '凭据已保存，还需要登录 Telegram 账号' },
    unconfigured: { tone: 'idle', label: '未配置', hint: '还没有填写 api_id 和 api_hash' },
    error: { tone: 'error', label: '连接失败', hint: error || '请检查凭据和网络' },
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
      toast('已从粘贴内容里识别出 api_id 和 api_hash', 'success')
      return true
    }
    return false
  }

  const save = async () => {
    const id = Number(appId.trim())
    if (!Number.isInteger(id) || id <= 0) {
      setError('api_id 必须是一串数字')
      return
    }
    if (!/^[a-f0-9]{32}$/i.test(appHash.trim())) {
      setError('api_hash 应该是 32 位十六进制字符')
      return
    }

    setBusy(true)
    setError(null)
    try {
      await api.updateSettings({ appId: id, appHash: appHash.trim() })
      await onSaved()
      toast('凭据已保存，正在连接 Telegram', 'success')
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
        <p className="mb-1.5 font-medium text-[var(--ink)]">从哪里拿到这两个值</p>
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
            并用你的手机号登录
          </li>
          <li>填写任意 App title 和 Short name，平台选 Desktop，提交</li>
          <li>页面上会出现 App api_id 和 App api_hash，把它们填到下面</li>
        </ol>
        <p className="mt-1.5">凭据只保存在这台服务器上，不会发送到别处。</p>
      </div>

      <Field label="api_id" hint="也可以把整段内容粘贴到任一输入框，会自动拆分">
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
        保存并连接
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
      toast('Telegram 账号已导出，请妥善保管文件', 'success')
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
    if (!confirm('导入会替换当前 Telegram 登录账号，确定继续吗？')) return

    setBusy(true)
    try {
      const account = JSON.parse(await file.text()) as TelegramAccountExport
      await api.importTelegramAccount(account)
      await onChanged()
      toast('Telegram 账号已导入', 'success')
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
      description="导出登录会话和 api 凭据，换服务器时直接导入，无需重新验证手机号。"
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
            导出的文件等同于这个 Telegram 账号的登录凭据，拿到它就能读取你的全部文件。请勿发送给他人。
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
          <Line label="当前状态" value={ready ? '已登录，可以导出' : '未登录，只能导入'} />
        </div>
      )}
    </Section>
  )
}
