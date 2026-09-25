# RFC-0062 kubectl through the platform's sign-in (`shpyrd auth kubectl`)

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0007 (authentication), RFC-0008 (teams, roles; the RBAC mirror),
RFC-0058 (external identity providers), RFC-0035 (cloud profiles)

**Creation date:** 2026-09-25

**Last update:** 2026-09-25

## Summary

Developers sign in to the dashboard with the company identity provider and get exactly
their project rights. `kubectl` against the same cluster is a different world: it
authenticates with the cloud's IAM (EKS access entries, OKE IAM policies) or with kind's
client certificates, so only operators have it. Shpyrd already keeps Kubernetes
RoleBindings per project for its members, by email and group (RFC-0008); this RFC makes
the API server recognise those identities, so `kubectl get pods -n app-shop` works for a
developer with the permissions the dashboard gives them, and adds `shpyrd auth kubectl` to
set it up on a developer's machine.

## Motivation

Debugging sometimes needs `kubectl`: `port-forward` to a database, `exec` into an
instance, `describe` of a stuck pod, `logs --previous`. Handing developers IAM users for
that duplicates identities and permissions; handing them the operator kubeconfig gives
them everything. The RBAC mirror was built for this and is inert until the API server
authenticates people by email.

### Goals

- The cluster's API server accepts tokens from the platform's identity provider, mapping
  `email` to the user and `groups` to groups, so the RFC-0008 bindings apply.
- `shpyrd auth kubectl [--context <ctx>]` writes a kubeconfig context that obtains such a
  token through the browser (the standard OIDC exec flow) and points at the cluster.
- Works on EKS (identity provider configuration), OKE Enhanced (OIDC authentication) and
  kind (API server flags); says clearly where it cannot.

### Non-Goals

- Replacing the operator's cloud credentials; operators keep IAM.
- A developer login for the shpyrd CLI itself (RFC-0052): the CLI's server-proxied
  commands still need an operator kubeconfig today; that RFC gives the CLI its own login.

## Proposal

- **Issuer.** The provider is the one configured for sign-in: an external OIDC provider
  (RFC-0058: Okta, any issuer) or the bundled Dex (`auth-local`). The API server fetches
  the issuer's discovery document from the cloud control plane, which needs the issuer to
  be reachable from the internet: Okta always is; Dex is when the platform is external
  (RFC-0036), not when the dashboard sits behind the internal front door. `cluster init`
  states which case applies.
- **Cluster side.**
  - EKS: `aws_eks_identity_provider_config` in `contrib/aws/terraform` (`issuer_url`,
    `client_id`, `username_claim: email`, `groups_claim: groups`, `groups_prefix: oidc:`),
    with the values Terraform gets from variables `kubectl_oidc_issuer`/`_client_id`; one
    provider per cluster is EKS's limit.
  - OKE Enhanced: the cluster's OpenID Connect authentication settings, same claims, in
    `contrib/oci/terraform`.
  - kind: `--oidc-*` API server flags in the cluster config written by `cluster create`.
- **RBAC mirror.** Group subjects gain the `oidc:` prefix where the cluster uses one
  (RFC-0008's membership controller reads the prefix from a setting); user subjects stay
  the email.
- **`shpyrd auth kubectl`.** Adds a kubeconfig context `<cluster>-<email>` with an `exec`
  credential plugin performing the authorization-code flow (`kubelogin`-compatible; the
  CLI can embed the flow so nothing else is installed), the cluster's server URL and CA
  from the platform (`/api/cluster/kubeconfig`, platform-admin or any member), and prints
  what the developer can do (`kubectl auth can-i --list -n app-<slug>`).
- **Dashboard.** The project members card shows "kubectl: enabled" with the command, or
  the reason it is off (issuer not reachable, cluster type).

### Alternatives

- **Cloud IAM per developer** (IAM users, access entries). Duplicates identities;
  permissions drift from the platform's roles.
- **A proxy in front of the API server** (kube-oidc-proxy, Teleport). Works everywhere,
  including behind the internal front door, at the price of another component in the
  request path. Kept as the fallback for clusters whose API server cannot be configured.

## Design Details

- Token claims: `email` (verified), `groups`; the API server maps them to `User` and
  `Group` subjects; RoleBindings already use `User: <email>` and `Group: <group>`.
- The exec plugin caches tokens under `~/.shpyrd/tokens/<issuer>` with refresh.
- `cluster init` records `SHPYRD_KUBECTL_OIDC=<issuer>` so the dashboard can show the
  state; the cluster page lists it.

## Open questions

1. Embed the OIDC exec flow in the shpyrd CLI or require `kubelogin`? Default: embed.
2. Groups prefix (`oidc:`) on by default? Default: yes, to keep platform groups apart from
   cloud IAM groups.

## Implementation History

- 2026-09-25: RFC written after the AWS profile (EKS supports the identity provider
  configuration natively).
