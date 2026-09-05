import { useEffect, useMemo, useState } from 'react'
import clsx from 'clsx'
import { AlertTriangle, ArrowRight, Check } from 'lucide-react'
import { api, type Entry } from '../lib/api'
import { Button, Field, Input, Modal, Select, Switch, toast } from './primitives'

/**
 * Batch rename.
 *
 * The rules are applied in a fixed order — replace, then case, then extension,
 * then affixes, then numbering — because a configurable order would multiply
 * the ways to get a confusing result without making anything possible that is
 * not already possible by running the dialog twice.
 *
 * Everything is previewed before anything is sent. A rename here edits every
 * Telegram caption backing the entry, so "try it and see" is an expensive way
 * to find out that a regular expression was wrong.
 */

type CaseMode = 'keep' | 'lower' | 'upper' | 'title'
type NumberPosition = 'suffix' | 'prefix' | 'replace'

interface Rules {
  find: string
  replace: string
  useRegex: boolean
  caseSensitive: boolean
  prefix: string
  suffix: string
  caseMode: CaseMode
  newExtension: string
  strip: string
  numbering: boolean
  numberStart: number
  numberStep: number
  numberPad: number
  numberPosition: NumberPosition
  numberSeparator: string
}

const DEFAULTS: Rules = {
  find: '',
  replace: '',
  useRegex: false,
  caseSensitive: false,
  prefix: '',
  suffix: '',
  caseMode: 'keep',
  newExtension: '',
  strip: '',
  numbering: false,
  numberStart: 1,
  numberStep: 1,
  numberPad: 2,
  numberPosition: 'suffix',
  numberSeparator: ' ',
}

interface Preview {
  entry: Entry
  next: string
  changed: boolean
  problem?: string
}

/** splitName separates the stem from the extension, so rules can target one
 *  without disturbing the other. A dotfile has no extension. */
function splitName(name: string): [string, string] {
  const dot = name.lastIndexOf('.')
  if (dot <= 0) return [name, '']
  return [name.slice(0, dot), name.slice(dot + 1)]
}

function toTitleCase(value: string): string {
  return value.replace(/\p{L}[\p{L}\p{N}']*/gu, (word) =>
    word.charAt(0).toUpperCase() + word.slice(1).toLowerCase(),
  )
}

function applyRules(entries: Entry[], rules: Rules): { previews: Preview[]; regexError?: string } {
  let matcher: RegExp | null = null
  let regexError: string | undefined

  if (rules.find) {
    try {
      const flags = rules.caseSensitive ? 'g' : 'gi'
      matcher = rules.useRegex
        ? new RegExp(rules.find, flags)
        : new RegExp(rules.find.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), flags)
    } catch (err) {
      regexError = err instanceof Error ? err.message : String(err)
    }
  }

  const previews: Preview[] = entries.map((entry, index) => {
    let [stem, ext] = splitName(entry.name)

    if (matcher) {
      // lastIndex has to be reset between entries: a global regex object keeps
      // its position, so every second name would be skipped without this.
      matcher.lastIndex = 0
      stem = stem.replace(matcher, rules.replace)
    }
    if (rules.strip) {
      const strip = new RegExp(`[${rules.strip.replace(/[.*+?^${}()|[\]\\-]/g, '\\$&')}]`, 'g')
      stem = stem.replace(strip, '')
    }

    switch (rules.caseMode) {
      case 'lower':
        stem = stem.toLowerCase()
        break
      case 'upper':
        stem = stem.toUpperCase()
        break
      case 'title':
        stem = toTitleCase(stem)
        break
    }

    if (rules.numbering) {
      const value = rules.numberStart + index * rules.numberStep
      const text = String(Math.max(0, value)).padStart(Math.max(1, rules.numberPad), '0')
      if (rules.numberPosition === 'prefix') stem = text + rules.numberSeparator + stem
      else if (rules.numberPosition === 'replace') stem = text
      else stem = stem + rules.numberSeparator + text
    }

    stem = rules.prefix + stem + rules.suffix

    const nextExt = rules.newExtension.trim().replace(/^\./, '') || ext
    const next = nextExt ? `${stem}.${nextExt}` : stem

    let problem: string | undefined
    if (!next.trim()) problem = '名称不能为空'
    else if (next.includes('/')) problem = '名称不可包含斜杠'
    else if (next === '.' || next === '..') problem = '保留名称不可使用'
    else if (new TextEncoder().encode(next).length > 255) problem = '名称超出 255 字节限制'

    return { entry, next, changed: next !== entry.name, problem }
  })

  // Conflicts are detected against the whole result set, not just the pairs
  // being changed: renaming A to B is a conflict even when B is not itself
  // being renamed.
  const counts = new Map<string, number>()
  for (const preview of previews) {
    counts.set(preview.next, (counts.get(preview.next) ?? 0) + 1)
  }
  for (const preview of previews) {
    if (!preview.problem && (counts.get(preview.next) ?? 0) > 1) {
      preview.problem = '与其它项目重名'
    }
  }

  return { previews, regexError }
}

export function BatchRename({
  open,
  entries,
  onClose,
  onDone,
}: {
  open: boolean
  entries: Entry[]
  onClose: () => void
  onDone: () => void
}) {
  const [rules, setRules] = useState<Rules>(DEFAULTS)
  const [excluded, setExcluded] = useState<Set<string>>(new Set())
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (open) {
      setRules(DEFAULTS)
      setExcluded(new Set())
    }
  }, [open])

  const { previews, regexError } = useMemo(() => applyRules(entries, rules), [entries, rules])

  const applicable = previews.filter(
    (p) => p.changed && !p.problem && !excluded.has(p.entry.path),
  )
  const problems = previews.filter((p) => p.problem && !excluded.has(p.entry.path)).length

  const patch = (next: Partial<Rules>) => setRules((prev) => ({ ...prev, ...next }))

  const submit = async () => {
    if (applicable.length === 0) return
    setBusy(true)
    try {
      const result = await api.batchRename(
        applicable.map((p) => ({ path: p.entry.path, name: p.next })),
      )
      if (result.failed > 0) {
        const first = result.results.find((r) => !r.ok)
        toast(
          `已重命名 ${result.renamed} 项，${result.failed} 项失败${first?.error ? `：${first.error}` : ''}`,
          'error',
        )
      } else {
        toast(`已重命名 ${result.renamed} 项`, 'success')
      }
      onDone()
      onClose()
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err), 'error')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="批量重命名"
      description={`按规则预览后提交，共 ${entries.length} 项。`}
      width="max-w-4xl"
      footer={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button
            variant="primary"
            loading={busy}
            disabled={applicable.length === 0}
            onClick={() => void submit()}
          >
            重命名 {applicable.length} 项
          </Button>
        </>
      }
    >
      <div className="grid gap-5 lg:grid-cols-[20rem_1fr]">
        <div className="space-y-4">
          <section className="space-y-3">
            <h3 className="text-xs font-medium uppercase tracking-wide text-[var(--faint)]">
              查找替换
            </h3>
            <Field label="查找" error={regexError}>
              <Input
                value={rules.find}
                onChange={(e) => patch({ find: e.target.value })}
                placeholder={rules.useRegex ? '例如 S(\\d+)E(\\d+)' : '例如 [1080p]'}
                className={rules.useRegex ? 'font-[family-name:var(--font-mono)] text-xs' : ''}
              />
            </Field>
            <Field
              label="替换为"
              hint={rules.useRegex ? '可用 $1 $2 引用捕获组' : undefined}
            >
              <Input
                value={rules.replace}
                onChange={(e) => patch({ replace: e.target.value })}
                className={rules.useRegex ? 'font-[family-name:var(--font-mono)] text-xs' : ''}
              />
            </Field>
            <div className="flex flex-wrap gap-4">
              <Switch
                checked={rules.useRegex}
                onChange={(v) => patch({ useRegex: v })}
                label="正则表达式"
              />
              <Switch
                checked={rules.caseSensitive}
                onChange={(v) => patch({ caseSensitive: v })}
                label="区分大小写"
              />
            </div>
          </section>

          <section className="space-y-3 border-t border-[var(--line)] pt-4">
            <h3 className="text-xs font-medium uppercase tracking-wide text-[var(--faint)]">
              增删与大小写
            </h3>
            <div className="grid grid-cols-2 gap-3">
              <Field label="前缀">
                <Input value={rules.prefix} onChange={(e) => patch({ prefix: e.target.value })} />
              </Field>
              <Field label="后缀" hint="加在扩展名前">
                <Input value={rules.suffix} onChange={(e) => patch({ suffix: e.target.value })} />
              </Field>
            </div>
            <Field label="删除字符" hint="逐字匹配，如 []()">
              <Input value={rules.strip} onChange={(e) => patch({ strip: e.target.value })} />
            </Field>
            <div className="grid grid-cols-2 gap-3">
              <Field label="大小写">
                <Select
                  value={rules.caseMode}
                  onChange={(e) => patch({ caseMode: e.target.value as CaseMode })}
                >
                  <option value="keep">保持不变</option>
                  <option value="lower">全部小写</option>
                  <option value="upper">全部大写</option>
                  <option value="title">首字母大写</option>
                </Select>
              </Field>
              <Field label="修改扩展名" hint="留空保持原样">
                <Input
                  value={rules.newExtension}
                  onChange={(e) => patch({ newExtension: e.target.value })}
                  placeholder="mkv"
                />
              </Field>
            </div>
          </section>

          <section className="space-y-3 border-t border-[var(--line)] pt-4">
            <Switch
              checked={rules.numbering}
              onChange={(v) => patch({ numbering: v })}
              label="顺序编号"
              hint="按列表顺序追加编号"
            />
            {rules.numbering && (
              <div className="grid grid-cols-2 gap-3">
                <Field label="起始">
                  <Input
                    type="number"
                    value={rules.numberStart}
                    onChange={(e) => patch({ numberStart: Number(e.target.value) })}
                  />
                </Field>
                <Field label="步长">
                  <Input
                    type="number"
                    min="1"
                    value={rules.numberStep}
                    onChange={(e) => patch({ numberStep: Math.max(1, Number(e.target.value)) })}
                  />
                </Field>
                <Field label="位数">
                  <Input
                    type="number"
                    min="1"
                    max="8"
                    value={rules.numberPad}
                    onChange={(e) => patch({ numberPad: Number(e.target.value) })}
                  />
                </Field>
                <Field label="位置">
                  <Select
                    value={rules.numberPosition}
                    onChange={(e) => patch({ numberPosition: e.target.value as NumberPosition })}
                  >
                    <option value="suffix">置于名称后</option>
                    <option value="prefix">置于名称前</option>
                    <option value="replace">替换原名称</option>
                  </Select>
                </Field>
                <Field label="分隔符">
                  <Input
                    value={rules.numberSeparator}
                    onChange={(e) => patch({ numberSeparator: e.target.value })}
                  />
                </Field>
              </div>
            )}
          </section>
        </div>

        <div className="min-w-0">
          <div className="mb-2 flex items-baseline justify-between">
            <h3 className="text-xs font-medium uppercase tracking-wide text-[var(--faint)]">预览</h3>
            <span className="text-xs text-[var(--muted)]">
              {`变更 ${applicable.length} 项`}
              {problems > 0 && (
                <span className="ml-2 text-[var(--color-danger)]">{problems} 项有冲突</span>
              )}
            </span>
          </div>

          <div className="max-h-[26rem] overflow-y-auto rounded-[var(--radius-card)] border border-[var(--line)]">
            {previews.map((preview) => {
              const off = excluded.has(preview.entry.path)
              return (
                <button
                  key={preview.entry.path}
                  onClick={() =>
                    setExcluded((prev) => {
                      const next = new Set(prev)
                      if (next.has(preview.entry.path)) next.delete(preview.entry.path)
                      else next.add(preview.entry.path)
                      return next
                    })
                  }
                  className={clsx(
                    'flex w-full items-center gap-2 border-b border-[var(--line)] px-3 py-2 text-left text-xs last:border-b-0',
                    'transition-colors hover:bg-[var(--sunk)]',
                    off && 'opacity-40',
                  )}
                >
                  <span
                    className={clsx(
                      'flex size-4 shrink-0 items-center justify-center rounded border',
                      off
                        ? 'border-[var(--line-strong)]'
                        : 'border-[var(--color-clay)] bg-[var(--color-clay)] text-white',
                    )}
                  >
                    {!off && <Check size={11} />}
                  </span>
                  <span className="min-w-0 flex-1 truncate text-[var(--muted)] line-through decoration-[var(--faint)]/60">
                    {preview.entry.name}
                  </span>
                  <ArrowRight size={12} className="shrink-0 text-[var(--faint)]" />
                  <span
                    className={clsx(
                      'min-w-0 flex-1 truncate',
                      preview.problem
                        ? 'text-[var(--color-danger)]'
                        : preview.changed
                          ? 'font-medium text-[var(--ink)]'
                          : 'text-[var(--faint)]',
                    )}
                  >
                    {preview.next}
                  </span>
                  {preview.problem && (
                    <span
                      className="flex shrink-0 items-center gap-1 text-[var(--color-danger)]"
                      title={preview.problem}
                    >
                      <AlertTriangle size={12} />
                    </span>
                  )}
                </button>
              )
            })}
          </div>

          <p className="mt-2 text-xs text-[var(--muted)]">
            点击单行可排除该项。重命名过程中的临时同名冲突由系统自动解决。
          </p>
        </div>
      </div>
    </Modal>
  )
}
