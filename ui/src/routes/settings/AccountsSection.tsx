import { useCallback, useEffect, useState } from 'react'
import clsx from 'clsx'
import { Plus, Radio, RefreshCw, Trash2, UserPlus } from 'lucide-react'
import { api, type TelegramAccount } from '../../lib/api'
import { Button, Field, Input, Modal, Spinner, toast } from '../../components/primitives'
import { Section, StatusDot } from './shared'

/**
 * Several Telegram accounts behind one drive.
 *
 * The thing this page has to get across, because getting it wrong wastes
 * someone's afternoon, is that a second account means a second *phone number*.
 * Telegram meters FLOOD_WAIT and transfer quota per account, so registering a
 * second api_id against the same number buys nothing at all — the two
 * authorizations share one budget.
 *
 * The second thing is that an account is not useful until it is both signed in
 * and admitted to the storage channel, so every card says plainly which of
 * those it is still missing.
 */

export function AccountsSection({
  hasChannel,
  onChanged,
}: {
  hasChannel: boolean
  onChanged: () => Promise<void>
}) {
  const [accounts, setAccounts] = useState<TelegramAccount[] | null>(null)
  const [adding, setAdding] = useState(false)
  const [busyId, setBusyId] = useState<string | null>(null)

  const reload = useCallback(async () => {
    try {
      const res = await api.telegramAccounts()
      setAccounts(res.accounts)
    } catch {
      setAccounts([])
    }
  }, [])

  useEffect(() => {
    void reload()
    // Cooldowns tick down and logins complete out of band, so the list refreshes
    // on its own rather than making someone hunt for a reload button.
    const timer = window.setInterval(() => void reload(), 5000)
    return () => window.clearInterval(timer)
  }, [reload])

  const act = async (id: string, label: string, fn: () => Promise<unknown>) => {
    setBusyId(id)
    try {
      await fn()
      await reload()
      await onChanged()
      toast(label, 'success')
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    } finally {
      setBusyId(null)
    }
  }

  const usable = (accounts ?? []).filter((a) => a.enabled && a.canPost).length

  return (
    <Section
      icon={<UserPlus size={16} />}
      title="Telegram 账号"
      description={
        usable > 1
          ? `${usable} 个账号在分担压力。上传和下载队列的额度是「每个账号」的，一个账号被限流时新任务自动走其它账号。`
          : '再加一个账号可以让上传和下载的额度翻倍。注意：必须是另一个手机号——Telegram 的限流是按账号算的，同一个号申请多个 api_id 没有任何作用。'
      }
      actions={
        <Button icon={<Plus size={14} />} onClick={() => setAdding(true)}>
          添加账号
        </Button>
      }
    >
      {accounts === null ? (
        <Spinner />
      ) : (
        <div className="space-y-2">
          {accounts.map((account) => (
            <AccountCard
              key={account.id}
              account={account}
              hasChannel={hasChannel}
              busy={busyId === account.id}
              onJoin={() =>
                void act(account.id, '已加入存储频道', () => api.joinStorageChannel(account.id))
              }
              onToggle={() =>
                void act(
                  account.id,
                  account.enabled ? '账号已停用' : '账号已启用',
                  () => api.updateTelegramAccount(account.id, { enabled: !account.enabled }),
                )
              }
              onRemove={() => {
                if (
                  !confirm(
                    `删除账号「${account.label || account.appId}」？\n\n` +
                      '它上传的文件不会被删除，其它账号会重新解析句柄后继续读取。',
                  )
                )
                  return
                void act(account.id, '账号已删除', () => api.deleteTelegramAccount(account.id))
              }}
            />
          ))}
        </div>
      )}

      <Modal
        open={adding}
        onClose={() => setAdding(false)}
        title="添加 Telegram 账号"
        description="需要另一个手机号，以及用那个号在 my.telegram.org 申请的 api_id / api_hash。"
        width="max-w-lg"
      >
        <AddAccountFlow
          hasChannel={hasChannel}
          onDone={async () => {
            setAdding(false)
            await reload()
            await onChanged()
          }}
        />
      </Modal>
    </Section>
  )
}

function AccountCard({
  account,
  hasChannel,
  busy,
  onJoin,
  onToggle,
  onRemove,
}: {
  account: TelegramAccount
  hasChannel: boolean
  busy: boolean
  onJoin: () => void
  onToggle: () => void
  onRemove: () => void
}) {
  const cooldown = Math.ceil((account.status.cooldownMs ?? 0) / 1000)
  const { tone, label } = describe(account, cooldown)

  return (
    <div
      className={clsx(
        'rounded-[var(--radius-card)] border border-[var(--line)] p-3',
        !account.enabled && 'opacity-60',
      )}
    >
      <div className="flex flex-wrap items-start gap-3">
        <span className="mt-1">
          <StatusDot tone={tone} />
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium">
              {account.label || `api_id ${account.appId}`}
            </span>
            {account.isPrimary && (
              <span className="rounded-full bg-[var(--sunk)] px-1.5 py-0.5 text-[10px] text-[var(--muted)]">
                主账号
              </span>
            )}
          </div>
          <p className="mt-0.5 text-xs text-[var(--muted)]">{label}</p>

          <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-[var(--muted)]">
            {account.status.phone && <span>{account.status.phone}</span>}
            {account.status.dc ? <span>DC{account.status.dc}</span> : null}
            <span>
              占用 上传 {account.activeUploads} / 下载 {account.activeDownloads}
            </span>
          </div>
        </div>

        <div className="flex shrink-0 flex-wrap gap-2">
          {hasChannel && account.status.state === 'ready' && !account.canPost && (
            <Button icon={<Radio size={14} />} loading={busy} onClick={onJoin}>
              加入存储频道
            </Button>
          )}
          {!account.isPrimary && (
            <>
              <Button icon={<RefreshCw size={14} />} loading={busy} onClick={onToggle}>
                {account.enabled ? '停用' : '启用'}
              </Button>
              <Button variant="danger" icon={<Trash2 size={14} />} loading={busy} onClick={onRemove}>
                删除
              </Button>
            </>
          )}
        </div>
      </div>
    </div>
  )
}

/** describe turns the several ways an account can be unusable into one line
 *  that says what to do about it. */
function describe(
  account: TelegramAccount,
  cooldownSeconds: number,
): { tone: 'ok' | 'warn' | 'error' | 'idle' | 'busy'; label: string } {
  if (!account.enabled) return { tone: 'idle', label: '已停用，不参与任何传输' }
  switch (account.status.state) {
    case 'unconfigured':
      return { tone: 'idle', label: '缺少 api_id / api_hash' }
    case 'connecting':
      return { tone: 'busy', label: '正在连接' }
    case 'unauthorized':
      return { tone: 'warn', label: '还没登录 Telegram 账号' }
    case 'error':
      return { tone: 'error', label: account.status.error || '连接失败' }
  }
  if (!account.canPost) {
    return {
      tone: 'warn',
      label: account.inChannel
        ? '在存储频道里但没有发消息权限，还不能存文件'
        : '还没加入存储频道，暂时不参与传输',
    }
  }
  if (cooldownSeconds > 0) {
    return { tone: 'warn', label: `被 Telegram 限流，${cooldownSeconds} 秒后恢复；新任务已转给其它账号` }
  }
  return { tone: 'ok', label: '正常，可以承担上传和下载' }
}

/**
 * Adding an account is three steps that have to happen in order — credentials,
 * login, join the channel — so the modal walks them rather than presenting a
 * form that fails at save time.
 */
function AddAccountFlow({
  hasChannel,
  onDone,
}: {
  hasChannel: boolean
  onDone: () => Promise<void>
}) {
  const [stage, setStage] = useState<'credentials' | 'phone' | 'code' | 'password' | 'channel'>(
    'credentials',
  )
  const [accountId, setAccountId] = useState('')
  const [label, setLabel] = useState('')
  const [appId, setAppId] = useState('')
  const [appHash, setAppHash] = useState('')
  const [phone, setPhone] = useState('')
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const [hint, setHint] = useState<string | undefined>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const finish = async () => {
    // The channel step is skipped when there is no channel yet: the drive is
    // still being set up, and selecting one admits every account at once.
    if (!hasChannel) {
      await onDone()
      return
    }
    setStage('channel')
  }

  if (stage === 'credentials') {
    return (
      <div className="space-y-4">
        <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs leading-relaxed text-[var(--muted)]">
          这必须是<b>另一个手机号</b>的账号。Telegram 的限流和传输配额是按账号算的，用同一个号再申请一组
          api_id 只是多一个登录会话，速度不会有任何变化。
        </p>
        <Field label="备注名" hint="只用来在这个列表里区分账号">
          <Input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="备用账号" />
        </Field>
        <Field label="api_id">
          <Input
            value={appId}
            inputMode="numeric"
            onChange={(e) => setAppId(e.target.value)}
            placeholder="1234567"
          />
        </Field>
        <Field label="api_hash" error={error ?? undefined}>
          <Input
            value={appHash}
            onChange={(e) => setAppHash(e.target.value)}
            className="font-[family-name:var(--font-mono)] text-xs"
            placeholder="0123456789abcdef0123456789abcdef"
          />
        </Field>
        <Button
          variant="primary"
          className="w-full"
          loading={busy}
          onClick={() =>
            void run(async () => {
              const id = Number(appId.trim())
              if (!Number.isInteger(id) || id <= 0) throw new Error('api_id 必须是一串数字')
              if (!/^[a-f0-9]{32}$/i.test(appHash.trim()))
                throw new Error('api_hash 应该是 32 位十六进制字符')
              const created = await api.addTelegramAccount({
                label: label.trim(),
                appId: id,
                appHash: appHash.trim(),
              })
              setAccountId(created.id)
              setStage('phone')
            })
          }
        >
          下一步：登录
        </Button>
      </div>
    )
  }

  if (stage === 'phone') {
    return (
      <div className="space-y-4">
        <Field label="手机号" hint="带国家区号，例如 +8613800138000" error={error ?? undefined}>
          <Input
            value={phone}
            onChange={(e) => setPhone(e.target.value)}
            placeholder="+86…"
            autoFocus
            inputMode="tel"
          />
        </Field>
        <Button
          variant="primary"
          className="w-full"
          loading={busy}
          onClick={() =>
            void run(async () => {
              const res = await api.accountSendCode(accountId, phone)
              if (res.alreadyAuthorized) {
                await finish()
                return
              }
              setStage('code')
            })
          }
        >
          发送验证码
        </Button>
      </div>
    )
  }

  if (stage === 'code') {
    return (
      <div className="space-y-4">
        <Field
          label="验证码"
          hint="Telegram 会把验证码发到这个号已登录的其它设备上"
          error={error ?? undefined}
        >
          <Input
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder="12345"
            autoFocus
            inputMode="numeric"
            className="text-center text-lg tracking-[0.4em] font-[family-name:var(--font-mono)]"
          />
        </Field>
        <Button
          variant="primary"
          className="w-full"
          loading={busy}
          onClick={() =>
            void run(async () => {
              const res = await api.accountSignIn(accountId, code)
              if (res.needsPassword) {
                setHint(res.passwordHint)
                setStage('password')
                return
              }
              await finish()
            })
          }
        >
          登录
        </Button>
      </div>
    )
  }

  if (stage === 'password') {
    return (
      <div className="space-y-4">
        <Field label="两步验证密码" hint={hint} error={error ?? undefined}>
          <Input
            value={password}
            type="password"
            onChange={(e) => setPassword(e.target.value)}
            autoFocus
          />
        </Field>
        <Button
          variant="primary"
          className="w-full"
          loading={busy}
          onClick={() =>
            void run(async () => {
              await api.accountSubmitPassword(accountId, password)
              await finish()
            })
          }
        >
          完成登录
        </Button>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <p className="text-xs leading-relaxed text-[var(--muted)]">
        最后一步：把这个账号加入存储频道，并授予发消息、编辑消息和删除消息的权限。
        编辑和删除是重命名、移动、删除文件时需要的——那些操作会改写别的账号发出的消息。
      </p>
      {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}
      <Button
        variant="primary"
        icon={<Radio size={14} />}
        className="w-full"
        loading={busy}
        onClick={() =>
          void run(async () => {
            await api.joinStorageChannel(accountId)
            await onDone()
          })
        }
      >
        加入存储频道
      </Button>
      <button
        onClick={() => void onDone()}
        className="w-full text-xs text-[var(--muted)] transition-colors hover:text-[var(--ink)]"
      >
        稍后再说（这个账号暂时不会参与传输）
      </button>
    </div>
  )
}
