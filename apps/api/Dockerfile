# The Web UI (apps/web, ADR 0020): built once on the build platform, since
# its output is the same files for every image platform. --ignore-scripts
# keeps dependencies' install scripts from running; the app's own postinstall
# only fences node_modules from Go, which this stage does not need.
FROM --platform=$BUILDPLATFORM node:26.12.1-alpine AS web

WORKDIR /src/apps/web

COPY apps/web/package.json apps/web/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund

COPY apps/web/ ./
COPY apps/api/openapi.yaml /src/apps/api/openapi.yaml
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

# The build context is the repository root, with this Dockerfile's own ignore
# file (Dockerfile.dockerignore): the root .dockerignore leaves out db/, and
# the API embeds db/migrations.
COPY . .
# The API embeds the UI's build (package heartbeat/apps/web).
COPY --from=web /src/apps/web/dist/app ./apps/web/dist/app

# Cross-compile for the requested image platform using the native build stage.
ARG TARGETOS
ARG TARGETARCH
# What heartbeat_service_info reports; the release workflow sets both. The
# defaults keep kind's content-derived image tags stable across commits that
# change no code.
ARG VERSION=dev
ARG REVISION=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -ldflags "-X heartbeat/internal/telemetry.version=$VERSION -X heartbeat/internal/telemetry.revision=$REVISION" \
    -o /out/api ./apps/api/cmd/api

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/api /app/api

EXPOSE 8081

USER nonroot:nonroot
# `api` serves; the chart's init container runs `api migrate up` first.
ENTRYPOINT ["/app/api"]
