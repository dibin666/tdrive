import { useCallback, useEffect, useState } from 'react'
import {
  ArrowUpCircle,
  Blocks,
  PackageOpen,
  PanelsTopLeft,
  Plus,
  Power,
  RefreshCw,
  Trash2,
} from 'lucide-react'
import {
  api,
  uiRoute,
  type PluginInspection,
  type PluginStatus,
  type PluginStoreItem,
  type PluginUpdate,
} from '../../lib/api'
import { useApp } from '../../app/context'
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

export function PluginsPage({ onNavigate }: { onNavigate: (to: string) => void }) {
  // The sidebar reads the same list from context, so anything that changes an
  // installation here has to tell it.
  const { refreshPlugins } = useApp()
  const [plugins, setPlugins] = useState<PluginStatus[]>([])
  const [updates, setUpdates] = useState<Record<string, PluginUpdate>>({})
  const [checking, setChecking] = useState(false)
  const [loading, setLoading] = useState(true)
  const [installOpen, setInstallOpen] = useState(false)
  const [storeOpen, setStoreOpen] = useState(false)
  const [manifestUrl, setManifestUrl] = useState('')
  const [inspection, setInspection] = useState<PluginInspection | null>(null)
  const [inspecting, setInspecting] = useState(false)
  const [installing, setInstalling] = useState(false)
  const [configuring, setConfiguring] = useState<PluginStatus | null>(null)
  const [configText, setConfigText] = useState('{}')
  const [configLoading, setConfigLoading] = useState(false)
  const [configSaving, setConfigSaving] = useState(false)

  /**
   * checkUpdates asks which installed plugins have a newer release.
   *
   * It runs on load rather than only on demand, because an update nobody looks
   * for is an update nobody installs — which is exactly what a page with no
   * indication of a new version leaves you with. The server caches the answer,
   * so arriving at this page does not re-fetch every manifest; `refresh` is what
   * the button next to it passes.
   */
  const checkUpdates = useCallback(async (refresh: boolean) => {
    setChecking(true)
    try {
      const report = await api.pluginUpdates(refresh)
      setUpdates(Object.fromEntries(report.plugins.map((item) => [item.id, item])))
      if (refresh) {
        toast(
          report.available > 0 ? `${report.available} 个插件可更新` : '所有插件均为最新版本',
          report.available > 0 ? 'info' : 'success',
        )
      }
      if (report.storeError && refresh) toast(`无法连接插件商店：${report.storeError}`, 'error')
    } catch (error) {
      // A failed check leaves the list usable; only an explicit one is worth
      // interrupting for.
      if (refresh) toast(errorMessage(error), 'error')
    } finally {
      setChecking(false)
    }
  }, [])

  const loadPlugins = useCallback(() => {
    setLoading(true)
    void api
      .plugins()
      .then(setPlugins)
      .catch((error) => toast(errorMessage(error), 'error'))
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    loadPlugins()
    void checkUpdates(false)
  }, [loadPlugins, checkUpdates])

  const inspectManifest = async (item: { manifestUrl: string; manifestDigest?: string }) => {
    setInspecting(true)
    try {
      const result = await api.inspectPlugin(item)
      setInspection(result)
      setInstallOpen(false)
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
      void refreshPlugins()
      toast(inspection.isUpdate ? `已更新至 v${inspection.manifest.version}` : '插件已安装并启用', 'success')
      // The report still offers the version that was just installed.
      void checkUpdates(true)
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
      void refreshPlugins()
    } catch (error) {
      toast(errorMessage(error), 'error')
    }
  }

  const uninstall = async (plugin: PluginStatus) => {
    if (!window.confirm(`确定卸载插件 ${plugin.manifest.name}？`)) return
    try {
      await api.uninstallPlugin(plugin.id)
      setPlugins((current) => current.filter((item) => item.id !== plugin.id))
      setUpdates((current) => {
        const next = { ...current }
        delete next[plugin.id]
        return next
      })
      if (configuring?.id === plugin.id) setConfiguring(null)
      void refreshPlugins()
    } catch (error) {
      toast(errorMessage(error), 'error')
    }
  }

  /** Updating reuses the installation flow rather than shortcutting it: the
   *  same manifest review and the same one confirmation, because new bytes are
   *  new bytes whether or not an older version of the plugin is already here. */
  const startUpdate = (update: PluginUpdate) => {
    if (!update.manifestUrl) {
      toast('该插件缺少清单地址，请手动安装更新', 'error')
      return
    }
    void inspectManifest({ manifestUrl: update.manifestUrl, manifestDigest: update.manifestDigest })
  }

  const availableUpdates = Object.values(updates).filter((item) => item.available).length

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
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('配置内容必须是 JSON 对象')
      settings = parsed as Record<string, unknown>
    } catch (error) {
      toast(errorMessage(error), 'error')
      return
    }
    setConfigSaving(true)
    try {
      await api.updatePluginSettings(configuring.id, settings)
      setConfiguring(null)
      toast('配置已保存', 'success')
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
        title="插件管理"
        actions={
          <div className="flex flex-wrap gap-2">
            <Button
              icon={<ArrowUpCircle size={15} />}
              loading={checking}
              onClick={() => void checkUpdates(true)}
            >
              检查更新
            </Button>
            <Button icon={<PackageOpen size={15} />} onClick={() => setStoreOpen(true)}>
              插件商店
            </Button>
            <Button variant="primary" icon={<Plus size={15} />} onClick={() => setInstallOpen(true)}>
              安装插件
            </Button>
          </div>
        }
      >
        {availableUpdates > 0 && (
          <div className="mb-3 flex items-center gap-2 rounded-[var(--radius-control)] bg-[var(--clay-soft)] px-3 py-2 text-xs text-[var(--color-clay)]">
            <ArrowUpCircle size={14} className="shrink-0" />
            {availableUpdates} 个插件有新版本可用，点击「更新」即可安装。
          </div>
        )}

        {loading ? (
          <div className="flex justify-center py-6"><Spinner /></div>
        ) : plugins.length === 0 ? (
          <div className="py-6 text-center text-sm text-[var(--muted)]">未安装插件</div>
        ) : (
          <div className="divide-y divide-[var(--line)]">
            {plugins.map((plugin) => (
              <PluginRow
                key={plugin.id}
                plugin={plugin}
                update={updates[plugin.id]}
                onEnabled={(enabled) => void setEnabled(plugin, enabled)}
                onOpen={() => onNavigate(`/plugin/${encodeURIComponent(plugin.id)}`)}
                onConfigure={() => void openConfiguration(plugin)}
                onUninstall={() => void uninstall(plugin)}
                onUpdate={() => updates[plugin.id] && startUpdate(updates[plugin.id])}
              />
            ))}
          </div>
        )}
      </Section>

      <div className="flex justify-end">
        <IconButton
          label="刷新列表"
          onClick={() => {
            loadPlugins()
            void checkUpdates(true)
          }}
        >
          <RefreshCw size={15} />
        </IconButton>
      </div>

      <InstallModal
        open={installOpen}
        onClose={() => setInstallOpen(false)}
        manifestUrl={manifestUrl}
        onManifestUrlChange={setManifestUrl}
        onInspect={() => void inspectManifest({ manifestUrl })}
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
          void inspectManifest({ manifestUrl: item.manifestUrl, manifestDigest: item.manifestDigest })
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
  update,
  onEnabled,
  onOpen,
  onConfigure,
  onUninstall,
  onUpdate,
}: {
  plugin: PluginStatus
  update?: PluginUpdate
  onEnabled: (enabled: boolean) => void
  onOpen: () => void
  onConfigure: () => void
  onUninstall: () => void
  onUpdate: () => void
}) {
  const hasUI = uiRoute(plugin) !== null
  return (
    <div className="flex flex-wrap items-center gap-3 py-3 first:pt-0 last:pb-0">
      <StatusDot tone={statusTone(plugin.status)} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
          <span className="truncate text-sm font-medium">{plugin.manifest.name}</span>
          <span className="text-xs text-[var(--faint)]">v{plugin.manifest.version}</span>
          {update?.available && (
            <span className="chip !border-transparent !bg-[var(--clay-soft)] !text-[var(--color-clay)]">
              新版本 v{update.latestVersion}
            </span>
          )}
        </div>
        <div className="mt-0.5 truncate text-xs text-[var(--muted)]">
          {plugin.manifest.author} · {plugin.status}
          {plugin.error ? ` · ${plugin.error}` : ''}
          {update?.error ? ` · 检查更新失败：${update.error}` : ''}
        </div>
      </div>
      {update?.available && (
        <button
          type="button"
          onClick={onUpdate}
          className="btn btn-primary !px-2 !py-1.5 text-xs"
          title={`更新到 v${update.latestVersion}`}
        >
          <ArrowUpCircle size={14} />
          更新
        </button>
      )}
      {hasUI && (
        <button type="button" onClick={onOpen} className="btn btn-ghost !px-2 !py-1.5 text-xs">
          <PanelsTopLeft size={14} />
          打开
        </button>
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

function InstallModal({
  open,
  onClose,
  manifestUrl,
  onManifestUrlChange,
  onInspect,
  loading,
}: {
  open: boolean
  onClose: () => void
  manifestUrl: string
  onManifestUrlChange: (value: string) => void
  onInspect: () => void
  loading: boolean
}) {
  return (
    <Modal
      open={open}
      onClose={onClose}
      title="安装插件"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" loading={loading} disabled={!manifestUrl.trim()} onClick={onInspect}>
            检查清单
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <Field label="清单地址" hint="指向 tdrive.plugin.json 的 Release 资源链接">
          <Input
            value={manifestUrl}
            onChange={(event) => onManifestUrlChange(event.target.value)}
            placeholder="https://github.com/owner/plugin/releases/download/v1.0.0/tdrive.plugin.json"
            autoComplete="url"
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
            {inspection?.isUpdate ? '确认更新' : '确认安装'}
          </Button>
        </>
      }
    >
      {inspection && manifest && (
        <div className="space-y-4 text-sm">
          {inspection.isUpdate && (
            <p className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2 text-xs text-[var(--muted)]">
              当前版本 v{inspection.currentVersion || '未知'}，将替换为 v{manifest.version}。
              插件将自动重启，保留既有配置与数据。
            </p>
          )}
          <div className="grid grid-cols-2 gap-3">
            <Info label="作者" value={manifest.author} />
            <Info label="许可证" value={manifest.license} />
            <Info label="平台" value={inspection.platform} mono />
            <Info label="二进制摘要" value={inspection.binaryDigest.slice(0, 16)} mono />
          </div>
          {manifest.description && <p className="text-[var(--muted)]">{manifest.description}</p>}
          {manifest.capabilities && manifest.capabilities.length > 0 && (
            <div className="flex flex-wrap gap-1.5">
              {manifest.capabilities.map((capability) => <span key={capability} className="chip">{capability}</span>)}
            </div>
          )}
          <div className="rounded-[var(--radius-control)] bg-[var(--clay-soft)] px-3 py-2 text-xs text-[var(--color-clay)]">
            {inspection.warning || '插件具备宿主完全访问权限，请确保来源可信。'}
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
        <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索插件" autoFocus />
        {fetching ? (
          <div className="flex justify-center py-8"><Spinner /></div>
        ) : items.length === 0 ? (
          <div className="py-8 text-center text-sm text-[var(--muted)]">未找到匹配插件</div>
        ) : (
          <div className="space-y-2">
            {items.map((item) => (
              <div key={item.id} className="panel flex items-center gap-3 p-3">
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                    <span className="truncate text-sm font-medium">{item.name}</span>
                    <span className="text-xs text-[var(--faint)]">v{item.version}</span>
                    {/* The marker is about this account only: somebody else
                        having installed a plugin says nothing about whether
                        this account has it. */}
                    {item.installed && (
                      <span className="chip">已安装 v{item.installedVersion}</span>
                    )}
                  </div>
                  <div className="mt-0.5 truncate text-xs text-[var(--muted)]">{item.author}</div>
                  {item.description && <p className="mt-1 line-clamp-2 text-xs text-[var(--muted)]">{item.description}</p>}
                </div>
                <Button loading={loading} onClick={() => onInspect(item)}>
                  {item.installed ? '重新安装' : '安装'}
                </Button>
              </div>
            ))}
          </div>
        )}
      </div>
    </Drawer>
  )
}
