# RFC-0064 Linux developer loop: privileged ports, image loading, browser trust

**Status:** implemented

**Owner:** Marcelo Paez Sequeira (shpyrd-io/shpyrd rfc-0021-structured-logs)

**Depends on:** RFC-0001, RFC-0057

**Creation date:** 2026-09-26

**Last update:** 2026-09-26

## Summary

Three defects that made the documented first run fail on Linux and nowhere else:
`cluster create` reported a port conflict that did not exist, `make dev-load` needed a
`kind` binary matching the library in `go.mod`, and `trust-ca` left the browser untrusting
the platform CA. Each is fixed where it belongs, and `cluster untrust-ca` undoes the last
one.

## Motivation

The README promises `cluster create` → `trust-ca` → `cluster dashboard` on "macOS or Linux".
On Linux all three steps misbehaved, and CI could not see any of it: the end-to-end job
passes explicit high ports, installs the latest `kind`, and has no browser.

### Goals

- The quick start works on a stock Linux machine, as a normal user, with nothing but Docker.
- A contributor's dev loop does not depend on a tool version nothing declares.
- Trusting the CA is undoable.

### Non-Goals

- Firefox's per-profile certificate store; `security.enterprise_roots.enabled` makes it read
  the system store, and writing into profiles a browser may not have created is worse than
  saying so.
- Windows.

## Proposal

**Privileged ports are not busy ports.** `PortFree` dialled the port, then tried to bind it
and read every error as "in use". Below `net.ipv4.ip_unprivileged_port_start` (1024, so the
default 80 and 443) a normal user cannot bind at all: the error is `EACCES` and says nothing
about kind, which maps host ports through the Docker daemon as root. A port counts as busy
only when something answers the dial or the bind fails with `EADDRINUSE`. `cluster.go` had a
second, subtly different check with its own message; it delegates to `localnet.BusyPorts`, so
there is one definition of a free port.

**The dev loop stops needing the kind CLI.** Clusters are created by the kind library in
`go.mod`, while `make dev-load` shelled out to whatever `kind` was on `PATH`. A binary older
than the library cannot read the node it just created:

```
ERROR: unknown containerd config version: 4 (supported versions: 2 and 3)
```

`dev-load` now does what `kind load docker-image` does -- find the cluster's nodes by the
label kind puts on them, save the image once, import it into containerd's `k8s.io` namespace
on each node with the snapshotter that node's config names -- and the end-to-end job calls
the same target, so nothing installs `kind` any more.

**`trust-ca` reaches the browser.** Chromium, Chrome, Brave, Vivaldi and Edge share an NSS
database at `~/.pki/nssdb` and do not read the operating system trust store, so `curl` and
the Go tools trusted the CA while the dashboard showed a warning. `trust-ca` also imports it
there when `certutil` is installed and the database exists, under the CA's common name. A
database the browser has not created is left alone and the command to run later is printed:
creating one writes a browser profile nobody asked for. `cluster untrust-ca` removes the CA
from every system store it finds and from the NSS database, naming each; the certificate
stays in `~/.shpyrd/ca`, so trusting it again does not invalidate certificates already
issued from it.

### Alternatives

- **Check the kind CLI's version in the Makefile** instead of dropping it. It keeps a
  dependency whose only job is one `ctr import`, and turns a silent failure into a louder
  one rather than removing it.
- **Ask for `sudo` and bind the privileged port to test it.** The daemon binds it, not
  shpyrd; testing the wrong thing with more privilege is not better.
- **Write into Firefox profiles.** Several may exist, none may exist yet, and the browser
  already has a setting that reads the store shpyrd updates.

## Design Details

- Colour, wrapping and the shape of `shpyrd logs` output are RFC-0021's, not this one's.
- `untrust-ca` needs the same administrator rights as `trust-ca`; nothing else changes, and
  neither command runs as part of `cluster create`. Silently editing a browser's trust store
  is not something creating a cluster should do.
- An operator can see the state directly: `trust list | grep shpyrd` for the system store,
  `certutil -d sql:$HOME/.pki/nssdb -L` for the browser's.
- Drawback: the CA now lives in two stores on Linux, so a machine cleaned by hand needs both.
  `untrust-ca` exists so that is not the normal path.

## Implementation History

- 2026-09-26: written after all three landed, and implemented.
  - `PortFree` distinguishes `EACCES` from `EADDRINUSE`; verified on Linux as uid 1000 that
    80 and 443 pass while 30050, held by a running cluster's registry, is still refused.
  - `make dev-load` imports the image itself; verified by the server running from it, its
    version matching `git describe`.
  - `trust-ca` imports into `~/.pki/nssdb`; `untrust-ca` added. Tested: the argv handed to
    `certutil`, that a database the browser has not made is not created, and an
    import/delete round trip in a throwaway database (skipped where `certutil` is missing,
    as on the CI runners). The half of `untrust-ca` behind `sudo` was exercised as far as
    the password prompt -- the right CA, the installed anchor found -- and no further.
  - The browser side of `trust-ca` is also recorded in RFC-0057, which owns local names and
    the front door.
