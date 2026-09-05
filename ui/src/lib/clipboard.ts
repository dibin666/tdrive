// Copying text when there is no secure context.
//
// navigator.clipboard only exists on HTTPS and on localhost. A tdrive is
// normally reached over plain HTTP at a LAN or tailnet address, where the whole
// API is undefined rather than merely restricted — so reading .writeText off it
// throws "Cannot read properties of undefined", which is what stopped every
// copy button in the UI from doing anything.
//
// The modern API is still tried first, because it is the only one that works
// with large text and without touching the DOM. The execCommand path behind it
// is deprecated but universally implemented, and it is the only thing available
// on the origin these deployments actually use.

export async function copyText(text: string): Promise<boolean> {
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Permission denied, or a browser that exposes the object but refuses to
      // use it outside a secure context. The fallback still has a chance.
    }
  }
  return legacyCopy(text)
}

function legacyCopy(text: string): boolean {
  if (typeof document === 'undefined') return false

  const area = document.createElement('textarea')
  area.value = text
  // Off-screen rather than display:none — execCommand ignores an element that
  // is not rendered — and readonly so mobile keyboards stay down.
  area.setAttribute('readonly', '')
  area.style.position = 'fixed'
  area.style.top = '0'
  area.style.left = '-9999px'
  document.body.appendChild(area)

  // Selecting steals whatever the user had selected, so it is put back.
  const selection = document.getSelection()
  const previous = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null

  let ok = false
  try {
    area.select()
    area.setSelectionRange(0, text.length)
    ok = document.execCommand('copy')
  } catch {
    ok = false
  } finally {
    area.remove()
    if (selection) {
      selection.removeAllRanges()
      if (previous) selection.addRange(previous)
    }
  }
  return ok
}

/** COPY_FAILED is the one message every caller shows, because the cause is
 *  always the same and the answer is always to select the text by hand. */
export const COPY_FAILED = '浏览器阻止了剪贴板访问，请手动选中文本复制'
