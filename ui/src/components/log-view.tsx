import { useEffect, useMemo, useRef, useState } from 'react'
import { cn } from '@/lib/utils'
import { apiStream, type LogLine } from '@/lib/api'
import { localTime } from '@/lib/format'

// Strips ANSI colour codes emitted by buildpacks and apps.
// eslint-disable-next-line no-control-regex
const ansi = /\x1b\[[0-9;]*m/g

const instancePalette = [
  'text-orange-300',
  'text-sky-300',
  'text-emerald-300',
  'text-violet-300',
  'text-amber-300',
  'text-cyan-300',
  'text-pink-300',
  'text-lime-300',
]

function instanceColor(name: string): string {
  let h = 0
  for (const c of name) h = (h * 31 + c.charCodeAt(0)) >>> 0
  return instancePalette[h % instancePalette.length]
}

type Level = 'error' | 'warn' | 'info'

function level(msg: string): Level {
  const m = msg.slice(0, 200).toLowerCase()
  if (/\b(error|err|fatal|panic|exception|traceback|failed)\b/.test(m) || /"level":"(error|fatal)"/.test(m)) return 'error'
  if (/\b(warn|warning)\b/.test(m) || /"level":"warn"/.test(m)) return 'warn'
  return 'info'
}

/** Structured app log viewer: time, instance and message columns, level
 * highlighting, text filter and follow (auto-scroll) mode. */
export function AppLogView({
  lines,
  follow,
  filter,
  className,
  empty = 'Waiting for log lines...',
}: {
  lines: LogLine[]
  follow: boolean
  filter: string
  className?: string
  empty?: string
}) {
  const ref = useRef<HTMLDivElement>(null)
  const visible = useMemo(() => {
    const f = filter.trim().toLowerCase()
    if (!f) return lines
    return lines.filter((l) => l.m.toLowerCase().includes(f) || l.i.toLowerCase().includes(f))
  }, [lines, filter])

  useEffect(() => {
    if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight
  }, [visible, follow])

  return (
    <div
      ref={ref}
      className={cn('h-[30rem] overflow-auto rounded-lg border bg-zinc-950 p-2 font-mono text-xs leading-5 text-zinc-100', className)}
    >
      {visible.length === 0 ? (
        <div className="p-2 text-zinc-500">{lines.length === 0 ? empty : 'No lines match the filter'}</div>
      ) : (
        visible.map((l, idx) => {
          const lvl = level(l.m)
          return (
            <div
              key={idx}
              className={cn(
                'grid grid-cols-[5.5rem_6.5rem_1fr] gap-2 whitespace-pre-wrap break-all px-1 hover:bg-zinc-900',
                lvl === 'error' && 'bg-red-950/40',
                lvl === 'warn' && 'bg-amber-950/30',
              )}
            >
              <span className="select-none text-zinc-500">{localTime(l.t)}</span>
              <span className={cn('truncate', instanceColor(l.i))} title={l.p}>
                {l.i}
              </span>
              <span className={cn(lvl === 'error' && 'text-red-200', lvl === 'warn' && 'text-amber-100')}>{l.m.replace(ansi, '')}</span>
            </div>
          )
        })
      )}
    </div>
  )
}

/** Plain text log viewer for build output. */
export function TextLogView({
  lines,
  follow = true,
  className,
  empty = 'No output',
}: {
  lines: string[]
  follow?: boolean
  className?: string
  empty?: string
}) {
  const ref = useRef<HTMLPreElement>(null)
  useEffect(() => {
    if (follow && ref.current) ref.current.scrollTop = ref.current.scrollHeight
  }, [lines, follow])
  return (
    <pre
      ref={ref}
      className={cn('h-[30rem] overflow-auto rounded-lg border bg-zinc-950 p-3 font-mono text-xs leading-5 text-zinc-100', className)}
    >
      {lines.length === 0 ? (
        <span className="text-zinc-500">{empty}</span>
      ) : (
        lines.map((l, i) => {
          const clean = l.replace(ansi, '')
          const step = clean.startsWith('===> ')
          return (
            <div key={i} className={cn(step && 'mt-2 font-semibold text-orange-300', /error|failed/i.test(clean) && !step && 'text-red-300')}>
              {clean}
            </div>
          )
        })
      )}
    </pre>
  )
}

/** Hook: streams NDJSON log lines from a path while enabled. */
export function useLogStream(path: string | null, deps: unknown[]) {
  const [lines, setLines] = useState<LogLine[]>([])
  const [error, setError] = useState<string | null>(null)
  const [reset, setReset] = useState(0)
  useEffect(() => {
    if (!path) return
    const ac = new AbortController()
    let first = true
    // Batch incoming lines and flush at most every 100 ms: re-rendering the
    // list per line is what makes log tails expensive in the browser.
    let pending: LogLine[] = []
    let timer: ReturnType<typeof setTimeout> | null = null
    const flush = () => {
      timer = null
      if (pending.length === 0) return
      const batch = pending
      pending = []
      setLines((prev) => {
        const base = first ? [] : prev
        first = false
        const next = base.concat(batch)
        return next.length > 5000 ? next.slice(-4000) : next
      })
    }
    apiStream(path, ac.signal, (raw) => {
      let l: LogLine
      try {
        l = JSON.parse(raw) as LogLine
      } catch {
        l = { i: '', p: '', m: raw }
      }
      pending.push(l)
      if (!timer) timer = setTimeout(flush, 100)
    })
      .then(flush)
      .catch((e: Error) => {
        if (e.name !== 'AbortError') setError(e.message)
      })
    return () => {
      ac.abort()
      if (timer) clearTimeout(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, reset, ...deps])
  return { lines, error, restart: () => {
    setLines([])
    setError(null)
    setReset((n) => n + 1)
  } }
}
