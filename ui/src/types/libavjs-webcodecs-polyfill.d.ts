/**
 * Types for the WebCodecs software-decoding polyfill.
 *
 * The package points its "types" entry at its own TypeScript sources rather
 * than a declaration file, so importing it normally drags several thousand
 * lines of third-party source into this project's type check — and that source
 * does not pass this project's stricter settings. tsconfig maps the package
 * name here instead, declaring exactly the surface this app uses.
 */
declare module 'libavjs-webcodecs-polyfill' {
  export interface LibAVPolyfillOptions {
    /** When false, the polyfill registers nothing on globalThis and only
     *  exposes its own classes, which is what a custom decoder wants. */
    polyfill?: boolean
    LibAV?: unknown
    libavOptions?: Record<string, unknown>
  }

  export function load(options?: LibAVPolyfillOptions): Promise<void>

  export class VideoDecoder {
    constructor(init: { output: (frame: unknown) => void; error: (error: unknown) => void })
    configure(config: unknown): Promise<void>
    decode(chunk: unknown): void
    flush(): Promise<void>
    close(): void
  }

  export class AudioDecoder {
    constructor(init: { output: (data: unknown) => void; error: (error: unknown) => void })
    configure(config: unknown): Promise<void>
    decode(chunk: unknown): void
    flush(): Promise<void>
    close(): void
  }
}

declare module '@libav.js/variant-webcodecs' {
  const libav: { base?: string; [key: string]: unknown }
  export default libav
}
