# shop

A small storefront (Go, built with buildpacks) with two process types and two attached
resources. Products come from an attached PostgreSQL database, orders go through an
attached Redis (Valkey) queue that the `worker` process drains. Without the attachments
the page still renders and says what to run.

```sh
shpyrd projects create shop
cd examples/shop && shpyrd deploy
shpyrd pg create db --project shop --size shared-m --storage 2Gi
shpyrd redis create cache --project shop
shpyrd attach db && shpyrd attach cache        # DATABASE_URL, REDIS_URL... appear in the app
shpyrd open
shpyrd logs -f                                 # web.N request lines (JSON) and worker.1 fulfilments
shpyrd scale web=2
```

- `cmd/web`: HTML storefront, `/api/products`, `POST /order` (pushes to Redis), `/healthz`.
- `cmd/worker`: `BLPOP` on the `orders` list, logs structured lines.
- `shpyrd.yaml`: sizes per process and `BP_GO_TARGETS` so both commands are built.
