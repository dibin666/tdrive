import clsx from 'clsx'
import { Check, Loader2, Minus, X } from 'lucide-react'
import {
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
} from 'react'

type Variant = 'primary' | 'outline' | 'ghost' | 'danger'

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  loading?: boolean
  icon?: ReactNode
}

export function Button({
  variant = 'outline',
  loading,
  icon,
  className,
  children,
  disabled,
  ...rest
}: ButtonProps) {
  return (
    <button
      className={clsx('btn', `btn-${variant}`, className)}
      disabled={disabled || loading}
      {...rest}
    >
      {loading ? <Loader2 size={15} className="animate-spin" /> : icon}
      {children}
    </button>
  )
}

/** IconButton is the square, label-free variant used in dense toolbars. */
export function IconButton({
  className,
  label,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return (
    <button
      className={clsx('btn btn-ghost !px-2 !py-2', className)}
      aria-label={label}
      title={label}
      {...rest}
    />
  )
}

export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return <input className={clsx('input', className)} {...rest} />
}

/** Select wraps a native <select>, which is the right control on a phone and
 *  needs no keyboard handling of its own. */
export function Select({
  className,
  children,
  ...rest
}: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select className={clsx('input cursor-pointer', className)} {...rest}>
      {children}
    </select>
  )
}

/**
 * Switch is the on/off control used everywhere a checkbox would be, because a
 * settings page full of native checkboxes reads as a form to fill in rather
 * than as a set of things that are currently true.
 */
export function Switch({
  checked,
  onChange,
  disabled,
  label,
  hint,
}: {
  checked: boolean
  onChange: (next: boolean) => void
  disabled?: boolean
  label?: ReactNode
  hint?: ReactNode
}) {
  return (
    <label
      className={clsx(
        'flex items-start gap-3',
        disabled ? 'cursor-not-allowed opacity-50' : 'cursor-pointer',
      )}
    >
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        disabled={disabled}
        onClick={() => !disabled && onChange(!checked)}
        className={clsx(
          'relative mt-0.5 h-5 w-9 shrink-0 rounded-full transition-colors',
          checked ? 'bg-[var(--color-clay)]' : 'bg-[var(--line-strong)]',
        )}
      >
        {/* left-0 is not optional. Without a horizontal offset the knob falls
            back to its static position, and inside a <button> that is the
            centre of the element's own anonymous content box — which put the
            knob past the right edge of the track, where a white circle on a
            white page is simply invisible. The switch then had no visible
            state at all. */}
        <span
          className={clsx(
            'absolute top-0.5 left-0 size-4 rounded-full bg-white shadow-sm transition-transform',
            checked ? 'translate-x-[1.125rem]' : 'translate-x-0.5',
          )}
        />
      </button>
      {(label || hint) && (
        <span className="min-w-0">
          {label && <span className="block text-sm">{label}</span>}
          {hint && <span className="mt-0.5 block text-xs text-[var(--muted)]">{hint}</span>}
        </span>
      )}
    </label>
  )
}

/**
 * Checkbox is the multi-select control for lists.
 *
 * Switch is the right control for a setting that is on or off; a list needs the
 * other thing — a box you tick, with an indeterminate state for a "select all"
 * header that is only partly true. It is a button rather than a native input
 * because the platform control cannot show that third state without imperative
 * DOM work, and on a phone it is both too small to hit and the wrong shape.
 *
 * The tap target is deliberately larger than the box: 16px of drawn checkbox
 * inside a 36px hit area, which is the difference between selecting a file and
 * opening it by accident.
 */
export function Checkbox({
  checked,
  indeterminate,
  onChange,
  label,
  className,
}: {
  checked: boolean
  indeterminate?: boolean
  onChange: (next: boolean) => void
  label: string
  className?: string
}) {
  const on = checked || Boolean(indeterminate)
  return (
    <button
      type="button"
      role="checkbox"
      aria-checked={indeterminate ? 'mixed' : checked}
      aria-label={label}
      title={label}
      onClick={(e) => {
        // Inside a row that selects on click, the box must be the only thing
        // that reacts to a tap on the box.
        e.stopPropagation()
        onChange(!checked)
      }}
      onDoubleClick={(e) => e.stopPropagation()}
      onPointerDown={(e) => e.stopPropagation()}
      className={clsx(
        'flex size-9 shrink-0 items-center justify-center sm:size-6',
        className,
      )}
    >
      <span
        className={clsx(
          'flex size-4 items-center justify-center rounded-[4px] border transition-colors',
          on
            ? 'border-[var(--color-clay)] bg-[var(--color-clay)] text-white'
            : 'border-[var(--line-strong)]',
        )}
      >
        {indeterminate ? <Minus size={11} strokeWidth={3} /> : checked ? <Check size={11} strokeWidth={3} /> : null}
      </span>
    </button>
  )
}

/**
 * Slider pairs a range input with a number box.
 *
 * Both exist because they answer different questions: the track shows where a
 * value sits between its limits, which is what makes a tuning parameter
 * comprehensible, while the box is the only way to type an exact number.
 */
export function Slider({
  value,
  min,
  max,
  step = 1,
  onChange,
  disabled,
  suffix,
  format,
}: {
  value: number
  min: number
  max: number
  step?: number
  onChange: (next: number) => void
  disabled?: boolean
  suffix?: string
  /** Renders the derived explanation under the track. */
  format?: (value: number) => ReactNode
}) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center gap-3">
        <input
          type="range"
          min={min}
          max={max}
          step={step}
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(Number(e.target.value))}
          className="h-1.5 min-w-0 flex-1 cursor-pointer appearance-none rounded-full bg-[var(--sunk)] accent-[var(--color-clay)] disabled:cursor-not-allowed"
        />
        <div className="flex shrink-0 items-center gap-1.5">
          <input
            type="number"
            min={min}
            max={max}
            step={step}
            value={value}
            disabled={disabled}
            onChange={(e) => {
              const next = Number(e.target.value)
              if (Number.isFinite(next)) onChange(next)
            }}
            className="input w-24 !py-1.5 text-right tabular-nums"
          />
          {suffix && <span className="text-xs text-[var(--faint)]">{suffix}</span>}
        </div>
      </div>
      {format && <p className="text-xs text-[var(--muted)]">{format(value)}</p>}
    </div>
  )
}

/** Segmented is a small tab strip for mutually exclusive filters. */
export function Segmented<T extends string>({
  value,
  options,
  onChange,
  className,
}: {
  value: T
  options: { value: T; label: ReactNode; count?: number }[]
  onChange: (next: T) => void
  className?: string
}) {
  return (
    <div
      className={clsx(
        'inline-flex shrink-0 items-center rounded-[var(--radius-control)] border border-[var(--line)] p-0.5',
        className,
      )}
      role="tablist"
    >
      {options.map((option) => (
        <button
          key={option.value}
          role="tab"
          aria-selected={value === option.value}
          onClick={() => onChange(option.value)}
          className={clsx(
            'flex items-center gap-1.5 rounded-md px-2.5 py-1 text-xs font-medium transition-colors',
            value === option.value
              ? 'bg-[var(--sunk)] text-[var(--ink)]'
              : 'text-[var(--muted)] hover:text-[var(--ink)]',
          )}
        >
          {option.label}
          {option.count !== undefined && (
            <span className="tabular-nums text-[10px] text-[var(--faint)]">{option.count}</span>
          )}
        </button>
      ))}
    </div>
  )
}

/** Chip is a toggleable filter tag. */
export function Chip({
  active,
  onClick,
  children,
  tone,
}: {
  active?: boolean
  onClick?: () => void
  children: ReactNode
  tone?: 'clay' | 'blue' | 'green' | 'purple' | 'neutral'
}) {
  const tones: Record<string, string> = {
    clay: 'data-[on=true]:!bg-[var(--clay-soft)] data-[on=true]:!text-[var(--color-clay)]',
    blue: 'data-[on=true]:!bg-blue-500/12 data-[on=true]:!text-blue-600 dark:data-[on=true]:!text-blue-400',
    green: 'data-[on=true]:!bg-green-500/12 data-[on=true]:!text-green-700 dark:data-[on=true]:!text-green-400',
    purple: 'data-[on=true]:!bg-purple-500/12 data-[on=true]:!text-purple-600 dark:data-[on=true]:!text-purple-400',
    neutral: 'data-[on=true]:!bg-[var(--sunk)] data-[on=true]:!text-[var(--ink)]',
  }
  return (
    <button
      type="button"
      data-on={active ? 'true' : 'false'}
      onClick={onClick}
      className={clsx(
        'chip cursor-pointer transition-colors hover:text-[var(--ink)]',
        'data-[on=true]:!border-transparent',
        tones[tone ?? 'clay'],
      )}
    >
      {children}
    </button>
  )
}

/**
 * Drawer slides in from the right on desktop and up from the bottom on a
 * phone, which is where a side panel stops having room to exist.
 */
export function Drawer({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  width = 'sm:max-w-lg',
}: {
  open: boolean
  onClose: () => void
  title: ReactNode
  description?: ReactNode
  children?: ReactNode
  footer?: ReactNode
  width?: string
}) {
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50 flex items-end justify-end sm:items-stretch" role="dialog" aria-modal="true">
      <div className="absolute inset-0 bg-black/25 backdrop-blur-[2px] fade-in" onClick={onClose} aria-hidden />
      <div
        className={clsx(
          'relative flex w-full flex-col bg-[var(--surface)] shadow-xl',
          'max-h-[92vh] rounded-t-[var(--radius-panel)] sheet-in',
          'sm:h-full sm:max-h-none sm:rounded-none sm:border-l sm:border-[var(--line)] sm:slide-in-right',
          width,
        )}
      >
        <header className="flex items-start justify-between gap-4 border-b border-[var(--line)] px-5 py-4">
          <div className="min-w-0">
            <h2 className="display truncate text-base">{title}</h2>
            {description && <p className="mt-0.5 text-xs text-[var(--muted)]">{description}</p>}
          </div>
          <IconButton label="关闭" onClick={onClose}>
            <X size={16} />
          </IconButton>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">{children}</div>
        {footer && (
          <footer className="flex justify-end gap-2 border-t border-[var(--line)] px-5 py-3 pb-safe">
            {footer}
          </footer>
        )}
      </div>
    </div>
  )
}

/** Meter is a labelled usage bar, used for quotas and the download cache. */
export function Meter({
  value,
  max,
  label,
  caption,
  tone = 'clay',
}: {
  value: number
  max: number
  label?: ReactNode
  caption?: ReactNode
  tone?: 'clay' | 'danger'
}) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0
  // Past 90% the bar turns red on its own: a quota that is nearly full is the
  // one fact the user needs before they start a large upload.
  const critical = tone === 'danger' || pct >= 90
  return (
    <div className="space-y-1">
      {(label || caption) && (
        <div className="flex items-baseline justify-between gap-2 text-xs">
          {label && <span className="text-[var(--muted)]">{label}</span>}
          {caption && <span className="tabular-nums text-[var(--faint)]">{caption}</span>}
        </div>
      )}
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-[var(--sunk)]">
        <div
          className={clsx(
            'h-full rounded-full transition-[width] duration-300',
            critical ? 'bg-[var(--color-danger)]' : 'bg-[var(--color-clay)]',
          )}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  )
}

export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: string
  hint?: string
  error?: string
  children: ReactNode
}) {
  return (
    <div>
      <label className="label">{label}</label>
      {children}
      {error ? (
        <p className="mt-1.5 text-xs text-[var(--color-danger)]">{error}</p>
      ) : hint ? (
        <p className="mt-1.5 text-xs text-[var(--faint)]">{hint}</p>
      ) : null}
    </div>
  )
}

export function Spinner({ className }: { className?: string }) {
  return <Loader2 size={16} className={clsx('animate-spin text-[var(--faint)]', className)} />
}

/**
 * Modal is a centred dialog on desktop and a bottom sheet on small screens,
 * which is where a centred box with a keyboard open stops working.
 */
export function Modal({
  open,
  onClose,
  title,
  description,
  children,
  footer,
  width = 'max-w-md',
}: {
  open: boolean
  onClose: () => void
  title: string
  description?: string
  children?: ReactNode
  footer?: ReactNode
  width?: string
}) {
  const ref = useRef<HTMLDivElement>(null)
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

  useEffect(() => {
    if (!open) return

    const previousActiveElement = document.activeElement as HTMLElement | null

    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCloseRef.current()
    }
    document.addEventListener('keydown', onKey)

    // Moving focus into the dialog only once when opened.
    const timer = setTimeout(() => {
      const container = ref.current
      if (!container) return
      const autoFocusEl = container.querySelector<HTMLElement>('[autofocus], [data-autofocus]')
      if (autoFocusEl) {
        autoFocusEl.focus()
      } else {
        const firstInput = container.querySelector<HTMLElement>(
          'input:not([disabled]):not([type="hidden"]), textarea:not([disabled]), select:not([disabled])'
        )
        if (firstInput) {
          firstInput.focus()
        } else {
          const firstFocusable = container.querySelector<HTMLElement>(
            'button:not([disabled]), [tabindex]:not([tabindex="-1"]), a[href]'
          )
          firstFocusable?.focus()
        }
      }
    }, 30)

    return () => {
      document.removeEventListener('keydown', onKey)
      clearTimeout(timer)
      if (previousActiveElement && typeof previousActiveElement.focus === 'function') {
        previousActiveElement.focus()
      }
    }
  }, [open])

  if (!open) return null

  return (
    <div
      className="fixed inset-0 z-50 flex items-end sm:items-center justify-center"
      role="dialog"
      aria-modal="true"
      aria-label={title}
    >
      <div
        className="absolute inset-0 bg-black/25 backdrop-blur-[2px] fade-in"
        onClick={onClose}
        aria-hidden
      />
      <div
        ref={ref}
        className={clsx(
          'relative w-full panel p-5 sm:p-6 max-h-[90vh] overflow-y-auto',
          'rounded-b-none sm:rounded-b-[var(--radius-panel)] sheet-in sm:rise-in',
          width,
        )}
      >
        <div className="flex items-start justify-between gap-4">
          <div>
            <h2 className="display text-lg">{title}</h2>
            {description && <p className="mt-1 text-sm text-[var(--muted)]">{description}</p>}
          </div>
          <IconButton label="关闭" onClick={onClose}>
            <X size={16} />
          </IconButton>
        </div>
        {children && <div className="mt-5">{children}</div>}
        {footer && <div className="mt-6 flex justify-end gap-2">{footer}</div>}
      </div>
    </div>
  )
}

/** Toast surfaces the result of an action without stealing focus. */
export interface ToastMessage {
  id: number
  text: string
  tone: 'info' | 'error' | 'success'
}

let toastId = 0
const toastListeners = new Set<(t: ToastMessage) => void>()

export function toast(text: string, tone: ToastMessage['tone'] = 'info') {
  const message = { id: ++toastId, text, tone }
  toastListeners.forEach((fn) => fn(message))
}

export function ToastHost() {
  const [items, setItems] = useState<ToastMessage[]>([])

  useEffect(() => {
    const push = (t: ToastMessage) => {
      setItems((prev) => [...prev, t])
      // Errors linger, because they usually need reading twice.
      setTimeout(() => setItems((prev) => prev.filter((i) => i.id !== t.id)),
        t.tone === 'error' ? 7000 : 3500)
    }
    toastListeners.add(push)
    return () => {
      toastListeners.delete(push)
    }
  }, [])

  if (items.length === 0) return null

  return (
    // Clear of the phone's tab bar, which a toast at bottom-4 covered.
    <div className="fixed bottom-[calc(4.25rem+env(safe-area-inset-bottom))] left-1/2 z-[60] w-full max-w-sm -translate-x-1/2 space-y-2 px-4 md:bottom-4">
      {items.map((item) => (
        <div
          key={item.id}
          role="status"
          className={clsx(
            'rise-in surface px-3.5 py-2.5 text-sm shadow-sm flex items-start gap-2',
            item.tone === 'error' && 'border-[var(--color-danger)]/40',
            item.tone === 'success' && 'border-[var(--color-success)]/40',
          )}
        >
          <span
            className={clsx(
              'mt-1.5 size-1.5 shrink-0 rounded-full',
              item.tone === 'error'
                ? 'bg-[var(--color-danger)]'
                : item.tone === 'success'
                  ? 'bg-[var(--color-success)]'
                  : 'bg-[var(--color-clay)]',
            )}
          />
          <span className="min-w-0 break-words">{item.text}</span>
        </div>
      ))}
    </div>
  )
}

/** Progress is a thin determinate bar; the clay fill is the only place the
 *  accent appears outside of actions. */
export function Progress({ value, className }: { value: number; className?: string }) {
  const pct = Math.max(0, Math.min(100, value))
  return (
    <div
      className={clsx('h-1 w-full overflow-hidden rounded-full bg-[var(--sunk)]', className)}
      role="progressbar"
      aria-valuenow={Math.round(pct)}
      aria-valuemin={0}
      aria-valuemax={100}
    >
      <div
        className="h-full rounded-full bg-[var(--color-clay)] transition-[width] duration-300 ease-out"
        style={{ width: `${pct}%` }}
      />
    </div>
  )
}

export function EmptyState({
  icon,
  title,
  description,
  action,
}: {
  icon: ReactNode
  title: string
  description?: string
  action?: ReactNode
}) {
  return (
    <div className="flex flex-col items-center justify-center py-20 text-center px-6">
      <div className="mb-4 text-[var(--faint)]">{icon}</div>
      <h3 className="display text-base">{title}</h3>
      {description && (
        <p className="mt-1.5 max-w-sm text-sm text-[var(--muted)]">{description}</p>
      )}
      {action && <div className="mt-5">{action}</div>}
    </div>
  )
}
