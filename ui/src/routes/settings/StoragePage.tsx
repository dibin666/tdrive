import { useCallback, useEffect, useState } from 'react'
import { Database, FolderOpen, HardDrive, Trash2 } from 'lucide-react'
import { api, type CacheStatus, type RuntimeSettings, type Stats } from '../../lib/api'
import { formatBytes } from '../../lib/format'
import { Button, Field, Input, Meter, Slider, Spinner, Switch, toast } from '../../components/primitives'
import { Section, Stat } from './shared'

/**
 * Storage settings: where the server may read files from, and where it may
 * write staged downloads to. Both are paths on the server rather than in the
 * drive, which is why they live together and away from everything else.
 */
export function StoragePage({ onChanged }: { onChanged: () => Promise<void> }) {
  const [settings, setSettings] = useState<RuntimeSettings | null>(null)
  const [localRoot, setLocalRoot] = useState('')
  const [cacheDir, setCacheDir] = useState('')
  const [cacheLimitGiB, setCacheLimitGiB] = useState(20)
  const [cacheTtlHours, setCacheTtlHours] = useState(24)
  const [shareTtlHours, setShareTtlHours] = useState(168)
  const [webdavEnabled, setWebdavEnabled] = useState(true)
  const [cache, setCache] = useState<CacheStatus | null>(null)
  const [stats, setStats] = useState<Stats | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const loadCache = useCallback(() => {
    void api.cache().then(setCache).catch(() => {})
  }, [])

  useEffect(() => {
    void api
      .settings()
      .then((value) => {
        setSettings(value)
        setLocalRoot(value.localRoot)
        setCacheDir(value.cacheDir)
        setCacheLimitGiB(Math.round(value.cacheLimit / (1024 * 1024 * 1024)))
        setCacheTtlHours(value.cacheTtlHours || 24)
        setShareTtlHours(value.shareTtlHours)
        setWebdavEnabled(value.webdavEnabled)
      })
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
    void api.stats().then(setStats).catch(() => {})
    loadCache()
  }, [loadCache])

  const save = async () => {
    setBusy(true)
    setError(null)
    try {
      const saved = await api.updateSettings({
        localRoot: localRoot.trim(),
        cacheDir: cacheDir.trim(),
        cacheLimit: Math.round(cacheLimitGiB * 1024 * 1024 * 1024),
        cacheTtlHours,
        shareTtlHours,
        webdavEnabled,
      })
      setSettings(saved)
      await onChanged()
      loadCache()
      toast('存储设置已保存', 'success')
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const purge = async () => {
    if (!confirm('清空全部暂存文件？正在进行的暂存下载会被中断。')) return
    try {
      const result = await api.purgeCache()
      toast(`已释放 ${formatBytes(result.freed)}`, 'success')
      loadCache()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    }
  }

  if (!settings) {
    return (
      <Section icon={<HardDrive size={16} />} title="存储">
        {error ? <p className="text-sm text-[var(--color-danger)]">{error}</p> : <Spinner />}
      </Section>
    )
  }

  return (
    <div className="space-y-4">
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
              有 {stats.brokenFiles} 个文件缺少分卷，无法完整下载。可以在「索引与维护」里重建索引。
            </p>
          )}
        </Section>
      )}

      <Section
        icon={<FolderOpen size={16} />}
        title="VPS 本地上传目录"
        description="服务器上一个可读目录。配置后，有对应权限的账号可以在上传对话框里直接选服务器上的文件，不经过浏览器。"
      >
        <Field
          label="目录路径"
          hint="留空表示禁用。Docker Compose 默认把宿主机目录挂载到容器内的 /vps"
        >
          <Input
            value={localRoot}
            placeholder="/vps"
            onChange={(e) => setLocalRoot(e.target.value)}
            className="font-[family-name:var(--font-mono)] text-xs"
          />
        </Field>
      </Section>

      <Section
        icon={<Database size={16} />}
        title="下载暂存"
        description="分卷文件多线程直连下载容易在卷边界失败。开启暂存后，服务器先把整个文件拼好写到磁盘，再由客户端高速取走。"
        actions={
          cache && cache.used > 0 ? (
            <Button icon={<Trash2 size={14} />} onClick={() => void purge()}>
              立即清理
            </Button>
          ) : undefined
        }
      >
        <div className="space-y-5">
          {cache && (
            <Meter
              value={cache.used}
              max={cache.limit || 1}
              label={`暂存占用（${cache.files} 个文件）`}
              caption={
                cache.limit > 0
                  ? `${formatBytes(cache.used)} / ${formatBytes(cache.limit)}`
                  : '已禁用'
              }
            />
          )}

          <Field label="暂存目录" hint="留空表示使用数据目录下的 cache 子目录">
            <Input
              value={cacheDir}
              placeholder={cache?.dir ?? '/data/cache'}
              onChange={(e) => setCacheDir(e.target.value)}
              className="font-[family-name:var(--font-mono)] text-xs"
            />
          </Field>

          <Field label="磁盘上限">
            <Slider
              value={cacheLimitGiB}
              min={0}
              max={500}
              step={1}
              suffix="GiB"
              onChange={setCacheLimitGiB}
              format={(value) =>
                value === 0
                  ? '设为 0 表示完全关闭暂存功能，分卷文件只能直连或分卷下载。'
                  : `超过 ${value} GiB 后按最近使用时间自动淘汰旧的暂存文件。`
              }
            />
          </Field>

          <Field label="暂存保留时长">
            <Slider
              value={cacheTtlHours}
              min={1}
              max={720}
              step={1}
              suffix="小时"
              onChange={setCacheTtlHours}
              format={(value) => `暂存完成后 ${value} 小时内可以重复下载，之后自动清理。`}
            />
          </Field>
        </div>
      </Section>

      <Section
        icon={<Database size={16} />}
        title="分享直链"
        description="生成的下载直链带独立令牌，可以粘贴到 aria2、IDM 等工具，支持多线程和断点续传。"
      >
        <Field label="默认有效期">
          <Slider
            value={shareTtlHours}
            min={0}
            max={8760}
            step={1}
            suffix="小时"
            onChange={setShareTtlHours}
            format={(value) =>
              value === 0
                ? '设为 0 表示新生成的链接默认永不过期，需要时手动撤销。'
                : `新链接默认 ${value} 小时后失效（约 ${(value / 24).toFixed(1)} 天）。`
            }
          />
        </Field>
      </Section>

      <Section icon={<HardDrive size={16} />} title="WebDAV">
        <Switch
          checked={webdavEnabled}
          onChange={setWebdavEnabled}
          label="启用 WebDAV 挂载"
          hint="关闭后 /dav 会返回 404，已挂载的客户端会断开"
        />
      </Section>

      {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}
      <Button variant="primary" loading={busy} onClick={() => void save()}>
        保存存储设置
      </Button>
    </div>
  )
}
