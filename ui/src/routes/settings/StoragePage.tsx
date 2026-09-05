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
    if (!confirm('确定清空全部暂存文件？正在进行的暂存任务将被中断。')) return
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
            <Stat label="目录" value={String(stats.dirs)} />
            <Stat label="分卷" value={String(stats.segments)} />
            <Stat label="总大小" value={formatBytes(stats.totalBytes)} />
          </div>
          {stats.brokenFiles > 0 && (
            <p className="mt-3 text-xs text-[var(--color-danger)]">
              {stats.brokenFiles} 个文件存在缺卷，无法完整下载。可在「维护与日志」中重建索引。
            </p>
          )}
        </Section>
      )}

      <Section
        icon={<FolderOpen size={16} />}
        title="VPS 本地上传"
        description="指定服务器本地只读挂载目录。具备权限的账号可直接添加其中的文件，无需经由浏览器上传。"
      >
        <Field
          label="目录路径"
          hint="留空表示停用。Docker Compose 默认挂载至容器内 /vps"
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
        description="多线程下载大分卷文件时，由服务器先在磁盘组装完整文件，再高速取回，提升稳定性。"
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
                  : '已停用'
              }
            />
          )}

          <Field label="暂存目录" hint="留空使用数据目录下的 cache 子目录">
            <Input
              value={cacheDir}
              placeholder={cache?.dir ?? '/data/cache'}
              onChange={(e) => setCacheDir(e.target.value)}
              className="font-[family-name:var(--font-mono)] text-xs"
            />
          </Field>

          <Field label="暂存容量上限">
            <Slider
              value={cacheLimitGiB}
              min={0}
              max={500}
              step={1}
              suffix="GiB"
              onChange={setCacheLimitGiB}
              format={(value) =>
                value === 0
                  ? '设为 0 完全关闭暂存，分卷文件仅支持直连或逐卷下载。'
                  : `超出 ${value} GiB 后按最少使用原则自动清理旧暂存。`
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
              format={(value) => `暂存完成后 ${value} 小时内可高速下载，超时自动删除。`}
            />
          </Field>
        </div>
      </Section>

      <Section
        icon={<Database size={16} />}
        title="公开直链"
        description="生成的下载链接携带独立 token，支持 aria2、IDM 等工具多线程与断点续传。"
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
                ? '设为 0 表示直链默认永久有效，需手动撤销。'
                : `新直链将于 ${value} 小时后过期（约 ${(value / 24).toFixed(1)} 天）。`
            }
          />
        </Field>
      </Section>

      <Section icon={<HardDrive size={16} />} title="WebDAV">
        <Switch
          checked={webdavEnabled}
          onChange={setWebdavEnabled}
          label="开启 WebDAV 服务"
          hint="关闭后 /dav 端点返回 404，已挂载客户端将断开连接"
        />
      </Section>

      {error && <p className="text-xs text-[var(--color-danger)]">{error}</p>}
      <Button variant="primary" loading={busy} onClick={() => void save()}>
        保存存储配置
      </Button>
    </div>
  )
}
