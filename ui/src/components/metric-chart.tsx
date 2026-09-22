import { Area, AreaChart, CartesianGrid, Legend, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import type { Chart } from '@/lib/api'
import { metricValue, timeLabel } from '@/lib/format'
import { cn } from '@/lib/utils'

const palette = ['var(--color-chart-1)', 'var(--color-chart-2)', 'var(--color-chart-3)', 'var(--color-chart-4)', 'var(--color-chart-5)']

// Status classes get semantic colours in the throughput chart.
const classColors: Record<string, string> = {
  '2xx': 'oklch(0.75 0.16 150)',
  '3xx': 'var(--color-chart-2)',
  '4xx': 'oklch(0.80 0.16 85)',
  '5xx': 'oklch(0.65 0.22 25)',
}

const subtitles: Record<string, string> = {
  throughput: 'requests per second by response class',
  latency: 'response time percentiles at the edge',
  instances: 'running instances per process type',
  cpu: 'of each process allocation; shared sizes can burst above 100%',
  memory: 'of each process allocation',
  network: 'pod network traffic',
}

type Props = {
  chart: Chart
  range: string
  releases?: { number: number; time: number; label: string }[]
  className?: string
}

export function MetricChart({ chart, range, releases = [], className }: Props) {
  // Merge series into one row per timestamp: { t, [name]: value }.
  const rows = new Map<number, Record<string, number>>()
  for (const s of chart.series) {
    for (const [t, v] of s.points) {
      const row = rows.get(t) ?? { t }
      row[s.name] = v
      rows.set(t, row)
    }
  }
  const data = [...rows.values()].sort((a, b) => a.t - b.t)
  const last = chart.series.map((s) => ({ name: s.name, v: s.points.length ? s.points[s.points.length - 1][1] : undefined }))
  const stacked = chart.kind === 'stacked' || chart.kind === 'step'
  const step = chart.kind === 'step'
  const percent = chart.unit === '%'
  const subtitle = subtitles[chart.id]
  const hot = percent && last.some((l) => (l.v ?? 0) >= 85)

  return (
    <Card size="sm" className={className}>
      <CardHeader>
        <CardTitle className="grid gap-0.5 text-sm font-medium">
          <div className="flex items-baseline justify-between gap-2">
            <span>{chart.title}</span>
            <span className={cn('truncate font-mono text-xs text-muted-foreground', hot && 'text-red-500')}>
              {last
                .filter((l) => l.v !== undefined)
                .map((l) => `${chart.series.length > 1 || chart.id === 'cpu' || chart.id === 'memory' ? l.name + ' ' : ''}${metricValue(l.v as number, chart.unit)}`)
                .join(' · ') || '-'}
            </span>
          </div>
          {subtitle && (
            <span className="text-xs font-normal text-muted-foreground">
              {chart.unit === 'cores' || chart.unit === 'bytes' ? 'absolute usage (no allocation set yet)' : subtitle}
            </span>
          )}
        </CardTitle>
      </CardHeader>
      <CardContent>
        {chart.error ? (
          <div className="flex h-44 items-center justify-center text-center text-xs text-muted-foreground">{chart.error}</div>
        ) : data.length === 0 ? (
          <div className="flex h-44 items-center justify-center text-xs text-muted-foreground">No data in this range</div>
        ) : (
          <div className="h-44">
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: -12 }}>
                <defs>
                  {chart.series.map((s, i) => (
                    <linearGradient key={s.name} id={`g-${chart.id}-${i}`} x1="0" y1="0" x2="0" y2="1">
                      <stop offset="0%" stopColor={color(chart, s.name, i)} stopOpacity={stacked ? 0.55 : 0.3} />
                      <stop offset="100%" stopColor={color(chart, s.name, i)} stopOpacity={stacked ? 0.35 : 0} />
                    </linearGradient>
                  ))}
                </defs>
                <CartesianGrid vertical={false} stroke="var(--color-border)" strokeDasharray="3 3" />
                <XAxis
                  dataKey="t"
                  type="number"
                  domain={['dataMin', 'dataMax']}
                  tickFormatter={(t: number) => timeLabel(t, range)}
                  tick={{ fontSize: 10, fill: 'var(--color-muted-foreground)' }}
                  tickLine={false}
                  axisLine={false}
                  minTickGap={48}
                />
                <YAxis
                  tick={{ fontSize: 10, fill: 'var(--color-muted-foreground)' }}
                  tickLine={false}
                  axisLine={false}
                  tickFormatter={(v: number) => metricValue(v, chart.unit).replace(/ (rps|cores|ms)$/, '')}
                  width={64}
                  allowDecimals={chart.unit !== 'count'}
                  domain={percent ? [0, (max: number) => Math.max(100, Math.ceil(max / 10) * 10)] : step ? [0, (max: number) => Math.max(1, Math.ceil(max))] : undefined}
                />
                <Tooltip
                  contentStyle={{ background: 'var(--color-popover)', border: '1px solid var(--color-border)', borderRadius: 8, fontSize: 12 }}
                  labelStyle={{ color: 'var(--color-muted-foreground)' }}
                  labelFormatter={(t) => new Date(Number(t) * 1000).toLocaleString()}
                  formatter={(v, name) => [metricValue(Number(v), chart.unit), String(name)]}
                />
                {chart.series.length > 1 && <Legend wrapperStyle={{ fontSize: 11 }} iconSize={8} />}
                {percent && <ReferenceLine y={100} stroke="oklch(0.65 0.22 25)" strokeDasharray="2 4" />}
                {releases.map((r) => (
                  <ReferenceLine
                    key={r.number}
                    x={r.time}
                    stroke="var(--color-primary)"
                    strokeDasharray="4 3"
                    label={{ value: r.label, position: 'insideTopRight', fontSize: 10, fill: 'var(--color-primary)' }}
                  />
                ))}
                {chart.series.map((s, i) => (
                  <Area
                    key={s.name}
                    type={step ? 'stepAfter' : 'monotone'}
                    dataKey={s.name}
                    name={s.name}
                    stackId={stacked ? 'a' : undefined}
                    stroke={color(chart, s.name, i)}
                    fill={`url(#g-${chart.id}-${i})`}
                    strokeWidth={1.5}
                    connectNulls
                    isAnimationActive={false}
                  />
                ))}
              </AreaChart>
            </ResponsiveContainer>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function color(chart: Chart, name: string, i: number): string {
  if (chart.id === 'throughput' && classColors[name]) return classColors[name]
  return palette[i % palette.length]
}
