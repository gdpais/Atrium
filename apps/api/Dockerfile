FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

# The build context is the repository root, with this Dockerfile's own ignore
# file (Dockerfile.dockerignore): the root .dockerignore leaves out db/, and
# the API embeds db/migrations.
COPY . .

# Cross-compile for the requested image platform using the native build stage.
ARG TARGETOS
ARG TARGETARCH
# What atrium_service_info reports; the release workflow sets both. The
# defaults keep kind's content-derived image tags stable across commits that
# change no code.
ARG VERSION=dev
ARG REVISION=
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -ldflags "-X atrium/internal/telemetry.version=$VERSION -X atrium/internal/telemetry.revision=$REVISION" \
    -o /out/api ./apps/api/cmd/api

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/api /app/api

EXPOSE 8081

USER nonroot:nonroot
# `api` serves; the chart's init container runs `api migrate up` first.
ENTRYPOINT ["/app/api"]
