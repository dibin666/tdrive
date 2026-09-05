import { useCallback, useEffect, useState } from 'react'
import clsx from 'clsx'
import {
  Check,
  Download,
  Globe,
  Hash,
  Plus,
  Radio,
  RefreshCw,
  SlidersHorizontal,
  Trash2,
  Upload,
  UserPlus,
} from 'lucide-react'
import { ApiError, api, type AccountChannels, type TelegramAccount } from '../../lib/api'
import { formatBytes } from '../../lib/format'
import { Button, Field, Input, Modal, Spinner, toast } from '../../components/primitives'
import { Section, StatusDot } from './shared'

/**
 * Several Telegram accounts behind one drive.
 *
 * The thing this page has to get across, because getting it wrong wastes
 * someone's afternoon, is that a second account means a second *phone number*.
 * Telegram meters FLOOD_WAIT and transfer quota per account, so registering a
 * second api_id against the same number buys nothing at all — the two
 * authorizations share one budget. In this drive, additional logins are
 * failover accounts: they do not multiply the global transfer queue limits.
 *
 * The second thing is that an account is not useful until it is both signed in
 * and admitted to the storage channel, so every card says plainly which of
 * those it is still missing.
 *
 * Admission itself comes in two flavours. "加入存储频道" lets the server do it,
 * which also detects an account somebody already added by hand. When that
 * cannot work — a primary that may not export invites — the channel is picked
 * directly out of the account's own channel list instead.
 */

export function AccountsSection({
  hasChannel,
  onChanged,
  onSwitchChannel,
}: {
  hasChannel: boolean
  onChanged: () => Promise<void>
  onSwitchChannel?: () => void
}) {
  const [accounts, setAccounts] = useState<TelegramAccount[] | null>(null)
  const [adding, setAdding] = useState(false)
  const [busyId, setBusyId] = useState<string | null>(null)
  // The account whose channel list is open, if any.
  const [picking, setPicking] = useState<TelegramAccount | null>(null)
  // The account whose outbound proxy editor is open, if any.
  const [proxyAccount, setProxyAccount] = useState<TelegramAccount | null>(null)
  // The account whose quota editor is open, if any.
  const [quotaAccount, setQuotaAccount] = useState<TelegramAccount | null>(null)

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
          ? `${usable} 个账号可用。主账号受限时自动调度备用账号。传输队列按全局并发数执行，不因账号增加而翻倍。`
          : '添加备用账号可在主账号受限时接替传输。注：必须为不同手机号，同一账号申请多个 api_id 无效。'
      }
      actions={
        <div className="flex flex-wrap gap-2">
          {hasChannel && onSwitchChannel && (
            <Button icon={<Radio size={14} />} onClick={onSwitchChannel}>
              切换频道
            </Button>
          )}
          <Button icon={<Plus size={14} />} onClick={() => setAdding(true)}>
            添加账号
          </Button>
        </div>
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
              onQuota={() => setQuotaAccount(account)}
              onJoin={() =>
                void act(account.id, '已加入存储频道', () => api.joinStorageChannel(account.id))
              }
              onPickChannel={() => setPicking(account)}
              onCheck={() =>
                void act(account.id, '频道检测完成', () => api.checkAccountChannel(account.id))
              }
              onProxy={() => setProxyAccount(account)}
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
                    `确定删除账号「${account.label || account.appId}」？\n\n` +
                      '该账号上传的文件不会被删除，其它可用账号可继续读取。',
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
        description="需准备独立手机号，以及在 my.telegram.org 申请的 api_id 与 api_hash。"
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

      <Modal
        open={picking !== null}
        onClose={() => setPicking(null)}
        title="手动选择存储频道"
        description={`选择「${picking?.label || '当前账号'}」所加入的存储频道以完成绑定。`}
        width="max-w-lg"
      >
        {picking && (
          <ChannelPicker
            accountId={picking.id}
            onLinked={async () => {
              setPicking(null)
              await reload()
              await onChanged()
            }}
          />
        )}
      </Modal>

      <Modal
        open={proxyAccount !== null}
        onClose={() => setProxyAccount(null)}
        title="配置账号代理"
        description={`仅作用于「${proxyAccount?.label || '当前账号'}」的 Telegram 网络连接。`}
        width="max-w-lg"
      >
        {proxyAccount && (
          <ProxyEditor
            account={proxyAccount}
            onSaved={async () => {
              setProxyAccount(null)
              await reload()
              await onChanged()
            }}
          />
        )}
      </Modal>

      <Modal
        open={quotaAccount !== null}
        onClose={() => setQuotaAccount(null)}
        title="配置每日传输配额"
        description={`配置「${quotaAccount?.label || (quotaAccount ? `api_id ${quotaAccount.appId}` : '')}」的每日上传与下载上限。`}
        width="max-w-lg"
      >
        {quotaAccount && (
          <QuotaEditor
            account={quotaAccount}
            onSaved={async () => {
              setQuotaAccount(null)
              await reload()
              await onChanged()
            }}
          />
        )}
      </Modal>
    </Section>
  )
}

function QuotaProgressBar({
  used,
  reserved,
  quota,
}: {
  used: number
  reserved: number
  quota: number
}) {
  if (quota <= 0) return null
  const usedPct = Math.min(100, Math.max(0, (used / quota) * 100))
  const reservedPct = Math.min(100 - usedPct, Math.max(0, (reserved / quota) * 100))
  const total = used + reserved
  const isExhausted = total >= quota
  const isNearLimit = total >= quota * 0.9

  return (
    <div className="relative h-1.5 w-full overflow-hidden rounded-full bg-[var(--line)]">
      {/* Used portion */}
      <div
        className={clsx(
          'absolute left-0 top-0 h-full transition-[width] duration-300',
          isExhausted
            ? 'bg-[var(--color-danger)]'
            : isNearLimit
              ? 'bg-[var(--color-warn)]'
              : 'bg-[var(--color-clay)]',
        )}
        style={{ width: `${usedPct}%` }}
      />
      {/* Reserved portion */}
      {reservedPct > 0 && (
        <div
          className={clsx(
            'absolute top-0 h-full opacity-60 transition-[left,width] duration-300',
            isExhausted
              ? 'bg-[var(--color-danger)]'
              : isNearLimit
                ? 'bg-[var(--color-warn)]'
                : 'bg-[var(--color-clay)]',
          )}
          style={{ left: `${usedPct}%`, width: `${reservedPct}%` }}
        />
      )}
    </div>
  )
}

function formatResetCountdown(resetAtMs: number): string {
  if (!resetAtMs) return ''
  const diffMs = resetAtMs - Date.now()
  if (diffMs <= 0) return '即将重置'
  const hours = Math.floor(diffMs / 3_600_000)
  const minutes = Math.floor((diffMs % 3_600_000) / 60_000)
  if (hours > 0) {
    return `${hours} 小时 ${minutes} 分钟后重置`
  }
  return `${Math.max(1, minutes)} 分钟后重置`
}

function AccountCard({
  account,
  hasChannel,
  busy,
  onQuota,
  onJoin,
  onPickChannel,
  onCheck,
  onProxy,
  onToggle,
  onRemove,
}: {
  account: TelegramAccount
  hasChannel: boolean
  busy: boolean
  onQuota: () => void
  onJoin: () => void
  onPickChannel: () => void
  onCheck: () => void
  onProxy: () => void
  onToggle: () => void
  onRemove: () => void
}) {
  const cooldown = Math.ceil((account.status.cooldownMs ?? 0) / 1000)
  const { tone, label } = describe(account, cooldown)
  const needsChannel = hasChannel && account.status.state === 'ready' && !account.canPost

  const uploadExhausted = account.uploadDailyQuota > 0 && account.uploadRemainingToday <= 0
  const downloadExhausted = account.downloadDailyQuota > 0 && account.downloadRemainingToday <= 0
  const resetCountdown = formatResetCountdown(account.quotaResetAt)

  return (
    <div
      className={clsx(
        'rounded-[var(--radius-card)] border border-[var(--line)] p-3.5',
        !account.enabled && 'opacity-60',
      )}
    >
      <div className="flex items-start gap-3">
        <span className="mt-1 shrink-0">
          <StatusDot tone={tone} />
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">
              {account.label || `api_id ${account.appId}`}
            </span>
            {account.isPrimary && (
              <span className="rounded-full bg-[var(--sunk)] px-1.5 py-0.5 text-[10px] text-[var(--muted)]">
                主账号
              </span>
            )}
            {uploadExhausted && (
              <span className="rounded-full bg-[var(--color-danger)]/15 px-1.5 py-0.5 text-[10px] font-medium text-[var(--color-danger)]">
                上传配额已耗尽
              </span>
            )}
            {downloadExhausted && (
              <span className="rounded-full bg-[var(--color-danger)]/15 px-1.5 py-0.5 text-[10px] font-medium text-[var(--color-danger)]">
                下载配额已耗尽
              </span>
            )}
          </div>
          <p className="mt-0.5 text-xs text-[var(--muted)]">{label}</p>

          <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-[var(--muted)]">
            {account.status.phone && <span className="shrink-0">{account.status.phone}</span>}
            {account.status.dc ? <span className="shrink-0">DC{account.status.dc}</span> : null}
            {account.channelTitle && <span className="break-all">频道「{account.channelTitle}」</span>}
            {account.proxyUrl && <span className="break-all">代理 {account.proxyUrl}</span>}
            <span className="shrink-0">
              占用 上传 {account.activeUploads} / 下载 {account.activeDownloads}
            </span>
          </div>

          {/* Daily Quota Section */}
          <div className="@container mt-3 rounded-[var(--radius-control)] bg-[var(--sunk)] p-3">
            <div className="grid grid-cols-1 gap-3 @lg:grid-cols-2">
              {/* Upload Quota */}
              <div className="min-w-0 space-y-1.5">
                <div className="flex items-center justify-between gap-2 text-xs">
                  <span className="flex items-center gap-1 font-medium text-[var(--ink)] shrink-0">
                    <Upload size={12} className="text-[var(--color-clay)] shrink-0" />
                    每日上传
                  </span>
                  {account.uploadDailyQuota > 0 ? (
                    uploadExhausted ? (
                      <span className="rounded bg-[var(--color-danger)]/15 px-1.5 py-0.5 text-[10px] font-medium text-[var(--color-danger)] shrink-0">
                        已耗尽
                      </span>
                    ) : (
                      <span className="text-[11px] text-[var(--muted)] shrink-0">
                        剩余 <span className="font-medium text-[var(--ink)]">{formatBytes(account.uploadRemainingToday)}</span>
                      </span>
                    )
                  ) : (
                    <span className="text-[11px] text-[var(--faint)] shrink-0">不限</span>
                  )}
                </div>

                {account.uploadDailyQuota > 0 ? (
                  <QuotaProgressBar
                    used={account.uploadUsedToday}
                    reserved={account.uploadReservedToday}
                    quota={account.uploadDailyQuota}
                  />
                ) : null}

                <div className="flex flex-wrap items-baseline justify-between gap-x-2 gap-y-0.5 text-[11px] text-[var(--muted)]">
                  <span className="shrink-0">
                    已用 {formatBytes(account.uploadUsedToday)}
                    {account.uploadReservedToday > 0 && (
                      <span className="text-[var(--faint)]">（保留中 {formatBytes(account.uploadReservedToday)}）</span>
                    )}
                  </span>
                  <span className="shrink-0 text-[var(--faint)]">
                    {account.uploadDailyQuota > 0 ? `/ ${formatBytes(account.uploadDailyQuota)}` : '配额不限'}
                  </span>
                </div>
              </div>

              {/* Download Quota */}
              <div className="min-w-0 space-y-1.5">
                <div className="flex items-center justify-between gap-2 text-xs">
                  <span className="flex items-center gap-1 font-medium text-[var(--ink)] shrink-0">
                    <Download size={12} className="text-[var(--color-clay)] shrink-0" />
                    每日下载
                  </span>
                  {account.downloadDailyQuota > 0 ? (
                    downloadExhausted ? (
                      <span className="rounded bg-[var(--color-danger)]/15 px-1.5 py-0.5 text-[10px] font-medium text-[var(--color-danger)] shrink-0">
                        已耗尽
                      </span>
                    ) : (
                      <span className="text-[11px] text-[var(--muted)] shrink-0">
                        剩余 <span className="font-medium text-[var(--ink)]">{formatBytes(account.downloadRemainingToday)}</span>
                      </span>
                    )
                  ) : (
                    <span className="text-[11px] text-[var(--faint)] shrink-0">不限</span>
                  )}
                </div>

                {account.downloadDailyQuota > 0 ? (
                  <QuotaProgressBar
                    used={account.downloadUsedToday}
                    reserved={account.downloadReservedToday}
                    quota={account.downloadDailyQuota}
                  />
                ) : null}

                <div className="flex flex-wrap items-baseline justify-between gap-x-2 gap-y-0.5 text-[11px] text-[var(--muted)]">
                  <span className="shrink-0">
                    已用 {formatBytes(account.downloadUsedToday)}
                    {account.downloadReservedToday > 0 && (
                      <span className="text-[var(--faint)]">（保留中 {formatBytes(account.downloadReservedToday)}）</span>
                    )}
                  </span>
                  <span className="shrink-0 text-[var(--faint)]">
                    {account.downloadDailyQuota > 0 ? `/ ${formatBytes(account.downloadDailyQuota)}` : '配额不限'}
                  </span>
                </div>
              </div>
            </div>

            <div className="mt-2.5 flex flex-wrap items-center justify-between gap-x-3 gap-y-1 border-t border-[var(--line)] pt-2 text-[10px] text-[var(--faint)]">
              <span>每日按 UTC 0 点重置（UTC {account.quotaDate || '当日'}）</span>
              {resetCountdown && <span className="shrink-0">{resetCountdown}</span>}
            </div>
          </div>

          {/* Action Buttons */}
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <Button icon={<SlidersHorizontal size={14} />} disabled={busy} onClick={onQuota}>
              配额
            </Button>
            <Button icon={<Globe size={14} />} disabled={busy} onClick={onProxy}>
              代理
            </Button>
            {hasChannel && account.status.state === 'ready' && (
              <Button icon={<RefreshCw size={14} />} loading={busy} onClick={onCheck}>
                检测频道
              </Button>
            )}
            {needsChannel && (
              <>
                <Button icon={<Radio size={14} />} loading={busy} onClick={onJoin}>
                  加入存储频道
                </Button>
                {/* The way out when the automatic join cannot work, and the way
                    in for an account somebody already added in a Telegram
                    client. */}
                <Button icon={<Hash size={14} />} disabled={busy} onClick={onPickChannel}>
                  手动选择频道
                </Button>
              </>
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
    </div>
  )
}

/**
 * Edits one account's outbound Telegram proxy.
 *
 * The current value is shown only in masked form. The input is intentionally
 * empty when an editor opens: putting the masked password back into a URL would
 * silently replace a real credential with a string of asterisks. Leaving the
 * field empty and saving clears the proxy and returns the account to a direct
 * connection.
 */
function ProxyEditor({
  account,
  onSaved,
}: {
  account: TelegramAccount
  onSaved: () => Promise<void>
}) {
  const [proxyUrl, setProxyUrl] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const save = async () => {
    setBusy(true)
    setError(null)
    try {
      await api.setTelegramAccountProxy(account.id, proxyUrl.trim())
      toast(proxyUrl.trim() ? '账号代理已更新' : '已清除账号代理', 'success')
      await onSaved()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4">
      <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs leading-relaxed text-[var(--muted)]">
        不同 Telegram 账号可配置独立代理出口，避免多账号共用同一出口 IP。
        支持 SOCKS5 与 HTTP 代理；代理密码仅保存在服务器，不回传浏览器。
      </p>

      {account.proxyUrl ? (
        <p className="text-xs text-[var(--muted)]">
          当前代理：<span className="font-[family-name:var(--font-mono)]">{account.proxyUrl}</span>
        </p>
      ) : (
        <p className="text-xs text-[var(--muted)]">当前未配置代理，使用服务器直连。</p>
      )}

      <Field
        label="代理地址"
        hint="例如 socks5://user:password@127.0.0.1:1080 或 http://user:password@proxy.example:8080；留空为直连"
        error={error ?? undefined}
      >
        <Input
          value={proxyUrl}
          onChange={(event) => setProxyUrl(event.target.value)}
          placeholder="socks5://127.0.0.1:1080"
          autoFocus
          autoComplete="off"
          className="font-[family-name:var(--font-mono)] text-xs"
        />
      </Field>

      <Button variant="primary" className="w-full" loading={busy} onClick={() => void save()}>
        保存并重新连接
      </Button>
    </div>
  )
}

/**
 * Pointing one account at the storage channel by hand.
 *
 * The list is what that account's own Telegram session can see, so the storage
 * channel appears in it the moment somebody adds the account to the channel in
 * a Telegram client — which is the whole point: no invite is exported, and the
 * primary account is not involved at all.
 *
 * Rows other than the storage channel stay clickable rather than being hidden.
 * Picking one is refused by the server with a plain explanation, which is much
 * easier to act on than a list that silently omits what someone is looking for.
 */
function ChannelPicker({
  accountId,
  onLinked,
}: {
  accountId: string
  onLinked: () => Promise<void>
}) {
  const [data, setData] = useState<AccountChannels | null>(null)
  const [loading, setLoading] = useState(true)
  const [linkingTgId, setLinkingTgId] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      setData(await api.accountChannels(accountId))
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }, [accountId])

  useEffect(() => {
    void load()
  }, [load])

  const link = async (tgId: number) => {
    setLinkingTgId(tgId)
    setError(null)
    try {
      await api.linkAccountChannel(accountId, tgId)
      toast('已对上存储频道', 'success')
      await onLinked()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLinkingTgId(null)
    }
  }

  const storageChannelFound = data?.channels.some((c) => c.tgId === data.storage.tgId) ?? false

  return (
    <div className="space-y-4">
      <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs leading-relaxed text-[var(--muted)]">
        下方为该账号可见的频道列表。请选择标有「存储频道」的一项。该账号须已加入频道并拥有发消息、编辑消息和删除消息权限。
      </p>

      {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}

      {loading && data === null ? (
        <div className="flex justify-center py-8">
          <Spinner />
        </div>
      ) : data === null ? null : data.channels.length === 0 ? (
        <p className="py-6 text-center text-sm text-[var(--muted)]">
          未发现可用频道。请先在 Telegram 客户端中加入「{data.storage.title}」。
        </p>
      ) : (
        <div className="max-h-64 space-y-1 overflow-y-auto">
          {data.channels.map((channel) => {
            const isStorage = channel.tgId === data.storage.tgId
            return (
              <button
                key={channel.tgId}
                disabled={!isStorage || linkingTgId !== null}
                onClick={isStorage ? () => void link(channel.tgId) : undefined}
                className="row w-full justify-between disabled:cursor-not-allowed disabled:opacity-45"
                data-selected={isStorage}
              >
                <span className="min-w-0 truncate text-sm">{channel.title}</span>
                <span className="flex shrink-0 items-center gap-1.5">
                  {!channel.canPost && <span className="chip">只读</span>}
                  {isStorage && (
                    <span className="chip inline-flex items-center gap-1">
                      <Check size={12} />
                      存储频道
                    </span>
                  )}
                </span>
              </button>
            )
          })}
        </div>
      )}

      {data !== null && !storageChannelFound && data.channels.length > 0 && (
        <p className="text-xs leading-relaxed text-[var(--muted)]">
          列表中未找到「{data.storage.title}」。请先在 Telegram 客户端中加入该频道，再点击刷新。
        </p>
      )}

      <Button
        icon={<RefreshCw size={14} />}
        className="w-full"
        loading={loading}
        onClick={() => void load()}
      >
        刷新频道列表
      </Button>
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
        ? `已加入频道「${account.channelTitle || '存储频道'}」，但缺少发消息权限`
        : `尚未加入频道「${account.channelTitle || '存储频道'}」，暂不参与传输`,
    }
  }
  if (cooldownSeconds > 0) {
    return { tone: 'warn', label: `Telegram 限流中，${cooldownSeconds} 秒后解除；任务将由备用账号接替` }
  }
  const uploadExhausted = account.uploadDailyQuota > 0 && account.uploadRemainingToday <= 0
  const downloadExhausted = account.downloadDailyQuota > 0 && account.downloadRemainingToday <= 0
  if (uploadExhausted && downloadExhausted) {
    return { tone: 'warn', label: '今日上传与下载配额已耗尽，任务由备用账号接替（UTC 0 点重置）' }
  }
  if (uploadExhausted) {
    return { tone: 'warn', label: '今日上传配额已耗尽，新上传任务由备用账号接替（UTC 0 点重置）' }
  }
  if (downloadExhausted) {
    return { tone: 'warn', label: '今日下载配额已耗尽，新下载任务由备用账号接替（UTC 0 点重置）' }
  }
  return { tone: 'ok', label: '正常运行，可承载上传与下载' }
}

const QUOTA_UNITS = [
  { value: 'GiB', label: 'GiB (1024³ 字节)', multiplier: 1024 * 1024 * 1024 },
  { value: 'GB', label: 'GB (1000³ 字节)', multiplier: 1000 * 1000 * 1000 },
  { value: 'TiB', label: 'TiB (1024⁴ 字节)', multiplier: 1024 * 1024 * 1024 * 1024 },
  { value: 'MiB', label: 'MiB (1024² 字节)', multiplier: 1024 * 1024 },
  { value: 'B', label: '字节 (B)', multiplier: 1 },
]

function bytesToInputState(bytes: number): { value: string; unit: string } {
  if (!bytes || bytes <= 0) return { value: '', unit: 'GiB' }
  const TIB = 1024 * 1024 * 1024 * 1024
  const GIB = 1024 * 1024 * 1024
  const MIB = 1024 * 1024

  if (bytes % TIB === 0) return { value: String(bytes / TIB), unit: 'TiB' }
  if (bytes % GIB === 0) return { value: String(bytes / GIB), unit: 'GiB' }
  if (bytes % (1000 * 1000 * 1000) === 0) return { value: String(bytes / (1000 * 1000 * 1000)), unit: 'GB' }
  if (bytes % MIB === 0) return { value: String(bytes / MIB), unit: 'MiB' }

  // Check if close to clean GiB
  const gibVal = bytes / GIB
  const roundedGib = parseFloat(gibVal.toFixed(3))
  if (Math.abs(roundedGib * GIB - bytes) < 1024) {
    return { value: String(roundedGib), unit: 'GiB' }
  }
  return { value: String(bytes), unit: 'B' }
}

function parseQuotaBytes(valStr: string, unit: string): { bytes: number; error?: string } {
  const trimmed = valStr.trim()
  if (!trimmed || trimmed === '0') return { bytes: 0 }
  const num = Number(trimmed)
  if (!Number.isFinite(num) || num < 0) {
    return { bytes: 0, error: '配额必须是非负数字' }
  }
  const item = QUOTA_UNITS.find((u) => u.value === unit)
  const mult = item?.multiplier ?? (1024 * 1024 * 1024)
  const bytes = Math.round(num * mult)
  if (bytes > Number.MAX_SAFE_INTEGER) {
    return { bytes: 0, error: '配额数值过大' }
  }
  return { bytes }
}

function formatBytesPreview(valStr: string, unit: string): string {
  const trimmed = valStr.trim()
  if (!trimmed || trimmed === '0') return '不限额（0 字节）'
  const num = Number(trimmed)
  if (!Number.isFinite(num) || num < 0) return '无效输入'
  const item = QUOTA_UNITS.find((u) => u.value === unit)
  const mult = item?.multiplier ?? (1024 * 1024 * 1024)
  const bytes = Math.round(num * mult)
  const formatted = formatBytes(bytes)
  const gib = (bytes / (1024 * 1024 * 1024)).toFixed(2)
  return `换算: ≈ ${bytes.toLocaleString()} 字节（${formatted} / ${gib} GiB）`
}

function QuotaInputField({
  label,
  icon,
  valStr,
  unit,
  onValChange,
  onUnitChange,
  usedToday,
  reservedToday,
}: {
  label: string
  icon: React.ReactNode
  valStr: string
  unit: string
  onValChange: (v: string) => void
  onUnitChange: (u: string) => void
  usedToday: number
  reservedToday: number
}) {
  const preview = formatBytesPreview(valStr, unit)
  const presets = [
    { label: '不限', val: '', unit: 'GiB' },
    { label: '10 GiB', val: '10', unit: 'GiB' },
    { label: '50 GiB', val: '50', unit: 'GiB' },
    { label: '100 GiB', val: '100', unit: 'GiB' },
    { label: '500 GiB', val: '500', unit: 'GiB' },
    { label: '1 TiB', val: '1', unit: 'TiB' },
  ]

  return (
    <div className="space-y-2.5 rounded-[var(--radius-control)] border border-[var(--line)] p-3">
      <div className="flex items-center justify-between">
        <span className="flex items-center gap-1.5 text-xs font-medium text-[var(--ink)]">
          {icon}
          {label}
        </span>
        <span className="text-[11px] text-[var(--muted)]">
          今日已用 {formatBytes(usedToday)}
          {reservedToday > 0 && ` (保留中 ${formatBytes(reservedToday)})`}
        </span>
      </div>

      <div className="flex gap-2">
        <Input
          type="number"
          min="0"
          step="any"
          value={valStr}
          onChange={(e) => onValChange(e.target.value)}
          placeholder="0 (不限额)"
          className="flex-1 font-[family-name:var(--font-mono)] text-xs"
        />
        <select
          value={unit}
          onChange={(e) => onUnitChange(e.target.value)}
          className="input !w-auto shrink-0 cursor-pointer text-xs"
        >
          {QUOTA_UNITS.map((u) => (
            <option key={u.value} value={u.value}>
              {u.label}
            </option>
          ))}
        </select>
      </div>

      {/* Preset pills */}
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-[11px] text-[var(--faint)]">快速预设:</span>
        {presets.map((p) => {
          const isSelected = valStr.trim() === p.val && (p.val === '' || unit === p.unit)
          return (
            <button
              key={p.label}
              type="button"
              onClick={() => {
                onValChange(p.val)
                onUnitChange(p.unit)
              }}
              className={clsx(
                'rounded-md px-2 py-0.5 text-[11px] transition-colors',
                isSelected
                  ? 'bg-[var(--color-clay)] font-medium text-white'
                  : 'bg-[var(--sunk)] text-[var(--muted)] hover:bg-[var(--line)] hover:text-[var(--ink)]',
              )}
            >
              {p.label}
            </button>
          )
        })}
      </div>

      <div className="text-[11px] text-[var(--muted)]">{preview}</div>
    </div>
  )
}

function QuotaEditor({
  account,
  onSaved,
}: {
  account: TelegramAccount
  onSaved: () => Promise<void>
}) {
  const initialUpload = bytesToInputState(account.uploadDailyQuota)
  const initialDownload = bytesToInputState(account.downloadDailyQuota)

  const [label, setLabel] = useState(account.label || '')
  const [uploadVal, setUploadVal] = useState(initialUpload.value)
  const [uploadUnit, setUploadUnit] = useState(initialUpload.unit)
  const [downloadVal, setDownloadVal] = useState(initialDownload.value)
  const [downloadUnit, setDownloadUnit] = useState(initialDownload.unit)

  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const save = async () => {
    setError(null)

    const uploadParsed = parseQuotaBytes(uploadVal, uploadUnit)
    if (uploadParsed.error) {
      setError(`上传配额错误：${uploadParsed.error}`)
      return
    }

    const downloadParsed = parseQuotaBytes(downloadVal, downloadUnit)
    if (downloadParsed.error) {
      setError(`下载配额错误：${downloadParsed.error}`)
      return
    }

    setBusy(true)
    try {
      await api.updateTelegramAccount(account.id, {
        label: label.trim() || undefined,
        uploadDailyQuota: uploadParsed.bytes,
        downloadDailyQuota: downloadParsed.bytes,
      })
      toast('账号配额已保存', 'success')
      await onSaved()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-4">
      <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs leading-relaxed text-[var(--muted)]">
        每日配额于 <b>UTC 0 点</b>（北京时间 08:00）重置。设为 <b>0</b> 或留空表示<b>不设限</b>。
        单账号配额耗尽后，后续传输将自动调度至其它可用账号。
      </p>

      {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}

      <Field label="账号备注" hint="用于在账号列表与传输记录中标识">
        <Input
          value={label}
          onChange={(e) => setLabel(e.target.value)}
          placeholder={`api_id ${account.appId}`}
        />
      </Field>

      <QuotaInputField
        label="每日上传配额"
        icon={<Upload size={13} className="text-[var(--color-clay)]" />}
        valStr={uploadVal}
        unit={uploadUnit}
        onValChange={setUploadVal}
        onUnitChange={setUploadUnit}
        usedToday={account.uploadUsedToday}
        reservedToday={account.uploadReservedToday}
      />

      <QuotaInputField
        label="每日下载配额"
        icon={<Download size={13} className="text-[var(--color-clay)]" />}
        valStr={downloadVal}
        unit={downloadUnit}
        onValChange={setDownloadVal}
        onUnitChange={setDownloadUnit}
        usedToday={account.downloadUsedToday}
        reservedToday={account.downloadReservedToday}
      />

      <Button variant="primary" className="w-full" loading={busy} onClick={() => void save()}>
        保存配额设置
      </Button>
    </div>
  )
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
  const [proxyUrl, setProxyUrl] = useState('')
  const [phone, setPhone] = useState('')
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const [hint, setHint] = useState<string | undefined>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [pickingChannel, setPickingChannel] = useState(false)

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

    // Check first so an account that was already added in Telegram is marked
    // usable without asking the operator to press the automatic join button.
    // A 409 means the normal next step is to join or manually select it; other
    // failures should remain visible because they usually indicate a broken
    // connection or proxy.
    try {
      const result = await api.checkAccountChannel(accountId)
      if (result.usable) {
        await onDone()
        return
      }
    } catch (err) {
      if (!(err instanceof ApiError) || err.status !== 409) throw err
    }
    setStage('channel')
  }

  if (stage === 'credentials') {
    return (
      <div className="space-y-4">
        <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs leading-relaxed text-[var(--muted)]">
          须为<b>不同手机号</b>注册的账号。Telegram 按账号实施限流与配额控制，
          同一手机号申请多组 api_id 无法提升性能或绕过限制。
        </p>
        <Field label="账号备注" hint="用于在账号列表中标识">
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
        <Field label="api_hash">
          <Input
            value={appHash}
            onChange={(e) => setAppHash(e.target.value)}
            className="font-[family-name:var(--font-mono)] text-xs"
            placeholder="0123456789abcdef0123456789abcdef"
          />
        </Field>
        <Field
          label="代理地址（可选）"
          hint="支持 socks5://host:port 或 http://host:port；建议各账号使用不同出口 IP"
        >
          <Input
            value={proxyUrl}
            onChange={(event) => setProxyUrl(event.target.value)}
            placeholder="socks5://127.0.0.1:1080"
            autoComplete="off"
            className="font-[family-name:var(--font-mono)] text-xs"
          />
        </Field>
        {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}
        <Button
          variant="primary"
          className="w-full"
          loading={busy}
          onClick={() =>
            void run(async () => {
              const id = Number(appId.trim())
              if (!Number.isInteger(id) || id <= 0) throw new Error('api_id 必须是一串数字')
              if (!/^[a-f0-9]{32}$/i.test(appHash.trim()))
                throw new Error('api_hash 必须为 32 位十六进制字符')
              const created = await api.addTelegramAccount({
                label: label.trim(),
                appId: id,
                appHash: appHash.trim(),
                proxyUrl: proxyUrl.trim() || undefined,
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
          hint="Telegram 会将验证码发送至该账号已登录的设备"
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

  // Picking the channel by hand takes over the step entirely, because the two
  // routes ask for different things and showing both at once reads as if the
  // channel had to be joined twice.
  if (pickingChannel) {
    return (
      <div className="space-y-4">
        <p className="text-xs leading-relaxed text-[var(--muted)]">
          自动加入失败。请在 Telegram 客户端中将该账号加入存储频道并赋予管理员权限，随后在下方绑定。
        </p>
        <ChannelPicker accountId={accountId} onLinked={onDone} />
        <button
          onClick={() => setPickingChannel(false)}
          className="w-full text-xs text-[var(--muted)] transition-colors hover:text-[var(--ink)]"
        >
          返回，再试一次自动加入
        </button>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <p className="text-xs leading-relaxed text-[var(--muted)]">
        最后一步：将该账号加入存储频道，并授予发消息、编辑消息与删除消息权限（重命名、移动与删除文件需改写消息内容）。
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
      <Button
        icon={<Hash size={14} />}
        className="w-full"
        disabled={busy}
        onClick={() => setPickingChannel(true)}
      >
        手动选择频道
      </Button>
      <button
        onClick={() => void onDone()}
        className="w-full text-xs text-[var(--muted)] transition-colors hover:text-[var(--ink)]"
      >
        稍后配置（该账号暂不参与传输）
      </button>
    </div>
  )
}
