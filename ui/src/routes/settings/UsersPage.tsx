import { useCallback, useEffect, useMemo, useState } from 'react'
import clsx from 'clsx'
import {
  Ban,
  Check,
  FolderTree,
  KeyRound,
  LogOut,
  MoreHorizontal,
  Search,
  ShieldCheck,
  Trash2,
  UserPlus,
  Users,
} from 'lucide-react'
import { api, type Perm, type Session, type User } from '../../lib/api'
import { formatBytes, formatDate, formatDateTime } from '../../lib/format'
import {
  Button,
  Chip,
  Drawer,
  Field,
  IconButton,
  Input,
  Meter,
  Modal,
  Segmented,
  Select,
  Spinner,
  Switch,
  toast,
} from '../../components/primitives'
import { ContextMenu, useContextMenu, type MenuItem } from '../../components/ContextMenu'
import { Section } from './shared'

/**
 * The account console.
 *
 * The old version was a list of usernames with a delete button, which is fine
 * for one household and useless for anything else. What an operator actually
 * needs to see at a glance is who is using space, who can do what, and who is
 * currently signed in — so those are the columns, and everything else lives in
 * a per-account drawer.
 */

const PERM_LABELS: Record<Perm, { label: string; hint: string; group: string }> = {
  read: { label: '浏览', hint: '查看目录和文件列表', group: '基础' },
  download: { label: '下载', hint: '读取文件内容、预览、生成媒体链接', group: '基础' },
  upload: { label: '上传', hint: '从浏览器上传文件', group: '写入' },
  mkdir: { label: '新建文件夹', hint: '', group: '写入' },
  rename: { label: '重命名', hint: '包含批量重命名', group: '写入' },
  move: { label: '移动', hint: '', group: '写入' },
  delete: { label: '删除', hint: '会同时删除 Telegram 上的消息', group: '写入' },
  webdav: { label: 'WebDAV', hint: '允许用这套账号密码挂载网络驱动器', group: '接入方式' },
  share: { label: '生成直链', hint: '创建可复用的下载链接', group: '接入方式' },
  uploadLocal: { label: 'VPS 本地上传', hint: '可读取服务器上挂载的目录', group: '服务器资源' },
  remoteFetch: { label: '离线下载', hint: '让服务器代为抓取外部 URL', group: '服务器资源' },
  stage: { label: '服务器暂存', hint: '下载前先在服务器磁盘上拼装文件', group: '服务器资源' },
}

const PERM_GROUPS = ['基础', '写入', '接入方式', '服务器资源']

export function UsersPage({ currentUserId }: { currentUserId: string }) {
  const [users, setUsers] = useState<User[] | null>(null)
  const [catalog, setCatalog] = useState<Perm[]>([])
  const [search, setSearch] = useState('')
  const [roleFilter, setRoleFilter] = useState<'all' | 'admin' | 'user'>('all')
  const [statusFilter, setStatusFilter] = useState<'all' | 'enabled' | 'disabled'>('all')
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const { menu, openMenu, closeMenu } = useContextMenu()

  const load = useCallback(() => {
    void api.users().then(setUsers).catch(() => {})
  }, [])

  useEffect(() => {
    load()
    void api
      .permissionCatalog()
      .then((c) => setCatalog(c.all))
      .catch(() => setCatalog(Object.keys(PERM_LABELS) as Perm[]))
  }, [load])

  const visible = useMemo(() => {
    if (!users) return []
    const query = search.trim().toLowerCase()
    return users.filter((user) => {
      if (roleFilter !== 'all' && user.role !== roleFilter) return false
      if (statusFilter === 'enabled' && !user.enabled) return false
      if (statusFilter === 'disabled' && user.enabled) return false
      if (query && !`${user.username} ${user.note}`.toLowerCase().includes(query)) return false
      return true
    })
  }, [roleFilter, search, statusFilter, users])

  const setEnabled = async (user: User, enabled: boolean) => {
    try {
      await api.updateUser(user.id, { enabled })
      toast(enabled ? `已启用 ${user.username}` : `已停用 ${user.username}`, 'success')
      load()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  const remove = async (user: User) => {
    if (!confirm(`删除账号 ${user.username}？该账号上传的文件不会被删除，但会失去归属。`)) return
    try {
      await api.deleteUser(user.id)
      toast(`已删除 ${user.username}`, 'success')
      load()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  const revokeAll = async (user: User) => {
    if (!confirm(`注销 ${user.username} 的全部登录会话？`)) return
    try {
      await api.revokeUserSessions(user.id)
      toast('已注销该账号的全部会话', 'success')
      load()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  const rowMenu = (user: User): MenuItem[] => [
    { id: 'edit', label: '编辑账号', icon: <ShieldCheck size={14} />, onSelect: () => setEditing(user) },
    {
      id: 'toggle',
      label: user.enabled ? '停用账号' : '启用账号',
      icon: <Ban size={14} />,
      onSelect: () => void setEnabled(user, !user.enabled),
      hidden: user.id === currentUserId,
    },
    {
      id: 'role',
      label: user.role === 'admin' ? '降级为普通用户' : '提升为管理员',
      icon: <ShieldCheck size={14} />,
      onSelect: async () => {
        try {
          await api.setUserRole(user.id, user.role === 'admin' ? 'user' : 'admin')
          load()
        } catch (err) {
          toast(err instanceof Error ? err.message : String(err), 'error')
        }
      },
      hidden: user.id === currentUserId,
    },
    {
      id: 'sessions',
      label: `注销全部会话（${user.sessions}）`,
      icon: <LogOut size={14} />,
      separated: true,
      onSelect: () => void revokeAll(user),
      disabled: user.sessions === 0,
    },
    {
      id: 'delete',
      label: '删除账号',
      icon: <Trash2 size={14} />,
      danger: true,
      separated: true,
      onSelect: () => void remove(user),
      hidden: user.id === currentUserId,
    },
  ]

  return (
    <>
      <Section
        icon={<Users size={16} />}
        title="账号"
        description="所有账号共用同一个网盘。权限决定一个账号能做什么，目录范围决定它能看到哪一部分。"
        actions={
          <Button icon={<UserPlus size={15} />} onClick={() => setCreating(true)}>
            添加账号
          </Button>
        }
      >
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <div className="relative min-w-40 flex-1 sm:max-w-64 sm:flex-none">
            <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--faint)]" />
            <input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="搜索用户名或备注"
              className="input !py-1.5 !pl-7 text-xs"
            />
          </div>
          <Segmented
            value={roleFilter}
            onChange={setRoleFilter}
            options={[
              { value: 'all', label: '全部角色' },
              { value: 'admin', label: '管理员' },
              { value: 'user', label: '普通' },
            ]}
          />
          <Segmented
            value={statusFilter}
            onChange={setStatusFilter}
            options={[
              { value: 'all', label: '全部状态' },
              { value: 'enabled', label: '启用' },
              { value: 'disabled', label: '停用' },
            ]}
          />
        </div>

        {users === null ? (
          <Spinner />
        ) : visible.length === 0 ? (
          <p className="py-8 text-center text-xs text-[var(--muted)]">没有符合条件的账号</p>
        ) : (
          <div className="divide-y divide-[var(--line)]">
            {visible.map((user) => (
              <UserRow
                key={user.id}
                user={user}
                isSelf={user.id === currentUserId}
                onEdit={() => setEditing(user)}
                onMenu={(position) => openMenu(position, rowMenu(user), user.username)}
              />
            ))}
          </div>
        )}
      </Section>

      <ContextMenu state={menu} onClose={closeMenu} />

      <NewUserModal
        open={creating}
        catalog={catalog}
        onClose={() => setCreating(false)}
        onCreated={load}
      />
      <UserDrawer
        user={editing}
        catalog={catalog}
        isSelf={editing?.id === currentUserId}
        onClose={() => setEditing(null)}
        onSaved={load}
      />
    </>
  )
}

function UserRow({
  user,
  isSelf,
  onEdit,
  onMenu,
}: {
  user: User
  isSelf: boolean
  onEdit: () => void
  onMenu: (position: { x: number; y: number }) => void
}) {
  const quotaSet = user.quotaBytes > 0

  return (
    <div
      className="group flex items-center gap-3 py-2.5"
      onContextMenu={(e) => {
        e.preventDefault()
        onMenu({ x: e.clientX, y: e.clientY })
      }}
    >
      <span
        className={clsx(
          'flex size-8 shrink-0 items-center justify-center rounded-full text-sm font-medium',
          user.enabled ? 'bg-[var(--clay-soft)] text-[var(--color-clay)]' : 'bg-[var(--sunk)] text-[var(--faint)]',
        )}
      >
        {user.username.slice(0, 1).toUpperCase()}
      </span>

      <button onClick={onEdit} className="min-w-0 flex-1 text-left">
        <span className="flex flex-wrap items-center gap-1.5">
          <span className={clsx('truncate text-sm', !user.enabled && 'text-[var(--faint)] line-through')}>
            {user.username}
          </span>
          {user.role === 'admin' && <span className="chip">管理员</span>}
          {isSelf && <span className="chip">你</span>}
          {!user.enabled && (
            <span className="chip !border-transparent !bg-[var(--danger-soft)] !text-[var(--color-danger)]">
              已停用
            </span>
          )}
          {user.scopePath && (
            <span className="chip" title={`只能访问 ${user.scopePath}`}>
              <FolderTree size={10} />
              {user.scopePath}
            </span>
          )}
        </span>
        <span className="mt-0.5 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-[11px] text-[var(--muted)]">
          {user.note && <span className="truncate">{user.note}</span>}
          <span title={user.lastLoginAt ? formatDateTime(user.lastLoginAt) : undefined}>
            {user.lastLoginAt ? `最后登录 ${formatDate(user.lastLoginAt)}` : '从未登录'}
            {user.lastLoginIp ? ` · ${user.lastLoginIp}` : ''}
          </span>
          {user.sessions > 0 && <span className="text-[var(--color-success)]">{user.sessions} 个会话在线</span>}
        </span>
      </button>

      <div className="hidden w-40 shrink-0 sm:block">
        {quotaSet ? (
          <Meter
            value={user.usedBytes}
            max={user.quotaBytes}
            caption={`${formatBytes(user.usedBytes)} / ${formatBytes(user.quotaBytes)}`}
          />
        ) : (
          <p className="text-right text-xs tabular-nums text-[var(--muted)]">
            {formatBytes(user.usedBytes)}
            <span className="ml-1 text-[var(--faint)]">不限</span>
          </p>
        )}
      </div>

      <IconButton
        label="更多操作"
        className="opacity-0 group-hover:opacity-100"
        onClick={(e) => onMenu({ x: e.currentTarget.getBoundingClientRect().left, y: e.currentTarget.getBoundingClientRect().bottom })}
      >
        <MoreHorizontal size={16} />
      </IconButton>
    </div>
  )
}

function PermissionGrid({
  value,
  catalog,
  onChange,
  disabled,
}: {
  value: Set<Perm>
  catalog: Perm[]
  onChange: (next: Set<Perm>) => void
  disabled?: boolean
}) {
  const toggle = (perm: Perm) => {
    const next = new Set(value)
    if (next.has(perm)) next.delete(perm)
    else next.add(perm)
    onChange(next)
  }

  return (
    <div className="space-y-4">
      {PERM_GROUPS.map((group) => {
        const perms = catalog.filter((perm) => PERM_LABELS[perm]?.group === group)
        if (perms.length === 0) return null
        return (
          <div key={group}>
            <h4 className="mb-2 text-[11px] font-medium uppercase tracking-wide text-[var(--faint)]">
              {group}
            </h4>
            <div className="space-y-2.5">
              {perms.map((perm) => (
                <Switch
                  key={perm}
                  checked={value.has(perm)}
                  disabled={disabled}
                  onChange={() => toggle(perm)}
                  label={PERM_LABELS[perm].label}
                  hint={PERM_LABELS[perm].hint || undefined}
                />
              ))}
            </div>
          </div>
        )
      })}
    </div>
  )
}

function UserDrawer({
  user,
  catalog,
  isSelf,
  onClose,
  onSaved,
}: {
  user: User | null
  catalog: Perm[]
  isSelf?: boolean
  onClose: () => void
  onSaved: () => void
}) {
  const [perms, setPerms] = useState<Set<Perm>>(new Set())
  const [inherit, setInherit] = useState(true)
  const [scope, setScope] = useState('')
  const [quotaGiB, setQuotaGiB] = useState('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [sessions, setSessions] = useState<Session[] | null>(null)
  const [password, setPassword] = useState('')

  useEffect(() => {
    if (!user) return
    setPerms(new Set(user.perms))
    setInherit(user.permsInherited)
    setScope(user.scopePath)
    setQuotaGiB(user.quotaBytes > 0 ? String(user.quotaBytes / (1024 * 1024 * 1024)) : '')
    setNote(user.note)
    setPassword('')
    setSessions(null)
    void api.userSessions(user.id).then(setSessions).catch(() => setSessions([]))
  }, [user])

  if (!user) return null

  const isAdmin = user.role === 'admin'

  const save = async () => {
    setBusy(true)
    try {
      const quota = quotaGiB.trim() === '' ? 0 : Math.round(Number(quotaGiB) * 1024 * 1024 * 1024)
      if (!Number.isFinite(quota) || quota < 0) {
        toast('配额必须是非负数字', 'error')
        return
      }
      await api.updateUser(user.id, {
        // Sending the role's default set is how "inherit" is expressed: the
        // server stores zero for it, and the account tracks the role again.
        perms: inherit ? [] : [...perms],
        scopePath: scope.trim(),
        quotaBytes: quota,
        note: note.trim(),
      })
      toast('账号已更新', 'success')
      onSaved()
      onClose()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    } finally {
      setBusy(false)
    }
  }

  const resetPassword = async () => {
    if (password.length < 8) {
      toast('新密码至少 8 位', 'error')
      return
    }
    try {
      await api.setUserPassword(user.id, password)
      setPassword('')
      toast('密码已重置，该账号的所有会话已注销', 'success')
      onSaved()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  return (
    <Drawer
      open
      onClose={onClose}
      title={user.username}
      description={`${isAdmin ? '管理员' : '普通用户'} · 已用 ${formatBytes(user.usedBytes)} · ${user.fileCount} 个文件`}
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" loading={busy} onClick={() => void save()}>
            保存
          </Button>
        </>
      }
    >
      <div className="space-y-6">
        <section>
          <h3 className="mb-2 text-sm font-medium">权限</h3>
          {isAdmin ? (
            <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs leading-relaxed text-[var(--muted)]">
              管理员始终拥有全部权限，无法逐项关闭。要限制这个账号，请先把它降级为普通用户。
            </p>
          ) : (
            <>
              <Switch
                checked={inherit}
                onChange={setInherit}
                label="跟随角色默认权限"
                hint="关闭后可以逐项控制这个账号能做什么"
              />
              {!inherit && (
                <div className="mt-4 border-t border-[var(--line)] pt-4">
                  <PermissionGrid value={perms} catalog={catalog} onChange={setPerms} />
                </div>
              )}
            </>
          )}
        </section>

        <section className="border-t border-[var(--line)] pt-5">
          <h3 className="mb-2 text-sm font-medium">目录范围</h3>
          <Field
            label="限定目录"
            hint="留空表示可以访问整个网盘。填写后这个账号只能看到该目录，并把它当作自己的根目录。"
          >
            <Input
              value={scope}
              onChange={(e) => setScope(e.target.value)}
              placeholder="/users/alice"
              disabled={isAdmin}
              className="font-[family-name:var(--font-mono)] text-xs"
            />
          </Field>
          {isAdmin && (
            <p className="mt-1.5 text-xs text-[var(--faint)]">管理员不受目录范围限制。</p>
          )}
        </section>

        <section className="border-t border-[var(--line)] pt-5">
          <h3 className="mb-2 text-sm font-medium">存储配额</h3>
          <Field label="配额（GiB）" hint="留空或填 0 表示不限制">
            <Input
              type="number"
              min="0"
              step="0.5"
              value={quotaGiB}
              onChange={(e) => setQuotaGiB(e.target.value)}
              placeholder="不限"
            />
          </Field>
          {user.quotaBytes > 0 && (
            <div className="mt-3">
              <Meter
                value={user.usedBytes}
                max={user.quotaBytes}
                label="当前用量"
                caption={`${formatBytes(user.usedBytes)} / ${formatBytes(user.quotaBytes)}`}
              />
            </div>
          )}
          <p className="mt-2 text-xs leading-relaxed text-[var(--faint)]">
            配额按这个账号上传的文件累计。归属信息会写进 Telegram 消息标签，重建索引后依然准确。
          </p>
        </section>

        <section className="border-t border-[var(--line)] pt-5">
          <Field label="备注" hint="只有管理员看得到">
            <Input value={note} onChange={(e) => setNote(e.target.value)} placeholder="例如：家里的电视盒子" />
          </Field>
        </section>

        <section className="border-t border-[var(--line)] pt-5">
          <h3 className="mb-2 flex items-center gap-1.5 text-sm font-medium">
            <LogOut size={14} className="text-[var(--faint)]" />
            登录会话
          </h3>
          {sessions === null ? (
            <Spinner />
          ) : sessions.length === 0 ? (
            <p className="text-xs text-[var(--muted)]">当前没有登录中的会话。</p>
          ) : (
            <div className="space-y-1.5">
              {sessions.map((session) => (
                <div
                  key={session.id}
                  className="flex items-center gap-2 rounded-[var(--radius-control)] bg-[var(--sunk)] px-2.5 py-2 text-xs"
                >
                  <div className="min-w-0 flex-1">
                    <p className="truncate">{describeAgent(session.userAgent)}</p>
                    <p className="truncate text-[11px] text-[var(--faint)]">
                      {session.ip || '未知地址'} · 最后活跃 {formatDate(Date.parse(session.lastUsedAt))}
                    </p>
                  </div>
                  <IconButton
                    label="注销这个会话"
                    onClick={async () => {
                      await api.revokeUserSession(user.id, session.id)
                      setSessions((prev) => prev?.filter((s) => s.id !== session.id) ?? null)
                      onSaved()
                    }}
                  >
                    <Ban size={14} />
                  </IconButton>
                </div>
              ))}
            </div>
          )}
        </section>

        <section className="border-t border-[var(--line)] pt-5">
          <h3 className="mb-2 flex items-center gap-1.5 text-sm font-medium">
            <KeyRound size={14} className="text-[var(--faint)]" />
            重置密码
          </h3>
          <div className="flex gap-2">
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="至少 8 位"
              autoComplete="new-password"
            />
            <Button className="shrink-0" disabled={password.length < 8} onClick={() => void resetPassword()}>
              重置
            </Button>
          </div>
          <p className="mt-1.5 text-xs text-[var(--faint)]">
            重置后这个账号的全部会话都会失效，WebDAV 也要用新密码。
          </p>
        </section>

        {isSelf && (
          <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs text-[var(--muted)]">
            这是你自己的账号，无法在这里停用或删除。
          </p>
        )}
      </div>
    </Drawer>
  )
}

function NewUserModal({
  open,
  catalog,
  onClose,
  onCreated,
}: {
  open: boolean
  catalog: Perm[]
  onClose: () => void
  onCreated: () => void
}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState<'user' | 'admin'>('user')
  const [scope, setScope] = useState('')
  const [quotaGiB, setQuotaGiB] = useState('')
  const [note, setNote] = useState('')
  const [customPerms, setCustomPerms] = useState(false)
  const [perms, setPerms] = useState<Set<Perm>>(new Set())
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!open) return
    setUsername('')
    setPassword('')
    setRole('user')
    setScope('')
    setQuotaGiB('')
    setNote('')
    setCustomPerms(false)
    setError(null)
    void api
      .permissionCatalog()
      .then((c) => setPerms(new Set(c.userDefault)))
      .catch(() => setPerms(new Set()))
  }, [open])

  const submit = async () => {
    setBusy(true)
    setError(null)
    try {
      const quota = quotaGiB.trim() === '' ? 0 : Math.round(Number(quotaGiB) * 1024 * 1024 * 1024)
      await api.createUser({
        username: username.trim(),
        password,
        role,
        perms: customPerms ? [...perms] : undefined,
        scopePath: scope.trim() || undefined,
        quotaBytes: quota || undefined,
        note: note.trim() || undefined,
      })
      onCreated()
      onClose()
      toast(`已创建账号 ${username.trim()}`, 'success')
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
      width="max-w-lg"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button
            variant="primary"
            loading={busy}
            disabled={!username.trim() || password.length < 8}
            onClick={() => void submit()}
          >
            创建
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="用户名">
            <Input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
          </Field>
          <Field label="密码" hint="至少 8 位">
            <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} />
          </Field>
        </div>

        <Field label="角色" hint="管理员可以修改设置、管理账号和重建索引">
          <Select value={role} onChange={(e) => setRole(e.target.value as 'user' | 'admin')}>
            <option value="user">普通用户</option>
            <option value="admin">管理员</option>
          </Select>
        </Field>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="限定目录" hint="留空表示整个网盘">
            <Input
              value={scope}
              onChange={(e) => setScope(e.target.value)}
              placeholder="/users/alice"
              disabled={role === 'admin'}
              className="font-[family-name:var(--font-mono)] text-xs"
            />
          </Field>
          <Field label="配额（GiB）" hint="留空表示不限">
            <Input
              type="number"
              min="0"
              step="0.5"
              value={quotaGiB}
              onChange={(e) => setQuotaGiB(e.target.value)}
              placeholder="不限"
            />
          </Field>
        </div>

        <Field label="备注">
          <Input value={note} onChange={(e) => setNote(e.target.value)} placeholder="可选" />
        </Field>

        {role === 'user' && (
          <div className="border-t border-[var(--line)] pt-4">
            <Switch
              checked={customPerms}
              onChange={setCustomPerms}
              label="自定义权限"
              hint="默认给予浏览、下载、上传、改名、移动、删除和 WebDAV"
            />
            {customPerms && (
              <div className="mt-4">
                <PermissionGrid value={perms} catalog={catalog} onChange={setPerms} />
              </div>
            )}
          </div>
        )}

        {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}
      </div>
    </Modal>
  )
}

/** describeAgent turns a user agent into something a person recognises. It is
 *  a heuristic on an unreliable string, so it always falls back to showing
 *  the raw value rather than claiming something wrong. */
export function describeAgent(agent: string): string {
  if (!agent) return '未知设备'
  const browser =
    /Edg\//.test(agent) ? 'Edge'
    : /OPR\//.test(agent) ? 'Opera'
    : /Firefox\//.test(agent) ? 'Firefox'
    : /Chrome\//.test(agent) ? 'Chrome'
    : /Safari\//.test(agent) ? 'Safari'
    : /curl\//i.test(agent) ? 'curl'
    : /rclone/i.test(agent) ? 'rclone'
    : null

  const platform =
    /Android/.test(agent) ? 'Android'
    : /iPhone|iPad|iOS/.test(agent) ? 'iOS'
    : /Mac OS X|Macintosh/.test(agent) ? 'macOS'
    : /Windows/.test(agent) ? 'Windows'
    : /Linux/.test(agent) ? 'Linux'
    : null

  if (browser && platform) return `${browser} · ${platform}`
  if (browser) return browser
  return agent.length > 48 ? `${agent.slice(0, 48)}…` : agent
}

/** Reused by the security page for the current user's own session list. */
export function SessionList({
  sessions,
  onRevoke,
}: {
  sessions: Session[]
  onRevoke: (id: string) => void
}) {
  if (sessions.length === 0) {
    return <p className="text-xs text-[var(--muted)]">当前没有其它登录中的会话。</p>
  }
  return (
    <div className="space-y-1.5">
      {sessions.map((session) => (
        <div
          key={session.id}
          className="flex items-center gap-2 rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs"
        >
          <div className="min-w-0 flex-1">
            <p className="flex items-center gap-2 truncate">
              {describeAgent(session.userAgent)}
              {session.current && (
                <span className="chip !border-transparent !bg-[var(--clay-soft)] !text-[var(--color-clay)]">
                  <Check size={9} />
                  当前设备
                </span>
              )}
            </p>
            <p className="truncate text-[11px] text-[var(--faint)]">
              {session.ip || '未知地址'} · 登录于 {formatDate(Date.parse(session.createdAt))} · 最后活跃{' '}
              {formatDate(Date.parse(session.lastUsedAt))}
            </p>
          </div>
          <IconButton label="注销这个会话" disabled={session.current} onClick={() => onRevoke(session.id)}>
            <Ban size={14} />
          </IconButton>
        </div>
      ))}
    </div>
  )
}

/** Chip is re-exported so the audit page can use the same filter pills. */
export { Chip }
