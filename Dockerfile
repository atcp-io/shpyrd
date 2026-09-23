# shpyrd-server: API + App controller + embedded dashboard.
#
#   docker build -t shpyrd-server:dev .
#   docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/shpyrd-io/shpyrd-server:vX.Y.Z .
#
# Stage 1 builds the UI, stage 2 embeds it into the Go binary, stage 3 is a
# distroless runtime image. Stages 1 and 2 run on the build platform and
# cross-compile for the target, so a multi-arch build needs no emulation.

FROM --platform=$BUILDPLATFORM node:24-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY ui/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist ./ui/dist
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X shpyrd/pkg/version.Version=${VERSION}" \
      -o /out/shpyrd-server ./cmd/shpyrd-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/shpyrd-server /shpyrd-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/shpyrd-server"]
