import { useEffect, useState } from 'react'
import { Blocks, ExternalLink, PackageOpen, Plus, Power, RefreshCw, Trash2 } from 'lucide-react'
import { api, type PluginInspection, type PluginStatus, type PluginStoreItem } from '../../lib/api'
import { Button, Drawer, Field, IconButton, Input, Modal, Spinner, Switch, toast } from '../../components/primitives'
import { Section, StatusDot } from './shared'

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : String(error)
}

function statusTone(status: string): 'ok' | 'warn' | 'error' | 'idle' | 'busy' {
  if (status === 'active') return 'ok'
  if (status === 'error') return 'error'
  if (status === 'disabled' || status === 'stopped') return 'idle'
  return 'busy'
}

export function PluginsPage() {
  const [plugins, setPlugins] = useState<PluginStatus[]>([])
  const [loading, setLoading] = useState(true)
  const [sourceOpen, setSourceOpen] = useState(false)
  const [storeOpen, setStoreOpen] = useState(false)
  const [sourceUrl, setSourceUrl] = useState('')
  const [sourceRef, setSourceRef] = useState('')
  const [inspection, setInspection] = useState<PluginInspection | null>(null)
  const [inspecting, setInspecting] = useState(false)
  const [installing, setInstalling] = useState(false)
  const [configuring, setConfiguring] = useState<PluginStatus | null>(null)
  const [configText, setConfigText] = useState('{}')
  const [configLoading, setConfigLoading] = useState(false)
  const [configSaving, setConfigSaving] = useState(false)

  const loadPlugins = () => {
    setLoading(true)
    void api
      .plugins()
      .then(setPlugins)
      .catch((error) => toast(errorMessage(error), 'error'))
      .finally(() => setLoading(false))
  }

  useEffect(() => {
    loadPlugins()
  }, [])

  const inspectSource = async (item: { sourceUrl: string; ref?: string; sourceDigest?: string }) => {
    setInspecting(true)
    try {
      const result = await api.inspectPlugin(item)
      setInspection(result)
      setSourceOpen(false)
    } catch (error) {
      toast(errorMessage(error), 'error')
    } finally {
      setInspecting(false)
    }
  }

  const installInspected = async () => {
    if (!inspection) return
    setInstalling(true)
    try {
      const installed = await api.installPlugin(inspection.inspectionId)
      setPlugins((current) => [installed, ...current.filter((item) => item.id !== installed.id)])
      setInspection(null)
      toast('插件已启用', 'success')
    } catch (error) {
      toast(errorMessage(error), 'error')
    } finally {
      setInstalling(false)
    }
  }

  const setEnabled = async (plugin: PluginStatus, enabled: boolean) => {
    try {
      const updated = await api.setPluginEnabled(plugin.id, enabled)
      setPlugins((current) => current.map((item) => (item.id === updated.id ? updated : item)))
    } catch (error) {
      toast(errorMessage(error), 'error')
    }
  }

  const uninstall = async (plugin: PluginStatus) => {
    if (!window.confirm(`卸载 ${plugin.manifest.name}？`)) return
    try {
      await api.uninstallPlugin(plugin.id)
      setPlugins((current) => current.filter((item) => item.id !== plugin.id))
      if (configuring?.id === plugin.id) setConfiguring(null)
    } catch (error) {
      toast(errorMessage(error), 'error')
    }
  }

  const openConfiguration = async (plugin: PluginStatus) => {
    setConfiguring(plugin)
    setConfigLoading(true)
    try {
      const settings = await api.pluginSettings(plugin.id)
      setConfigText(JSON.stringify(settings, null, 2))
    } catch (error) {
      setConfigText('{}')
      toast(errorMessage(error), 'error')
    } finally {
      setConfigLoading(false)
    }
  }

  const saveConfiguration = async () => {
    if (!configuring) return
    let settings: Record<string, unknown>
    try {
      const parsed: unknown = JSON.parse(configText)
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('配置必须是 JSON 对象')
      settings = parsed as Record<string, unknown>
    } catch (error) {
      toast(errorMessage(error), 'error')
      return
    }
    setConfigSaving(true)
    try {
      await api.updatePluginSettings(configuring.id, settings)
      setConfiguring(null)
      toast('已保存', 'success')
    } catch (error) {
      toast(errorMessage(error), 'error')
    } finally {
      setConfigSaving(false)
    }
  }

  return (
    <>
      <Section
        icon={<Blocks size={16} />}
        title="插件"
        actions={
          <div className="flex gap-2">
            <Button icon={<PackageOpen size={15} />} onClick={() => setStoreOpen(true)}>
              商店
            </Button>
            <Button variant="primary" icon={<Plus size={15} />} onClick={() => setSourceOpen(true)}>
              源码安装
            </Button>
          </div>
        }
      >
        {loading ? (
          <div className="flex justify-center py-6"><Spinner /></div>
        ) : plugins.length === 0 ? (
          <div className="py-6 text-center text-sm text-[var(--muted)]">暂无插件</div>
        ) : (
          <div className="divide-y divide-[var(--line)]">
            {plugins.map((plugin) => (
              <PluginRow
                key={plugin.id}
                plugin={plugin}
                onEnabled={(enabled) => void setEnabled(plugin, enabled)}
                onConfigure={() => void openConfiguration(plugin)}
                onUninstall={() => void uninstall(plugin)}
              />
            ))}
          </div>
        )}
      </Section>

      <div className="flex justify-end">
        <IconButton label="刷新插件" onClick={loadPlugins}>
          <RefreshCw size={15} />
        </IconButton>
      </div>

      <SourceModal
        open={sourceOpen}
        onClose={() => setSourceOpen(false)}
        sourceUrl={sourceUrl}
        sourceRef={sourceRef}
        onSourceUrlChange={setSourceUrl}
        onSourceRefChange={setSourceRef}
        onInspect={() => void inspectSource({ sourceUrl, ref: sourceRef || undefined })}
        loading={inspecting}
      />

      <InspectionModal
        inspection={inspection}
        onClose={() => setInspection(null)}
        onInstall={() => void installInspected()}
        loading={installing}
      />

      <StoreDrawer
        open={storeOpen}
        onClose={() => setStoreOpen(false)}
        onInspect={(item) =>
          void inspectSource({ sourceUrl: item.repositoryUrl, ref: item.ref, sourceDigest: item.sourceDigest })
        }
        loading={inspecting}
      />

      <Modal
        open={configuring !== null}
        onClose={() => setConfiguring(null)}
        title={configuring ? `${configuring.manifest.name} 配置` : '插件配置'}
        footer={
          <>
            <Button onClick={() => setConfiguring(null)}>取消</Button>
            <Button variant="primary" loading={configSaving} onClick={() => void saveConfiguration()}>
              保存
            </Button>
          </>
        }
      >
        {configLoading ? (
          <div className="flex justify-center py-8"><Spinner /></div>
        ) : (
          <textarea
            value={configText}
            onChange={(event) => setConfigText(event.target.value)}
            spellCheck={false}
            className="input min-h-48 w-full resize-y font-[family-name:var(--font-mono)] text-xs"
            aria-label="插件配置 JSON"
          />
        )}
      </Modal>
    </>
  )
}

function PluginRow({
  plugin,
  onEnabled,
  onConfigure,
  onUninstall,
}: {
  plugin: PluginStatus
  onEnabled: (enabled: boolean) => void
  onConfigure: () => void
  onUninstall: () => void
}) {
  const route = plugin.manifest.routes?.find((item) => item.ui)
  return (
    <div className="flex flex-wrap items-center gap-3 py-3 first:pt-0 last:pb-0">
      <StatusDot tone={statusTone(plugin.status)} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
          <span className="truncate text-sm font-medium">{plugin.manifest.name}</span>
          <span className="text-xs text-[var(--faint)]">v{plugin.manifest.version}</span>
        </div>
        <div className="mt-0.5 truncate text-xs text-[var(--muted)]">
          {plugin.manifest.author} · {plugin.status}
          {plugin.error ? ` · ${plugin.error}` : ''}
        </div>
      </div>
      {route && (
        <a
          href={`/plugins/${encodeURIComponent(plugin.id)}${route.path.replace(/\/\*$/, '')}`}
          className="btn btn-ghost !px-2 !py-1.5 text-xs"
        >
          <ExternalLink size={14} />
          打开
        </a>
      )}
      <Switch checked={plugin.enabled} onChange={onEnabled} label={plugin.enabled ? '启用' : '停用'} />
      <div className="flex gap-1">
        <IconButton label="配置" onClick={onConfigure}>
          <Power size={15} />
        </IconButton>
        <IconButton label="卸载" onClick={onUninstall} className="text-[var(--color-danger)]">
          <Trash2 size={15} />
        </IconButton>
      </div>
    </div>
  )
}

function SourceModal({
  open,
  onClose,
  sourceUrl,
  sourceRef,
  onSourceUrlChange,
  onSourceRefChange,
  onInspect,
  loading,
}: {
  open: boolean
  onClose: () => void
  sourceUrl: string
  sourceRef: string
  onSourceUrlChange: (value: string) => void
  onSourceRefChange: (value: string) => void
  onInspect: () => void
  loading: boolean
}) {
  return (
    <Modal
      open={open}
      onClose={onClose}
      title="源码安装"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" loading={loading} disabled={!sourceUrl.trim()} onClick={onInspect}>
            检查
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <Field label="源码地址">
          <Input
            value={sourceUrl}
            onChange={(event) => onSourceUrlChange(event.target.value)}
            placeholder="https://github.com/owner/plugin"
            autoComplete="url"
          />
        </Field>
        <Field label="版本或提交" hint="留空使用默认分支">
          <Input
            value={sourceRef}
            onChange={(event) => onSourceRefChange(event.target.value)}
            placeholder="v1.0.0 或 commit"
            spellCheck={false}
          />
        </Field>
      </div>
    </Modal>
  )
}

function InspectionModal({
  inspection,
  onClose,
  onInstall,
  loading,
}: {
  inspection: PluginInspection | null
  onClose: () => void
  onInstall: () => void
  loading: boolean
}) {
  const manifest = inspection?.manifest
  return (
    <Modal
      open={inspection !== null}
      onClose={onClose}
      title={manifest ? `${manifest.name} ${manifest.version}` : '检查结果'}
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" loading={loading} onClick={onInstall}>
            确认安装
          </Button>
        </>
      }
    >
      {inspection && manifest && (
        <div className="space-y-4 text-sm">
          <div className="grid grid-cols-2 gap-3">
            <Info label="作者" value={manifest.author} />
            <Info label="许可证" value={manifest.license} />
            <Info label="API" value={String(manifest.apiVersion)} />
            <Info label="摘要" value={inspection.sourceDigest.slice(0, 16)} mono />
          </div>
          {manifest.description && <p className="text-[var(--muted)]">{manifest.description}</p>}
          {manifest.capabilities && manifest.capabilities.length > 0 && (
            <div className="flex flex-wrap gap-1.5">
              {manifest.capabilities.map((capability) => <span key={capability} className="chip">{capability}</span>)}
            </div>
          )}
          <div className="rounded-[var(--radius-control)] bg-[var(--clay-soft)] px-3 py-2 text-xs text-[var(--color-clay)]">
            {inspection.warning || '全信任插件将使用 tdrive 公开的全部功能。'}
          </div>
        </div>
      )}
    </Modal>
  )
}

function Info({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2">
      <div className="text-[11px] text-[var(--faint)]">{label}</div>
      <div className={`mt-0.5 truncate text-sm ${mono ? 'font-[family-name:var(--font-mono)]' : ''}`}>{value}</div>
    </div>
  )
}

function StoreDrawer({
  open,
  onClose,
  onInspect,
  loading,
}: {
  open: boolean
  onClose: () => void
  onInspect: (item: PluginStoreItem) => void
  loading: boolean
}) {
  const [query, setQuery] = useState('')
  const [items, setItems] = useState<PluginStoreItem[]>([])
  const [fetching, setFetching] = useState(false)

  useEffect(() => {
    if (!open) return
    let cancelled = false
    setFetching(true)
    void api
      .pluginStore(query)
      .then((result) => {
        if (!cancelled) setItems(result.plugins)
      })
      .catch((error) => {
        if (!cancelled) toast(errorMessage(error), 'error')
      })
      .finally(() => {
        if (!cancelled) setFetching(false)
      })
    return () => {
      cancelled = true
    }
  }, [open, query])

  return (
    <Drawer open={open} onClose={onClose} title="插件商店" width="sm:max-w-lg">
      <div className="space-y-4">
        <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索" autoFocus />
        {fetching ? (
          <div className="flex justify-center py-8"><Spinner /></div>
        ) : items.length === 0 ? (
          <div className="py-8 text-center text-sm text-[var(--muted)]">暂无插件</div>
        ) : (
          <div className="space-y-2">
            {items.map((item) => (
              <div key={item.id} className="panel flex items-center gap-3 p-3">
                <div className="min-w-0 flex-1">
                  <div className="flex items-baseline gap-2">
                    <span className="truncate text-sm font-medium">{item.name}</span>
                    <span className="text-xs text-[var(--faint)]">v{item.version}</span>
                  </div>
                  <div className="mt-0.5 truncate text-xs text-[var(--muted)]">{item.author}</div>
                  {item.description && <p className="mt-1 line-clamp-2 text-xs text-[var(--muted)]">{item.description}</p>}
                </div>
                <Button loading={loading} onClick={() => onInspect(item)}>安装</Button>
              </div>
            ))}
          </div>
        )}
      </div>
    </Drawer>
  )
}
