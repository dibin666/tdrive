import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import clsx from 'clsx'
import { isCoarsePointer } from '../lib/gestures'

/**
 * A context menu that is the same menu on a mouse and on a finger.
 *
 * On a pointer device it opens where the cursor is and flips at the viewport
 * edges. On a touch device the identical item list renders as a bottom sheet,
 * because a 180px-wide menu anchored under a fingertip is unusable and because
 * the sheet is where a phone user already expects secondary actions to appear.
 * Sharing the item list rather than the presentation is what keeps the two
 * from drifting apart.
 */

export interface MenuItem {
  id: string
  label: ReactNode
  icon?: ReactNode
  /** Rendered right-aligned, for a keyboard shortcut hint. */
  hint?: string
  onSelect?: () => void
  disabled?: boolean
  danger?: boolean
  /** Draws a separator above this item. */
  separated?: boolean
  /** Hidden entries are dropped rather than greyed out, for actions the
   *  account has no permission to perform at all. */
  hidden?: boolean
}

export interface MenuPosition {
  x: number
  y: number
}

export interface ContextMenuState {
  open: boolean
  position: MenuPosition
  items: MenuItem[]
  title?: string
}

export const closedMenu: ContextMenuState = { open: false, position: { x: 0, y: 0 }, items: [] }

/** useContextMenu owns the open/closed state and gives back an opener that
 *  takes the event and the items to show. */
export function useContextMenu() {
  const [state, setState] = useState<ContextMenuState>(closedMenu)

  const open = useCallback(
    (position: MenuPosition, items: MenuItem[], title?: string) => {
      setState({ open: true, position, items: items.filter((i) => !i.hidden), title })
    },
    [],
  )

  const close = useCallback(() => setState(closedMenu), [])

  return { menu: state, openMenu: open, closeMenu: close }
}

export function ContextMenu({ state, onClose }: { state: ContextMenuState; onClose: () => void }) {
  const ref = useRef<HTMLDivElement>(null)
  const [coarse, setCoarse] = useState(false)
  const [placed, setPlaced] = useState<MenuPosition>(state.position)
  const [activeIndex, setActiveIndex] = useState(-1)

  useEffect(() => {
    if (state.open) setCoarse(isCoarsePointer())
  }, [state.open])

  // The menu is positioned after it has been measured, because flipping at an
  // edge needs a real height and rendering it off-screen first would flicker.
  useLayoutEffect(() => {
    if (!state.open || coarse) return
    const node = ref.current
    if (!node) return

    const { width, height } = node.getBoundingClientRect()
    const margin = 8
    let x = state.position.x
    let y = state.position.y
    if (x + width + margin > window.innerWidth) x = Math.max(margin, x - width)
    if (y + height + margin > window.innerHeight) y = Math.max(margin, y - height)
    setPlaced({ x, y })
  }, [state.open, state.position, state.items, coarse])

  useEffect(() => {
    if (!state.open) {
      setActiveIndex(-1)
      return
    }

    const selectable = state.items.filter((i) => !i.disabled)

    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        onClose()
        return
      }
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault()
        setActiveIndex((prev) => {
          const step = e.key === 'ArrowDown' ? 1 : -1
          let next = prev
          for (let i = 0; i < state.items.length; i++) {
            next = (next + step + state.items.length) % state.items.length
            if (!state.items[next].disabled) return next
          }
          return prev
        })
        return
      }
      if (e.key === 'Enter' && activeIndex >= 0) {
        e.preventDefault()
        const item = state.items[activeIndex]
        if (item && !item.disabled) {
          onClose()
          item.onSelect?.()
        }
      }
    }

    // A click anywhere else closes the menu, including a right-click that is
    // opening a different menu.
    const onPointerDown = (e: PointerEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) onClose()
    }

    document.addEventListener('keydown', onKey)
    document.addEventListener('pointerdown', onPointerDown, true)
    window.addEventListener('resize', onClose)
    window.addEventListener('blur', onClose)
    // Scrolling would leave the menu pointing at nothing.
    window.addEventListener('scroll', onClose, true)

    void selectable
    return () => {
      document.removeEventListener('keydown', onKey)
      document.removeEventListener('pointerdown', onPointerDown, true)
      window.removeEventListener('resize', onClose)
      window.removeEventListener('blur', onClose)
      window.removeEventListener('scroll', onClose, true)
    }
  }, [state.open, state.items, activeIndex, onClose])

  if (!state.open || state.items.length === 0) return null

  const body = (
    <>
      {state.title && (
        <div className="truncate border-b border-[var(--line)] px-3 py-2 text-[11px] font-medium text-[var(--faint)]">
          {state.title}
        </div>
      )}
      <div className={clsx(coarse ? 'py-1' : 'p-1')}>
        {state.items.map((item, index) => (
          <div key={item.id}>
            {item.separated && <div className="my-1 h-px bg-[var(--line)]" />}
            <button
              type="button"
              disabled={item.disabled}
              onMouseEnter={() => setActiveIndex(index)}
              onClick={() => {
                if (item.disabled) return
                onClose()
                item.onSelect?.()
              }}
              className={clsx(
                'flex w-full items-center gap-2.5 rounded-[var(--radius-control)] text-left transition-colors',
                coarse ? 'px-4 py-3 text-[15px]' : 'px-2.5 py-1.5 text-[13px]',
                item.disabled
                  ? 'cursor-not-allowed text-[var(--faint)] opacity-60'
                  : item.danger
                    ? 'text-[var(--color-danger)] hover:bg-[var(--danger-soft)]'
                    : 'text-[var(--ink)] hover:bg-[var(--sunk)]',
                !coarse && activeIndex === index && !item.disabled && 'bg-[var(--sunk)]',
              )}
            >
              <span className={clsx('shrink-0', item.danger ? '' : 'text-[var(--faint)]')}>
                {item.icon}
              </span>
              <span className="min-w-0 flex-1 truncate">{item.label}</span>
              {item.hint && (
                <span className="shrink-0 font-[family-name:var(--font-mono)] text-[10px] text-[var(--faint)]">
                  {item.hint}
                </span>
              )}
            </button>
          </div>
        ))}
      </div>
    </>
  )

  if (coarse) {
    return createPortal(
      <div className="fixed inset-0 z-[70] flex items-end" role="menu">
        <div className="absolute inset-0 bg-black/25 backdrop-blur-[2px] fade-in" onClick={onClose} aria-hidden />
        <div
          ref={ref}
          className="relative w-full rounded-t-[var(--radius-panel)] border-t border-[var(--line)] bg-[var(--surface)] pb-safe shadow-xl sheet-in"
        >
          <div className="mx-auto mt-2 h-1 w-9 rounded-full bg-[var(--line-strong)]" />
          {body}
        </div>
      </div>,
      document.body,
    )
  }

  return createPortal(
    <div
      ref={ref}
      role="menu"
      style={{ left: placed.x, top: placed.y }}
      className="surface fixed z-[70] min-w-[11rem] max-w-[16rem] py-0 shadow-lg fade-in"
    >
      {body}
    </div>,
    document.body,
  )
}
