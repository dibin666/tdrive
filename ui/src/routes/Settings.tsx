import { useEffect, useRef, useState, type ChangeEvent, type ReactNode } from 'react'
import clsx from 'clsx'
import {
  Copy,
  Database,
  Download,
  HardDrive,
  KeyRound,
  LogOut,
  RefreshCw,
  Send,
  SlidersHorizontal,
  Trash2,
  Upload,
  UserPlus,
  Users,
} from 'lucide-react'
import {
  api,
  type IndexStatus,
  type RuntimeSettings,
  type Stats,
  type TelegramAccountExport,
  type User,
} from '../lib/api'
import { events } from '../lib/events'
import { formatBytes } from '../lib/format'
import { useApp } from '../app/context'
import { Button, Field, Input, Modal, Progress, Spinner, toast } from '../components/primitives'
import { ChannelStep, CredentialsStep, LoginStep } from './Setup'

type SettingsTab = 'general' | 'runtime' | 'account' | 'telegram' | 'index'

interface TabItem {
  id: SettingsTab
  label: string
  icon: typeof SlidersHorizontal
  desc: string
  adminOnly?: boolean
}

const TABS: TabItem[] = [
  { id: 'general', label: '常规设置', icon: SlidersHorizontal, desc: 'WebDAV 挂载与存储概览' },
  { id: 'runtime', label: '运行参数', icon: SlidersHorizontal, desc: '分片、并发与日志设置', adminOnly: true },
  { id: 'account', label: '账号与安全', icon: KeyRound, desc: '修改密码与用户管理' },
  { id: 'telegram', label: 'Telegram 存储', icon: Send, desc: 'Telegram 账户与存储频道', adminOnly: true },
  { id: 'index', label: '索引与维护', icon: Database, desc: '频道消息扫描与索引重建', adminOnly: true },
]

export function Settings() {
  const { user, status, refreshStatus, signOut } = useApp()
  const [stats, setStats] = useState<Stats | null>(null)
  const [activeTab, setActiveTab] = useState<SettingsTab>('general')

  useEffect(() => {
    void api.stats().then(setStats).catch(() => {})
  }, [])

  const isAdmin = user?.role === 'admin'
  const visibleTabs = TABS.filter((t) => !t.adminOnly || isAdmin)

  return (
    <div className="h-full min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto w-full max-w-4xl px-4 py-6 sm:px-6">
        <header className="mb-6">
          <h1 className="display text-xl">设置</h1>
          <p className="mt-1 text-sm text-[var(--muted)]">
            已登录为 {user?.username}
            {isAdmin ? '（管理员）' : ''}
          </p>
        </header>

        <div className="flex flex-col gap-6 md:flex-row md:items-start">
          <nav className="flex shrink-0 gap-1.5 overflow-x-auto pb-2 md:w-44 md:flex-col md:overflow-visible md:pb-0">
            {visibleTabs.map((tab) => {
              const Icon = tab.icon
              const selected = activeTab === tab.id
              return (
                <button
                  key={tab.id}
                  onClick={() => setActiveTab(tab.id as SettingsTab)}
                  className={clsx(
                    'row shrink-0 !justify-start rounded-lg px-3 py-2 text-left text-sm transition-colors',
                    selected
                      ? 'bg-[var(--sunk)] font-medium text-[var(--ink)]'
                      : 'text-[var(--muted)] hover:bg-[var(--sunk)]/60 hover:text-[var(--ink)]',
                  )}
                >
                  <Icon
                    size={16}
                    className={selected ? 'text-[var(--color-clay)]' : 'text-[var(--faint)]'}
                  />
                  <span>{tab.label}</span>
                </button>
              )
            })}
          </nav>

          <div className="min-w-0 flex-1 space-y-6">
            {activeTab === 'general' && (
              <>
                {status?.webdavPath && (
                  <WebDAVSection path={status.webdavPath} username={user?.username ?? ''} />
                )}
                {stats && (
                  <Section icon={<HardDrive size={16} />} title="存储概览">
                    <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
                      <Stat label="文件" value={String(stats.files)} />
                      <Stat label="文件夹" value={String(stats.dirs)} />
                      <Stat label="分卷" value={String(stats.segments)} />
                      <Stat label="总大小" value={formatBytes(stats.totalBytes)} />
                    </div>
                    {stats.brokenFiles > 0 && (
                      <p className="mt-3 text-xs text-[var(--color-danger)]">
                        有 {stats.brokenFiles} 个文件缺少分卷，无法完整下载。
                      </p>
                    )}
                  </Section>
                )}
                <Section icon={<LogOut size={16} />} title="退出登录" description="退出当前账号在当前浏览器上的登录状态。">
                  <Button icon={<LogOut size={15} />} onClick={() => void signOut()}>
                    退出登录
                  </Button>
                </Section>
              </>
            )}

            {activeTab === 'account' && (
              <>
                <PasswordSection />
                {isAdmin && <UsersSection currentUserId={user?.id ?? ''} />}
              </>
            )}

            {activeTab === 'runtime' && isAdmin && <RuntimeSettingsSection onChanged={refreshStatus} />}

            {activeTab === 'telegram' && isAdmin && (
              <TelegramSection onChanged={refreshStatus} />
            )}

            {activeTab === 'index' && isAdmin && (
              <IndexSection />
            )}
          </div>
        </div>
      </div>
    </div>
  )
}

function Section({
  icon,
  title,
  description,
  children,
}: {
  icon: ReactNode
  title: string
  description?: string
  children: ReactNode
}) {
  return (
    <section className="panel p-5">
      <header className="mb-4 flex items-start gap-2.5">
        <span className="mt-0.5 text-[var(--faint)]">{icon}</span>
        <div>
          <h2 className="display text-base">{title}</h2>
          {description && <p className="mt-0.5 text-xs text-[var(--muted)]">{description}</p>}
        </div>
      </header>
      {children}
    </section>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2.5">
      <div className="text-[11px] text-[var(--faint)]">{label}</div>
      <div className="mt-0.5 text-sm font-medium tabular-nums">{value}</div>
    </div>
  )
}

function WebDAVSection({ path, username }: { path: string; username: string }) {
  const url = `${window.location.origin}${path}/`
  return (
    <Section
      icon={<Send size={16} />}
      title="WebDAV"
      description="用同一套账号密码挂载。分卷文件在这里也是单个文件。"
    >
      <div className="flex items-center gap-2">
        <Input readOnly value={url} className="font-[family-name:var(--font-mono)] text-xs" />
        <Button
          icon={<Copy size={14} />}
          onClick={() => {
            void navigator.clipboard.writeText(url)
            toast('地址已复制', 'success')
          }}
        >
          复制
        </Button>
      </div>
      <p className="mt-2.5 text-xs leading-relaxed text-[var(--muted)]">
        用户名 <span className="font-[family-name:var(--font-mono)]">{username}</span>，密码就是登录密码。
        rclone、Finder、Windows 资源管理器都可以直接挂。
      </p>
    </Section>
  )
}

function PasswordSection() {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async () => {
    if (next.length < 8) return setError('新密码至少 8 位')
    setBusy(true)
    setError(null)
    try {
      await api.changeOwnPassword(current, next)
      toast('密码已修改，其它设备需要重新登录', 'success')
      setCurrent('')
      setNext('')
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={<Users size={16} />}
      title="修改密码"
      description="修改后所有已登录的会话都会失效，WebDAV 也要用新密码。"
    >
      <div className="space-y-3">
        <Field label="当前密码">
          <Input
            type="password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            autoComplete="current-password"
          />
        </Field>
        <Field label="新密码" error={error ?? undefined}>
          <Input
            type="password"
            value={next}
            onChange={(e) => setNext(e.target.value)}
            autoComplete="new-password"
          />
        </Field>
        <Button variant="primary" loading={busy} onClick={() => void submit()}>
          保存
        </Button>
      </div>
    </Section>
  )
}

function RuntimeSettingsSection({ onChanged }: { onChanged: () => Promise<void> }) {
  const [settings, setSettings] = useState<RuntimeSettings | null>(null)
  const [segmentMiB, setSegmentMiB] = useState('')
  const [poolSize, setPoolSize] = useState('')
  const [uploadThreads, setUploadThreads] = useState('')
  const [streamConcurrency, setStreamConcurrency] = useState('')
  const [webdavEnabled, setWebdavEnabled] = useState(true)
  const [logLevel, setLogLevel] = useState('info')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    void api
      .settings()
      .then((value) => {
        setSettings(value)
        setSegmentMiB(String(value.segmentSize / (1024 * 1024)))
        setPoolSize(String(value.poolSize))
        setUploadThreads(String(value.uploadThreads))
        setStreamConcurrency(String(value.streamConcurrency))
        setWebdavEnabled(value.webdavEnabled)
        setLogLevel(value.logLevel)
      })
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
  }, [])

  const save = async () => {
    if (!settings) return

    const segment = Number(segmentMiB)
    const pool = Number(poolSize)
    const uploads = Number(uploadThreads)
    const streams = Number(streamConcurrency)
    if (!Number.isFinite(segment) || segment <= 0 || !Number.isInteger(segment * 2) || segment > 2000) {
      setError('分片大小应为 0.5 到 2000 MiB 之间、以 0.5 MiB 为步长')
      return
    }
    if (![pool, uploads, streams].every((value) => Number.isInteger(value) && value >= 1)) {
      setError('并发参数必须是大于等于 1 的整数')
      return
    }

    setBusy(true)
    setError(null)
    try {
      const saved = await api.updateSettings({
        segmentSize: Math.round(segment * 1024 * 1024),
        poolSize: pool,
        uploadThreads: uploads,
        streamConcurrency: streams,
        webdavEnabled,
        logLevel,
      })
      setSettings(saved)
      setSegmentMiB(String(saved.segmentSize / (1024 * 1024)))
      setPoolSize(String(saved.poolSize))
      setUploadThreads(String(saved.uploadThreads))
      setStreamConcurrency(String(saved.streamConcurrency))
      setWebdavEnabled(saved.webdavEnabled)
      setLogLevel(saved.logLevel)
      await onChanged()
      toast('运行设置已保存并立即生效', 'success')
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={<SlidersHorizontal size={16} />}
      title="运行参数"
      description="调整存储分片、上传下载并发、WebDAV 和日志级别。修改会立即生效；分片大小只影响新上传的文件。"
    >
      {settings === null ? (
        error ? <p className="text-sm text-[var(--color-danger)]">{error}</p> : <Spinner />
      ) : (
        <div className="space-y-4">
          <Field label="分片大小（MiB）" hint="范围 0.5–2000，步长 0.5；已有文件不受影响">
            <Input
              type="number"
              min="0.5"
              max="2000"
              step="0.5"
              value={segmentMiB}
              onChange={(e) => setSegmentMiB(e.target.value)}
            />
          </Field>
          <div className="grid gap-4 sm:grid-cols-3">
            <Field label="Telegram 连接池">
              <Input
                type="number"
                min="1"
                step="1"
                value={poolSize}
                onChange={(e) => setPoolSize(e.target.value)}
              />
            </Field>
            <Field label="上传线程">
              <Input
                type="number"
                min="1"
                step="1"
                value={uploadThreads}
                onChange={(e) => setUploadThreads(e.target.value)}
              />
            </Field>
            <Field label="下载并发块数">
              <Input
                type="number"
                min="1"
                step="1"
                value={streamConcurrency}
                onChange={(e) => setStreamConcurrency(e.target.value)}
              />
            </Field>
          </div>
          <label className="flex cursor-pointer items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={webdavEnabled}
              onChange={(e) => setWebdavEnabled(e.target.checked)}
              className="size-4 accent-[var(--color-clay)]"
            />
            启用 WebDAV
          </label>
          <Field label="日志级别">
            <select className="input" value={logLevel} onChange={(e) => setLogLevel(e.target.value)}>
              <option value="debug">debug</option>
              <option value="info">info</option>
              <option value="warn">warn</option>
              <option value="error">error</option>
            </select>
          </Field>
          {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}
          <Button variant="primary" loading={busy} onClick={() => void save()}>
            保存设置
          </Button>
        </div>
      )}
    </Section>
  )
}

function TelegramSection({ onChanged }: { onChanged: () => Promise<void> }) {
  const { status } = useApp()
  const tg = status?.telegram
  const [showChannel, setShowChannel] = useState(false)

  if (!tg) return null

  return (
    <Section icon={<Send size={16} />} title="Telegram">
      <TelegramCredentialsSection onChanged={onChanged} />
      {tg.state === 'unconfigured' ? (
        <CredentialsStep onDone={onChanged} />
      ) : tg.state === 'ready' ? (
        <>
          <div className="space-y-2 text-sm">
            <Line label="账号" value={tg.firstName || tg.username || String(tg.userId)} />
            {tg.phone && <Line label="手机号" value={tg.phone} />}
            <Line label="数据中心" value={tg.dc ? `DC${tg.dc}` : '—'} />
            <Line label="Premium" value={tg.premium ? '是' : '否'} />
          </div>
          <div className="mt-4 flex flex-wrap gap-2">
            <Button onClick={() => setShowChannel(true)}>更换存储频道</Button>
            <Button
              variant="danger"
              onClick={async () => {
                if (!confirm('退出 Telegram 登录？已上传的文件不会被删除，但在重新登录前无法访问。')) return
                await api.telegramLogout()
                await onChanged()
              }}
            >
              退出 Telegram
            </Button>
          </div>
        </>
      ) : (
        <LoginStep onDone={onChanged} />
      )}

      <TelegramAccountMigration ready={tg.state === 'ready'} onChanged={onChanged} />

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
    </Section>
  )
}

function TelegramCredentialsSection({ onChanged }: { onChanged: () => Promise<void> }) {
  const [settings, setSettings] = useState<RuntimeSettings | null>(null)
  const [appId, setAppId] = useState('')
  const [appHash, setAppHash] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    void api
      .settings()
      .then((value) => {
        setSettings(value)
        setAppId(value.appId ? String(value.appId) : '')
        setAppHash(value.appHash)
      })
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
  }, [])

  const save = async () => {
    const id = Number(appId.trim())
    if (!Number.isInteger(id) || id <= 0 || !appHash.trim()) {
      setError('api_id 和 api_hash 都不能为空')
      return
    }
    setBusy(true)
    setError(null)
    try {
      const saved = await api.updateSettings({ appId: id, appHash: appHash.trim() })
      setSettings(saved)
      setAppId(String(saved.appId))
      setAppHash(saved.appHash)
      await onChanged()
      toast('Telegram API 凭据已保存', 'success')
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="mb-5 border-b border-[var(--line)] pb-5">
      <h3 className="text-sm font-medium">API 凭据</h3>
      <p className="mt-1 text-xs leading-relaxed text-[var(--muted)]">
        来自 my.telegram.org 的 api_id 和 api_hash。修改后会重新连接 Telegram；凭据只保存在本服务器。
      </p>
      {settings === null ? (
        error ? <p className="mt-3 text-xs text-[var(--color-danger)]">{error}</p> : <div className="mt-3"><Spinner /></div>
      ) : (
        <div className="mt-3 space-y-3">
          <Field label="api_id">
            <Input value={appId} onChange={(e) => setAppId(e.target.value)} inputMode="numeric" />
          </Field>
          <Field label="api_hash" error={error ?? undefined}>
            <Input
              value={appHash}
              onChange={(e) => setAppHash(e.target.value)}
              className="font-[family-name:var(--font-mono)] text-xs"
            />
          </Field>
          <Button variant="primary" loading={busy} onClick={() => void save()}>
            保存并连接
          </Button>
        </div>
      )}
    </div>
  )
}

function TelegramAccountMigration({
  ready,
  onChanged,
}: {
  ready: boolean
  onChanged: () => Promise<void>
}) {
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
    <div className="mt-5 border-t border-[var(--line)] pt-4">
      <div className="flex items-start gap-2.5">
        <KeyRound size={15} className="mt-0.5 shrink-0 text-[var(--faint)]" />
        <div className="min-w-0">
          <h3 className="text-sm font-medium">账号迁移</h3>
          <p className="mt-1 text-xs leading-relaxed text-[var(--muted)]">
            导出 Telegram 登录会话和 api_id、api_hash，换服务器时可直接导入，无需重新验证手机号。
            文件包含登录凭据，请勿发送给他人。
          </p>
          <div className="mt-3 flex flex-wrap gap-2">
            <Button
              icon={<Download size={14} />}
              loading={busy}
              disabled={!ready}
              onClick={() => void exportAccount()}
            >
              导出账号
            </Button>
            <Button
              icon={<Upload size={14} />}
              loading={busy}
              onClick={() => fileInput.current?.click()}
            >
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
        </div>
      </div>
    </div>
  )
}

function Line({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-4">
      <span className="text-[var(--muted)]">{label}</span>
      <span className="truncate">{value}</span>
    </div>
  )
}

function UsersSection({ currentUserId }: { currentUserId: string }) {
  const [users, setUsers] = useState<User[] | null>(null)
  const [open, setOpen] = useState(false)

  const load = () => void api.users().then(setUsers).catch(() => {})
  useEffect(load, [])

  return (
    <Section
      icon={<Users size={16} />}
      title="账号"
      description="所有账号共用同一个网盘。管理员可以改 Telegram 设置和重建索引。"
    >
      {users === null ? (
        <Spinner />
      ) : (
        <div className="space-y-1">
          {users.map((u) => (
            <div key={u.id} className="row justify-between">
              <span className="flex min-w-0 items-center gap-2">
                <span className="truncate text-sm">{u.username}</span>
                {u.role === 'admin' && <span className="chip">管理员</span>}
                {u.id === currentUserId && <span className="chip">你</span>}
              </span>
              {u.id !== currentUserId && (
                <Button
                  variant="danger"
                  icon={<Trash2 size={14} />}
                  onClick={async () => {
                    if (!confirm(`删除账号 ${u.username}？`)) return
                    try {
                      await api.deleteUser(u.id)
                      load()
                    } catch (err) {
                      toast(err instanceof Error ? err.message : String(err), 'error')
                    }
                  }}
                />
              )}
            </div>
          ))}
        </div>
      )}

      <Button className="mt-3" icon={<UserPlus size={15} />} onClick={() => setOpen(true)}>
        添加账号
      </Button>

      <NewUserModal open={open} onClose={() => setOpen(false)} onCreated={load} />
    </Section>
  )
}

function NewUserModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: () => void
}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [admin, setAdmin] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (open) {
      setUsername('')
      setPassword('')
      setAdmin(false)
      setError(null)
    }
  }, [open])

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      await api.createUser(username.trim(), password, admin ? 'admin' : 'user')
      onCreated()
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="添加账号"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" loading={busy} onClick={() => void submit()}>
            创建
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <Field label="用户名">
          <Input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
        </Field>
        <Field label="密码" hint="至少 8 位" error={error ?? undefined}>
          <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} />
        </Field>
        <label className="flex cursor-pointer items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={admin}
            onChange={(e) => setAdmin(e.target.checked)}
            className="size-4 accent-[var(--color-clay)]"
          />
          设为管理员
        </label>
      </div>
    </Modal>
  )
}

function IndexSection() {
  const [state, setState] = useState<IndexStatus | null>(null)

  useEffect(() => {
    void api.indexStatus().then(setState).catch(() => {})
    return events.subscribe((event) => {
      if (event.type === 'index') {
        setState((prev) => ({ ...(prev ?? ({} as IndexStatus)), ...(event.data as IndexStatus) }))
      }
    })
  }, [])

  return (
    <Section
      icon={<Database size={16} />}
      title="重建索引"
      description="扫描频道里所有带 #tdrive 标签的消息，从标签还原整棵目录树和分卷关系。本地索引损坏或换机器时用得上。"
    >
      {state?.running ? (
        <div className="space-y-2">
          <p className="text-sm text-[var(--muted)]">
            已扫描 {state.scanned} 条消息，找到 {state.dirs} 个文件夹、{state.files} 个文件
          </p>
          <Progress value={100} className="animate-pulse" />
        </div>
      ) : (
        <>
          {state?.done && !state.error && (
            <p className="mb-3 text-xs text-[var(--muted)]">
              上次重建：{state.dirs} 个文件夹、{state.files} 个文件、{state.segments} 个分卷
              {state.broken > 0 && `，其中 ${state.broken} 个文件缺卷`}
            </p>
          )}
          {state?.error && <p className="mb-3 text-xs text-[var(--color-danger)]">{state.error}</p>}
          <Button
            icon={<RefreshCw size={15} />}
            onClick={async () => {
              if (!confirm('重建会用频道里的内容覆盖当前索引，确定继续？')) return
              try {
                setState(await api.rebuildIndex())
              } catch (err) {
                toast(err instanceof Error ? err.message : String(err), 'error')
              }
            }}
          >
            开始重建
          </Button>
        </>
      )}
    </Section>
  )
}
