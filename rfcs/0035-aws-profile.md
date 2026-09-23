# RFC-0035 AWS profile

**Status:** provisional

**Owner:** unassigned

**Depends on:** RFC-0034, RFC-0045

**Creation date:** 2026-09-22

**Last update:** 2026-09-22

## Summary

`shpyrd cluster init --profile aws --domain apps.example.com` installs the platform on an
existing EKS cluster: load balancing, storage classes, ECR as the registry with IRSA
credentials for builds, Let's Encrypt certificates, and the EKS OIDC link so the RBAC
mirror applies to `kubectl` users.

## Motivation

The local profile proves the product; the first production target is AWS.

### Goals

- A working platform on EKS in one command given IAM prerequisites.
- Builds push to ECR without static keys.

### Non-Goals

- Provisioning EKS itself (eksctl/Terraform, documented); RDS-backed Postgres.

## Proposal

- Profile `aws`: components ingress-nginx behind an NLB (AWS Load Balancer Controller
  installed by the profile), `gp3` default StorageClass and `efs` (RWX, RFC-0041),
  cert-manager with a Let's Encrypt ClusterIssuer, ECR registry: `SHPYRD_REGISTRY_HOST`
  is the account's ECR endpoint; kpack and BuildKit authenticate through an IRSA role
  (`shpyrd-build`) and the ECR credential helper; monitoring as today.
- Prerequisites checked by `cluster init`: IAM roles for the LB controller and builds, the
  cluster's OIDC provider, a Route53 zone or an external DNS pointing at the NLB (RFC-0036
  automates it).
- Documentation: an eksctl example, IAM policies, costs.

## Design Details

- `deploy/profiles/aws/profile.yaml` with overlays per component; `Substitute` vars for
  account, region, role ARNs.
- Tests: an e2e job (opt-in, needs credentials) exercising deploy, Dockerfile build, volume,
  Postgres.

## Open questions

1. Existing EKS cluster only (default: yes); provisioning as a later RFC.
2. Test account/region available? Needed before implementation starts.

## Implementation History

- 2026-09-22: RFC written.
