import type { HighlighterCore } from 'shiki/core'

/**
 * A deliberately small syntax highlighter.
 *
 * Importing `codeToHtml` from shiki's default entry pulls in every grammar it
 * ships — around 200 languages and 11 MB of chunks. They load lazily, so a
 * browser never fetches most of them, but this app's assets are embedded into
 * the server binary, so they are 11 MB every deployment carries around
 * forever to highlight a config file.
 *
 * So the highlighter is assembled from the grammars this file actually maps an
 * extension to, using the JavaScript regex engine rather than the Oniguruma
 * WASM build — one less binary blob, and the difference only shows up on
 * grammars far more exotic than these.
 */

/** Grammar loaders, keyed by the language id used in the map below. Each one
 *  is a dynamic import, so a language is fetched the first time a file needs
 *  it and never again. */
const GRAMMARS: Record<string, () => Promise<unknown>> = {
  bash: () => import('shiki/langs/bash.mjs'),
  c: () => import('shiki/langs/c.mjs'),
  cpp: () => import('shiki/langs/cpp.mjs'),
  csharp: () => import('shiki/langs/csharp.mjs'),
  css: () => import('shiki/langs/css.mjs'),
  docker: () => import('shiki/langs/docker.mjs'),
  go: () => import('shiki/langs/go.mjs'),
  html: () => import('shiki/langs/html.mjs'),
  ini: () => import('shiki/langs/ini.mjs'),
  java: () => import('shiki/langs/java.mjs'),
  javascript: () => import('shiki/langs/javascript.mjs'),
  json: () => import('shiki/langs/json.mjs'),
  jsx: () => import('shiki/langs/jsx.mjs'),
  kotlin: () => import('shiki/langs/kotlin.mjs'),
  lua: () => import('shiki/langs/lua.mjs'),
  make: () => import('shiki/langs/make.mjs'),
  markdown: () => import('shiki/langs/markdown.mjs'),
  php: () => import('shiki/langs/php.mjs'),
  python: () => import('shiki/langs/python.mjs'),
  ruby: () => import('shiki/langs/ruby.mjs'),
  rust: () => import('shiki/langs/rust.mjs'),
  scss: () => import('shiki/langs/scss.mjs'),
  shell: () => import('shiki/langs/shellscript.mjs'),
  sql: () => import('shiki/langs/sql.mjs'),
  swift: () => import('shiki/langs/swift.mjs'),
  toml: () => import('shiki/langs/toml.mjs'),
  tsx: () => import('shiki/langs/tsx.mjs'),
  typescript: () => import('shiki/langs/typescript.mjs'),
  vue: () => import('shiki/langs/vue.mjs'),
  xml: () => import('shiki/langs/xml.mjs'),
  yaml: () => import('shiki/langs/yaml.mjs'),
}

/** Extension to grammar id. Anything absent renders as plain text, which is a
 *  perfectly good outcome and much better than a wrong grammar. */
const BY_EXTENSION: Record<string, string> = {
  go: 'go',
  ts: 'typescript', mts: 'typescript', cts: 'typescript',
  tsx: 'tsx',
  js: 'javascript', mjs: 'javascript', cjs: 'javascript',
  jsx: 'jsx',
  py: 'python',
  rs: 'rust',
  c: 'c', h: 'c',
  cpp: 'cpp', cc: 'cpp', cxx: 'cpp', hpp: 'cpp', hxx: 'cpp',
  java: 'java',
  kt: 'kotlin', kts: 'kotlin',
  swift: 'swift',
  rb: 'ruby',
  php: 'php',
  cs: 'csharp',
  sh: 'shell', bash: 'bash', zsh: 'shell', fish: 'shell',
  sql: 'sql',
  lua: 'lua',
  vue: 'vue',
  css: 'css',
  scss: 'scss', less: 'scss',
  html: 'html', htm: 'html',
  json: 'json',
  yaml: 'yaml', yml: 'yaml',
  toml: 'toml',
  ini: 'ini', conf: 'ini', cfg: 'ini', properties: 'ini',
  xml: 'xml', svg: 'xml', plist: 'xml',
  md: 'markdown', markdown: 'markdown', mdx: 'markdown',
}

export function languageFor(name: string): string | null {
  const lower = name.toLowerCase()
  if (lower === 'dockerfile' || lower.endsWith('.dockerfile')) return 'docker'
  if (lower === 'makefile') return 'make'
  const ext = lower.slice(lower.lastIndexOf('.') + 1)
  return BY_EXTENSION[ext] ?? null
}

let corePromise: Promise<HighlighterCore> | null = null
const loaded = new Set<string>()

async function core(): Promise<HighlighterCore> {
  if (!corePromise) {
    corePromise = (async () => {
      const [{ createHighlighterCore }, { createJavaScriptRegexEngine }, light, dark] =
        await Promise.all([
          import('shiki/core'),
          import('shiki/engine/javascript'),
          import('shiki/themes/github-light.mjs'),
          import('shiki/themes/github-dark.mjs'),
        ])

      return createHighlighterCore({
        themes: [light.default, dark.default],
        langs: [],
        engine: createJavaScriptRegexEngine(),
      })
    })()
  }
  return corePromise
}

/**
 * highlight renders code to HTML carrying both themes, so switching between
 * light and dark does not require re-highlighting. Returns null when the
 * language is unknown, which the caller renders as plain text.
 */
export async function highlight(code: string, language: string | null): Promise<string | null> {
  if (!language || !GRAMMARS[language]) return null

  const highlighter = await core()
  if (!loaded.has(language)) {
    await highlighter.loadLanguage((await GRAMMARS[language]()) as never)
    loaded.add(language)
  }

  return highlighter.codeToHtml(code, {
    lang: language,
    themes: { light: 'github-light', dark: 'github-dark' },
    defaultColor: false,
  })
}
