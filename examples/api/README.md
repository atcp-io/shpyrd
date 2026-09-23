# api

A JSON API in Python (standard library only) built from a **Dockerfile** with BuildKit.
The Dockerfile ends with a numeric `USER 1000`: shpyrd runs processes as non-root and
Kubernetes verifies by uid.

```sh
shpyrd projects create api
cd examples/api && shpyrd deploy               # ==> Building with Dockerfile (Dockerfile)
curl https://api.<domain>/                     # {"service":"api","version":"1",...}
```

Change `API_VERSION` in the Dockerfile and deploy again to watch a build in the Activity
panel and roll back afterwards with `shpyrd rollback`.
