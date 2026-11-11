# PatientZero's image (ADR 0010). Build it from the repository root, the
# context that holds both this module and the db-collector's oslog fixtures
# the logging schema is built from:
#
#   docker build -f tools/patientzero/Dockerfile .
#
# A test tool: never published, never part of a release.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build

WORKDIR /src/tools/patientzero

COPY tools/patientzero/go.mod tools/patientzero/go.sum ./
RUN go mod download

COPY tools/patientzero/ ./

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/patientzero ./cmd/patientzero

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/patientzero /app/patientzero
# The one definition of the logging schema (PATIENTZERO_SCHEMA_DIR).
COPY services/db-collector/internal/collectors/outsystems/testdata/oslog/ /app/oslog/

EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/app/patientzero"]
