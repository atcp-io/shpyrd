import { getToken, setToken } from './auth'

// ---- types mirroring pkg/api ----------------------------------------------

export type PublicConfig = {
  version: string
  domain: string
  httpsPort: string
  grafanaUrl: string
  authRequired: boolean
  metrics: boolean
}

export type ProcessStatus = {
  desired: number
  ready: number
  updated?: number
  failing?: number
  reason?: string
  size?: string
  cpu?: string
  memory?: string
}

export type InstanceSize = { name: string; kind: 'shared' | 'dedicated'; cpu: string; memory: string; description?: string }
export type SizeCatalog = { default: string; sizes: InstanceSize[] }

export type AppSummary = {
  name: string
  namespace: string
  phase: string
  message?: string
  url?: string
  digest?: string
  release: number
  source?: string
  processes?: Record<string, ProcessStatus>
  createdAt: string
}

export type Release = {
  number: number
  digest: string
  build?: number
  source?: string
  description?: string
  createdAt: string
  processes?: string[]
  kind: 'deploy' | 'config' | 'rollback'
}

export type Condition = { type: string; status: string; reason?: string; message?: string; lastTransitionTime: string }

export type ProcessSpec = { replicas?: number; port?: number; command?: string[]; args?: string[]; size?: string }

export type AppDetail = {
  name: string
  namespace: string
  createdAt: string
  spec: {
    source?: {
      git?: { url: string; revision?: string }
      blob?: { sha256?: string; ref?: string }
      subPath?: string
    }
    pinnedDigest?: string
    processes?: Record<string, ProcessSpec>
    env?: { name: string; value?: string }[]
    domains?: string[]
    build?: { env?: { name: string; value?: string }[]; builder?: string }
  }
  status: {
    phase: string
    message?: string
    digest?: string
    url?: string
    latestBuild?: string
    releases: Release[]
    conditions?: Condition[]
  }
  processes?: Record<string, ProcessStatus>
}

export type Point = [number, number]
export type Series = { name: string; points: Point[] }
export type Chart = {
  id: string
  title: string
  unit: 'rps' | 'ms' | 'cores' | 'bytes' | 'count' | 'bytes/s' | string
  kind: 'line' | 'stacked' | 'step'
  series: Series[]
  error?: string
}
export type MetricsResponse = {
  range: string
  step: number
  charts: Chart[]
  releases: { number: number; time: number; label: string }[]
}

export type BuildInfo = {
  name: string
  number: number
  status: 'Building' | 'Succeeded' | 'Failed'
  reason?: string
  message?: string
  digest?: string
  source?: string
  startedAt: string
  completedAt?: string
  stepsCompleted?: string[]
}

export type ConfigVar = { name: string; updatedAt?: string }

export type LogLine = { t?: string; i: string; p: string; m: string }

export type ClusterSummary = {
  install?: { profile: string; version: string; domain: string; updatedAt: string }
  components: { name: string; version?: string; appliedAt: string }[]
  nodes: {
    name: string
    ready: boolean
    roles: string
    arch: string
    os: string
    kubeletVersion: string
    cpu: string
    memory: string
    pods: string
  }[]
  apps: number
  phases: Record<string, number>
}

export type NodeUsage = {
  name: string
  cpuUsedPct: number
  memoryUsedPct: number
  cpuRequestedPct: number
  memoryRequestedPct: number
  cpuCores: number
  memoryBytes: number
  pods: number
  podCapacity: number
}

export type ClusterMetrics = {
  range: string
  nodes: NodeUsage[]
  total: NodeUsage
  charts: Chart[]
}

export type HelmRelease = {
  name: string
  namespace: string
  chart: string
  chartVersion: string
  appVersion?: string
  status: string
  revision: number
  updatedAt: string
}

// ---- client -----------------------------------------------------------------

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

function headers(extra?: HeadersInit): Headers {
  const h = new Headers(extra)
  const tok = getToken()
  if (tok) h.set('Authorization', `Bearer ${tok}`)
  return h
}

async function handle(res: Response): Promise<Response> {
  if (res.status === 401) {
    setToken(null)
    throw new ApiError(401, 'unauthorized')
  }
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body?.error) msg = body.error
    } catch {
      // not json
    }
    throw new ApiError(res.status, msg)
  }
  return res
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await handle(await fetch(path, { ...init, headers: headers(init.headers) }))
  return (await res.json()) as T
}

function json(method: string, body: unknown): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }
}

/** Streams a text response line by line until aborted or finished. */
export async function apiStream(path: string, signal: AbortSignal, onLine: (line: string) => void): Promise<void> {
  const res = await handle(await fetch(path, { headers: headers(), signal }))
  const reader = res.body?.getReader()
  if (!reader) return
  const dec = new TextDecoder()
  let buf = ''
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    buf += dec.decode(value, { stream: true })
    let i: number
    while ((i = buf.indexOf('\n')) >= 0) {
      onLine(buf.slice(0, i))
      buf = buf.slice(i + 1)
    }
  }
  if (buf) onLine(buf)
}

const app = (ns: string, name: string) => `/api/apps/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`

export const api = {
  config: () => request<PublicConfig>('/api/config'),
  apps: () => request<AppSummary[]>('/api/apps'),
  createApp: (body: { name: string; domains?: string[]; processes?: Record<string, ProcessSpec>; git?: { url: string; revision?: string }; subPath?: string }) =>
    request<AppSummary>('/api/apps', json('POST', body)),
  app: (ns: string, name: string) => request<AppDetail>(app(ns, name)),
  deleteApp: (ns: string, name: string) => request<{ status: string }>(app(ns, name), { method: 'DELETE' }),
  deploy: (ns: string, name: string, body: { git?: { url: string; revision?: string }; subPath?: string; image?: string }) =>
    request<AppSummary>(`${app(ns, name)}/deploy`, json('POST', body)),
  configVars: (ns: string, name: string) => request<{ vars: ConfigVar[] }>(`${app(ns, name)}/secrets`),
  updateConfigVars: (ns: string, name: string, body: { set?: Record<string, string>; unset?: string[]; dotenv?: string }) =>
    request<{ vars: ConfigVar[] }>(`${app(ns, name)}/secrets`, json('PUT', body)),
  metrics: (ns: string, name: string, range: string) => request<MetricsResponse>(`${app(ns, name)}/metrics?range=${range}`),
  builds: (ns: string, name: string) => request<BuildInfo[]>(`${app(ns, name)}/builds`),
  buildLogsPath: (ns: string, name: string, build: string, follow: boolean) =>
    `${app(ns, name)}/builds/${encodeURIComponent(build)}/logs?follow=${follow}`,
  logsPath: (ns: string, name: string, q: { process?: string; tail?: number; follow?: boolean }) => {
    const p = new URLSearchParams({ format: 'json', tail: String(q.tail ?? 200), follow: q.follow ? 'true' : 'false' })
    if (q.process) p.set('process', q.process)
    return `${app(ns, name)}/logs?${p}`
  },
  scale: (ns: string, name: string, process: string, replicas: number) =>
    request<AppSummary>(`${app(ns, name)}/scale`, json('POST', { process, replicas })),
  resize: (ns: string, name: string, process: string, size: string) =>
    request<AppSummary>(`${app(ns, name)}/resize`, json('POST', { process, size })),
  sizes: () => request<SizeCatalog>('/api/sizes'),
  saveSizes: (catalog: SizeCatalog) => request<SizeCatalog>('/api/sizes', json('PUT', catalog)),
  rollback: (ns: string, name: string, release: number) =>
    request<AppSummary>(`${app(ns, name)}/rollback`, json('POST', { release })),
  cluster: () => request<ClusterSummary>('/api/cluster'),
  clusterMetrics: (range: string) => request<ClusterMetrics>(`/api/cluster/metrics?range=${range}`),
  helmReleases: () => request<HelmRelease[]>('/api/helm/releases'),
}
