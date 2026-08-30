import { useCallback, useEffect, useState } from 'react'
import { Database, Download, RefreshCw, ScrollText, Search } from 'lucide-react'
import { api, type AuditEntry, type IndexStatus } from '../../lib/api'
import { events } from '../../lib/events'
import { formatDateTime } from '../../lib/format'
import { Button, Progress, Select, Spinner, toast } from '../../components/primitives'
import { Section } from './shared'

const ACTION_LABELS: Record<string, string> = {
  'user.create': '创建账号',
  'user.delete': '删除账号',
  'user.update': '修改账号',
  'user.role': '修改角色',
  'user.password': '重置密码',
  'user.enable': '启用账号',
  'user.disable': '停用账号',
  'session.revoke': '注销会话',
  'settings.update': '修改设置',
  'telegram.configure': '配置 Telegram',
  'telegram.logout': '退出 Telegram',
  'telegram.import': '导入 Telegram 账号',
  'telegram.export': '导出 Telegram 账号',
  'telegram.channel': '切换存储频道',
  'index.rebuild': '重建索引',
  'share.create': '生成直链',
  'share.revoke': '撤销直链',
  'transfer.delete': '删除传输记录',
  'download.stage': '服务器暂存下载',
  'cache.purge': '清理暂存',
  'file.delete': '删除文件',
  'file.batchRename': '批量重命名',
}

export function MaintenancePage() {
  return (
    <div className="space-y-4">
      <IndexSection />
      <AuditSection />
    </div>
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
      description="扫描频道里所有带 #tdrive 标签的消息，从标签还原整棵目录树、分卷关系和文件归属。本地索引损坏或换机器时用得上。"
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

function AuditSection() {
  const [entries, setEntries] = useState<AuditEntry[] | null>(null)
  const [action, setAction] = useState('')
  const [search, setSearch] = useState('')

  const load = useCallback(() => {
    void api
      .audit({ action: action || undefined, q: search.trim() || undefined, limit: 200 })
      .then(setEntries)
      .catch(() => setEntries([]))
  }, [action, search])

  useEffect(load, [load])

  const exportCsv = () => {
    if (!entries || entries.length === 0) return
    const header = ['时间', '操作者', 'IP', '动作', '目标', '详情']
    const rows = entries.map((entry) => [
      formatDateTime(Date.parse(entry.at)),
      entry.actorName,
      entry.ip ?? '',
      ACTION_LABELS[entry.action] ?? entry.action,
      entry.target ?? '',
      entry.detail ?? '',
    ])
    const csv = [header, ...rows]
      .map((row) => row.map((cell) => `"${String(cell).replace(/"/g, '""')}"`).join(','))
      .join('\n')

    // The BOM is what makes Excel open a UTF-8 CSV without mangling Chinese.
    const blob = new Blob(['﻿' + csv], { type: 'text/csv;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = `tdrive-audit-${new Date().toISOString().slice(0, 10)}.csv`
    link.click()
    setTimeout(() => URL.revokeObjectURL(url), 10_000)
  }

  return (
    <Section
      icon={<ScrollText size={16} />}
      title="操作日志"
      description="账号、设置、索引和分享链接的变更记录。这是这里唯一无法从 Telegram 重建的数据。"
      actions={
        <Button icon={<Download size={14} />} onClick={exportCsv} disabled={!entries?.length}>
          导出 CSV
        </Button>
      }
    >
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Select
          className="!w-auto !py-1.5 text-xs"
          value={action}
          onChange={(e) => setAction(e.target.value)}
        >
          <option value="">全部动作</option>
          {Object.entries(ACTION_LABELS).map(([key, label]) => (
            <option key={key} value={key}>
              {label}
            </option>
          ))}
        </Select>
        <div className="relative min-w-40 flex-1 sm:max-w-56 sm:flex-none">
          <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-[var(--faint)]" />
          <input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="搜索操作者或目标"
            className="input !py-1.5 !pl-7 text-xs"
          />
        </div>
        <Button className="!py-1.5" onClick={load}>
          <RefreshCw size={13} />
        </Button>
      </div>

      {entries === null ? (
        <Spinner />
      ) : entries.length === 0 ? (
        <p className="py-6 text-center text-xs text-[var(--muted)]">没有匹配的记录</p>
      ) : (
        <div className="max-h-[28rem] overflow-y-auto">
          <table className="w-full text-xs">
            <thead className="sticky top-0 bg-[var(--surface)]">
              <tr className="text-left text-[11px] text-[var(--faint)]">
                <th className="py-1.5 pr-3 font-medium">时间</th>
                <th className="py-1.5 pr-3 font-medium">操作者</th>
                <th className="py-1.5 pr-3 font-medium">动作</th>
                <th className="py-1.5 font-medium">目标</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-[var(--line)]">
              {entries.map((entry) => (
                <tr key={entry.id}>
                  <td className="whitespace-nowrap py-1.5 pr-3 tabular-nums text-[var(--muted)]">
                    {formatDateTime(Date.parse(entry.at))}
                  </td>
                  <td className="py-1.5 pr-3">
                    {entry.actorName || '系统'}
                    {entry.ip && <span className="ml-1.5 text-[var(--faint)]">{entry.ip}</span>}
                  </td>
                  <td className="py-1.5 pr-3">{ACTION_LABELS[entry.action] ?? entry.action}</td>
                  <td className="max-w-[16rem] truncate py-1.5" title={`${entry.target ?? ''} ${entry.detail ?? ''}`}>
                    {entry.target}
                    {entry.detail && <span className="ml-1.5 text-[var(--faint)]">{entry.detail}</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Section>
  )
}
