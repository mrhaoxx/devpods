# DevPod Quickstart

From zero to `ssh alice+pod@gateway` in ~10 minutes.

---

## 1. Prerequisites

**Cluster:**

- Kubernetes ≥ 1.25 (PodSecurity Admission v1)
- CNI that enforces `NetworkPolicy` — Calico / Cilium / Antrea / Flannel+Calico / Weave
- A `StorageClass` if you'll use `spec.persistence` (optional)

**Tools:**

- `kubectl` ≥ 1.22 (server-side apply with `--force-conflicts`)
- `helm` 3.x
- `docker` (or `podman`) with `buildx` if you'll build images locally
- `ssh` client (any modern OpenSSH)

---

## 2. Build & load images

The chart's defaults pull `ghcr.io/mrhaoxx/devpod-*:dev`, which CI builds
from main (amd64 and arm64); a release's images carry its tag (`v0.9.16`).
To use your own registry, build each image from the repository root:

```bash
for c in controller gateway supervisor agent webui snapshot; do
  docker buildx build --platform linux/amd64,linux/arm64 \
      -f images/$c/Dockerfile -t REGISTRY/devpod-$c:TAG --push .
done
```

Then install (with your registry, `--set image.<name>.repository=… --set
image.<name>.tag=…` for each image):

```bash
# The gateway's host keys, made once
kubectl create namespace devpod-system
ssh-keygen -t ed25519 -N '' -f host -q && ssh-keygen -t ed25519 -N '' -f internal -q
for k in host internal; do
  kubectl -n devpod-system create secret generic devpod-gateway-$k-key \
      --from-file=ssh_host_ed25519_key=$k --from-file=ssh_host_ed25519_key.pub=$k.pub
done
rm -f host host.pub internal internal.pub

# Apply CRDs (chart only installs them on first install)
kubectl apply --server-side --force-conflicts -f deploy/chart/crds/

# Install chart
helm install devpod ./deploy/chart \
    --set image.controller.repository=REGISTRY/devpod-controller \
    --set image.controller.tag=v0.1.0 \
    --set image.gateway.repository=REGISTRY/devpod-gateway \
    --set image.gateway.tag=v0.1.0 \
    --set image.supervisor.repository=REGISTRY/devpod-supervisor \
    --set image.supervisor.tag=v0.1.0
```

The two key Secrets are provisioned out of band (above), never by the chart.

---

## 3. Verify the install

```bash
kubectl -n devpod-system get pods            # controller + gateway × 2 Running
kubectl get gatewayconfig default            # read by the gateway (--gateway-config-name)
kubectl explain devpod.spec                  # CRD docs visible
```

---

## 4. Create your first user + DevPod

```bash
# Your laptop's pubkey
PUBKEY=$(cat ~/.ssh/id_ed25519.pub)

# Register the user
kubectl apply -f - <<EOF
apiVersion: devpod.io/v1alpha1
kind: User
metadata: {name: alice}
spec:
  pubkeys:
    - "$PUBKEY"
EOF

# Provision a DevPod
kubectl apply -f - <<EOF
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: scratchpad, namespace: devpods}
spec:
  owner: alice
  running: true
  shell: bash                     # optional; bash | zsh | fish
  pod:
    spec:
      containers:
        - name: dev
          image: ubuntu:24.04
EOF

kubectl -n devpods wait --for=jsonpath='{.status.phase}'=Running devpod/scratchpad --timeout=180s
```

---

## 5. SSH in

If `devpod-gateway` is `ClusterIP` (the default), port-forward to test:

```bash
kubectl -n devpod-system port-forward svc/devpod-gateway 2222:22 &
ssh -p 2222 alice+scratchpad@127.0.0.1
```

Login syntax is **`<user>+<devpod-name>@<gateway>`**. The gateway looks up
`alice`'s pubkey, matches against the offered key, then proxies to the
DevPod's in-container sshd.

For external access, change `gateway.service.type` to `LoadBalancer`,
`NodePort`, or expose via a TCP-aware Ingress (nginx + `tcp-services`,
Istio TCP route, etc.).

---

## 6. (Optional) Enable LDAP as a second pubkey source

If you have an existing LDAP directory you'd rather use than the `User` CRD
for day-to-day pubkeys (the CRD stays as the "break-glass" admin surface):

```bash
# Provide the two Secrets in devpod-system
kubectl -n devpod-system create secret generic devpod-ldap-ca \
    --from-file=ca.crt=/path/to/ldap-ca.pem
kubectl -n devpod-system create secret generic devpod-ldap-bind \
    --from-literal=password='svc-password'

# Re-helm with LDAP enabled
helm upgrade --install devpod ./deploy/chart \
    --reuse-values \
    --set gateway.ldap.enabled=true \
    --set gateway.ldap.url=ldaps://ldap.example.com:636 \
    --set gateway.ldap.bindDN="cn=svc,ou=System,dc=example,dc=com" \
    --set gateway.ldap.baseDN="dc=example,dc=com" \
    --set gateway.ldap.caSecret.name=devpod-ldap-ca \
    --set gateway.ldap.caSecret.namespace=devpod-system \
    --set gateway.ldap.bindPasswordSecret.name=devpod-ldap-bind \
    --set gateway.ldap.bindPasswordSecret.namespace=devpod-system

kubectl -n devpod-system rollout restart deploy/devpod-gateway
```

Gateway behavior: `User` CRD lookup first → on miss or no key match, query
LDAP. A configurable in-memory cache (default 5 min TTL + 15 min stale-grace)
softens LDAP outages.

Users with no `User` CRD work end-to-end: the controller will create the
DevPod, the gateway will authenticate from LDAP.

---

## 6b. (Optional) Enable the Web UI

Browser self-service with GitLab OAuth login: list/create/hibernate/wake/
delete DevPods, manage SSH pubkeys, per-user quotas. GitLab user `xxxx`
acts as DevPod user `<prefix>xxxx` (prefix configurable).

```bash
# 1. Register an OAuth application in your GitLab instance:
#    redirect URI = https://devpod.example.com/auth/callback
#    scopes = openid profile   (confidential application)

# 2. Provide the two Secrets
kubectl -n devpod-system create secret generic devpod-webui-oauth \
    --from-literal=client-secret='<gitlab application secret>'
kubectl -n devpod-system create secret generic devpod-webui-session \
    --from-literal=session-key="$(openssl rand -base64 48)"

# 3. Re-helm with the webui enabled
helm upgrade --install devpod ./deploy/chart \
    --reuse-values \
    --set webui.enabled=true \
    --set webui.baseURL=https://devpod.example.com \
    --set webui.userPrefix=gl- \
    --set 'webui.admins={youradmin}' \
    --set webui.gitlab.issuerURL=https://gitlab.example.com \
    --set webui.gitlab.clientID=<application id> \
    --set webui.gitlab.clientSecretSecret.name=devpod-webui-oauth \
    --set webui.sessionKeySecret.name=devpod-webui-session
```

Expose `svc/devpod-webui` via your Ingress at `webui.baseURL` (TLS at
the Ingress; an `http://` baseURL disables the Secure cookie attribute
and is for dev/e2e only).

What users and admins get:

- Everyone: **我的 DevPod** (live per-second metrics in every row, create
  and edit in a drawer with a live quota check), the DevPod page (live
  cards, 监控 charts from 1 s to 7 days, 进程, 会话, 容器, 事件), 设置
  (SSH keys, password), ⌘K / Ctrl+K search, light/dark theme.
- Admins (`webui.admins` or `User.spec.admin`): 集群总览, 全部 DevPod
  (bulk 休眠 / 启动 / 删除), 节点 (allocation, GPU occupants, Kore
  topology), 用户与配额 (quota bars, batch 设配额 / 设审计策略 / 重置密码 /
  休眠, CSV import `username,displayName[,password]`). Admins can grant
  admin to others from the user drawer.
- Live numbers come from the telemetry agent (`--set telemetry.enabled=true`;
  needs audit's bundled or external ClickHouse, see the `telemetry`
  values in `deploy/chart/values.yaml`); a DevPod reports once its Pod is
  next recreated, and shows "—" until then.

CPU-binding (Kore) is for admins: an admin writes the
`kore.zjusct.io/*` annotations in a DevPod's YAML (on `spec.pod.metadata`);
anyone else's DevPod carrying them is refused, and they cannot be changed
from the YAML tab. To give users a ready-made environment, save it as a
config and mark it official (配置 page).

Per-user quotas live on `User.spec.quota` (defaults from
`webui.defaultQuota`). Quota is webui-layer policy — anyone holding a
kubeconfig bypasses it.

---

## 6c. (Optional) Enable session audit

The gateway records every SSH session — raw streams (TTY replay), exec
commands, and the content of SFTP / scp / ZMODEM transfers — to S3 +
ClickHouse; the web UI shows sessions, replays, command / file search.

Quick start with bundled single-replica storage (Silo for S3, ClickHouse).
Use `--reset-then-reuse-values` (Helm ≥ 3.14), not `--reuse-values`: the
latter keeps the old release's values and drops the chart's new `audit.*`
defaults.

```bash
helm upgrade --install devpod ./deploy/chart --reset-then-reuse-values \
    --set audit.enabled=true \
    --set audit.s3.bundled.enabled=true \
    --set audit.clickhouse.bundled.enabled=true
```

A `devpod-audit-migrate` hook Job creates the bucket, lifecycle rules and
tables on every install/upgrade. Retention is `audit.retentionDays`
(default 180).

Production: point at existing storage instead (no HA in bundled mode):

```bash
kubectl -n devpod-system create secret generic audit-s3 \
    --from-literal=access-key=... --from-literal=secret-key=...
for c in gateway webui migrate; do
  kubectl -n devpod-system create secret generic audit-ch-$c \
      --from-literal=username=devpod_$c --from-literal=password=...
done
helm upgrade --install devpod ./deploy/chart --reset-then-reuse-values \
    --set audit.enabled=true \
    --set audit.s3.endpoint=https://s3.example.com \
    --set audit.s3.credentialsSecret=audit-s3 \
    --set audit.s3.manageLifecycle=true \
    --set audit.clickhouse.url=clickhouse://clickhouse.example.com:9000 \
    --set audit.clickhouse.credentialsSecrets.gateway=audit-ch-gateway \
    --set audit.clickhouse.credentialsSecrets.webui=audit-ch-webui \
    --set audit.clickhouse.credentialsSecrets.migrate=audit-ch-migrate
```

ClickHouse grants: gateway `INSERT`, webui `SELECT, INSERT`, migrate `ALL`
on the audit database.

Optional hard policies (all default to Allow) — cluster default in
`audit.policy`, per-user override on `User.spec.auditPolicy` (admins can
edit it in the UI):

```yaml
audit:
  policy:
    download: Deny          # sftp get / scp -f / sz
    maxFileSize: 1Gi
    forwarding: {local: Allow, remote: Deny, recordContent: false}
    failMode: Open          # Closed: refuse sessions when the spool is full
```

Denying downloads while local forwarding stays allowed leaves VS Code's
file download open; the UI warns about that combination. Policies only
cover recognised protocols — `cat`/`tar`/`rsync` over exec and copy-paste
are recorded, not blocked.

---

## 7. Hibernate / persist / collaborate

```yaml
spec:
  owner: alice
  collaborators: [bob]               # bob can also ssh alice+scratchpad@...
  running: false                     # pause: pod deleted, PVC kept
  persistence:
    size: 20Gi
    storageClass: standard
    mountPath: /home/alice
```

Set `running: false` to hibernate (controller deletes Pod, keeps PVC);
flip back to `true` to resume. The home directory survives.

---

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `Permission denied (publickey)` immediately | Username not registered (`User` CRD missing or LDAP doesn't have it) |
| `Connection closed by 127.0.0.1` mid-handshake | DevPod hasn't reached `phase=Running` yet, or `status.endpoint` empty |
| DevPod stuck in `Pending` | Check controller logs: `kubectl -n devpod-system logs deploy/devpod-controller` |
| Controller logs `owner User not found` | The DevPod's owner has no User CRD; on LDAP-enabled installs this is fine, the gateway will still authenticate. On pure-CRD installs, create the User. |
| PVC stuck pending | No default StorageClass; set `spec.persistence.storageClass` explicitly |
| `lookups_total{outcome=cache_stale}` climbing | LDAP is failing; check LDAP server / `devpod_gateway_ldap_connection_state` gauge |
| Gateway pod CrashLoopBackOff after LDAP enable | CA Secret missing or key not named `ca.crt`; bind password Secret missing or key not named `password` |

See `FOLLOWUPS.md` for known open issues.
