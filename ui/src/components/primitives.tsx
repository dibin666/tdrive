import clsx from 'clsx'
import { Loader2, X } from 'lucide-react'
import {
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
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

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    // Moving focus into the dialog is what makes Escape and Tab behave.
    const timer = setTimeout(() => {
      ref.current?.querySelector<HTMLElement>('input, textarea, button')?.focus()
    }, 30)
    return () => {
      document.removeEventListener('keydown', onKey)
      clearTimeout(timer)
    }
  }, [open, onClose])

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
    <div className="fixed bottom-4 left-1/2 z-[60] -translate-x-1/2 space-y-2 px-4 w-full max-w-sm pb-safe">
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
