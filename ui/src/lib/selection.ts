import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

/**
 * File-manager selection.
 *
 * Getting this right is mostly about the anchor. Ctrl-clicking toggles one
 * item and moves the anchor there; Shift-clicking selects everything between
 * the anchor and the item you clicked, without moving the anchor, so a second
 * Shift-click widens or narrows the same range instead of starting a new one.
 * That behaviour is what every desktop file manager does, and getting it
 * subtly wrong is immediately noticeable even to people who could not describe
 * the rule.
 */

export interface SelectionApi<T> {
  selected: ReadonlySet<string>
  /** The item the keyboard is on, drawn with a focus ring. */
  cursor: string | null
  size: number
  isSelected: (key: string) => boolean
  /** Handle a click, reading the modifier keys off the event. */
  click: (key: string, e: { shiftKey: boolean; ctrlKey: boolean; metaKey: boolean }) => void
  /** Select exactly this item, e.g. when a right-click lands outside the
   *  current selection. */
  only: (key: string) => void
  add: (keys: string[]) => void
  toggle: (key: string) => void
  selectAll: () => void
  clear: () => void
  set: (keys: string[]) => void
  /** Move the cursor by n rows, optionally extending the selection. */
  moveCursor: (delta: number, extend: boolean) => void
  cursorTo: (key: string, extend: boolean) => void
  selectedItems: T[]
}

export function useSelection<T>(items: T[], keyOf: (item: T) => string): SelectionApi<T> {
  const [selected, setSelected] = useState<Set<string>>(() => new Set())
  const [cursor, setCursor] = useState<string | null>(null)
  const anchorRef = useRef<string | null>(null)

  const keys = useMemo(() => items.map(keyOf), [items, keyOf])

  // A listing that changed under the selection — a delete elsewhere, an event
  // stream refresh — must not leave phantom keys selected, or the action bar
  // will offer to operate on files that are no longer there.
  useEffect(() => {
    setSelected((prev) => {
      if (prev.size === 0) return prev
      const present = new Set(keys)
      let changed = false
      const next = new Set<string>()
      for (const key of prev) {
        if (present.has(key)) next.add(key)
        else changed = true
      }
      return changed ? next : prev
    })
    setCursor((prev) => (prev && keys.includes(prev) ? prev : null))
  }, [keys])

  const rangeBetween = useCallback(
    (from: string | null, to: string) => {
      const end = keys.indexOf(to)
      if (end < 0) return [to]
      const start = from ? keys.indexOf(from) : -1
      if (start < 0) return [to]
      const [lo, hi] = start <= end ? [start, end] : [end, start]
      return keys.slice(lo, hi + 1)
    },
    [keys],
  )

  const click = useCallback<SelectionApi<T>['click']>(
    (key, e) => {
      const additive = e.ctrlKey || e.metaKey

      if (e.shiftKey) {
        const range = rangeBetween(anchorRef.current ?? cursor, key)
        setSelected((prev) => {
          // Ctrl+Shift adds the range to what is already selected; plain
          // Shift replaces, which is what makes a mis-aimed range easy to
          // correct by clicking again.
          const next = additive ? new Set(prev) : new Set<string>()
          for (const k of range) next.add(k)
          return next
        })
        setCursor(key)
        return
      }

      if (additive) {
        setSelected((prev) => {
          const next = new Set(prev)
          if (next.has(key)) next.delete(key)
          else next.add(key)
          return next
        })
        anchorRef.current = key
        setCursor(key)
        return
      }

      setSelected(new Set([key]))
      anchorRef.current = key
      setCursor(key)
    },
    [cursor, rangeBetween],
  )

  const only = useCallback((key: string) => {
    setSelected(new Set([key]))
    anchorRef.current = key
    setCursor(key)
  }, [])

  const add = useCallback((next: string[]) => {
    setSelected((prev) => {
      const merged = new Set(prev)
      for (const key of next) merged.add(key)
      return merged
    })
  }, [])

  const toggle = useCallback((key: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
    anchorRef.current = key
    setCursor(key)
  }, [])

  const selectAll = useCallback(() => {
    setSelected(new Set(keys))
    if (keys.length > 0) {
      anchorRef.current = keys[0]
      setCursor(keys[keys.length - 1])
    }
  }, [keys])

  const clear = useCallback(() => {
    setSelected(new Set())
    anchorRef.current = null
    setCursor(null)
  }, [])

  const set = useCallback((next: string[]) => {
    setSelected(new Set(next))
    anchorRef.current = next[0] ?? null
    setCursor(next[next.length - 1] ?? null)
  }, [])

  const cursorTo = useCallback(
    (key: string, extend: boolean) => {
      if (extend) {
        const range = rangeBetween(anchorRef.current ?? cursor, key)
        setSelected(new Set(range))
      } else {
        setSelected(new Set([key]))
        anchorRef.current = key
      }
      setCursor(key)
    },
    [cursor, rangeBetween],
  )

  const moveCursor = useCallback(
    (delta: number, extend: boolean) => {
      if (keys.length === 0) return
      const current = cursor ? keys.indexOf(cursor) : -1
      let next = current + delta
      if (current < 0) next = delta > 0 ? 0 : keys.length - 1
      next = Math.max(0, Math.min(keys.length - 1, next))
      cursorTo(keys[next], extend)
    },
    [cursor, cursorTo, keys],
  )

  const selectedItems = useMemo(
    () => items.filter((item) => selected.has(keyOf(item))),
    [items, keyOf, selected],
  )

  return {
    selected,
    cursor,
    size: selected.size,
    isSelected: useCallback((key: string) => selected.has(key), [selected]),
    click,
    only,
    add,
    toggle,
    selectAll,
    clear,
    set,
    moveCursor,
    cursorTo,
    selectedItems,
  }
}

/**
 * Rubber-band selection over a scrollable list.
 *
 * The rectangle is tracked in the container's own coordinate space rather than
 * the viewport's, so it stays anchored to the rows when the list scrolls
 * during a drag — which it does, because dragging toward the bottom edge is
 * exactly how someone selects more than one screenful.
 */
export interface MarqueeState {
  active: boolean
  rect: { left: number; top: number; width: number; height: number } | null
}

export function useMarquee(
  containerRef: React.RefObject<HTMLElement | null>,
  onSelect: (keys: string[], additive: boolean) => void,
) {
  const [state, setState] = useState<MarqueeState>({ active: false, rect: null })
  const originRef = useRef<{ x: number; y: number; additive: boolean } | null>(null)

  const onPointerDown = useCallback(
    (e: React.PointerEvent) => {
      // Only a primary mouse button on empty space starts a marquee. Touch is
      // excluded because a drag there means scrolling, and a pen is left to
      // behave like touch.
      if (e.pointerType !== 'mouse' || e.button !== 0) return
      const target = e.target as HTMLElement
      if (target.closest('[data-selectable]') || target.closest('button, a, input')) return

      const container = containerRef.current
      if (!container) return
      const bounds = container.getBoundingClientRect()
      originRef.current = {
        x: e.clientX - bounds.left + container.scrollLeft,
        y: e.clientY - bounds.top + container.scrollTop,
        additive: e.ctrlKey || e.metaKey || e.shiftKey,
      }
      setState({ active: true, rect: null })
    },
    [containerRef],
  )

  useEffect(() => {
    if (!state.active) return
    const container = containerRef.current
    if (!container) return

    const move = (e: PointerEvent) => {
      const origin = originRef.current
      if (!origin) return
      const bounds = container.getBoundingClientRect()
      const x = e.clientX - bounds.left + container.scrollLeft
      const y = e.clientY - bounds.top + container.scrollTop

      const rect = {
        left: Math.min(origin.x, x),
        top: Math.min(origin.y, y),
        width: Math.abs(x - origin.x),
        height: Math.abs(y - origin.y),
      }
      setState({ active: true, rect })

      // A few pixels of movement is a click that wobbled, not a drag.
      if (rect.width < 4 && rect.height < 4) return

      const hits: string[] = []
      for (const node of container.querySelectorAll<HTMLElement>('[data-selectable]')) {
        const nodeBounds = node.getBoundingClientRect()
        const nodeLeft = nodeBounds.left - bounds.left + container.scrollLeft
        const nodeTop = nodeBounds.top - bounds.top + container.scrollTop
        const intersects =
          nodeLeft < rect.left + rect.width &&
          nodeLeft + nodeBounds.width > rect.left &&
          nodeTop < rect.top + rect.height &&
          nodeTop + nodeBounds.height > rect.top
        if (intersects) {
          const key = node.dataset.selectable
          if (key) hits.push(key)
        }
      }
      onSelect(hits, origin.additive)
    }

    const up = () => {
      originRef.current = null
      setState({ active: false, rect: null })
    }

    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', up)
    window.addEventListener('pointercancel', up)
    return () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', up)
      window.removeEventListener('pointercancel', up)
    }
  }, [state.active, containerRef, onSelect])

  return { marquee: state, onPointerDown }
}
