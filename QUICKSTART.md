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

For a fresh kind / orbstack / minikube cluster:

```bash
bash hack/e2e-up.sh
```

That script cross-compiles the controller / gateway binaries, builds the
three images locally (`devpod-controller:e2e`, `devpod-gateway:e2e`,
`devpod-supervisor:e2e`), loads them into the cluster, applies the CRDs,
helm-installs the chart, and auto-generates the gateway host keys.

For a production cluster, build & push to your registry, then helm-install
yourself:

```bash
# Build (substitute your registry / version)
docker build -t REGISTRY/devpod-controller:v0.1.0 -f - . <<EOF
FROM gcr.io/distroless/static
COPY bin/devpod-controller /usr/local/bin/devpod-controller
ENTRYPOINT ["/usr/local/bin/devpod-controller"]
EOF
docker build -t REGISTRY/devpod-gateway:v0.1.0 ...      # same shape
docker buildx build -t REGISTRY/devpod-supervisor:v0.1.0 \
    -f images/supervisor/Dockerfile --push .

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

Provision the two gateway host-key Secrets out of band before `helm install`,
or let `hack/e2e-up.sh` generate them.

---

## 3. Verify the install

```bash
kubectl -n devpod-system get pods            # controller + gateway × 2 Running
kubectl get gatewayconfig default            # cluster singleton
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

CPU-binding (Kore) is template-mediated: ordinary users can only pick
admin-curated `DevPodTemplate` CRs — raw `kore.zjusct.io/*` annotations
are rejected. Seed templates with kubectl (admin UI comes in M2):

```yaml
apiVersion: devpod.io/v1alpha1
kind: DevPodTemplate
metadata: {name: pin8}
spec:
  displayName: "Exclusive 8 cores, single NUMA"
  binding:
    annotations:
      kore.zjusct.io/pin: "true"
      kore.zjusct.io/numa-policy: single
    resources:
      requests: {cpu: "8"}
      limits:   {cpu: "8", memory: "16Gi"}
```

Per-user quotas live on `User.spec.quota` (defaults from
`webui.defaultQuota`). Quota is webui-layer policy — anyone holding a
kubeconfig bypasses it.

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
