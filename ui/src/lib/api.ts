import { getToken, setToken } from "./auth";

// ---- types mirroring pkg/api ----------------------------------------------

export type PublicConfig = {
  version: string;
  domain: string;
  httpsPort: string;
  grafanaUrl: string;
  dashboardUrl?: string;
  authRequired: boolean;
  metrics: boolean;
  auth: {
    token: boolean;
    /** Sign-in buttons (external providers); kind picks the icon. */
    providers: { id: string; label: string; kind?: string }[];
    /** Provider behind the email/password form, when one is enabled. */
    password?: { id: string; label: string };
  };
  extensions: string[];
  /** What this server offers beyond the core ("workspaces", ...); empty on the open-source platform. */
  capabilities?: string[];
  /** The workspace answering at this host. */
  workspace?: { slug: string; name: string; implicit: boolean };
  /** Storage rules of this cluster's profile (RFC-0060). */
  volumes?: { minSize?: string; snapshots: boolean };
};

export type Identity = {
  subject: string;
  email?: string;
  name?: string;
  groups?: string[];
  provider: string;
  admin: boolean;
  roles?: {
    platform?: "platform-admin" | "platform-viewer" | "";
    projects?: Record<string, "user" | "viewer" | "developer" | "admin">;
    enforced: boolean;
  };
};

/** The workspace (RFC-0033): the tenant every project belongs to. */
export type WorkspaceInfo = {
  slug: string;
  name: string;
  implicit: boolean;
  /** Apps live one label under it. */
  domain?: string;
  /** Host of an explicit workspace's dashboard; absent for the implicit one. */
  address?: string;
  /** Where this workspace's dashboard answers. */
  url?: string;
  status?: "active" | "suspended";
  /** The workspace's plan (ceilings) and what it uses today; absent without a plan. */
  limits?: {
    projects?: number;
    instances?: number;
    cpu?: string;
    memory?: string;
    storage?: string;
  };
  usage?: {
    projects: number;
    instances: number;
    cpu: string;
    memory: string;
    storage: string;
  };
  joinPolicy: "open" | "company" | "listed";
  createdAt: string;
  updatedAt: string;
};

/** A claimed email domain (RFC-0033): prove it with the TXT record. */
export type DomainClaim = {
  domain: string;
  connector?: string;
  verified: boolean;
  verifiedAt?: string;
  record: string;
  recordValue: string;
};

/** Login methods: Dex connectors plus the local password method. */
export type LoginMethods = {
  password: boolean;
  connectors: { id: string; type: string; name: string; detail?: string }[];
  kinds: string[];
  callback: string;
};

/** Someone the workspace has seen sign in. */
export type Person = {
  email: string;
  name?: string;
  provider?: string;
  groups: string[];
  realm: string;
  status: "active" | "suspended";
  firstSeenAt: string;
  lastSeenAt: string;
};

export type Team = {
  name: string;
  description?: string;
  members: string[];
  groups: string[];
  platformRole?: string;
  /** The built-in team of every person who signed in. */
  everyone?: boolean;
};

export type Member = {
  name: string;
  project: string;
  role: "user" | "viewer" | "developer" | "admin";
  user?: string;
  team?: string;
};

export type AuditEntry = {
  time: string;
  actor: string;
  action: string;
  target?: string;
  detail?: string;
  from?: string;
  via: string;
};

export type LocalUser = { email: string; name?: string; createdAt: string };

export type ExtensionInfo = {
  name: string;
  description: string;
  enabled: boolean;
  component?: string;
};

export type ProcessStatus = {
  desired: number;
  ready: number;
  updated?: number;
  failing?: number;
  reason?: string;
  size?: string;
  cpu?: string;
  memory?: string;
  pinned?: string;
};

export type VolumeInfo = {
  name: string;
  namespace: string;
  size: string;
  capacity?: string;
  shared: boolean;
  storageClass?: string;
  phase: "Pending" | "Bound" | "Failed" | "Restoring" | string;
  message?: string;
  mountedBy: string[];
  createdAt: string;
  /** Snapshot the current disk was restored from, if any. */
  restoredFrom?: string;
  /** Provider rule applied at creation (a size rounded up, for example). */
  note?: string;
};

export type SnapshotInfo = {
  name: string;
  volume: string;
  size?: string;
  ready: boolean;
  message?: string;
  createdAt: string;
};

export type RestoreVolumeResult = {
  volume: VolumeInfo;
  inPlace: boolean;
  message: string;
};

export type InstanceSize = {
  name: string;
  kind: "shared" | "dedicated";
  cpu: string;
  memory: string;
  description?: string;
};
export type SizeCatalog = { default: string; sizes: InstanceSize[] };

export type AppSummary = {
  /** Identifier used in URLs, the CLI and the hostname. */
  slug: string;
  /** Human name; equals the slug when none was given. */
  displayName: string;
  namespace: string;
  phase: string;
  message?: string;
  url?: string;
  digest?: string;
  release: number;
  source?: string;
  processes?: Record<string, ProcessStatus>;
  createdAt: string;
  /** "external" (public LB, default) or "internal" (private LB, RFC-0036). */
  exposure?: "external" | "internal";
  /** Who may open the app (RFC-0033). */
  access: "public" | "authenticated" | "identified";
  allow?: AllowEntry[];
};

export type Release = {
  number: number;
  digest: string;
  build?: number;
  source?: string;
  description?: string;
  createdAt: string;
  processes?: string[];
  kind: "deploy" | "config" | "rollback";
};

export type Condition = {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime: string;
};

export type ProcessSpec = {
  replicas?: number;
  port?: number;
  command?: string[];
  args?: string[];
  size?: string;
  volumes?: { name: string; path: string }[];
};

export type AppDetail = {
  slug: string;
  displayName: string;
  namespace: string;
  createdAt: string;
  spec: {
    source?: {
      git?: { url: string; revision?: string };
      blob?: { sha256?: string; ref?: string };
      subPath?: string;
    };
    pinnedDigest?: string;
    processes?: Record<string, ProcessSpec>;
    env?: { name: string; value?: string }[];
    domains?: string[];
    bindings?: { kind: string; name: string; prefix?: string }[];
    exposure?: "external" | "internal";
    access?: "public" | "authenticated" | "identified";
    build?: {
      strategy?: "buildpacks" | "dockerfile";
      env?: { name: string; value?: string }[];
      builder?: string;
      dockerfile?: string;
      target?: string;
    };
  };
  status: {
    phase: string;
    message?: string;
    digest?: string;
    url?: string;
    latestBuild?: string;
    releases: Release[];
    conditions?: Condition[];
    /** Custom domains' DNS and certificate state (RFC-0034). */
    domains?: DomainStatus[];
  };
  processes?: Record<string, ProcessStatus>;
};

export type DomainStatus = {
  host: string;
  dns: "ok" | "missing" | "wrong" | "unknown";
  target?: string;
  address?: string;
  certificate: "ready" | "issuing" | "failed" | "wildcard";
  message?: string;
};

export type DomainsResult = {
  host?: string;
  target: string;
  address?: string;
  domains: DomainStatus[];
  app: AppSummary;
};

export type Point = [number, number];
export type Series = { name: string; points: Point[] };
export type Chart = {
  id: string;
  title: string;
  unit: "rps" | "ms" | "cores" | "bytes" | "count" | "bytes/s" | string;
  kind: "line" | "stacked" | "step";
  series: Series[];
  error?: string;
};
export type MetricsResponse = {
  range: string;
  step: number;
  charts: Chart[];
  releases: { number: number; time: number; label: string }[];
};

export type BuildInfo = {
  name: string;
  number: number;
  strategy: "buildpacks" | "dockerfile";
  status: "Building" | "Succeeded" | "Failed";
  reason?: string;
  message?: string;
  digest?: string;
  source?: string;
  startedAt: string;
  completedAt?: string;
  stepsCompleted?: string[];
};

export type ConfigVar = { name: string; updatedAt?: string };
export type GlobalsResponse = { vars: ConfigVar[]; projects: number };

/** A log drain (RFC-0023): header names only, never values. */
export type Drain = {
  name: string;
  url: string;
  format: "json" | "syslog";
  processes?: string[];
  headers?: string[];
  cluster: boolean;
  phase: "Pending" | "Active" | "Failing";
  message?: string;
  lastDeliveryAt?: string;
  sent: number;
  errors: number;
  createdAt: string;
};
export type CreateDrain = {
  name?: string;
  url: string;
  format?: "json" | "syslog";
  headers?: Record<string, string>;
  processes?: string[];
};
export type BoundVar = { name: string; provider: string };

export type ResourceInfo = {
  kind: string;
  name: string;
  phase: string;
  message?: string;
  endpoint?: string;
  details?: Record<string, string>;
  attachedTo: string[];
  data: boolean;
  bindable?: boolean;
  createdAt: string;
};

export type LogLine = { t?: string; i: string; p: string; m: string };

export type ClusterSummary = {
  install?: {
    profile: string;
    version: string;
    domain: string;
    updatedAt: string;
  };
  components: { name: string; version?: string; appliedAt: string }[];
  extensions: ExtensionInfo[];
  nodes: {
    name: string;
    ready: boolean;
    roles: string;
    arch: string;
    os: string;
    kubeletVersion: string;
    cpu: string;
    memory: string;
    pods: string;
    instanceType?: string;
    zone?: string;
  }[];
  apps: number;
  phases: Record<string, number>;
  externalLBAddress?: string;
  internalLBAddress?: string;
};

export type NodeUsage = {
  name: string;
  cpuUsedPct: number;
  memoryUsedPct: number;
  cpuRequestedPct: number;
  memoryRequestedPct: number;
  cpuCores: number;
  memoryBytes: number;
  pods: number;
  podCapacity: number;
};

export type ClusterMetrics = {
  range: string;
  nodes: NodeUsage[];
  total: NodeUsage;
  charts: Chart[];
};

// The image registry (RFC-0059).
/** The platform's object store (RFC-0046). */
/** One entry in an app's allow list (RFC-0033 phase 5). */
export type AllowEntry = {
  project?: string;
  platform?: "actions" | "mcp";
};

/** A personal API token (RFC-0031). */
export type APIToken = {
  id: string;
  name: string;
  ownerEmail?: string;
  platformRole?: string;
  projectRoles?: Record<string, string>;
  createdAt: string;
  expiresAt?: string;
  lastUsedAt?: string;
};

/** One tile of the launcher: an app the caller may open (RFC-0033). */
export type LauncherApp = {
  slug: string;
  displayName: string;
  url?: string;
  access: "public" | "authenticated" | "identified";
  phase: string;
  role?: string;
};

/** Platform backups (RFC-0037): the target, the schedule, the archives. */
export type BackupInfo = {
  enabled: boolean;
  target?: string;
  endpoint?: string;
  schedule?: string;
  keep?: number;
  accessKey: boolean;
  lastScheduled?: string;
  lastSuccessful?: string;
  runs: BackupRun[];
  archives: BackupArchive[];
  error?: string;
};

export type BackupRun = {
  name: string;
  status: "running" | "succeeded" | "failed";
  started?: string;
  finished?: string;
  message?: string;
};

export type BackupArchive = {
  name: string;
  size: number;
  modified: string;
};

export type ObjectStorageSummary = {
  endpoint: string;
  totalBytes: number;
  usedBytes: number;
  measuredAt?: string;
  message?: string;
  buckets: {
    namespace: string;
    name: string;
    bucket: string;
    phase: string;
    message?: string;
    usedBytes: number;
    objects: number;
    retentionDays?: number;
  }[];
};

export type RegistryInfo = {
  mode: "in-cluster" | "external";
  host: string;
  tls: boolean;
  ready: boolean;
  message?: string;
  storage?: { usedBytes: number; capacityBytes: number; size: string };
  images?: {
    repositories: number;
    tags: number;
    largest: { name: string; tags: number }[];
    error?: string;
  };
  certificate?: { issuer: string; notAfter: string };
  gc?: {
    schedule: string;
    nextRun?: string;
    running: boolean;
    startedAt?: string;
    lastRun?: string;
    lastResult?: string;
    lastDuration?: string;
    reclaimedBytes: number;
    usedBytes: number;
  };
};

export type HelmRelease = {
  name: string;
  namespace: string;
  chart: string;
  chartVersion: string;
  appVersion?: string;
  status: string;
  revision: number;
  updatedAt: string;
};

// ---- client -----------------------------------------------------------------

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

function headers(extra?: HeadersInit): Headers {
  const h = new Headers(extra);
  const tok = getToken();
  if (tok) h.set("Authorization", `Bearer ${tok}`);
  // Cookie sessions prove intent with the CSRF cookie echoed as a header.
  const csrf = readCookie("shpyrd_csrf");
  if (csrf) h.set("X-Shpyrd-CSRF", csrf);
  return h;
}

function readCookie(name: string): string | null {
  const m = document.cookie.match(new RegExp("(?:^|; )" + name + "=([^;]*)"));
  return m ? decodeURIComponent(m[1]) : null;
}

async function handle(res: Response): Promise<Response> {
  if (res.status === 401) {
    setToken(null);
    throw new ApiError(401, "unauthorized");
  }
  if (!res.ok) {
    let msg = `${res.status} ${res.statusText}`;
    try {
      const body = await res.json();
      if (body?.error) msg = body.error;
    } catch {
      // not json
    }
    throw new ApiError(res.status, msg);
  }
  return res;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const res = await handle(
    await fetch(path, { ...init, headers: headers(init.headers) }),
  );
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

function json(method: string, body: unknown): RequestInit {
  return {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  };
}

/** Streams a text response line by line until aborted or finished. */
export async function apiStream(
  path: string,
  signal: AbortSignal,
  onLine: (line: string) => void,
): Promise<void> {
  const res = await handle(await fetch(path, { headers: headers(), signal }));
  const reader = res.body?.getReader();
  if (!reader) return;
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    let i: number;
    while ((i = buf.indexOf("\n")) >= 0) {
      onLine(buf.slice(0, i));
      buf = buf.slice(i + 1);
    }
  }
  if (buf) onLine(buf);
}

/** Base path of a project: the server derives the namespace from the slug. */
const project = (slug: string) => `/api/projects/${encodeURIComponent(slug)}`;

export const api = {
  config: () => request<PublicConfig>("/api/config"),
  me: () => request<Identity>("/api/me"),
  logout: () =>
    request<{ redirect: string }>("/api/auth/logout", { method: "POST" }),
  /** Email/password sign-in on our own page (RFC-0012). Errors keep the
   * server's message: a wrong password is a 401 like any other, but here
   * it must not be mistaken for an expired session. */
  passwordLogin: async (body: {
    email: string;
    password: string;
    next?: string;
  }): Promise<{ next: string }> => {
    const res = await fetch("/api/auth/password", json("POST", body));
    if (!res.ok) {
      let msg = `${res.status} ${res.statusText}`;
      try {
        const b = await res.json();
        if (b?.error) msg = b.error;
      } catch {
        // not json
      }
      throw new ApiError(res.status, msg);
    }
    return (await res.json()) as { next: string };
  },
  tokenLogin: async (body: {
    token: string;
    next?: string;
  }): Promise<{ next: string }> => {
    const res = await fetch("/api/auth/token", json("POST", body));
    if (!res.ok) {
      let msg = `${res.status} ${res.statusText}`;
      try {
        const b = await res.json();
        if (b?.error) msg = b.error;
      } catch {
        // not json
      }
      throw new ApiError(res.status, msg);
    }
    return (await res.json()) as { next: string };
  },
  loginUrl: (provider: string, next: string) =>
    `/api/auth/login?provider=${encodeURIComponent(provider)}&next=${encodeURIComponent(next)}`,
  workspace: () => request<WorkspaceInfo>("/api/workspace"),
  updateWorkspace: (body: { name?: string; joinPolicy?: string }) =>
    request<WorkspaceInfo>("/api/workspace", json("PATCH", body)),
  domainClaims: () => request<DomainClaim[]>("/api/workspace/domain-claims"),
  claimDomain: (domain: string, connector: string) =>
    request<DomainClaim>(
      "/api/workspace/domain-claims",
      json("POST", { domain, connector }),
    ),
  verifyDomain: (domain: string) =>
    request<DomainClaim>(
      `/api/workspace/domain-claims/${encodeURIComponent(domain)}/verify`,
      { method: "POST" },
    ),
  unclaimDomain: (domain: string) =>
    request<void>(
      `/api/workspace/domain-claims/${encodeURIComponent(domain)}`,
      { method: "DELETE" },
    ),
  loginMethods: () => request<LoginMethods>("/api/auth/connectors"),
  addConnector: (body: {
    type: string;
    id?: string;
    name?: string;
    clientId: string;
    clientSecret: string;
    org?: string;
    hostedDomain?: string;
    tenant?: string;
    issuer?: string;
  }) => request<{ id: string }>("/api/auth/connectors", json("POST", body)),
  removeConnector: (id: string) =>
    request<void>(`/api/auth/connectors/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
  people: () => request<Person[]>("/api/workspace/people"),
  setPersonStatus: (email: string, status: "active" | "suspended") =>
    request<Person>(
      `/api/workspace/people/${encodeURIComponent(email)}`,
      json("PATCH", { status }),
    ),
  forgetPerson: (email: string) =>
    request<void>(`/api/workspace/people/${encodeURIComponent(email)}`, {
      method: "DELETE",
    }),
  teams: () => request<Team[]>("/api/teams"),
  putTeam: (body: Team) => request<Team>("/api/teams", json("POST", body)),
  deleteTeam: (name: string) =>
    request<void>(`/api/teams/${encodeURIComponent(name)}`, {
      method: "DELETE",
    }),
  members: (slug: string) => request<Member[]>(`${project(slug)}/members`),
  addMember: (
    slug: string,
    body: { role: string; user?: string; team?: string },
  ) => request<Member>(`${project(slug)}/members`, json("POST", body)),
  removeMember: (slug: string, name: string) =>
    request<void>(`${project(slug)}/members/${encodeURIComponent(name)}`, {
      method: "DELETE",
    }),
  audit: (slug: string, limit = 50) =>
    request<AuditEntry[]>(`${project(slug)}/audit?limit=${limit}`),
  users: () => request<LocalUser[]>("/api/users"),
  createUser: (body: { email: string; name?: string; password: string }) =>
    request<LocalUser>("/api/users", json("POST", body)),
  setUserPassword: (email: string, password: string) =>
    request<void>(
      `/api/users/${encodeURIComponent(email)}/password`,
      json("PUT", { password }),
    ),
  deleteUser: (email: string) =>
    request<void>(`/api/users/${encodeURIComponent(email)}`, {
      method: "DELETE",
    }),
  apps: () => request<AppSummary[]>("/api/projects"),
  createApp: (body: {
    /** Display name, any text; the slug is derived unless given. */
    name: string;
    slug?: string;
    domains?: string[];
    processes?: Record<string, ProcessSpec>;
    git?: { url: string; revision?: string };
    subPath?: string;
  }) => request<AppSummary>("/api/projects", json("POST", body)),
  app: (slug: string) => request<AppDetail>(project(slug)),
  renameApp: (slug: string, name: string) =>
    request<AppDetail>(project(slug), json("PATCH", { name })),
  deleteApp: (slug: string) =>
    request<{ status: string }>(project(slug), { method: "DELETE" }),
  deploy: (
    slug: string,
    body: {
      git?: { url: string; revision?: string };
      subPath?: string;
      image?: string;
      strategy?: "buildpacks" | "dockerfile";
      dockerfile?: string;
    },
  ) => request<AppSummary>(`${project(slug)}/deploy`, json("POST", body)),
  configVars: (slug: string) =>
    request<{ vars: ConfigVar[]; bound?: BoundVar[]; global?: ConfigVar[] }>(
      `${project(slug)}/secrets`,
    ),
  /** Log drains (RFC-0023): project scope and cluster scope. */
  drains: (slug: string) => request<Drain[]>(`${project(slug)}/drains`),
  createDrain: (slug: string, body: CreateDrain) =>
    request<Drain>(`${project(slug)}/drains`, json("POST", body)),
  deleteDrain: (slug: string, name: string) =>
    request<void>(`${project(slug)}/drains/${encodeURIComponent(name)}`, {
      method: "DELETE",
    }),
  clusterDrains: () => request<Drain[]>("/api/drains"),
  createClusterDrain: (body: CreateDrain) =>
    request<Drain>("/api/drains", json("POST", body)),
  deleteClusterDrain: (name: string) =>
    request<void>(`/api/drains/${encodeURIComponent(name)}`, {
      method: "DELETE",
    }),
  /** Global config vars (RFC-0016): names only, and how many projects get them. */
  globals: () => request<GlobalsResponse>("/api/globals"),
  updateGlobals: (body: {
    set?: Record<string, string>;
    unset?: string[];
    dotenv?: string;
  }) => request<GlobalsResponse>("/api/globals", json("PUT", body)),
  updateConfigVars: (
    slug: string,
    body: { set?: Record<string, string>; unset?: string[]; dotenv?: string },
  ) =>
    request<{ vars: ConfigVar[] }>(
      `${project(slug)}/secrets`,
      json("PUT", body),
    ),
  metrics: (slug: string, range: string) =>
    request<MetricsResponse>(`${project(slug)}/metrics?range=${range}`),
  builds: (slug: string) => request<BuildInfo[]>(`${project(slug)}/builds`),
  buildLogsPath: (slug: string, build: string, follow: boolean) =>
    `${project(slug)}/builds/${encodeURIComponent(build)}/logs?follow=${follow}`,
  logsPath: (
    slug: string,
    q: { process?: string; tail?: number; follow?: boolean },
  ) => {
    const p = new URLSearchParams({
      format: "json",
      tail: String(q.tail ?? 200),
      follow: q.follow ? "true" : "false",
    });
    if (q.process) p.set("process", q.process);
    return `${project(slug)}/logs?${p}`;
  },
  scale: (slug: string, process: string, replicas: number) =>
    request<AppSummary>(
      `${project(slug)}/scale`,
      json("POST", { process, replicas }),
    ),
  resize: (slug: string, process: string, size: string) =>
    request<AppSummary>(
      `${project(slug)}/resize`,
      json("POST", { process, size }),
    ),
  applyProcesses: (
    slug: string,
    processes: Record<string, { size?: string; replicas?: number }>,
  ) =>
    request<AppSummary>(
      `${project(slug)}/processes`,
      json("POST", { processes }),
    ),
  sizes: () => request<SizeCatalog>("/api/sizes"),
  saveSizes: (catalog: SizeCatalog) =>
    request<SizeCatalog>("/api/sizes", json("PUT", catalog)),
  setExposure: (slug: string, exposure: "external" | "internal") =>
    request<AppSummary>(`${project(slug)}/exposure`, json("PUT", { exposure })),
  redeploy: (slug: string, action?: "restart" | "rebuild") =>
    request<{ action: string; message: string }>(
      `${project(slug)}/redeploy`,
      json("POST", action ? { action } : {}),
    ),
  rollback: (slug: string, release: number) =>
    request<AppSummary>(`${project(slug)}/rollback`, json("POST", { release })),
  resources: (slug: string) =>
    request<ResourceInfo[]>(`${project(slug)}/resources`),
  createResource: (
    slug: string,
    body: { kind: string; name: string; spec: Record<string, unknown> },
  ) => request<ResourceInfo>(`${project(slug)}/resources`, json("POST", body)),
  deleteResource: (slug: string, kind: string, name: string, force = false) =>
    request<void>(
      `${project(slug)}/resources/${kind}/${encodeURIComponent(name)}${force ? "?force=true" : ""}`,
      { method: "DELETE" },
    ),
  attach: (
    slug: string,
    body: { kind: string; name: string; prefix?: string },
  ) => request<AppSummary>(`${project(slug)}/bindings`, json("POST", body)),
  detach: (slug: string, kind: string, rname: string) =>
    request<AppSummary>(
      `${project(slug)}/bindings/${kind}/${encodeURIComponent(rname)}`,
      { method: "DELETE" },
    ),
  domains: (slug: string) => request<DomainsResult>(`${project(slug)}/domains`),
  addDomain: (slug: string, host: string) =>
    request<DomainsResult>(`${project(slug)}/domains`, json("POST", { host })),
  removeDomain: (slug: string, host: string) =>
    request<DomainsResult>(
      `${project(slug)}/domains/${encodeURIComponent(host)}`,
      {
        method: "DELETE",
      },
    ),
  volumes: (slug: string) => request<VolumeInfo[]>(`${project(slug)}/volumes`),
  createVolume: (
    slug: string,
    body: {
      name: string;
      size: string;
      storageClass?: string;
      shared?: boolean;
    },
  ) => request<VolumeInfo>(`${project(slug)}/volumes`, json("POST", body)),
  resizeVolume: (slug: string, name: string, size: string) =>
    request<VolumeInfo>(
      `${project(slug)}/volumes/${encodeURIComponent(name)}`,
      json("PUT", { size }),
    ),
  deleteVolume: (slug: string, name: string, force = false) =>
    request<void>(
      `${project(slug)}/volumes/${encodeURIComponent(name)}${force ? "?force=true" : ""}`,
      { method: "DELETE" },
    ),
  snapshots: (slug: string, volume: string) =>
    request<SnapshotInfo[]>(
      `${project(slug)}/volumes/${encodeURIComponent(volume)}/snapshots`,
    ),
  createSnapshot: (slug: string, volume: string, name?: string) =>
    request<SnapshotInfo>(
      `${project(slug)}/volumes/${encodeURIComponent(volume)}/snapshots`,
      json("POST", name ? { name } : {}),
    ),
  deleteSnapshot: (slug: string, volume: string, snapshot: string) =>
    request<void>(
      `${project(slug)}/volumes/${encodeURIComponent(volume)}/snapshots/${encodeURIComponent(snapshot)}`,
      { method: "DELETE" },
    ),
  restoreVolume: (
    slug: string,
    volume: string,
    body: { snapshot: string; to?: string },
  ) =>
    request<RestoreVolumeResult>(
      `${project(slug)}/volumes/${encodeURIComponent(volume)}/restore`,
      json("POST", body),
    ),
  cluster: () => request<ClusterSummary>("/api/cluster"),
  clusterMetrics: (range: string) =>
    request<ClusterMetrics>(`/api/cluster/metrics?range=${range}`),
  registry: () => request<RegistryInfo>("/api/cluster/registry"),
  objectStorage: () =>
    request<ObjectStorageSummary>("/api/cluster/object-storage"),
  allow: (slug: string) => request<AllowEntry[]>(`${project(slug)}/allow`),
  setAllow: (slug: string, entries: AllowEntry[]) =>
    request<AllowEntry[]>(`${project(slug)}/allow`, json("PUT", entries)),
  setAccess: (slug: string, access: string) =>
    request<AppSummary>(`${project(slug)}/access`, json("PUT", { access })),
  preview: (slug: string, body: { teams: string[]; anonymous?: boolean }) =>
    request<{ url: string }>(`${project(slug)}/preview`, json("POST", body)),
  launcher: () => request<LauncherApp[]>("/api/launcher"),
  tokens: () => request<APIToken[]>("/api/tokens"),
  createToken: (body: {
    name: string;
    platformRole?: string;
    projectRoles?: Record<string, string>;
    expiresIn?: string;
  }) =>
    request<APIToken & { token: string }>("/api/tokens", json("POST", body)),
  revokeToken: (id: string) =>
    request<void>(`/api/tokens/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
  backups: () => request<BackupInfo>("/api/cluster/backups"),
  runBackup: () =>
    request<{ job: string; status: string }>("/api/cluster/backups", {
      method: "POST",
    }),
  registryGC: () =>
    request<{ status: string }>("/api/cluster/registry/gc", { method: "POST" }),
  helmReleases: () => request<HelmRelease[]>("/api/helm/releases"),
};
