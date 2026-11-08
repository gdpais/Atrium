.PHONY: help tools-check install-tools kind-up kind-down kind-deploy kind-assignments kind-images kind-status kind-e2e health sqlserver-dev-init sqlserver-dev-up sqlserver-dev-down patientzero-up patientzero-down patientzero-load patientzero-load-stop patientzero-scenario patientzero-scenarios patientzero-scale patientzero-scale-measure patientzero-scale-collect patientzero-scale-check patientzero-scale-down chart-deps chart-check rules-check generate generate-check test test-integration test-sqlserver test-race vet lint vulncheck docs-site

SQLSERVER_DEV_ENV_FILE := .env.sqlserver-dev
# Health checks fail fast on HTTP errors (keeping the body) and never hang.
CURL_CHECK := curl -sS --fail-with-body --connect-timeout 2 --max-time 5 -w '\n'
# Toolchain pins (ADR 0005). The kind node image tracks the newest Kubernetes
# minor Amazon EKS supports; kubectl must stay within one minor of it.
KIND_VERSION := v0.33.0
KIND_NODE_IMAGE := kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed
HELM_MIN_VERSION := v4.3.1
# Exact versions CI installs (make install-tools). Helm 3 only checks that the
# chart still renders with it (ADR 0005).
KUBECTL_VERSION := v1.36.4
HELM_VERSION := v4.3.1
HELM3_VERSION := v3.22.1
# go run fetches this exact version and checks it against the Go checksum
# database, so there is nothing to install.
GOVULNCHECK_VERSION := v1.9.0
# Linters for make lint, pinned like govulncheck: staticcheck through go run,
# shellcheck as an image (it is a Haskell binary, not a Go module).
STATICCHECK_VERSION := v0.8.2
SHELLCHECK_IMAGE := koalaman/shellcheck:v0.11.0
# oapi-codegen writes the API's server types from its OpenAPI spec, and sqlc
# its typed queries from SQL (ADR 0013). Fetched by go run like govulncheck,
# so their dependencies stay out of go.mod. sqlc builds with cgo (its
# PostgreSQL parser is C), so it needs a C compiler; the first build takes
# about half a minute.
OAPI_CODEGEN_VERSION := v2.8.2
SQLC_VERSION := v1.31.1
TOOLS_DIR ?= .tmp/bin
HELM ?= helm
CHART_DIR := infra/helm/heartbeat
# full: every component. minimal: collector, Prometheus, Alertmanager, Grafana.
PROFILE ?= full
# promtool comes from the same image as the Prometheus server.
PROMETHEUS_IMAGE := prom/prometheus:v3.15.1
# The HTML docs link source files (Go, YAML, SQL, ...) to this repository URL.
DOCS_REPO_URL := https://github.com/gdpais/Heartbeat
DOCS_REPO_REF := master
# The generator is its own Go module (tools/docsite/go.mod), so the services'
# module does not depend on its Markdown parser.
DOCSITE_MODULE := tools/docsite
# PatientZero, the synthetic OutSystems test application (ADR 0010), is its
# own module too: test code stays out of the product's module and releases.
PATIENTZERO_MODULE := tools/patientzero
# Its chart's image tag and SQL host are required and have no default (the
# install script sets them), so chart-check passes placeholders.
PATIENTZERO_CHART_DIR := $(PATIENTZERO_MODULE)/chart
PATIENTZERO_CHART_CHECK_VALUES := --set image.tag=ci --set sql.host=ci
# staticcheck has no -C flag, so lint cds into each module; CURDIR keeps the
# one shared cache.
STATICCHECK := GOCACHE=$(CURDIR)/.tmp/gocache go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
GOVULNCHECK := GOCACHE=$$(pwd)/.tmp/gocache go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
DOCSITE := GOCACHE=$$(pwd)/.tmp/gocache go -C $(DOCSITE_MODULE) run ./cmd/docsite -root ../.. -repo-url $(DOCS_REPO_URL) -ref $(DOCS_REPO_REF)
API_SPEC := apps/api/openapi.yaml
API_GENERATED := apps/api/internal/openapi/openapi.gen.go
OAPI_CODEGEN := GOCACHE=$$(pwd)/.tmp/gocache go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) -config apps/api/oapi-codegen.yaml
SQLC := GOCACHE=$$(pwd)/.tmp/gocache CGO_ENABLED=1 go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) -f apps/api/sqlc.yaml

help:
	@printf '%s\n' \
		'Heartbeat developer commands:' \
		'  make tools-check         Verify docker, kind, kubectl, helm and go against the pinned versions' \
		'  make install-tools       Install the pinned kind, kubectl, helm and helm3 into $$TOOLS_DIR (CI)' \
		'  make kind-up             Create the kind cluster, build and load images, deploy the chart (PROFILE=full|minimal)' \
		'  make kind-deploy         Redeploy the chart to the existing kind cluster (PROFILE=full|minimal)' \
		'  make kind-assignments    Render the approved probe assignments in the API and deploy them as managedCollectors' \
		'  make kind-images         Rebuild both images and load them into kind' \
		'  make kind-status         Show pods and services in the kind cluster' \
		'  make kind-down           Delete the kind cluster' \
		'  make health              Check the collector, Prometheus, Alertmanager and Grafana on their local ports' \
		'  make sqlserver-dev-init  Create the ignored .env.sqlserver-dev with random SA and collector passwords' \
		'  make sqlserver-dev-up    Start a dev-only SQL Server container and point the collector at it' \
		'  make sqlserver-dev-down  Remove the dev SQL Server container and its data; redeploy without it' \
		'  make patientzero-up      Run PatientZero on kind against the sandbox SQL Server; Heartbeat reads its logs (needs sqlserver-dev-up)' \
		'  make patientzero-down    Remove PatientZero, its databases and login; redeploy Heartbeat without its collector' \
		'  make patientzero-load    Run k6 load against PatientZero on kind and follow it (LOAD=smoke|steady|peak|soak|day, LOAD_DURATION)' \
		'  make patientzero-load-stop  Stop the PatientZero load' \
		'  make patientzero-scenario  Run a PatientZero scenario with its load and check its signals (NAME=<scenario>; no NAME lists them)' \
		'  make patientzero-scenarios Run every PatientZero scenario in order, about 2.5 hours (SCENARIOS="a b" for some)' \
		'  make patientzero-scale   Fill patientzero_scale with a production-sized log and measure the log queries (SCALE, SCALE_SEED, SCALE_END)' \
		'  make patientzero-scale-measure  Measure the log queries on patientzero_scale again' \
		'  make patientzero-scale-collect  Redeploy Heartbeat with a collector that reads patientzero_scale (SCALE_LOOKBACK)' \
		'  make patientzero-scale-check  Compare the users, sessions and error fingerprints in Loki with expected.json (SCALE_WAIT)' \
		'  make patientzero-scale-down  Drop patientzero_scale and remove its collector' \
		'  make kind-e2e            Run the acceptance checks on a separate, temporary kind cluster' \
		'  make chart-deps          Fetch the chart dependencies listed in Chart.lock' \
		'  make chart-check         Lint and render the chart for every values profile (needs chart-deps), and the PatientZero chart' \
		'  make rules-check         Validate Prometheus rules and run their promtool unit tests (requires Docker)' \
		'  make generate            Regenerate the committed generated code: the API server types from apps/api/openapi.yaml, its queries from apps/api/internal/store/queries' \
		'  make generate-check      Fail when the committed generated code differs from what make generate writes' \
		'  make test                Check the generated code, then run all Go tests without Docker integration tests' \
		'  make test-integration    Run isolated PostgreSQL and Valkey integration tests (requires Docker)' \
		'  make test-sqlserver      Run every SQL Server probe and OutSystems log query against a disposable SQL Server container (requires Docker)' \
		'  make test-race           Run all Docker-free tests with the race detector' \
		'  make vet                 Run Go static checks' \
		'  make lint                Check Go formatting, run staticcheck on every Go module and shellcheck on every shell script (requires Docker)' \
		'  make vulncheck           Report known vulnerabilities reachable from the Go code, standard library included' \
		'  make docs-site           Build the HTML docs site into docs/site (not committed; CI publishes it)'

tools-check:
	@KIND_VERSION=$(KIND_VERSION) KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) HELM_MIN_VERSION=$(HELM_MIN_VERSION) scripts/tools-check.sh

install-tools:
	@KIND_VERSION=$(KIND_VERSION) KUBECTL_VERSION=$(KUBECTL_VERSION) HELM_VERSION=$(HELM_VERSION) HELM3_VERSION=$(HELM3_VERSION) scripts/install-tools.sh $(TOOLS_DIR)

kind-up:
	@KIND_NODE_IMAGE='$(KIND_NODE_IMAGE)' PROFILE=$(PROFILE) scripts/kind.sh up

kind-deploy:
	@PROFILE=$(PROFILE) scripts/kind.sh deploy

kind-images:
	@scripts/kind.sh images

kind-assignments:
	@PROFILE=$(PROFILE) scripts/kind.sh assignments

kind-status:
	@scripts/kind.sh status

kind-down:
	@scripts/kind.sh down

# kind maps these NodePorts to the same 127.0.0.1 ports Compose used.
health:
	$(CURL_CHECK) http://localhost:8082/healthz
	$(CURL_CHECK) http://localhost:8082/readyz
	$(CURL_CHECK) http://localhost:9090/-/ready
	$(CURL_CHECK) http://localhost:9093/-/ready
	$(CURL_CHECK) http://localhost:3001/api/health

# Generates random per-machine passwords instead of copying shared ones: sa
# for container setup, and the collector's least-privilege login
# (scripts/sqlserver-login.sh). A file from before the dedicated login, whose
# collector credential is sa (or missing), gets one added; the SA password is
# kept. The file must hold plain KEY=value lines, which is how scripts/kind.sh
# reads it: no `export`, no quotes. The "Dev-" prefix plus hex digits
# satisfies SQL Server password complexity.
sqlserver-dev-init:
	@set -eu; umask 077; file=$(SQLSERVER_DEV_ENV_FILE); key=HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV; \
	unreadable() { echo "error: cannot read $$file" >&2; exit 2; }; \
	if [ ! -f "$$file" ]; then \
		printf 'MSSQL_SA_PASSWORD=Dev-%s\n' "$$(openssl rand -hex 16)" > "$$file"; \
		echo "Created $$file with a random SA password"; \
	fi; \
	status=0; grep -Eq "^[[:space:]]*export[[:space:]]|^[A-Za-z_][A-Za-z0-9_]*=[\"']" "$$file" || status=$$?; \
	case $$status in \
	0) echo "error: $$file must hold plain KEY=value lines (no export, no quotes)" >&2; exit 1 ;; \
	1) ;; \
	*) unreadable ;; \
	esac; \
	current=$$(grep "^$$key=" "$$file") || [ $$? -eq 1 ] || unreadable; \
	user=$$(printf '%s\n' "$$current" | head -n 1 | cut -d= -f2- | cut -s -d: -f1 | tr 'A-Z' 'a-z'); \
	if [ -z "$$user" ] || [ "$$user" = sa ]; then \
		rest=$$(grep -v "^$$key=" "$$file") || [ $$? -eq 1 ] || unreadable; \
		{ if [ -n "$$rest" ]; then printf '%s\n' "$$rest"; fi; \
			printf '%s=heartbeat_collector:Dev-%s\n' "$$key" "$$(openssl rand -hex 16)"; } > "$$file.tmp"; \
		mv "$$file.tmp" "$$file"; \
		echo "Added a least-privilege collector login to $$file"; \
	fi
	chmod 600 $(SQLSERVER_DEV_ENV_FILE)

sqlserver-dev-up:
	@KIND_NODE_IMAGE='$(KIND_NODE_IMAGE)' PROFILE=$(PROFILE) SQLSERVER_ENV_FILE=$(SQLSERVER_DEV_ENV_FILE) scripts/kind.sh sqlserver-up

sqlserver-dev-down:
	@PROFILE=$(PROFILE) scripts/kind.sh sqlserver-down

patientzero-up:
	@PROFILE=$(PROFILE) SQLSERVER_ENV_FILE=$(SQLSERVER_DEV_ENV_FILE) $(PATIENTZERO_MODULE)/scripts/patientzero.sh up

patientzero-down:
	@PROFILE=$(PROFILE) SQLSERVER_ENV_FILE=$(SQLSERVER_DEV_ENV_FILE) $(PATIENTZERO_MODULE)/scripts/patientzero.sh down

# LOAD, not PROFILE: PROFILE is the Heartbeat chart's values profile.
patientzero-load:
	@LOAD='$(LOAD)' LOAD_DURATION='$(LOAD_DURATION)' LOAD_SEED='$(LOAD_SEED)' $(PATIENTZERO_MODULE)/scripts/patientzero.sh load

patientzero-load-stop:
	@$(PATIENTZERO_MODULE)/scripts/patientzero.sh load-stop

# Fault scenarios (tools/patientzero/scenarios): the run journal is saved to
# .tmp/patientzero-scenarios/<run>.jsonl.
patientzero-scenario:
	@SCENARIO='$(NAME)' SCENARIO_LOAD='$(SCENARIO_LOAD)' $(PATIENTZERO_MODULE)/scripts/patientzero.sh scenario

# SCALE, not PROFILE or LOAD: the volume relative to the surveyed installation.
# Every scenario in order (tools/patientzero/scripts/patientzero.sh), about
# two and a half hours; SCENARIOS="a b" for some of them.
patientzero-scenarios:
	@SCENARIOS='$(SCENARIOS)' $(PATIENTZERO_MODULE)/scripts/patientzero.sh scenarios

patientzero-scale:
	@PROFILE=$(PROFILE) SCALE='$(SCALE)' SCALE_SEED='$(SCALE_SEED)' SCALE_END='$(SCALE_END)' SQLSERVER_ENV_FILE=$(SQLSERVER_DEV_ENV_FILE) $(PATIENTZERO_MODULE)/scripts/patientzero.sh scale

patientzero-scale-measure:
	@SQLSERVER_ENV_FILE=$(SQLSERVER_DEV_ENV_FILE) $(PATIENTZERO_MODULE)/scripts/patientzero.sh scale-measure

patientzero-scale-collect:
	@PROFILE=$(PROFILE) SCALE_LOOKBACK='$(SCALE_LOOKBACK)' SQLSERVER_ENV_FILE=$(SQLSERVER_DEV_ENV_FILE) $(PATIENTZERO_MODULE)/scripts/patientzero.sh scale-collect

patientzero-scale-check:
	@SCALE_WAIT='$(SCALE_WAIT)' $(PATIENTZERO_MODULE)/scripts/patientzero.sh scale-check

patientzero-scale-down:
	@PROFILE=$(PROFILE) SQLSERVER_ENV_FILE=$(SQLSERVER_DEV_ENV_FILE) $(PATIENTZERO_MODULE)/scripts/patientzero.sh scale-down

kind-e2e:
	@KIND_NODE_IMAGE='$(KIND_NODE_IMAGE)' scripts/kind-e2e.sh

chart-deps:
	scripts/chart-deps.sh

# HELM=<path> repeats the checks with another Helm binary (CI uses Helm 3).
chart-check:
	$(HELM) lint --strict $(CHART_DIR) -f infra/helm/values/kind.yaml
	HELM=$(HELM) GOCACHE=$$(pwd)/.tmp/gocache go test -tags=chart -count=1 -run '^TestChart(Profiles|Collector)' ./tests
	$(HELM) lint --strict $(PATIENTZERO_CHART_DIR) $(PATIENTZERO_CHART_CHECK_VALUES)
	HELM=$(HELM) GOCACHE=$$(pwd)/.tmp/gocache go test -tags=chart -count=1 -run TestPatientZeroChart ./tests

rules-check:
	docker run --rm -v "$$(pwd)/infra/helm/heartbeat/files/prometheus/rules:/rules:ro" --entrypoint promtool $(PROMETHEUS_IMAGE) check rules /rules/heartbeat.rules.yml /rules/heartbeat-config-reload.rules.yml /rules/heartbeat-collector-missing.rules.yml /rules/heartbeat-api-missing.rules.yml /rules/heartbeat-self.rules.yml /rules/generated/heartbeat-rendered.rules.yml
	docker run --rm -v "$$(pwd)/infra/helm/heartbeat/files/prometheus/rules:/rules:ro" -w /rules/tests --entrypoint promtool $(PROMETHEUS_IMAGE) test rules heartbeat.rules.test.yml

generate:
	$(OAPI_CODEGEN) -o $(API_GENERATED) $(API_SPEC)
	$(SQLC) generate

# Regenerates into .tmp and compares, so the working tree is never touched: a
# spec, query or migration edited, or a generator bumped, without make
# generate fails here. sqlc diff compares in memory and writes nothing.
generate-check:
	@mkdir -p .tmp/generate
	$(OAPI_CODEGEN) -o .tmp/generate/openapi.gen.go $(API_SPEC)
	@diff -u $(API_GENERATED) .tmp/generate/openapi.gen.go || { \
		echo "error: $(API_GENERATED) is stale: run make generate and commit the result" >&2; exit 1; }
	@$(SQLC) diff || { \
		echo "error: the sqlc code in apps/api/internal/store is stale: run make generate and commit the result" >&2; exit 1; }

test: generate-check
	GOCACHE=$$(pwd)/.tmp/gocache go test ./...
	GOCACHE=$$(pwd)/.tmp/gocache go -C $(DOCSITE_MODULE) test ./...
	GOCACHE=$$(pwd)/.tmp/gocache go -C $(PATIENTZERO_MODULE) test ./...

# The tests in tests/ apply the migrations with psql in Compose projects; the
# API's (apps/api) start their own PostgreSQL and Valkey containers.
test-integration:
	GOCACHE=$$(pwd)/.tmp/gocache go test -tags=integration -count=1 -timeout=10m ./tests ./apps/api/...

test-sqlserver:
	scripts/sqlserver-test.sh

test-race:
	GOCACHE=$$(pwd)/.tmp/gocache go test -race ./...
	GOCACHE=$$(pwd)/.tmp/gocache go -C $(DOCSITE_MODULE) test -race ./...
	GOCACHE=$$(pwd)/.tmp/gocache go -C $(PATIENTZERO_MODULE) test -race ./...

vet:
	GOCACHE=$$(pwd)/.tmp/gocache go vet ./...
	GOCACHE=$$(pwd)/.tmp/gocache go -C $(DOCSITE_MODULE) vet ./...
	GOCACHE=$$(pwd)/.tmp/gocache go -C $(PATIENTZERO_MODULE) vet ./...

# gofmt -l lists the files it would change; any output fails. staticcheck
# runs its default checks, with the build tags of the Docker and chart tests
# so their files are checked too. shellcheck fails on warnings and errors and
# leaves its notes (style suggestions) to the author. A deliberate exception
# gets a "# shellcheck disable=SCxxxx # reason" line above it.
lint:
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); \
		if [ -n "$$unformatted" ]; then printf 'error: not gofmt-formatted (run gofmt -w):\n%s\n' "$$unformatted" >&2; exit 1; fi
	$(STATICCHECK) -tags=integration,chart,sqlserver ./...
	cd $(DOCSITE_MODULE) && $(STATICCHECK) ./...
	cd $(PATIENTZERO_MODULE) && $(STATICCHECK) ./...
	docker run --rm -v "$$(pwd):/mnt:ro" -w /mnt $(SHELLCHECK_IMAGE) --severity=warning $$(git ls-files '*.sh')

# Fails on any vulnerability reachable from the code; the standard library
# counts, so a Go patch release with security fixes needs the go directives
# and the Dockerfile build images bumped.
vulncheck:
	$(GOVULNCHECK) ./...
	$(GOVULNCHECK) -C $(DOCSITE_MODULE) ./...
	$(GOVULNCHECK) -C $(PATIENTZERO_MODULE) ./...

docs-site:
	$(DOCSITE)
