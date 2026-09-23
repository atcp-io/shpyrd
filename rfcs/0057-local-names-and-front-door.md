# RFC-0057 Local names and front door

**Status:** implemented

**Owner:** Patrick Negri (shpyrd-io/shpyrd main)

**Depends on:** RFC-0001 (implemented)

**Creation date:** 2026-09-22

**Last update:** 2026-09-23

## Summary

Two explicit, detectable choices for the local profile: how `*.<domain>` resolves to the
machine (public `nip.io`, the default, or a local wildcard through dnsmasq and a macOS
resolver file), and who owns ports 80/443 (kind directly, high ports, or an existing
Caddy acting as the front door). `shpyrd cluster create` detects the situation, proposes
the fitting mode, records it, and the platform's URLs come out clean:
`https://shpyrd.shpyrd.test` instead of `https://shpyrd.127.0.0.1.nip.io:8443`.

## Motivation

Today the only knob is `--domain` (default `127.0.0.1.nip.io`) and kind takes 80/443 or
whatever ports are passed. Developers who already run a local reverse proxy on 443 (Caddy
is common: it also gives them a trusted local CA) end up with URLs carrying `:8443` and a
second CA to trust; developers offline or on locked-down DNS cannot resolve `nip.io`.

### Goals

- URLs without ports and without an IP address in them, on a laptop, offline.
- Coexist with a Caddy that already serves 80/443: shpyrd becomes one more site behind
  it, certificates come from the CA the user already trusts.
- One `sudo` prompt at most (like `shpyrd cluster trust-ca`); everything recorded and
  reversible.

### Non-Goals

- Real domains, Let's Encrypt, DNS providers (RFC-0034, RFC-0036).
- Linux and Windows support for the resolver step in the first version (documented
  manual steps; the Caddy front door works anywhere Caddy runs).

## Proposal

### Names

| Mode | Flag | Effect |
| --- | --- | --- |
| `nip.io` (default) | `--domain 127.0.0.1.nip.io` | nothing to install; public DNS answers 127.0.0.1 |
| local wildcard | `--domain shpyrd.test --local-dns` | `/etc/resolver/test` (`nameserver 127.0.0.1`) and a dnsmasq rule `address=/.test/127.0.0.1`; dnsmasq installed and started through Homebrew when missing; any `*.shpyrd.test` (and any other `.test` name) resolves locally |

`.test` is reserved (RFC 2606) and never leaks to public DNS. A domain under another TLD
with `--local-dns` gets a resolver file for that exact domain instead of the whole TLD.

### Front door

| Mode | Flag | Effect | Certificates |
| --- | --- | --- | --- |
| kind directly | default when 80/443 are free | kind maps 80/443 to ingress-nginx | shpyrd CA (`cluster trust-ca`) |
| high ports | `--http-port 8080 --https-port 8443` | today's fallback; URLs carry the port | shpyrd CA |
| Caddy | `--front-door caddy` | kind on high ports; shpyrd writes `~/.shpyrd/caddy/shpyrd.caddy` and reloads Caddy through its admin API | Caddy's internal CA, already trusted; no `trust-ca`, no port in URLs |

The Caddy site file:

```
# managed by shpyrd
*.shpyrd.test, shpyrd.test {
	tls internal
	reverse_proxy 127.0.0.1:8080 {
		header_up X-Forwarded-Proto https
	}
}
```

Caddy terminates TLS and speaks plain HTTP to kind's HTTP port; ingress-nginx is
configured with `use-forwarded-headers: true` so it sees `https` and does not redirect
(no loop) and apps receive the right scheme. The user adds `import ~/.shpyrd/caddy/*.caddy`
to their Caddyfile once (the CLI prints the exact line and the path of the Caddyfile it
found: Homebrew's `/opt/homebrew/etc/Caddyfile` or the one from the running process's
command line); after that shpyrd owns only its file and reloads via `POST
localhost:2019/load` or `caddy reload`.

### Detection and recording

`shpyrd cluster create` (and `cluster init` on kind) checks: are 80 and 443 free; does
`localhost:2019` answer Caddy's admin API; is dnsmasq present; does the domain end in a
reserved TLD. It proposes the fitting combination ("Caddy is serving 443; use it as the
front door for *.shpyrd.test? [Y/n]"; `--yes` accepts), and records `SHPYRD_DOMAIN`,
`SHPYRD_LOCAL_DNS`, `SHPYRD_FRONT_DOOR` and the ports in the install record so `cluster
init`, `cluster status` ("Names: dnsmasq (*.shpyrd.test) · Front door: Caddy on 443 → kind
8080") and `cluster dashboard` keep working. `shpyrd cluster destroy` removes the Caddy
site file and, with `--local-dns`, offers to remove the resolver entries.

## Design Details

- `SHPYRD_DASHBOARD_URL`/`SHPYRD_AUTH_URL` (derived vars) omit the port when the front door
  is Caddy; the Dex issuer follows, so sign-in works without `:8443`.
- kind's ingress-nginx values gain `use-forwarded-headers: true` only in front-door mode
  (behind Caddy the proxy is trusted; on 80/443 directly the header stays untrusted).
- New `pkg/localnet`: port probing, Caddy admin detection, resolver/dnsmasq file writers
  (sudo through the same helper as `trust-ca`), Caddy site rendering and reload.
- Docs: Installation gets a "Local names and ports" section with the three setups and the
  Caddyfile import line; the FAQ entry about `:8443` disappears.

## Implementation History

- 2026-09-22: RFC written after a look at a platform2 laptop setup (Homebrew Caddy on 443
  with `tls internal`, dnsmasq `address=/.test/127.0.0.1`, `/etc/resolver/test`).
- 2026-09-23: implemented and verified on that laptop: the running cluster moved from
  `https://shpyrd.127.0.0.1.nip.io:8443` to `https://shpyrd.shpyrd.test` behind the existing
  Caddy with one `cluster init --domain shpyrd.test --front-door caddy`. Notes:
  - `pkg/localnet`: ports (dial + IPv4 bind probe; a dual-stack wildcard listen does not
    conflict with a loopback socket on macOS), Caddy detection (admin API, Caddyfile from
    the process command line or Homebrew's path), site file, import line, reload
    (`caddy reload` or `POST /load`), local DNS status/setup/removal.
  - The site file adds `flush_interval -1` so log streams are not buffered by Caddy.
  - `use-forwarded-headers` was already on unconditionally; it is now derived
    (`SHPYRD_FORWARDED_HEADERS`) and true only behind the front door.
  - Public URLs: derived `SHPYRD_URL_PORT` ("443" behind Caddy, else kind's https port) feeds
    the server's `SHPYRD_HTTPS_PORT`, so the CLI, the API and the App controller agree
    without changing the server. `SHPYRD_HTTP_PORT`/`SHPYRD_HTTPS_PORT` keep meaning the host
    ports kind maps.
  - `cluster init` now seeds domain, ports, front door and local DNS from the install
    record when the flags are not given; before, a bare re-run silently reset them.
  - Dex reads its config at start only: its pod template now carries the URLs as an
    annotation so a domain or front-door change rolls it (otherwise the issuer stays stale
    and the local provider cannot register).
  - `cluster destroy` reads the record before deleting the cluster, removes the Caddy site
    (and reloads), and offers to remove the dnsmasq rule only when shpyrd wrote it.
  - Recorded: `SHPYRD_FRONT_DOOR`, `SHPYRD_LOCAL_DNS`; `cluster status` prints
    "Names: dnsmasq (*.shpyrd.test) · Front door: Caddy on 443 -> kind :8080".
  - Prompts default to Yes and are answered by `--yes` or a non-terminal stdin, so CI keeps
    working non-interactively.
