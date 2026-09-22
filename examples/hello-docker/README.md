# hello-docker

The `examples/hello` service built from a Dockerfile (BuildKit) instead of
buildpacks. The presence of `Dockerfile` selects the strategy; `shpyrd.yaml`
pins it and adds a build arg and a multi-stage target.

```sh
shpyrd projects create hello-docker
cd examples/hello-docker
shpyrd deploy                          # ==> Building with Dockerfile (Dockerfile)
shpyrd open
shpyrd logs -f --process worker
shpyrd shell                           # sh in the Alpine image
shpyrd run hello worker                # one-off instance
```

Dockerfile images have a single entrypoint, so every process type other than
`web` declares its `command` in `shpyrd.yaml`.

Processes run as non-root: the Dockerfile ends with a numeric `USER` (`USER
1000`) so Kubernetes can verify it. A named user or no `USER` at all is refused
with an explanation.

The `web` process mounts a persistent volume at `/data`; create it before the
first deploy with `shpyrd volumes create data --size 1Gi`. Because it is a
single-instance volume, `web` runs one instance and rolls out with Recreate.
