# Multi-stage build: compile with the Go toolchain, run on a small Alpine
# image (Alpine keeps a shell so `shpyrd shell` works; distroless would not).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -o /out/hello .

FROM alpine:3.21 AS runtime
ARG BUILT_WITH=buildkit
ENV BUILT_WITH=$BUILT_WITH
RUN adduser -D -u 1000 app
USER app
COPY --from=build /out/hello /usr/local/bin/hello
EXPOSE 8080
CMD ["hello", "web"]
