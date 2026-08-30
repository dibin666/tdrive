import { useEffect, useState } from 'react'
import clsx from 'clsx'
import {
  Database,
  Gauge,
  HardDrive,
  KeyRound,
  Send,
  SlidersHorizontal,
  Users,
} from 'lucide-react'
import { api, type RuntimeSettings } from '../../lib/api'
import { useApp } from '../../app/context'
import { formatBytes } from '../../lib/format'
import { Button, Meter, Select, Spinner, toast } from '../../components/primitives'
import { Section, Stat } from './shared'
import { TelegramPage } from './TelegramPage'
import { PerformancePage } from './PerformancePage'
import { StoragePage } from './StoragePage'
import { UsersPage } from './UsersPage'
import { SecurityPage } from './SecurityPage'
import { MaintenancePage } from './MaintenancePage'

/**
 * Settings, split by the question each page answers.
 *
 * The previous single page mixed "who can log in", "how fast should uploads
 * go" and "where is the Telegram channel" into one scroll, which made every
 * one of them harder to find. The split is by audience as much as by topic:
 * a non-administrator now has two pages that are entirely about their own
 * account, and never sees the eight tuning sliders.
 */

type Tab = 'account' | 'security' | 'users' | 'telegram' | 'performance' | 'storage' | 'maintenance'

interface TabItem {
  id: Tab
  label: string
  icon: typeof SlidersHorizontal
  adminOnly?: boolean
}

const TABS: TabItem[] = [
  { id: 'account', label: '概览', icon: Gauge },
  { id: 'security', label: '账号与安全', icon: KeyRound },
  { id: 'users', label: '用户管理', icon: Users, adminOnly: true },
  { id: 'telegram', label: 'Telegram', icon: Send, adminOnly: true },
  { id: 'performance', label: '性能参数', icon: SlidersHorizontal, adminOnly: true },
  { id: 'storage', label: '存储与暂存', icon: HardDrive, adminOnly: true },
  { id: 'maintenance', label: '索引与日志', icon: Database, adminOnly: true },
]

export function Settings() {
  const { user, status, refreshStatus } = useApp()
  const [tab, setTab] = useState<Tab>('account')

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
            {visibleTabs.map((item) => {
              const Icon = item.icon
              const selected = tab === item.id
              return (
                <button
                  key={item.id}
                  onClick={() => setTab(item.id)}
                  className={clsx(
                    'row shrink-0 !justify-start rounded-lg px-3 py-2 text-left text-sm transition-colors',
                    selected
                      ? 'bg-[var(--sunk)] font-medium text-[var(--ink)]'
                      : 'text-[var(--muted)] hover:bg-[var(--sunk)]/60 hover:text-[var(--ink)]',
                  )}
                >
                  <Icon size={16} className={selected ? 'text-[var(--color-clay)]' : 'text-[var(--faint)]'} />
                  <span>{item.label}</span>
                </button>
              )
            })}
          </nav>

          <div className="min-w-0 flex-1 space-y-4">
            {tab === 'account' && <OverviewPage />}
            {tab === 'security' && <SecurityPage webdavPath={status?.webdavPath} />}
            {tab === 'users' && isAdmin && <UsersPage currentUserId={user?.id ?? ''} />}
            {tab === 'telegram' && isAdmin && <TelegramPage onChanged={refreshStatus} />}
            {tab === 'performance' && isAdmin && <PerformancePage />}
            {tab === 'storage' && isAdmin && <StoragePage onChanged={refreshStatus} />}
            {tab === 'maintenance' && isAdmin && <MaintenancePage />}
          </div>
        </div>
      </div>
    </div>
  )
}

/** The overview is what a non-administrator sees first: their own usage, what
 *  they are allowed to do, and the state of the drive as a whole. */
function OverviewPage() {
  const { user, status } = useApp()
  const [settings, setSettings] = useState<RuntimeSettings | null>(null)
  const [theme, setTheme] = useState<string>(() => localStorage.getItem('tdrive.theme') ?? 'system')

  const isAdmin = user?.role === 'admin'

  useEffect(() => {
    if (!isAdmin) return
    void api.settings().then(setSettings).catch(() => {})
  }, [isAdmin])

  if (!user) return <Spinner />

  const quotaSet = user.quotaBytes > 0

  return (
    <div className="space-y-4">
      <Section icon={<Gauge size={16} />} title="我的用量">
        <div className="space-y-3">
          {quotaSet ? (
            <Meter
              value={user.usedBytes}
              max={user.quotaBytes}
              label="已用配额"
              caption={`${formatBytes(user.usedBytes)} / ${formatBytes(user.quotaBytes)}`}
            />
          ) : (
            <div className="grid grid-cols-2 gap-3">
              <Stat label="已上传" value={formatBytes(user.usedBytes)} />
              <Stat label="文件数" value={String(user.fileCount)} hint="配额不限" />
            </div>
          )}

          {user.scopePath && (
            <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs text-[var(--muted)]">
              这个账号被限定在{' '}
              <span className="font-[family-name:var(--font-mono)] text-[var(--ink)]">{user.scopePath}</span>{' '}
              目录内，在文件页看到的根目录就是它。
            </p>
          )}
        </div>
      </Section>

      <Section
        icon={<KeyRound size={16} />}
        title="我的权限"
        description={
          user.permsInherited ? '当前跟随角色的默认权限。' : '管理员为这个账号单独配置过权限。'
        }
      >
        <div className="flex flex-wrap gap-1.5">
          {user.perms.map((perm) => (
            <span key={perm} className="chip">
              {PERM_TEXT[perm] ?? perm}
            </span>
          ))}
        </div>
      </Section>

      <Section icon={<HardDrive size={16} />} title="外观">
        <Select
          value={theme}
          onChange={(e) => {
            const next = e.target.value
            setTheme(next)
            const dark =
              next === 'dark' ||
              (next === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches)
            document.documentElement.classList.toggle('dark', dark)
            try {
              localStorage.setItem('tdrive.theme', next)
            } catch {
              /* private browsing */
            }
          }}
        >
          <option value="system">跟随系统</option>
          <option value="light">浅色</option>
          <option value="dark">深色</option>
        </Select>
      </Section>

      {isAdmin && settings && (
        <Section
          icon={<SlidersHorizontal size={16} />}
          title="当前运行参数"
          description="完整调整在「性能参数」和「存储与暂存」里。"
        >
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <Stat label="存储分片" value={formatBytes(settings.segmentSize)} />
            <Stat label="上传分片" value={`${settings.uploadPartSize / 1024} KiB`} />
            <Stat label="同时上传" value={`${settings.uploadConcurrency} 个`} />
            <Stat label="同时下载" value={`${settings.downloadConcurrency} 个`} />
          </div>
          <p className="mt-3 text-xs text-[var(--muted)]">
            版本 {status?.version ?? '—'} · WebDAV{' '}
            {settings.webdavEnabled ? `已启用（${status?.webdavPath ?? '/dav'}）` : '已关闭'}
          </p>
        </Section>
      )}

      {isAdmin && !settings && (
        <Button
          onClick={() =>
            void api
              .settings()
              .then(setSettings)
              .catch((err) => toast(err instanceof Error ? err.message : String(err), 'error'))
          }
        >
          加载运行参数
        </Button>
      )}
    </div>
  )
}

const PERM_TEXT: Record<string, string> = {
  read: '浏览',
  download: '下载',
  upload: '上传',
  uploadLocal: 'VPS 本地上传',
  remoteFetch: '离线下载',
  mkdir: '新建文件夹',
  rename: '重命名',
  move: '移动',
  delete: '删除',
  webdav: 'WebDAV',
  stage: '服务器暂存',
  share: '生成直链',
}
