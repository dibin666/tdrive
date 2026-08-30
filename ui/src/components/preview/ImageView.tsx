import { useCallback, useEffect, useRef, useState } from 'react'
import clsx from 'clsx'
import { Maximize, Minus, Plus, RotateCw } from 'lucide-react'
import type { ViewerProps } from './PreviewModal'
import { IconButton, Spinner } from '../primitives'

/**
 * An image viewer with zoom and pan, because the common case for a drive full
 * of photos and scans is wanting to look closely at one, and a fixed-size
 * <img> makes that impossible.
 *
 * Zooming is anchored to the pointer rather than the centre, which is the
 * difference between "zoom in and then hunt for the thing you wanted" and
 * "zoom in on the thing you wanted".
 */

const MIN_SCALE = 0.1
const MAX_SCALE = 16

export default function ImageView({ entry, link }: ViewerProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const [scale, setScale] = useState(1)
  const [offset, setOffset] = useState({ x: 0, y: 0 })
  const [rotation, setRotation] = useState(0)
  const [loaded, setLoaded] = useState(false)
  const [natural, setNatural] = useState<{ width: number; height: number } | null>(null)
  const dragging = useRef<{ x: number; y: number; ox: number; oy: number } | null>(null)

  const reset = useCallback(() => {
    setScale(1)
    setOffset({ x: 0, y: 0 })
    setRotation(0)
  }, [])

  useEffect(() => {
    setLoaded(false)
    reset()
  }, [entry.id, reset])

  const zoomAt = useCallback(
    (factor: number, clientX?: number, clientY?: number) => {
      const container = containerRef.current
      setScale((prev) => {
        const next = Math.max(MIN_SCALE, Math.min(MAX_SCALE, prev * factor))
        if (container && clientX !== undefined && clientY !== undefined) {
          const bounds = container.getBoundingClientRect()
          // Keep the point under the cursor fixed while the scale changes.
          const px = clientX - bounds.left - bounds.width / 2
          const py = clientY - bounds.top - bounds.height / 2
          const ratio = next / prev
          setOffset((current) => ({
            x: px - (px - current.x) * ratio,
            y: py - (py - current.y) * ratio,
          }))
        }
        return next
      })
    },
    [],
  )

  const onWheel = (e: React.WheelEvent) => {
    e.preventDefault()
    zoomAt(e.deltaY < 0 ? 1.15 : 1 / 1.15, e.clientX, e.clientY)
  }

  const onPointerDown = (e: React.PointerEvent) => {
    if (scale <= 1) return
    dragging.current = { x: e.clientX, y: e.clientY, ox: offset.x, oy: offset.y }
    ;(e.target as HTMLElement).setPointerCapture(e.pointerId)
  }

  const onPointerMove = (e: React.PointerEvent) => {
    const drag = dragging.current
    if (!drag) return
    setOffset({ x: drag.ox + (e.clientX - drag.x), y: drag.oy + (e.clientY - drag.y) })
  }

  const onPointerUp = () => {
    dragging.current = null
  }

  return (
    <div className="flex h-full flex-col">
      <div
        ref={containerRef}
        onWheel={onWheel}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        onDoubleClick={(e) => (scale > 1 ? reset() : zoomAt(2.5, e.clientX, e.clientY))}
        className={clsx(
          'relative flex min-h-0 flex-1 items-center justify-center overflow-hidden bg-[var(--sunk)]',
          scale > 1 ? 'cursor-grab active:cursor-grabbing' : 'cursor-zoom-in',
        )}
      >
        {!loaded && (
          <div className="absolute inset-0 flex items-center justify-center">
            <Spinner className="size-5" />
          </div>
        )}
        <img
          src={link.url}
          alt={entry.name}
          draggable={false}
          onLoad={(e) => {
            setLoaded(true)
            setNatural({
              width: e.currentTarget.naturalWidth,
              height: e.currentTarget.naturalHeight,
            })
          }}
          style={{
            transform: `translate(${offset.x}px, ${offset.y}px) scale(${scale}) rotate(${rotation}deg)`,
            // Transitioning only when not dragging keeps panning responsive
            // while a zoom step still animates.
            transition: dragging.current ? 'none' : 'transform 140ms var(--ease-out-soft)',
          }}
          className="max-h-full max-w-full select-none object-contain"
        />
      </div>

      <div className="flex shrink-0 items-center gap-1 border-t border-[var(--line)] px-3 py-1.5">
        <IconButton label="缩小" onClick={() => zoomAt(1 / 1.4)}>
          <Minus size={15} />
        </IconButton>
        <span className="w-14 text-center text-xs tabular-nums text-[var(--muted)]">
          {Math.round(scale * 100)}%
        </span>
        <IconButton label="放大" onClick={() => zoomAt(1.4)}>
          <Plus size={15} />
        </IconButton>
        <IconButton label="旋转" onClick={() => setRotation((r) => (r + 90) % 360)}>
          <RotateCw size={15} />
        </IconButton>
        <IconButton label="适应窗口" onClick={reset}>
          <Maximize size={15} />
        </IconButton>
        {natural && (
          <span className="ml-auto text-xs tabular-nums text-[var(--faint)]">
            {natural.width} × {natural.height}
          </span>
        )}
      </div>
    </div>
  )
}
