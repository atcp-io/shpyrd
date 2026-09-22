# hello-world example

The smallest useful shpyrd app: a Go module with two process types and no
Dockerfile. The Paketo Go buildpack builds both commands in the cluster.

- `cmd/web` renders an HTML "Hello world" page showing the instance that
  served the request and the `GREETING` config var.
- `cmd/worker` is a background job that logs a line every 10 seconds.

`shpyrd.yaml` declares the app name, the two process types and the
`BP_GO_TARGETS` build setting that tells the buildpack to build both.

```sh
shpyrd apps create hello-world
cd examples/hello
shpyrd deploy                        # builds, then runs web (1) and worker (1)
shpyrd open                          # https://hello-world.<domain>

shpyrd secrets set GREETING="Olá mundo"   # both processes restart with it
shpyrd scale web=3 worker=2
shpyrd logs -f                            # web.1, web.2, worker.1 ... lines
shpyrd logs -f --process worker
```
