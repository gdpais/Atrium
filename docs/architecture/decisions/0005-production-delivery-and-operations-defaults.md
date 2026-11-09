# 0005. Production delivery and operations defaults

- Status: Accepted. Chart side implemented 2026-10-03 (exact version pins,
  chart rules below, Watchdog route, `production.example.yaml`). Update
  2026-11-08: the config repository `heartbeat-deploy` exists, with its CI
  and an Argo CD Application, and also holds each environment's inventory
  ([ADR 0021](0021-inventory-as-code-and-no-web-ui.md)); the AWS
  infrastructure and the Argo CD install are not built [TODO 18]. The kind
  profile has no Discord receiver yet.
- Date: 2026-09-30

## Context

[ADR 0003](0003-helm-on-kind-and-production.md) chose Helm on kind and
production Kubernetes but left production decisions open. Production will run
on **AWS**. There is no production cluster yet and no GitOps tooling.

The notification requirements and the constraints around them:
- Production notifications must reach Microsoft Teams, Slack and WhatsApp.
- Microsoft disabled the Office 365 connector webhooks for Teams in May 2026; Power Automate Workflows webhooks replace them.
- Alertmanager has no native WhatsApp receiver.
- WhatsApp Business allows business-initiated messages only through pre-approved templates.
- Amazon EKS supports Kubernetes up to 1.36 (upstream latest is 1.37).

## Decision

| Concern | Choice | Why |
| --- | --- | --- |
| Platform | **Amazon EKS** | Production on AWS. |
| Kubernetes version | Newest minor EKS supports (**1.36** today). kind pins the same minor (`kindest/node:v1.36.4@sha256:099e0493…cbfaed`) and moves only when EKS does | Local and CI test the API version production runs. |
| Deploy mechanism | **Argo CD (GitOps)** from the first production deploy; no interim CI push pipeline | Production does not exist yet, so a push pipeline would be thrown away. GitOps keeps cluster credentials out of CI, makes every change a reviewed commit, reverts manual drift, and rolls back via `git revert`. |
| Desired state | A separate config repository (`heartbeat-deploy`) with Argo CD `Application`s and per-environment values; this repo keeps the chart, defaults and `production.example.yaml` | Argo CD best practice: clean audit trail, separate write access for production, no CI commit loops. |
| Registry | **Amazon ECR** for images (multi-arch, referenced by digest) and the chart (OCI). GitHub Actions pushes through GitHub OIDC → IAM role; no long-lived AWS keys | EKS nodes pull with IAM (no pull secrets), in-region, with image scanning. |
| Secrets | **AWS Secrets Manager**, synced into Kubernetes Secrets by **External Secrets Operator** using EKS Pod Identity. ESO also refreshes Argo CD's short-lived ECR credentials | Managed, IAM-scoped, audited in CloudTrail, supports rotation. The chart only consumes `existingSecret` names, so the backend stays swappable. |
| Versions | Latest stable that production supports, pinned exactly (tools, CI runner image, node image by digest, `Chart.lock`, image digests in production); Renovate proposes updates, kind CI must pass | Current without floating tags; upgrades are reviewed, one change at a time. |
| Alert notifications | Discord now; Slack (`slack_configs`) and Teams (`msteamsv2_configs`) in production; webhook URLs from mounted Secrets | Native Alertmanager receivers; the legacy `msteams_configs` depends on the retired connectors. |
| WhatsApp | Production, **critical severity only**: Alertmanager `sns_configs` → Amazon SNS topic → small Lambda → AWS End User Messaging Social `SendWhatsAppMessage` with an approved utility template | No custom code in the cluster; AWS-native IAM and audit; SNS can fan out to more channels later. Critical-only because messages cost money, reach personal phones and are rate-limited by Meta. |
| Dead-man's switch | Alertmanager Watchdog → healthchecks.io, which notifies the chat channels | Chat receivers cannot detect silence; the watcher must be outside Heartbeat's failure domain (ADR 0003, risk 1), and outside AWS too. |
| Grafana access | Admin login. kind: `admin/admin`, loopback only. Production: generated admin password from Secrets Manager, anonymous access off, TLS at ingress | Matches [ADR 0004](0004-core-technology-choices.md)'s local-auth-first choice; SSO is a later values change. |

Vault was considered for secrets and not chosen. It is a system to operate
(HA storage, unsealing, upgrades) or a paid HCP service, and its license
changed in 2023 (OpenBao is the open fork). It earns its cost for multi-cloud
or dynamic secrets. Heartbeat is AWS-only, and the monitored databases belong to
other teams, so dynamic SQL Server logins are unlikely to be allowed.

Chart rules that follow from Argo CD (it renders with `helm template` and
applies the result itself):
- Deterministic rendering: no `randAlphaNum`, `genCA` or `lookup`. Secrets are
  created outside the chart.
- No reliance on Helm release state (`.Release.Revision`, `IsInstall`, `IsUpgrade`),
  and no Helm hooks unless Argo's hook mapping is acceptable.
- The chart does not create its namespace.
- It must render with Helm 3 as well as Helm 4.

## Consequences

- `helm list` shows nothing in production; history and rollback live in Git and Argo CD.
- The team has to learn Argo CD. An optional local rehearsal (Argo CD in kind)
  precedes the first production deploy.
- AWS-specific pieces (ECR, Secrets Manager, SNS, ingress, storage) live in
  production values and the config repo. kind keeps local equivalents (loaded
  images, generated Secrets, Discord only), so the local loop needs no AWS account.
- Production defaults, adjustable during the production track:
  - EKS Auto Mode (AWS manages nodes, EBS CSI, load balancing and VPC CNI with NetworkPolicy);
  - infrastructure as code with OpenTofu and the community EKS module;
  - internal ALB + ACM certificate for Grafana;
  - gp3 EBS for Prometheus; S3 for Loki;
  - RDS for PostgreSQL once the API needs it.
- WhatsApp needs, before it can go live:
  - a Meta WhatsApp Business Account with a verified business and phone number, linked in AWS End User Messaging Social;
  - an approved alert template;
  - recipient opt-in. Phone numbers are personal data.
  WhatsApp gives no acknowledgement or escalation. If on-call escalation becomes a requirement, that calls for a paging tool, which would be a separate decision.
- Still open: where the monitored SQL Servers live relative to the EKS VPC (on-premises
  via VPN/Direct Connect, EC2 or RDS). It determines network design and is needed
  before the first production deploy.
