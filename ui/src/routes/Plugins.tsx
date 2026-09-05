import { useEffect, useRef } from 'react'
import { Blocks, ChevronRight, RefreshCw } from 'lucide-react'
import { can, pluginFrameSrc, uiRoute, type PluginStatus } from '../lib/api'
import { useApp } from '../app/context'
import { Button, IconButton } from '../components/primitives'

/**
 * PluginView renders a plugin's own page inside the shell.
 *
 * It replaces the fixed overlay that used to open from Settings. Living in
 * <main> means the sidebar stays put, the plugin's row keeps its highlight, and
 * the URL is a real destination somebody can bookmark or reload — none of which
 * an overlay could offer. The frame is same-origin, so the plugin page reads
 * the theme and the compiled stylesheet, and its requests carry the session
 * cookie.
 */
export function PluginView({ id, onNavigate }: { id: string; onNavigate: (to: string) => void }) {
  const { plugins } = useApp()
  const frame = useRef<HTMLIFrameElement>(null)
  const plugin = plugins.find((item) => item.id === id)

  if (!plugin || !plugin.enabled || uiRoute(plugin) === null) {
    // A bookmark that outlived its plugin would otherwise render a blank
    // frame, which looks like the plugin is broken rather than gone.
    return (
      <div className="flex h-full flex-col items-center justify-center gap-3 px-6 text-center">
        <Blocks size={28} className="text-[var(--faint)]" />
        <p className="text-sm text-[var(--muted)]">未找到该插件，或该插件已停用。</p>
        <Button onClick={() => onNavigate('/plugin')}>返回插件列表</Button>
      </div>
    )
  }

  const source = pluginFrameSrc(plugin)
  return (
    <div className="flex h-full min-h-0 flex-1 flex-col">
      <header className="flex shrink-0 items-center gap-3 border-b border-[var(--line)] px-4 py-2.5">
        <Blocks size={16} className="text-[var(--color-clay)]" />
        <div className="min-w-0 flex-1">
          <span className="truncate text-sm font-medium">{plugin.manifest.name}</span>
          <span className="ml-2 text-xs text-[var(--faint)]">v{plugin.manifest.version}</span>
        </div>
        <IconButton
          label="刷新"
          onClick={() => {
            // Reassigning src rather than calling location.reload() keeps this
            // working regardless of what the plugin navigated to inside.
            if (frame.current) frame.current.src = source
          }}
        >
          <RefreshCw size={15} />
        </IconButton>
      </header>
      <iframe
        ref={frame}
        src={source}
        title={plugin.manifest.name}
        className="min-h-0 w-full flex-1 border-0"
      />
    </div>
  )
}

/**
 * PluginList is where the mobile 插件 tab lands.
 *
 * Phones get a bottom bar instead of a sidebar, so there is nowhere to list
 * plugins inline; this is that list. It is reachable on desktop too, which is
 * what makes /plugin a real route rather than a mobile special case.
 */
export function PluginList({ onNavigate }: { onNavigate: (to: string) => void }) {
  const { plugins, user, refreshPlugins } = useApp()

  // The list is the point of this page, so it is worth a re-read on arrival
  // rather than trusting whatever the last install left in context.
  useEffect(() => {
    void refreshPlugins()
  }, [refreshPlugins])

  const openable = plugins.filter((plugin) => plugin.enabled && uiRoute(plugin) !== null)

  return (
    <div className="h-full min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto w-full max-w-2xl px-4 py-6 sm:px-6">
        <header className="mb-6">
          <h1 className="display text-xl">插件</h1>
          <p className="mt-1 text-sm text-[var(--muted)]">
            仅显示当前账号已安装的插件。
          </p>
        </header>

        {openable.length === 0 ? (
          <div className="flex flex-col items-center gap-3 py-12 text-center">
            <Blocks size={28} className="text-[var(--faint)]" />
            <p className="text-sm text-[var(--muted)]">
              {plugins.length === 0 ? '尚未安装任何插件。' : '已安装插件均无操作界面。'}
            </p>
            {can(user, 'plugins') && (
              <Button onClick={() => onNavigate('/settings')}>去安装</Button>
            )}
          </div>
        ) : (
          <div className="divide-y divide-[var(--line)]">
            {openable.map((plugin) => (
              <PluginRow
                key={plugin.id}
                plugin={plugin}
                onOpen={() => onNavigate(`/plugin/${encodeURIComponent(plugin.id)}`)}
              />
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

function PluginRow({ plugin, onOpen }: { plugin: PluginStatus; onOpen: () => void }) {
  return (
    <button onClick={onOpen} className="row w-full !justify-start !py-3 text-left text-sm">
      <Blocks size={16} className="shrink-0 text-[var(--color-clay)]" />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2">
          <span className="truncate font-medium">{plugin.manifest.name}</span>
          <span className="text-xs text-[var(--faint)]">v{plugin.manifest.version}</span>
        </div>
        {plugin.manifest.description && (
          <p className="mt-0.5 line-clamp-1 text-xs text-[var(--muted)]">
            {plugin.manifest.description}
          </p>
        )}
      </div>
      <ChevronRight size={15} className="shrink-0 text-[var(--faint)]" />
    </button>
  )
}
