import { useCallback, useEffect, useState } from 'react'
import { Copy, KeyRound, Link2, LogOut, Monitor, Trash2 } from 'lucide-react'
import { api, type ShareRecord, type Session } from '../../lib/api'
import { useApp } from '../../app/context'
import { COPY_FAILED, copyText } from '../../lib/clipboard'
import { formatDate } from '../../lib/format'
import { Button, Field, IconButton, Input, Spinner, toast } from '../../components/primitives'
import { Section } from './shared'
import { SessionList } from './UsersPage'

/**
 * The account's own security page: change the password, see where it is signed
 * in, and revoke the download links it has handed out. All three are things a
 * non-administrator needs and none of them were reachable before.
 */
export function SecurityPage({ webdavPath }: { webdavPath?: string }) {
  const { user, signOut } = useApp()
  const [sessions, setSessions] = useState<Session[] | null>(null)
  const [shares, setShares] = useState<ShareRecord[] | null>(null)

  const loadSessions = useCallback(() => {
    void api.mySessions().then(setSessions).catch(() => setSessions([]))
  }, [])
  const loadShares = useCallback(() => {
    void api.shares().then(setShares).catch(() => setShares([]))
  }, [])

  useEffect(() => {
    loadSessions()
    loadShares()
  }, [loadSessions, loadShares])

  return (
    <div className="space-y-4">
      <PasswordSection />

      {webdavPath && <WebDAVSection path={webdavPath} username={user?.username ?? ''} />}

      <Section
        icon={<Monitor size={16} />}
        title="登录会话"
        description="当前账号的有效会话。注销后对应设备需重新登录。"
      >
        {sessions === null ? (
          <Spinner />
        ) : (
          <SessionList
            sessions={sessions}
            onRevoke={async (id) => {
              await api.revokeMySession(id)
              loadSessions()
              toast('会话已注销', 'success')
            }}
          />
        )}
      </Section>

      <Section
        icon={<Link2 size={16} />}
        title="公开直链"
        description="已生成的独立访问链接。撤销后立即失效并中断传输。"
      >
        {shares === null ? (
          <Spinner />
        ) : shares.length === 0 ? (
          <p className="text-xs text-[var(--muted)]">尚未生成任何下载直链。</p>
        ) : (
          <div className="space-y-1.5">
            {shares.map((share) => (
              <div
                key={share.id}
                className="flex items-center gap-2 rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs"
              >
                <div className="min-w-0 flex-1">
                  <p className="truncate">
                    {share.kind === 'segment' ? `分卷 ${share.label}` : '完整文件'}
                    <span className="ml-2 font-[family-name:var(--font-mono)] text-[var(--faint)]">
                      {share.fileId.slice(0, 8)}…
                    </span>
                  </p>
                  <p className="text-[11px] text-[var(--faint)]">
                    创建于 {formatDate(Date.parse(share.createdAt))} · 已使用 {share.hits} 次
                  </p>
                </div>
                <IconButton
                  label="撤销直链"
                  onClick={async () => {
                    await api.revokeShare(share.id)
                    loadShares()
                    toast('直链已撤销', 'success')
                  }}
                >
                  <Trash2 size={14} />
                </IconButton>
              </div>
            ))}
          </div>
        )}
      </Section>

      <Section
        icon={<LogOut size={16} />}
        title="退出登录"
        description="仅退出当前浏览器会话，其它设备不受影响。"
      >
        <Button icon={<LogOut size={15} />} onClick={() => void signOut()}>
          退出登录
        </Button>
      </Section>
    </div>
  )
}

function PasswordSection() {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async () => {
    if (next.length < 8) return setError('新密码至少 8 位字符')
    if (next !== confirm) return setError('两次输入的新密码不一致')
    setBusy(true)
    setError(null)
    try {
      await api.changeOwnPassword(current, next)
      toast('密码已修改，所有设备需重新登录', 'success')
      setCurrent('')
      setNext('')
      setConfirm('')
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      icon={<KeyRound size={16} />}
      title="修改密码"
      description="修改密码将使当前所有有效会话失效，WebDAV 亦须同步更新。"
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
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="新密码" hint="至少 8 位字符">
            <Input
              type="password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
              autoComplete="new-password"
            />
          </Field>
          <Field label="确认新密码" error={error ?? undefined}>
            <Input
              type="password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
            />
          </Field>
        </div>
        <Button variant="primary" loading={busy} onClick={() => void submit()}>
          更新密码
        </Button>
      </div>
    </Section>
  )
}

function WebDAVSection({ path, username }: { path: string; username: string }) {
  const url = `${window.location.origin}${path}/`
  return (
    <Section
      icon={<Link2 size={16} />}
      title="WebDAV"
      description="使用网盘账号凭据挂载。分卷文件透明合并，并发限制全局生效。"
    >
      <div className="flex items-center gap-2">
        <Input readOnly value={url} className="font-[family-name:var(--font-mono)] text-xs" />
        <Button
          icon={<Copy size={14} />}
          onClick={() => {
            void copyText(url).then((ok) => toast(ok ? 'WebDAV 地址已复制' : COPY_FAILED, ok ? 'success' : 'error'))
          }}
        >
          复制
        </Button>
      </div>
      <p className="mt-2.5 text-xs leading-relaxed text-[var(--muted)]">
        用户名 <span className="font-[family-name:var(--font-mono)]">{username}</span>，密码与登录密码一致。
        支持 rclone、Finder 及 Windows 资源管理器挂载。账号须具备 WebDAV 权限。
      </p>
    </Section>
  )
}
