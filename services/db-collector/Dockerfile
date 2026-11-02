FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

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
    -o /out/db-collector ./services/db-collector/cmd/db-collector

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/db-collector /app/db-collector

EXPOSE 8082

USER nonroot:nonroot
ENTRYPOINT ["/app/db-collector"]
