// Copies the libav.js WebAssembly build into public/ so it ships with the app.
//
// The software decoder is the last resort for playing HEVC in a browser that
// cannot decode it natively. Its upstream loader would fetch the WASM from a
// CDN, which is the wrong shape for a self-hosted drive: the whole point of
// this deployment is that it works on a machine that only talks to Telegram.
// So the files are served from the app's own origin instead.
//
// Only the single-threaded WASM build is copied. The threaded one needs
// cross-origin isolation headers this server does not send, and the asm.js
// fallback is 4.5 MB for browsers that no longer exist.

import { createRequire } from 'node:module'
import { copyFileSync, mkdirSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'

const require = createRequire(import.meta.url)

// The package restricts its "exports" map, so its own entry point is resolved
// and the dist directory derived from it rather than asking for package.json.
const source = dirname(require.resolve('@libav.js/variant-webcodecs'))
const target = new URL('../public/libav/', import.meta.url).pathname

mkdirSync(target, { recursive: true })

const wanted = readdirSync(source).filter(
  (name) => name.endsWith('.wasm.mjs') || name.endsWith('.wasm.wasm') || name === 'libav-webcodecs.mjs',
)

for (const name of wanted) {
  copyFileSync(join(source, name), join(target, name))
}

console.log(`copied ${wanted.length} libav files into public/libav`)
