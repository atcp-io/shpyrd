# blog

A Node.js blog (built with buildpacks, no dependencies) whose visit counter lives on a
**persistent volume** mounted at `/data`, so it survives deploys and restarts.

```sh
shpyrd projects create blog
shpyrd volumes create data --size 1Gi --project blog
cd examples/blog && shpyrd deploy
shpyrd open                                    # "visit #N (counter on the volume)"
shpyrd scale web=2                             # refused: single-instance volume
```

The volume is single-instance (ReadWriteOnce), so `web` runs one instance with Recreate
rollouts; the dashboard pins the instance count.
