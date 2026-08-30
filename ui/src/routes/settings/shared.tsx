import type { ReactNode } from 'react'
import clsx from 'clsx'

/** Section is the card every settings block sits in. */
export function Section({
  icon,
  title,
  description,
  actions,
  children,
  className,
}: {
  icon?: ReactNode
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <section className={clsx('panel p-5', className)}>
      <header className="mb-4 flex items-start gap-2.5">
        {icon && <span className="mt-0.5 text-[var(--faint)]">{icon}</span>}
        <div className="min-w-0 flex-1">
          <h2 className="display text-base">{title}</h2>
          {description && (
            <p className="mt-0.5 text-xs leading-relaxed text-[var(--muted)]">{description}</p>
          )}
        </div>
        {actions && <div className="shrink-0">{actions}</div>}
      </header>
      {children}
    </section>
  )
}

export function Stat({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="rounded-[var(--radius-control)] bg-[var(--sunk)] px-3 py-2.5">
      <div className="text-[11px] text-[var(--faint)]">{label}</div>
      <div className="mt-0.5 text-sm font-medium tabular-nums">{value}</div>
      {hint && <div className="mt-0.5 text-[11px] text-[var(--faint)]">{hint}</div>}
    </div>
  )
}

export function Line({ label, value }: { label: ReactNode; value: ReactNode }) {
  return (
    <div className="flex justify-between gap-4 text-sm">
      <span className="shrink-0 text-[var(--muted)]">{label}</span>
      <span className="min-w-0 truncate text-right">{value}</span>
    </div>
  )
}

/** StatusDot is the connection light on the Telegram page. */
export function StatusDot({ tone }: { tone: 'ok' | 'warn' | 'error' | 'idle' | 'busy' }) {
  const colours: Record<string, string> = {
    ok: 'bg-[var(--color-success)]',
    warn: 'bg-[var(--color-warn)]',
    error: 'bg-[var(--color-danger)]',
    idle: 'bg-[var(--faint)]',
    busy: 'bg-[var(--color-clay)] animate-pulse',
  }
  return (
    <span className="relative flex size-2.5 shrink-0">
      {tone === 'ok' && (
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-[var(--color-success)] opacity-40" />
      )}
      <span className={clsx('relative inline-flex size-2.5 rounded-full', colours[tone])} />
    </span>
  )
}
