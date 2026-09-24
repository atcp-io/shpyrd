# RFCs

Design changes to shpyrd are proposed as short RFCs before they are built. Every RFC is
sized to be implemented completely; when a feature is too large, it is split and the parts
declare what they depend on.

## Process

1. Discuss the idea in a GitHub issue or on Discord.
2. Copy `0000-template.md` to `NNNN-title.md`, fill it in and open a pull request.
3. Address feedback with additive commits; the status moves from `provisional` to
   `implementable` when the open questions are settled.
4. To implement one: set **Owner** and the status to `in progress` in a first commit, keep
   the Implementation History current, and set `implemented` when it lands (partial scope
   spelled out after the word).

## Statuses

| Status | Meaning |
| --- | --- |
| `provisional` | A proposal. The Open questions section lists what must be settled, each with a default that applies when nobody objects. |
| `implementable` | Decided; anyone can pick it up. |
| `in progress` | Being implemented; the Owner field names who and where. |
| `implemented` | Merged. Partial scope, if any, follows the word. |
| `deferred`, `rejected`, `withdrawn`, `replaced` | Not going ahead (as is). |

## Index

| RFC | Title | Status | Depends on |
| --- | --- | --- | --- |
| [0001](0001-mvp-local-platform.md) | MVP: local platform, App CRD and CLI | implemented | |
| [0002](0002-extension-model.md) | Extension model | implemented (framework) | |
| [0003](0003-projects-and-resources.md) | Projects and resources | implemented | |
| [0004](0004-dockerfile-builds.md) | Dockerfile builds (BuildKit) | implemented | |
| [0005](0005-shell-and-one-off-commands.md) | Shell and one-off commands | implemented (CLI); browser terminal → 0026 | |
| [0006](0006-persistent-volumes.md) | Persistent volumes | implemented (RWO); shared → 0041 | |
| [0007](0007-authentication.md) | Authentication (OIDC, Dex) | implemented (3.1 local users); 3.2 → 0014, 3.3 → 0012 + 0058 | |
| [0008](0008-teams-roles-and-security.md) | Teams, roles and security | implemented; quotas → 0042, enforce PSS → 0043, supply chain → 0044, tokens → 0031, durable audit → 0025 | |
| [0009](0009-postgres-resource.md) | Postgres resource (CloudNativePG) | implemented; backups → 0038, pooling/rotation → 0039 | |
| [0010](0010-redis-resource.md) | Redis resource (Valkey) | implemented; HA → 0040 | |
| [0011](0011-project-identity-and-product-language.md) | Project identity and product language | implemented | |
| [0012](0012-sign-in-experience.md) | Sign-in experience: shpyrd's own sign-in page (local sign-in, sign-out) | implemented | 0007 |
| [0013](0013-email-delivery.md) | Email delivery (`mail` extension) | provisional | 0002 |
| [0014](0014-account-lifecycle.md) | Account lifecycle (invites, reset, verification, lockout) | provisional | 0012, 0013 |
| [0015](0015-grafana-sign-in.md) | Grafana sign-in through shpyrd | provisional | 0007 |
| [0016](0016-global-config-vars.md) | Global config vars | implemented | 0003 |
| [0017](0017-git-credentials.md) | Git credentials for private repositories | provisional | 0004 |
| [0018](0018-repository-monitoring.md) | Repository monitoring and auto-deploy | provisional | 0017 |
| [0019](0019-health-checks-and-rollouts.md) | Health checks and zero-downtime rollouts | implemented | 0001 |
| [0020](0020-maintenance-mode.md) | Maintenance mode | provisional | 0001 |
| [0021](0021-structured-logs.md) | Structured logs in the viewer and the CLI | implementable | |
| [0022](0022-log-pipeline.md) | Log pipeline (agent + Loki) — split, see 0022a and 0022b | provisional | 0046 |
| [0022a](0022a-log-agent.md) | Log agent (Vector DaemonSet, container log limits) | implemented | 0001 |
| [0023](0023-log-drains.md) | Log drains (project and cluster scope, syslog, HTTPS) | implemented | 0022a |
| [0024](0024-runs-and-scheduled-tasks.md) | Run history and scheduled tasks | provisional | 0005, 0022 |
| [0025](0025-audit-trail-v2.md) | Audit trail v2 (durable, cluster-wide) | provisional | 0008, 0022 |
| [0026](0026-web-terminal.md) | Web terminal | implementable | 0005, 0008 |
| [0027](0027-application-metrics-v2.md) | Application metrics v2 | implementable | 0011 |
| [0028](0028-resource-pages-and-metrics.md) | Resource detail pages and metrics | provisional | 0006, 0009, 0010 |
| [0029](0029-opentelemetry.md) | OpenTelemetry | provisional | 0016 |
| [0030](0030-notifications.md) | Notifications (webhook, Slack, email) | provisional | 0013 (email) |
| [0031](0031-api-tokens.md) | Per-user API tokens | implementable | 0008 |
| [0032](0032-mcp-connector.md) | MCP connector | provisional | 0031 (remote) |
| [0033](0033-workspaces.md) | Workspaces | provisional (blocked on definition) | 0008, 0016 |
| [0034](0034-domains-and-certificates.md) | Domains and certificates | provisional | 0011 |
| [0035](0035-cloud-profiles.md) | Cloud profiles: Oracle Cloud (OKE) and AWS (EKS) | implemented (`oci`, network policy); AWS provisional | 0045 |
| [0036](0036-load-balancer-exposure.md) | Load balancer exposure: internal and external front doors | provisional | 0035, 0061 |
| [0037](0037-platform-backup-and-restore.md) | Platform backup and restore | provisional | 0046 |
| [0038](0038-postgres-backups-and-pitr.md) | Postgres backups and PITR | implementable | 0009, 0046 |
| [0039](0039-postgres-pooling-rotation-resize.md) | Postgres pooling, rotation and resize | implementable | 0009 |
| [0040](0040-redis-ha-and-exporter.md) | Redis high availability and metrics exporter | provisional | 0010 |
| [0041](0041-shared-volumes.md) | Shared volumes (`storage-rwx`) | implementable | 0006 |
| [0042](0042-project-quotas.md) | Project quotas | implementable | 0008 |
| [0043](0043-builds-namespace-and-pod-security.md) | Builds namespace and enforce-mode Pod Security | implementable | 0004, 0008 |
| [0044](0044-supply-chain.md) | Supply chain and encryption at rest | implementable | 0045 |
| [0045](0045-published-binaries-and-ci.md) | Published binaries, images and CI | implemented (signing/SBOM deferred) | |
| [0046](0046-object-storage.md) | Object storage extension | implementable | 0002 |
| [0047](0047-autoscaling.md) | Autoscaling (min/max mode, HPA, KEDA) | implementable | 0019, 0042 |
| [0048](0048-cost-visibility.md) | Cost visibility | implementable | 0042 |
| [0049](0049-gitops-export.md) | GitOps export | rejected | |
| [0050](0050-git-push-receiver.md) | Git push deploys | rejected | |
| [0051](0051-agents-and-background-processes.md) | Agents as a separate kind | rejected (agents are apps or runs) | |
| [0052](0052-api-first-cli-and-login.md) | API-first CLI and `shpyrd login` | implementable | 0031, 0026 |
| [0053](0053-mfa-and-passkeys.md) | MFA and passkeys | implementable | 0012, 0014 |
| [0054](0054-github-app.md) | GitHub App integration | implementable | 0017, 0018 |
| [0055](0055-environments-and-promotion.md) | Environments and promotion | deferred (Git branches per environment) | |
| [0056](0056-tracing-backend.md) | Tracing backend (Jaeger) | implementable | 0029, 0015 |
| [0057](0057-local-names-and-front-door.md) | Local names and front door (dnsmasq wildcard, Caddy on 443) | implemented | 0001 |
| [0058](0058-external-identity-providers.md) | External identity providers (Okta and OIDC, GitHub, Google) | implemented (GitHub round trip pending an OAuth app) | 0012 |
| [0059](0059-in-cluster-registry-on-cloud.md) | In-cluster registry as the default on every profile | implemented | 0035, 0060 |
| [0060](0060-volumes-on-cloud-profiles.md) | Volumes on cloud profiles: storage classes, provider minimums, snapshots | provisional | 0006, 0035, 0041 |
| [0061](0061-dns-providers.md) | DNS providers: automatic records and wildcard certificates | provisional | 0035, 0036 |

## Phases

| Phase | RFCs | State |
| --- | --- | --- |
| A | 0001 | done |
| B | 0003, 0004, 0005, 0006 | done |
| C | 0002, 0007 | done |
| D | 0008 | done |
| E | 0009, 0010 | done |
| F | 0045, 0057, 0011, 0012, 0058, 0016, 0019, 0046 | done |
| G | 0022a, 0021, 0017, 0018, 0023, 0024, 0025 | delivery and logs: agent, structured viewer, git creds, auto-deploy, drains, run history, durable audit |
| G (old) | 0017, 0018, 0021, 0022, 0024, 0025, 0023 | superseded by new G above |
| H | 0027, 0028, 0015, 0029, 0030 | observability and notifications |
| I | 0031, 0032, 0026, 0020, 0014, 0013 | access, automation, runtime |
| J | 0035 (done), 0059 (done), 0060, 0036, 0061, 0034, 0037 | cloud: OKE profile, in-cluster registry, volumes, front doors, DNS, custom domains, backup |
| K | 0038, 0039, 0040, 0041, 0042, 0043, 0044, 0033 | data stores, security, workspaces |
| L | 0047, 0048, 0052, 0053, 0054, 0056 | autoscaling, cost, API-first CLI, MFA and passkeys, GitHub App, tracing |

Anyone may pick an `implementable` RFC in any phase; the phases only suggest an order that
keeps dependencies satisfied.
