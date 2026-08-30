import { useCallback, useEffect, useRef, useState } from 'react'

/**
 * Touch gestures, built on pointer events rather than a library.
 *
 * Everything here has to coexist with scrolling, which is the hard part: a
 * vertical drag belongs to the scroll container and must not be stolen, while
 * a horizontal drag on a row is a swipe and must not scroll. The rule used
 * throughout is to watch the first ~10px of movement, decide which axis the
 * gesture is on, and then commit to that decision for the rest of the drag.
 */

const LONG_PRESS_MS = 500
const LONG_PRESS_SLOP = 10
const SWIPE_THRESHOLD = 56
const AXIS_LOCK = 10

/** haptic is a no-op everywhere it is not supported, which is most desktops
 *  and all of iOS Safari. */
function haptic(pattern: number | number[] = 10) {
  if (typeof navigator !== 'undefined' && 'vibrate' in navigator) {
    try {
      navigator.vibrate(pattern)
    } catch {
      /* some browsers throw when the page is not visible */
    }
  }
}

export interface LongPressOptions {
  onLongPress: (e: { clientX: number; clientY: number }) => void
  /** Fires for an ordinary tap that was not a long press. */
  onTap?: (e: { clientX: number; clientY: number }) => void
  enabled?: boolean
}

/**
 * useLongPress gives touch users the equivalent of a right-click.
 *
 * It only arms for touch and pen: on a mouse there is a real context menu
 * button, and holding one down is not a gesture anybody expects to do
 * anything.
 */
export function useLongPress({ onLongPress, onTap, enabled = true }: LongPressOptions) {
  const timer = useRef<number | undefined>(undefined)
  const start = useRef<{ x: number; y: number } | null>(null)
  const fired = useRef(false)

  const cancel = useCallback(() => {
    if (timer.current !== undefined) {
      window.clearTimeout(timer.current)
      timer.current = undefined
    }
    start.current = null
  }, [])

  useEffect(() => cancel, [cancel])

  const onPointerDown = useCallback(
    (e: React.PointerEvent) => {
      if (!enabled || (e.pointerType !== 'touch' && e.pointerType !== 'pen')) return
      const { clientX, clientY } = e
      start.current = { x: clientX, y: clientY }
      fired.current = false
      timer.current = window.setTimeout(() => {
        fired.current = true
        haptic()
        onLongPress({ clientX, clientY })
      }, LONG_PRESS_MS)
    },
    [enabled, onLongPress],
  )

  const onPointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!start.current) return
      const dx = Math.abs(e.clientX - start.current.x)
      const dy = Math.abs(e.clientY - start.current.y)
      // Scrolling must not turn into a long press just because a finger
      // rested for half a second on the way down the page.
      if (dx > LONG_PRESS_SLOP || dy > LONG_PRESS_SLOP) cancel()
    },
    [cancel],
  )

  const onPointerUp = useCallback(
    (e: React.PointerEvent) => {
      const wasArmed = start.current !== null
      cancel()
      if (wasArmed && !fired.current && onTap) {
        onTap({ clientX: e.clientX, clientY: e.clientY })
      }
    },
    [cancel, onTap],
  )

  return {
    onPointerDown,
    onPointerMove,
    onPointerUp,
    onPointerCancel: cancel,
    /** True when the last gesture was a long press, so a click handler can
     *  ignore the synthetic click that follows. */
    consumedRef: fired,
  }
}

export interface SwipeState {
  /** Current horizontal offset in pixels; negative is a left swipe. */
  offset: number
  /** True once the swipe passed the threshold and the actions are showing. */
  open: boolean
}

/**
 * useSwipe reveals row actions on a horizontal drag.
 *
 * The row is translated live rather than snapping at the end, because a swipe
 * that gives no feedback until it completes feels broken on the first try and
 * teaches nobody that it exists.
 */
export function useSwipe(options: { onOpen?: () => void; enabled?: boolean } = {}) {
  const { onOpen, enabled = true } = options
  const [state, setState] = useState<SwipeState>({ offset: 0, open: false })
  const start = useRef<{ x: number; y: number } | null>(null)
  const axis = useRef<'none' | 'x' | 'y'>('none')

  const close = useCallback(() => setState({ offset: 0, open: false }), [])

  const onPointerDown = useCallback(
    (e: React.PointerEvent) => {
      if (!enabled || e.pointerType === 'mouse') return
      start.current = { x: e.clientX, y: e.clientY }
      axis.current = 'none'
    },
    [enabled],
  )

  const onPointerMove = useCallback(
    (e: React.PointerEvent) => {
      if (!start.current) return
      const dx = e.clientX - start.current.x
      const dy = e.clientY - start.current.y

      if (axis.current === 'none') {
        if (Math.abs(dx) < AXIS_LOCK && Math.abs(dy) < AXIS_LOCK) return
        // Once the axis is decided it does not change, so a swipe that drifts
        // does not suddenly start scrolling and vice versa.
        axis.current = Math.abs(dx) > Math.abs(dy) ? 'x' : 'y'
      }
      if (axis.current !== 'x') return

      // Only left swipes reveal anything, and the offset is clamped so the row
      // cannot be dragged off the screen.
      const offset = Math.max(-120, Math.min(0, dx + (state.open ? -SWIPE_THRESHOLD : 0)))
      setState((prev) => ({ ...prev, offset }))
    },
    [state.open],
  )

  const onPointerUp = useCallback(() => {
    if (axis.current === 'x') {
      setState((prev) => {
        const open = prev.offset <= -SWIPE_THRESHOLD
        if (open && !prev.open) {
          haptic(8)
          onOpen?.()
        }
        return { offset: open ? -SWIPE_THRESHOLD : 0, open }
      })
    }
    start.current = null
    axis.current = 'none'
  }, [onOpen])

  return {
    swipe: state,
    close,
    handlers: {
      onPointerDown,
      onPointerMove,
      onPointerUp,
      onPointerCancel: onPointerUp,
    },
  }
}

/**
 * usePullToRefresh runs a refresh when the user drags down at the top of a
 * scroll container.
 *
 * It deliberately does nothing unless the container is already scrolled to the
 * very top, so it can never fight with an ordinary scroll gesture.
 */
export function usePullToRefresh(
  containerRef: React.RefObject<HTMLElement | null>,
  onRefresh: () => Promise<void> | void,
  enabled = true,
) {
  const [pull, setPull] = useState(0)
  const [refreshing, setRefreshing] = useState(false)
  const start = useRef<number | null>(null)
  const armed = useRef(false)

  const THRESHOLD = 72

  useEffect(() => {
    const container = containerRef.current
    if (!container || !enabled) return

    const down = (e: PointerEvent) => {
      if (e.pointerType !== 'touch' || refreshing) return
      armed.current = container.scrollTop <= 0
      start.current = armed.current ? e.clientY : null
    }

    const move = (e: PointerEvent) => {
      if (start.current === null || !armed.current) return
      const dy = e.clientY - start.current
      if (dy <= 0) {
        setPull(0)
        return
      }
      // Resistance: the further it is pulled the slower it moves, which is
      // what stops the gesture feeling like a bug when it does not fire.
      setPull(Math.min(THRESHOLD * 1.5, dy * 0.5))
    }

    const up = async () => {
      const distance = pull
      start.current = null
      armed.current = false
      if (distance >= THRESHOLD && !refreshing) {
        setRefreshing(true)
        haptic(12)
        try {
          await onRefresh()
        } finally {
          setRefreshing(false)
        }
      }
      setPull(0)
    }

    container.addEventListener('pointerdown', down)
    container.addEventListener('pointermove', move)
    container.addEventListener('pointerup', up)
    container.addEventListener('pointercancel', up)
    return () => {
      container.removeEventListener('pointerdown', down)
      container.removeEventListener('pointermove', move)
      container.removeEventListener('pointerup', up)
      container.removeEventListener('pointercancel', up)
    }
  }, [containerRef, enabled, onRefresh, pull, refreshing])

  return { pull, refreshing, threshold: THRESHOLD }
}

/**
 * useEdgeSwipe navigates back when the user swipes right from the left edge,
 * which is the gesture both mobile platforms have trained everyone to expect.
 */
export function useEdgeSwipe(onBack: () => void, enabled = true) {
  const start = useRef<{ x: number; y: number } | null>(null)

  useEffect(() => {
    if (!enabled) return

    const down = (e: PointerEvent) => {
      if (e.pointerType !== 'touch') return
      // Only from the left edge: a swipe that starts in the middle of the
      // listing is a scroll or a row swipe.
      start.current = e.clientX <= 24 ? { x: e.clientX, y: e.clientY } : null
    }

    const up = (e: PointerEvent) => {
      const origin = start.current
      start.current = null
      if (!origin) return
      const dx = e.clientX - origin.x
      const dy = Math.abs(e.clientY - origin.y)
      if (dx > 80 && dy < 60) {
        haptic(8)
        onBack()
      }
    }

    window.addEventListener('pointerdown', down)
    window.addEventListener('pointerup', up)
    return () => {
      window.removeEventListener('pointerdown', down)
      window.removeEventListener('pointerup', up)
    }
  }, [enabled, onBack])
}

/** isTouchDevice decides between a context menu and a bottom sheet. It reads
 *  the pointer capability rather than the user agent, so a laptop with a
 *  touchscreen still gets the desktop behaviour it also supports. */
export function isCoarsePointer(): boolean {
  return typeof window !== 'undefined' && window.matchMedia('(pointer: coarse)').matches
}
