import { useEffect, useState, type ReactNode } from 'react'
import clsx from 'clsx'
import { ArrowUpDown, ChevronRight, FolderOpen, Moon, Settings as Cog, Sun } from 'lucide-react'
import { api, type Entry } from '../lib/api'
import { events } from '../lib/events'
import { useApp } from '../app/context'
import { uploads, type Transfer } from '../lib/uploads'
import { Logo } from '../routes/Setup'
import { EntryIcon } from './icons'

type Nav = 'files' | 'transfers' | 'settings'

const NAV: { id: Nav; label: string; icon: typeof FolderOpen; href: string }[] = [
  { id: 'files', label: '文件', icon: FolderOpen, href: '/files' },
  { id: 'transfers', label: '传输', icon: ArrowUpDown, href: '/transfers' },
  { id: 'settings', label: '设置', icon: Cog, href: '/settings' },
]

/**
 * The application chrome.
 *
 * Three layouts from one tree: a persistent tree sidebar on wide screens, an
 * icon rail on tablets, and a bottom tab bar on phones. The main content is
 * identical in all three, so nothing is duplicated per breakpoint.
 */
export function Shell({
  active,
  path,
  onNavigate,
  children,
}: {
  active: Nav
  path: string
  onNavigate: (to: string) => void
  children: ReactNode
}) {
  const { theme, toggleTheme } = useApp()
  const [transfers, setTransfers] = useState<Transfer[]>([])

  useEffect(() => uploads.subscribe(setTransfers), [])
  const busy = transfers.filter((t) => t.state === 'uploading').length

  return (
    <div className="flex h-full min-h-0 overflow-hidden">
      <aside className="hidden w-60 shrink-0 flex-col border-r border-[var(--line)] md:flex">
        <div className="px-4 py-4">
          <button onClick={() => onNavigate('/files')} className="text-left">
            <Logo />
          </button>
        </div>

        <nav className="space-y-0.5 px-2">
          {NAV.map((item) => (
            <button
              key={item.id}
              onClick={() => onNavigate(item.href)}
              className={clsx(
                'row w-full !justify-start text-sm',
                active === item.id && 'bg-[var(--sunk)] font-medium',
              )}
            >
              <item.icon size={16} className={active === item.id ? 'text-[var(--color-clay)]' : 'text-[var(--faint)]'} />
              {item.label}
              {item.id === 'transfers' && busy > 0 && (
                <span className="ml-auto chip !bg-[var(--clay-soft)] !text-[var(--color-clay)] !border-transparent">
                  {busy}
                </span>
              )}
            </button>
          ))}
        </nav>

        <div className="mt-4 min-h-0 flex-1 overflow-y-auto px-2 pb-4">
          <FolderTree currentPath={path} onNavigate={onNavigate} />
        </div>

        <div className="border-t border-[var(--line)] p-2">
          <button onClick={toggleTheme} className="row w-full !justify-start text-sm text-[var(--muted)]">
            {theme === 'dark' ? <Moon size={15} /> : <Sun size={15} />}
            {theme === 'dark' ? '深色' : '浅色'}
          </button>
        </div>
      </aside>

      <main className="flex min-w-0 flex-1 flex-col min-h-0 overflow-hidden pb-14 md:pb-0">{children}</main>

      {/* Phones get a bottom bar; a sidebar at that width steals too much of
          the listing, and thumbs live at the bottom of the screen. */}
      <nav className="fixed inset-x-0 bottom-0 z-30 flex border-t border-[var(--line)] bg-[var(--bg)]/95 backdrop-blur pb-safe md:hidden">
        {NAV.map((item) => (
          <button
            key={item.id}
            onClick={() => onNavigate(item.href)}
            className={clsx(
              'flex flex-1 flex-col items-center gap-0.5 py-2 text-[11px] transition-colors',
              active === item.id ? 'text-[var(--color-clay)]' : 'text-[var(--faint)]',
            )}
          >
            <span className="relative">
              <item.icon size={19} />
              {item.id === 'transfers' && busy > 0 && (
                <span className="absolute -right-1.5 -top-0.5 size-1.5 rounded-full bg-[var(--color-clay)]" />
              )}
            </span>
            {item.label}
          </button>
        ))}
      </nav>
    </div>
  )
}

/**
 * A lazily expanded folder tree. Only the folders someone opens are fetched,
 * which matters because a drive can hold far more directories than are worth
 * loading to draw a sidebar.
 */
function FolderTree({
  currentPath,
  onNavigate,
}: {
  currentPath: string
  onNavigate: (to: string) => void
}) {
  return (
    <div>
      <div className="px-2 pb-1.5 text-[11px] font-medium uppercase tracking-wide text-[var(--faint)]">
        目录
      </div>
      <TreeNode path="/" depth={0} currentPath={currentPath} onNavigate={onNavigate} defaultOpen />
    </div>
  )
}

function TreeNode({
  path,
  depth,
  currentPath,
  onNavigate,
  defaultOpen = false,
}: {
  path: string
  depth: number
  currentPath: string
  onNavigate: (to: string) => void
  defaultOpen?: boolean
}) {
  const [open, setOpen] = useState(defaultOpen)
  const [children, setChildren] = useState<Entry[] | null>(null)

  useEffect(() => {
    if (!open) return
    let cancelled = false
    const load = () =>
      void api
        .list(path)
        .then((r) => {
          if (!cancelled) setChildren(r.entries.filter((e) => e.isDir))
        })
        .catch(() => {})
    load()
    return events.subscribe((event) => {
      if (event.type === 'tree') load()
    })
  }, [open, path])

  const name = path === '/' ? '全部文件' : path.slice(path.lastIndexOf('/') + 1)
  const selected = currentPath === path

  return (
    <div>
      <div
        className={clsx('row !py-1.5 cursor-pointer text-sm', selected && 'bg-[var(--sunk)] font-medium')}
        style={{ paddingLeft: `${0.5 + depth * 0.75}rem` }}
        onClick={() => onNavigate(`/files${path === '/' ? '' : path}`)}
      >
        <button
          onClick={(e) => {
            e.stopPropagation()
            setOpen((v) => !v)
          }}
          aria-label={open ? '折叠' : '展开'}
          className="shrink-0 text-[var(--faint)]"
        >
          <ChevronRight
            size={13}
            className={clsx('transition-transform duration-150', open && 'rotate-90')}
          />
        </button>
        <EntryIcon name={name} isDir size={15} />
        <span className="truncate">{name}</span>
      </div>

      {open &&
        children?.map((child) => (
          <TreeNode
            key={child.path}
            path={child.path}
            depth={depth + 1}
            currentPath={currentPath}
            onNavigate={onNavigate}
          />
        ))}
    </div>
  )
}
