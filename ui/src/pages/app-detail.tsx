import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, ArrowLeft, ExternalLink, Hammer, KeyRound, Loader2, Minus, Plus, RefreshCw, Rocket, Trash2, Undo2, X } from 'lucide-react'
import { toast } from 'sonner'
import { api, apiStream, type AppDetail, type BuildInfo } from '@/lib/api'
import { ago, duration } from '@/lib/format'
import { PhaseBadge } from '@/components/phase-badge'
import { ProcessChips } from '@/components/process-chips'
import { MetricChart } from '@/components/metric-chart'
import { AppLogView, TextLogView, useLogStream } from '@/components/log-view'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { cn } from '@/lib/utils'

export function AppDetailPage() {
  const { ns = '', name = '' } = useParams()
  const qc = useQueryClient()
  const app = useQuery({ queryKey: ['app', ns, name], queryFn: () => api.app(ns, name), refetchInterval: 4000 })

  if (app.isLoading) {
    return (
      <div className="grid gap-4">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-40 w-full" />
      </div>
    )
  }
  if (app.error || !app.data) {
    return (
      <Alert variant="destructive">
        <AlertTitle>Could not load project</AlertTitle>
        <AlertDescription>{(app.error as Error)?.message ?? 'not found'}</AlertDescription>
      </Alert>
    )
  }
  const a = app.data
  const refresh = () => qc.invalidateQueries({ queryKey: ['app', ns, name] })
  const building = a.status.phase === 'Building'
  const busy = isBusy(a)

  return (
    <div className="grid gap-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid gap-1">
          <Link to="/" className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:underline">
            <ArrowLeft className="size-3" /> Projects
          </Link>
          <div className="flex items-center gap-3">
            <h1 className="text-2xl font-semibold tracking-tight">{a.name}</h1>
            <PhaseBadge phase={a.status.phase} />
          </div>
          <div className="flex flex-wrap items-center gap-3 pt-1">
            <ProcessChips processes={a.processes} />
            {a.status.url && (
              <a href={a.status.url} target="_blank" rel="noreferrer" className="text-xs text-muted-foreground hover:underline">
                {a.status.url.replace(/^https:\/\//, '')}
              </a>
            )}
          </div>
        </div>
        <div className="flex items-center gap-2">
          {a.status.url && (
            <Button asChild variant="outline" size="sm">
              <a href={a.status.url} target="_blank" rel="noreferrer">
                Open <ExternalLink data-icon="inline-end" />
              </a>
            </Button>
          )}
          <DeployDialog app={a} onDone={refresh} disabled={busy} />
          <DestroyDialog app={a} />
        </div>
      </div>

      <ActivityPanel app={a} onChanged={refresh} />
      {building && <BuildBanner app={a} />}

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="metrics">Metrics</TabsTrigger>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="builds">Builds</TabsTrigger>
          <TabsTrigger value="config">Config</TabsTrigger>
        </TabsList>
        <TabsContent value="overview" className="grid gap-6 pt-4">
          <Overview app={a} onChanged={refresh} />
        </TabsContent>
        <TabsContent value="metrics" className="pt-4">
          <Metrics app={a} />
        </TabsContent>
        <TabsContent value="logs" className="pt-4">
          <Logs app={a} />
        </TabsContent>
        <TabsContent value="builds" className="pt-4">
          <Builds app={a} />
        </TabsContent>
        <TabsContent value="config" className="pt-4">
          <Config app={a} />
        </TabsContent>
      </Tabs>
    </div>
  )
}

/** Streams a text endpoint into a lines state, batching updates (100 ms) so
 * long outputs do not re-render per line. Returns the effect cleanup. */
function streamText(path: string, ac: AbortController, setLines: (f: (prev: string[]) => string[]) => void, onError?: (e: Error) => void) {
  let first = true
  let pending: string[] = []
  let timer: ReturnType<typeof setTimeout> | null = null
  const flush = () => {
    timer = null
    if (pending.length === 0) return
    const batch = pending
    pending = []
    setLines((prev) => {
      const base = first ? [] : prev
      first = false
      return base.concat(batch)
    })
  }
  apiStream(path, ac.signal, (l) => {
    pending.push(l)
    if (!timer) timer = setTimeout(flush, 100)
  })
    .then(flush)
    .catch((e: Error) => {
      if (e.name !== 'AbortError') onError?.(e)
    })
  return () => {
    ac.abort()
    if (timer) clearTimeout(timer)
  }
}

// ---- activity: what is happening right now ------------------------------------

/** A release is in flight: building or rolling out. Actions that would start
 * another release are disabled meanwhile. */
function isBusy(app: AppDetail): boolean {
  return app.status.phase === 'Building' || app.status.phase === 'Deploying'
}

function ActivityPanel({ app, onChanged }: { app: AppDetail; onChanged: () => void }) {
  const current = app.status.releases[app.status.releases.length - 1]
  const previous = app.status.releases[app.status.releases.length - 2]
  const phase = app.status.phase
  const procs = Object.entries(app.processes ?? {}).sort(([a], [b]) => a.localeCompare(b))
  const rollback = useMutation({
    mutationFn: (n: number) => api.rollback(app.namespace, app.name, n),
    onSuccess: (_, n) => {
      toast.success(`Rolling back to v${n}`)
      onChanged()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  if (phase === 'Pending') {
    return (
      <Alert>
        <Rocket className="size-4" />
        <AlertTitle>Nothing deployed yet</AlertTitle>
        <AlertDescription>
          Use <strong>Deploy</strong> to build from a Git repository, or run <code className="font-mono text-xs">shpyrd deploy --project {app.name}</code>{' '}
          from a checkout.
        </AlertDescription>
      </Alert>
    )
  }
  if (phase === 'Deploying') {
    return (
      <Card className="border-amber-500/30">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <Loader2 className="size-4 animate-spin text-amber-500" />
            Rolling out {current ? `v${current.number}` : ''}
            {current?.description && <span className="font-normal text-muted-foreground">— {current.description}</span>}
          </CardTitle>
          <CardDescription>New instances start one by one; previous instances keep serving until the new ones are ready.</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-3 sm:grid-cols-2">
          {procs.map(([name, p]) => (
            <RolloutRow key={name} name={name} status={p} />
          ))}
        </CardContent>
      </Card>
    )
  }
  if (phase === 'Failed') {
    return (
      <Card className="border-red-500/40">
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm text-red-500">
            <AlertTriangle className="size-4" /> Release {current ? `v${current.number}` : ''} is not healthy
          </CardTitle>
          <CardDescription className="break-words">{app.status.message}</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap items-center gap-3">
          {procs.map(([name, p]) => (
            <RolloutRow key={name} name={name} status={p} />
          ))}
          {previous && (
            <Button size="sm" variant="outline" className="ml-auto" disabled={rollback.isPending} onClick={() => rollback.mutate(previous.number)}>
              <Undo2 data-icon="inline-start" /> Roll back to v{previous.number}
            </Button>
          )}
        </CardContent>
      </Card>
    )
  }
  return null
}

function RolloutRow({ name, status }: { name: string; status: { desired: number; ready: number; updated?: number; failing?: number; reason?: string } }) {
  const updated = status.updated ?? 0
  const pct = status.desired ? Math.round((Math.min(updated, status.desired) / status.desired) * 100) : 100
  const failing = (status.failing ?? 0) > 0
  return (
    <div className="grid min-w-56 flex-1 gap-1 text-xs">
      <div className="flex justify-between">
        <span className="font-medium">{name}</span>
        <span className={cn('font-mono text-muted-foreground', failing && 'text-red-500')}>
          {failing ? `${status.failing} failing` : `${updated}/${status.desired} on new release · ${status.ready} serving`}
        </span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className={cn('h-full rounded-full transition-all', failing ? 'bg-red-500' : 'bg-amber-500')} style={{ width: `${pct}%` }} />
      </div>
      {failing && status.reason && <span className="break-words text-[11px] text-red-500/80">{status.reason}</span>}
    </div>
  )
}

// ---- build banner (live build output while Building) -------------------------

function BuildBanner({ app }: { app: AppDetail }) {
  const build = app.status.latestBuild
  const [lines, setLines] = useState<string[]>([])
  useEffect(() => {
    if (!build) return
    const ac = new AbortController()
    return streamText(api.buildLogsPath(app.namespace, app.name, build, true), ac, setLines)
  }, [app.namespace, app.name, build])
  return (
    <Card className="border-sky-500/30">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-sm">
          <Hammer className="size-4 animate-pulse text-sky-400" /> Building {build ? <span className="font-mono text-xs text-muted-foreground">{build}</span> : null}
        </CardTitle>
        <CardDescription>{app.status.message}</CardDescription>
      </CardHeader>
      <CardContent>
        <TextLogView lines={lines} className="h-64" empty={build ? 'Waiting for the build to start...' : 'Waiting for kpack to schedule the build...'} />
      </CardContent>
    </Card>
  )
}

// ---- overview -----------------------------------------------------------------

function Overview({ app, onChanged }: { app: AppDetail; onChanged: () => void }) {
  const processes = Object.keys({ ...(app.spec.processes ?? { web: {} }), ...(app.processes ?? {}) }).sort()
  const releases = [...app.status.releases].reverse()
  const current = releases[0]
  const busy = isBusy(app)

  const rollback = useMutation({
    mutationFn: (n: number) => api.rollback(app.namespace, app.name, n),
    onSuccess: (_, n) => {
      toast.success(`Rolling back to v${n} (build and config)`)
      onChanged()
    },
    onError: (e: Error) => toast.error(e.message),
  })

  return (
    <>
      <div className="grid gap-4 md:grid-cols-3">
        <Card size="sm">
          <CardHeader>
            <CardTitle className="text-sm">Source</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-1 text-sm">
            {app.spec.source?.git && (
              <>
                <Row k="Git" v={app.spec.source.git.url} mono />
                <Row k="Revision" v={app.spec.source.git.revision || 'main'} mono />
              </>
            )}
            {app.spec.source?.blob && (
              <>
                <Row k="Archive" v={app.spec.source.blob.sha256?.slice(0, 12) ?? '-'} mono />
                <Row k="Commit" v={app.spec.source.blob.ref || 'local checkout'} mono />
              </>
            )}
            {app.spec.source?.subPath && <Row k="Directory" v={app.spec.source.subPath} mono />}
            {!app.spec.source && !app.spec.pinnedDigest && (
              <p className="text-muted-foreground">
                No source yet. Use <strong>Deploy</strong> above or <code className="font-mono text-xs">shpyrd deploy --project {app.name}</code>.
              </p>
            )}
            {app.spec.pinnedDigest && <Row k="Pinned build" v={app.spec.pinnedDigest} mono />}
            {app.spec.build?.env?.length ? (
              <Row k="Build env" v={app.spec.build.env.map((e) => `${e.name}=${e.value ?? ''}`).join(' ')} mono />
            ) : null}
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader>
            <CardTitle className="text-sm">Release</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-1 text-sm">
            <Row k="Current" v={current ? `v${current.number} · ${current.description ?? ''}` : '-'} />
            <Row k="Build" v={current?.build ? `#${current.build}` : app.status.digest?.slice(0, 7) || '-'} mono />
            <Row k="Domains" v={(app.spec.domains?.length ? app.spec.domains : [app.status.url?.replace(/^https:\/\//, '') ?? '-']).join(', ')} mono />
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader>
            <CardTitle className="text-sm">Processes</CardTitle>
            <CardDescription>Instances and size per process type. Sizes come from the cluster catalog (Cluster page).</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-3">
            {processes.map((p) => (
              <ProcessRow key={p} app={app} process={p} onChanged={onChanged} />
            ))}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Releases</CardTitle>
          <CardDescription>
            A release is a build plus its config vars. Deploys create new builds; config changes and rollbacks reuse existing ones. Rollback re-releases an
            earlier release exactly as it was.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {releases.length === 0 ? (
            <p className="text-sm text-muted-foreground">No releases yet.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Release</TableHead>
                  <TableHead>What changed</TableHead>
                  <TableHead>Build</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right"></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {releases.map((r, i) => {
                  const currentProcs = Object.keys(app.spec.processes ?? { web: {} })
                  const missing = r.processes ? currentProcs.filter((p) => !r.processes!.includes(p)) : []
                  return (
                    <TableRow key={r.number}>
                      <TableCell className="font-mono text-xs">
                        v{r.number}{' '}
                        {i === 0 && (
                          <Badge variant="secondary" className="ml-1">
                            current
                          </Badge>
                        )}
                      </TableCell>
                      <TableCell className="text-sm">
                        <ReleaseKind kind={r.kind} />
                        {r.description}
                        {r.processes && r.processes.length > 0 && (
                          <span className="ml-2 font-mono text-[10px] text-muted-foreground">{r.processes.join(' · ')}</span>
                        )}
                        {i !== 0 && missing.length > 0 && (
                          <span className="ml-2 text-[10px] text-amber-500" title={`This build has no ${missing.join(', ')} process; those instances would fail to start`}>
                            no {missing.join(', ')} process
                          </span>
                        )}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground" title={r.digest ? `image digest ${r.digest}` : undefined}>
                        {r.build ? `#${r.build}` : r.digest ? r.digest.slice(0, 7) : '-'}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">{ago(r.createdAt)}</TableCell>
                      <TableCell className="text-right">
                        <Button
                          variant="outline"
                          size="xs"
                          disabled={i === 0 || busy || rollback.isPending}
                          title={
                            i === 0
                              ? 'Current release'
                              : busy
                                ? 'Wait for the current release to finish rolling out'
                                : missing.length
                                  ? `Re-release v${r.number}; its build has no ${missing.join(', ')} process`
                                  : `Re-release v${r.number}: its build and its config vars`
                          }
                          onClick={() => rollback.mutate(r.number)}
                        >
                          Rollback
                        </Button>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </>
  )
}

function ReleaseKind({ kind }: { kind: string }) {
  const cls =
    kind === 'rollback'
      ? 'border-violet-500/30 bg-violet-500/10 text-violet-600 dark:text-violet-300'
      : kind === 'config'
        ? 'border-sky-500/30 bg-sky-500/10 text-sky-600 dark:text-sky-300'
        : 'border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-300'
  return (
    <Badge variant="outline" className={cn('mr-2 text-[10px] uppercase tracking-wide', cls)}>
      {kind}
    </Badge>
  )
}

function Row({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <span className="shrink-0 text-muted-foreground">{k}</span>
      <span className={cn('truncate text-right', mono && 'font-mono text-xs')} title={v}>
        {v}
      </span>
    </div>
  )
}

function ProcessRow({ app, process, onChanged }: { app: AppDetail; process: string; onChanged: () => void }) {
  const desired = app.spec.processes?.[process]?.replicas ?? app.processes?.[process]?.desired ?? 1
  const st = app.processes?.[process]
  const catalog = useQuery({ queryKey: ['sizes'], queryFn: api.sizes, staleTime: 60_000 })
  const scale = useMutation({
    mutationFn: (n: number) => api.scale(app.namespace, app.name, process, n),
    onSuccess: (_, n) => {
      toast.success(`${process} scaled to ${n}`)
      onChanged()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const resize = useMutation({
    mutationFn: (size: string) => api.resize(app.namespace, app.name, process, size),
    onSuccess: (_, size) => {
      toast.success(`${process} resized to ${size}; new release rolling out`)
      onChanged()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const spec = app.spec.processes?.[process]
  const sizeName = spec?.size || st?.size || catalog.data?.default || ''
  const alloc = st?.cpu && st?.memory ? `${st.cpu} CPU · ${st.memory}` : ''
  const ok = st ? st.ready >= st.desired && st.desired > 0 : false
  const busy = isBusy(app)
  return (
    <div className="grid gap-1.5 text-sm">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className={cn('size-2 rounded-full', !st || st.desired === 0 ? 'bg-muted-foreground' : ok ? 'bg-emerald-500' : 'animate-pulse bg-amber-500')} />
          <span className="font-medium">{process}</span>
          {spec?.port && <span className="font-mono text-[10px] text-muted-foreground">:{spec.port}</span>}
          <span className="text-xs text-muted-foreground">{st ? `${st.ready} of ${st.desired} running` : 'not deployed'}</span>
        </div>
        <div className="flex items-center gap-1">
          <Button variant="outline" size="icon-xs" disabled={desired <= 0 || scale.isPending} onClick={() => scale.mutate(desired - 1)}>
            <Minus />
          </Button>
          <span className="w-6 text-center font-mono text-xs">{desired}</span>
          <Button variant="outline" size="icon-xs" disabled={scale.isPending} onClick={() => scale.mutate(desired + 1)}>
            <Plus />
          </Button>
        </div>
      </div>
      <div className="flex items-center justify-between gap-2 pl-4">
        <Select value={sizeName} onValueChange={(v) => resize.mutate(v)} disabled={busy || resize.isPending || !catalog.data}>
          <SelectTrigger className="h-7 w-44 text-xs" size="sm">
            <SelectValue placeholder="size" />
          </SelectTrigger>
          <SelectContent>
            {catalog.data?.sizes.map((sz) => (
              <SelectItem key={sz.name} value={sz.name}>
                <span className="font-mono text-xs">{sz.name}</span>
                <span className="ml-2 text-xs text-muted-foreground">
                  {sz.cpu} CPU · {sz.memory}
                </span>
              </SelectItem>
            ))}
            {sizeName === 'custom' && <SelectItem value="custom">custom</SelectItem>}
          </SelectContent>
        </Select>
        <span className="font-mono text-[11px] text-muted-foreground">{alloc}</span>
      </div>
    </div>
  )
}

// ---- metrics ------------------------------------------------------------------

function Metrics({ app }: { app: AppDetail }) {
  const [range, setRange] = useState('1h')
  const config = useQuery({ queryKey: ['config'], queryFn: api.config, staleTime: 60_000 })
  const m = useQuery({
    queryKey: ['metrics', app.namespace, app.name, range],
    queryFn: () => api.metrics(app.namespace, app.name, range),
    refetchInterval: 30_000,
    enabled: config.data?.metrics !== false,
  })
  if (config.data && !config.data.metrics) {
    return (
      <Alert>
        <AlertTitle>Metrics disabled</AlertTitle>
        <AlertDescription>The server has no Prometheus configured (install the monitoring component).</AlertDescription>
      </Alert>
    )
  }
  return (
    <div className="grid gap-4">
      <div className="flex items-center justify-between gap-4">
        <p className="text-sm text-muted-foreground">
          Traffic measured at the edge; CPU and memory as a percentage of each process allocation. Orange dashed lines mark releases,
          the red line is 100%.
        </p>
        <Select value={range} onValueChange={setRange}>
          <SelectTrigger className="w-32" size="sm">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="1h">Last hour</SelectItem>
            <SelectItem value="6h">Last 6 hours</SelectItem>
            <SelectItem value="24h">Last 24 hours</SelectItem>
            <SelectItem value="7d">Last 7 days</SelectItem>
          </SelectContent>
        </Select>
      </div>
      {m.error && (
        <Alert variant="destructive">
          <AlertTitle>Could not load metrics</AlertTitle>
          <AlertDescription>{(m.error as Error).message}</AlertDescription>
        </Alert>
      )}
      <div className="grid gap-4 md:grid-cols-2">
        {(m.data?.charts ?? []).map((c) => (
          <MetricChart key={c.id} chart={c} range={range} releases={m.data?.releases} />
        ))}
        {m.isLoading && [0, 1, 2, 3].map((i) => <Skeleton key={i} className="h-60 w-full" />)}
      </div>
    </div>
  )
}

// ---- logs ---------------------------------------------------------------------

function Logs({ app }: { app: AppDetail }) {
  const processes = Object.keys(app.processes ?? app.spec.processes ?? { web: {} }).sort()
  const [process, setProcess] = useState<string>('all')
  const [follow, setFollow] = useState(true)
  const [filter, setFilter] = useState('')
  const path = useMemo(
    () => api.logsPath(app.namespace, app.name, { process: process === 'all' ? undefined : process, tail: 300, follow }),
    [app.namespace, app.name, process, follow],
  )
  const { lines, error, restart } = useLogStream(path, [])

  return (
    <div className="grid gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Select value={process} onValueChange={setProcess}>
          <SelectTrigger className="w-40" size="sm">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All processes</SelectItem>
            {processes.map((p) => (
              <SelectItem key={p} value={p}>
                {p}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter lines..." className="h-7 w-56 text-xs" />
        <Button variant={follow ? 'default' : 'outline'} size="sm" onClick={() => setFollow((f) => !f)}>
          {follow ? 'Live' : 'Paused'}
        </Button>
        <Button variant="outline" size="sm" onClick={restart}>
          <RefreshCw data-icon="inline-start" /> Reload
        </Button>
        <span className="ml-auto text-xs text-muted-foreground">{lines.length} lines</span>
      </div>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <AppLogView lines={lines} follow={follow} filter={filter} empty={error ? '' : 'Waiting for log lines...'} />
    </div>
  )
}

// ---- builds -------------------------------------------------------------------

function Builds({ app }: { app: AppDetail }) {
  const builds = useQuery({
    queryKey: ['builds', app.namespace, app.name],
    queryFn: () => api.builds(app.namespace, app.name),
    refetchInterval: app.status.phase === 'Building' ? 5000 : 30_000,
  })
  const [selected, setSelected] = useState<string | null>(null)
  const list = builds.data ?? []
  const active = list.find((b) => b.name === selected) ?? list[0]
  const [lines, setLines] = useState<string[]>([])
  const activeName = active?.name
  const activeStatus = active?.status
  useEffect(() => {
    if (!activeName) return
    const ac = new AbortController()
    return streamText(api.buildLogsPath(app.namespace, app.name, activeName, activeStatus === 'Building'), ac, setLines, (e) =>
      setLines([`(${e.message})`]),
    )
  }, [app.namespace, app.name, activeName, activeStatus])

  return (
    <div className="grid gap-4 lg:grid-cols-[20rem_1fr]">
      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-sm">Builds</CardTitle>
          <CardDescription>
            Each deploy compiles the source into a build; releases (including config changes and rollbacks) run one of these builds.
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-1">
          {list.length === 0 && <p className="text-sm text-muted-foreground">No builds yet.</p>}
          {list.map((b) => (
            <BuildRow
              key={b.name}
              build={b}
              active={b.name === active?.name}
              onSelect={() => setSelected(b.name)}
              releases={app.status.releases.filter((r) => r.build === b.number).map((r) => r.number)}
            />
          ))}
        </CardContent>
      </Card>
      <div className="grid gap-2">
        {active && (
          <div className="flex items-center gap-2 text-sm">
            <span className="font-mono text-xs">{active.name}</span>
            <BuildStatus status={active.status} />
            {active.reason && <span className="text-xs text-muted-foreground">reason: {active.reason.toLowerCase()}</span>}
            {active.digest && <span className="ml-auto font-mono text-xs text-muted-foreground">build {active.digest}</span>}
          </div>
        )}
        <TextLogView lines={lines} follow={active?.status === 'Building'} empty={active ? 'Loading...' : 'Select a build'} />
      </div>
    </div>
  )
}

function BuildRow({ build, active, onSelect, releases }: { build: BuildInfo; active: boolean; onSelect: () => void; releases: number[] }) {
  return (
    <button
      type="button"
      onClick={onSelect}
      className={cn(
        'grid gap-0.5 rounded-md border px-3 py-2 text-left text-sm transition-colors hover:bg-accent',
        active && 'border-primary/50 bg-accent',
      )}
    >
      <div className="flex items-center justify-between">
        <span className="font-medium">Build #{build.number}</span>
        <BuildStatus status={build.status} />
      </div>
      <div className="flex justify-between text-xs text-muted-foreground">
        <span>{build.source || '-'}</span>
        <span>
          {ago(build.startedAt)} · {duration(build.startedAt, build.completedAt)}
        </span>
      </div>
      {releases.length > 0 && (
        <div className="text-[10px] text-muted-foreground">used by {releases.map((n) => `v${n}`).join(', ')}</div>
      )}
    </button>
  )
}

function BuildStatus({ status }: { status: BuildInfo['status'] }) {
  const cls =
    status === 'Succeeded'
      ? 'bg-emerald-500/15 text-emerald-500 border-emerald-500/30'
      : status === 'Failed'
        ? 'bg-red-500/15 text-red-500 border-red-500/30'
        : 'bg-sky-500/15 text-sky-400 border-sky-500/30'
  return (
    <Badge variant="outline" className={cn('text-[10px]', cls)}>
      {status}
    </Badge>
  )
}

// ---- config (write-only config vars) -----------------------------------------

function Config({ app }: { app: AppDetail }) {
  const qc = useQueryClient()
  const key = ['config-vars', app.namespace, app.name]
  const vars = useQuery({ queryKey: key, queryFn: () => api.configVars(app.namespace, app.name), refetchInterval: 15_000 })
  const update = useMutation({
    mutationFn: (body: { set?: Record<string, string>; unset?: string[]; dotenv?: string }) => api.updateConfigVars(app.namespace, app.name, body),
    onSuccess: (_, body) => {
      const n = Object.keys(body.set ?? {}).length + (body.dotenv ? body.dotenv.split('\n').filter((l) => l.includes('=')).length : 0)
      toast.success(body.unset?.length ? `Removed ${body.unset.join(', ')}` : `Saved ${n} config var${n === 1 ? '' : 's'}; restarting processes`)
      qc.invalidateQueries({ queryKey: key })
    },
    onError: (e: Error) => toast.error(e.message),
  })
  const [newName, setNewName] = useState('')
  const [newValue, setNewValue] = useState('')
  const [editing, setEditing] = useState<string | null>(null)
  const [editValue, setEditValue] = useState('')
  const validName = /^[A-Za-z_][A-Za-z0-9_]*$/.test(newName)

  return (
    <div className="grid gap-4 lg:grid-cols-[1fr_20rem]">
      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-4">
          <div>
            <CardTitle className="flex items-center gap-2 text-sm">
              <KeyRound className="size-4" /> Config vars
            </CardTitle>
            <CardDescription>
              Injected into every process as environment variables. Values are write-only: they can be replaced or removed but never
              read back. Changing them creates a release and restarts the processes.
            </CardDescription>
          </div>
          <BulkDialog onSubmit={(dotenv) => update.mutate({ dotenv })} pending={update.isPending} />
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Value</TableHead>
                <TableHead>Updated</TableHead>
                <TableHead className="text-right"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(vars.data?.vars ?? []).length === 0 && (
                <TableRow>
                  <TableCell colSpan={4} className="py-6 text-center text-sm text-muted-foreground">
                    No config vars yet.
                  </TableCell>
                </TableRow>
              )}
              {vars.data?.vars.map((v) => (
                <TableRow key={v.name}>
                  <TableCell className="font-mono text-xs">{v.name}</TableCell>
                  <TableCell>
                    {editing === v.name ? (
                      <form
                        className="flex items-center gap-2"
                        onSubmit={(e) => {
                          e.preventDefault()
                          update.mutate({ set: { [v.name]: editValue } })
                          setEditing(null)
                          setEditValue('')
                        }}
                      >
                        <Input
                          type="password"
                          autoFocus
                          autoComplete="off"
                          value={editValue}
                          onChange={(e) => setEditValue(e.target.value)}
                          placeholder="New value"
                          className="h-7 text-xs"
                        />
                        <Button type="submit" size="xs" disabled={update.isPending}>
                          Save
                        </Button>
                        <Button type="button" size="icon-xs" variant="ghost" onClick={() => setEditing(null)}>
                          <X />
                        </Button>
                      </form>
                    ) : (
                      <span className="font-mono text-xs text-muted-foreground">••••••••</span>
                    )}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">{v.updatedAt ? ago(v.updatedAt) : '-'}</TableCell>
                  <TableCell className="text-right">
                    <div className="inline-flex gap-1">
                      <Button size="xs" variant="outline" onClick={() => setEditing(v.name)} disabled={editing === v.name}>
                        Replace
                      </Button>
                      <Button
                        size="xs"
                        variant="ghost"
                        className="text-destructive hover:text-destructive"
                        onClick={() => update.mutate({ unset: [v.name] })}
                        disabled={update.isPending}
                      >
                        Remove
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <form
            className="mt-4 grid gap-2 sm:grid-cols-[1fr_1fr_auto]"
            onSubmit={(e) => {
              e.preventDefault()
              if (!validName) return
              update.mutate({ set: { [newName]: newValue } })
              setNewName('')
              setNewValue('')
            }}
          >
            <Input value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="NAME" className="font-mono text-xs" autoComplete="off" />
            <Input type="password" value={newValue} onChange={(e) => setNewValue(e.target.value)} placeholder="value" autoComplete="off" />
            <Button type="submit" size="sm" disabled={!validName || update.isPending}>
              <Plus data-icon="inline-start" /> Add
            </Button>
          </form>
        </CardContent>
      </Card>

      <Card size="sm">
        <CardHeader>
          <CardTitle className="text-sm">Plain environment</CardTitle>
          <CardDescription>Non-secret variables from the app spec, plus PORT for processes with a port.</CardDescription>
        </CardHeader>
        <CardContent>
          {app.spec.env?.length ? (
            <ul className="grid gap-1 font-mono text-xs">
              {app.spec.env.map((e) => (
                <li key={e.name} className="flex justify-between rounded border px-2 py-1">
                  <span>{e.name}</span>
                  <span className="text-muted-foreground">{e.value ?? '(from ref)'}</span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm text-muted-foreground">None.</p>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

function BulkDialog({ onSubmit, pending }: { onSubmit: (dotenv: string) => void; pending: boolean }) {
  const [open, setOpen] = useState(false)
  const [text, setText] = useState('')
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="outline">
          Bulk edit
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add from .env</DialogTitle>
          <DialogDescription>Paste KEY=VALUE lines. Existing names are replaced, others are kept. Nothing is echoed back.</DialogDescription>
        </DialogHeader>
        <textarea
          className="h-48 w-full rounded-md border bg-background p-2 font-mono text-xs"
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={'DATABASE_URL=postgres://...\nREDIS_URL=redis://...'}
          autoComplete="off"
          spellCheck={false}
        />
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>
            Cancel
          </Button>
          <Button
            disabled={!text.trim() || pending}
            onClick={() => {
              onSubmit(text)
              setText('')
              setOpen(false)
            }}
          >
            Save
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

// ---- deploy / destroy dialogs -------------------------------------------------

function DeployDialog({ app, onDone, disabled }: { app: AppDetail; onDone: () => void; disabled?: boolean }) {
  const [open, setOpen] = useState(false)
  const [git, setGit] = useState(app.spec.source?.git?.url ?? '')
  const [ref, setRef] = useState(app.spec.source?.git?.revision ?? '')
  const [path, setPath] = useState(app.spec.source?.subPath ?? '')
  const deploy = useMutation({
    mutationFn: () => api.deploy(app.namespace, app.name, { git: { url: git.trim(), revision: ref.trim() || 'main' }, subPath: path.trim() || undefined }),
    onSuccess: () => {
      toast.success('Deploy requested; building')
      setOpen(false)
      onDone()
    },
    onError: (e: Error) => toast.error(e.message),
  })
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" disabled={disabled} title={disabled ? 'Wait for the current release to finish' : undefined}>
          <Rocket data-icon="inline-start" /> Deploy
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deploy from Git</DialogTitle>
          <DialogDescription>
            Builds the repository with buildpacks and releases it. New commits on the branch rebuild automatically. To deploy a local
            checkout use <code className="font-mono text-xs">shpyrd deploy</code>.
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (git.trim()) deploy.mutate()
          }}
        >
          <div className="grid gap-2">
            <Label htmlFor="dep-git">Repository URL</Label>
            <Input id="dep-git" value={git} onChange={(e) => setGit(e.target.value)} placeholder="https://github.com/org/repo" autoFocus />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="grid gap-2">
              <Label htmlFor="dep-ref">Branch, tag or commit</Label>
              <Input id="dep-ref" value={ref} onChange={(e) => setRef(e.target.value)} placeholder="main" />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="dep-path">Directory</Label>
              <Input id="dep-path" value={path} onChange={(e) => setPath(e.target.value)} placeholder="services/api" />
            </div>
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={!git.trim() || deploy.isPending}>
              Deploy
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function DestroyDialog({ app }: { app: AppDetail }) {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [confirm, setConfirm] = useState('')
  const destroy = useMutation({
    mutationFn: () => api.deleteApp(app.namespace, app.name),
    onSuccess: () => {
      toast.success(`Deleting ${app.name}`)
      qc.invalidateQueries({ queryKey: ['apps'] })
      navigate('/')
    },
    onError: (e: Error) => toast.error(e.message),
  })
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm" className="text-destructive hover:text-destructive">
          <Trash2 data-icon="inline-start" /> Destroy
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Destroy {app.name}?</DialogTitle>
          <DialogDescription>
            This deletes the project with all its resources: builds, releases, config vars and running processes. Type its name to confirm.
          </DialogDescription>
        </DialogHeader>
        <Input value={confirm} onChange={(e) => setConfirm(e.target.value)} placeholder={app.name} autoComplete="off" />
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={() => destroy.mutate()} disabled={confirm !== app.name || destroy.isPending}>
            Destroy app
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
