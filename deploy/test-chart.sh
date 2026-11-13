#!/usr/bin/env bash
# Render the chart in the audit configurations and assert the wiring.
# Requires helm. Run from anywhere; CI runs it in the helm-render job.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/deploy/chart"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT
fail=0

render() { # name, helm --set args...
  local name=$1; shift
  helm template devpod "$CHART" "$@" >"$OUT/$name.yaml"
  helm lint "$CHART" "$@" >/dev/null || { echo "FAIL [$name]: helm lint"; fail=1; }
}
has() { grep -qF -- "$2" "$OUT/$1.yaml" || { echo "FAIL [$1]: missing: $2"; fail=1; }; }
# doc RENDER KIND NAME: print that one object of a render.
doc() { KIND=$2 NAME=$3 perl -0777 -ne 'for (split /^---$/m) { print if /^kind: \Q$ENV{KIND}\E$/m && /^  name: \Q$ENV{NAME}\E$/m }' "$OUT/$1.yaml"; }
docHas() { # RENDER KIND NAME TEXT
  grep -qF -- "$4" <<<"$(doc "$1" "$2" "$3")" || { echo "FAIL [$1]: $2/$3 lacks: $4"; fail=1; }
}
lacks() { if grep -qF -- "$2" "$OUT/$1.yaml"; then echo "FAIL [$1]: unexpected: $2"; fail=1; fi; }

WEBUI=(--set webui.enabled=true --set webui.passwordAuth=true --set webui.sessionKeySecret.name=sk)

# (a) defaults: audit is off and nothing audit-related renders.
render defaults "${WEBUI[@]}"
lacks defaults "devpod-audit-"
lacks defaults "--audit-"
lacks defaults "devpod-live-key"
lacks defaults "terminationGracePeriodSeconds: 300"

lacks defaults "webshell"
lacks defaults "--http-"

# snapshots: the controller gets the node socket path (default) and, when
# set, a separate Job image for Docker nodes; values from before the
# snapshot block still render.
docHas defaults Deployment devpod-controller "- --snapshot-image=ghcr.io/mrhaoxx/devpod-snapshot:dev"
docHas defaults Deployment devpod-controller "- --snapshot-containerd-socket=/run/containerd/containerd.sock"
lacks defaults "--snapshot-docker-image"
render snapshot "${WEBUI[@]}" --set image.snapshot.repository=reg.test/library/devpod-snapshot --set image.snapshot.tag=v1 \
  --set snapshot.containerdSocket=/run/k3s/containerd/containerd.sock --set snapshot.dockerImage=reg.test/docker:cli
docHas snapshot Deployment devpod-controller "- --snapshot-image=reg.test/library/devpod-snapshot:v1"
docHas snapshot Deployment devpod-controller "- --snapshot-containerd-socket=/run/k3s/containerd/containerd.sock"
docHas snapshot Deployment devpod-controller "- --snapshot-docker-image=reg.test/docker:cli"
lacks snapshot "--snapshot-plain-http-registries"
render snapplain "${WEBUI[@]}" --set 'snapshot.plainHTTPRegistries={10.0.0.8:32500,reg.local}'
docHas snapplain Deployment devpod-controller "- --snapshot-plain-http-registries=10.0.0.8:32500,reg.local"
lacks defaults "devpod-http-key"
lacks defaults "--cli-"
lacks defaults "devpod-cli-key"
lacks defaults "--snapshots"

# port links behind a public entry on a port of its own
render httpport "${WEBUI[@]}" --set gateway.http.enabled=true --set gateway.http.baseDomain=apps.test --set gateway.http.suffix=-dev \
  --set webui.baseURL=https://devpod.test:4430 --set gateway.http.externalPort=4430
docHas httpport Deployment devpod-webui "- --http-proxy-port=4430"
docHas httpport Deployment devpod-webui "- --http-proxy-host={id}-{port}-dev.apps.test"
docHas httpport Deployment devpod-gateway "- --http-login-url=https://devpod.test:4430/http-auth"
lacks defaults "--http-proxy-port"

# LDAP sign-in: the console mounts the gateway's two LDAP Secrets and
# needs no other login method; without the gateway's LDAP it does not render.
lacks defaults "--ldap-auth"
LDAP=(--set gateway.ldap.enabled=true --set gateway.ldap.url=ldaps://id.test --set gateway.ldap.bindDN=cn=svc --set gateway.ldap.baseDN=dc=test
  --set gateway.ldap.caSecret.name=ldap-ca --set gateway.ldap.bindPasswordSecret.name=ldap-bind)
render ldap --set webui.enabled=true --set webui.sessionKeySecret.name=sk --set webui.ldapAuth=true "${LDAP[@]}"
docHas ldap Deployment devpod-webui "- --ldap-auth=true"
docHas ldap Deployment devpod-webui "{name: ldap-secrets, mountPath: /etc/devpod/webui/ldap, readOnly: true}"
docHas ldap Deployment devpod-webui 'name: "ldap-bind"'
docHas ldap Deployment devpod-webui 'name: "ldap-ca"'
lacks ldap "--password-auth"
if helm template devpod "$CHART" --set webui.enabled=true --set webui.sessionKeySecret.name=sk --set webui.ldapAuth=true >/dev/null 2>&1; then
  echo "FAIL [ldap]: webui.ldapAuth rendered without gateway.ldap"; fail=1
fi

# snapshots (snapshot spec v2): webui flags from the snapshot values; the
# webui may read the snapshot Job's log.
render snapshot "${WEBUI[@]}" --set snapshot.enabled=true \
  --set 'snapshot.imageTemplate=reg.test/users/{user}/{name}:{tag}' --set snapshot.pushSecret=push --set snapshot.maxPerUser=5
docHas snapshot Deployment devpod-webui "- --snapshots=true"
docHas snapshot Deployment devpod-webui "- --snapshot-image-template=reg.test/users/{user}/{name}:{tag}"
docHas snapshot Deployment devpod-webui "- --snapshot-push-secret=push"
docHas snapshot Deployment devpod-webui "- --snapshot-max-per-user=5"
docHas snapshot Role devpod-webui 'resources: ["pods/log"]'
render snapshot-bare "${WEBUI[@]}" --set snapshot.enabled=true
docHas snapshot-bare Deployment devpod-webui "- --snapshots=true"
lacks snapshot-bare "--snapshot-image-template"

# CLI over SSH (docs/cli.md): the gateway calls the web UI's API with a
# kept shared key.
render cli "${WEBUI[@]}" --set gateway.cli.enabled=true --set gateway.cli.user=dp
docHas cli Secret devpod-cli-key '"helm.sh/resource-policy": keep'
docHas cli Deployment devpod-gateway "- --cli-user=dp"
docHas cli Deployment devpod-gateway "- --cli-api-url=http://devpod-webui.devpod-system.svc:80"
docHas cli Deployment devpod-gateway "- --cli-api-key-file=/etc/devpod/cli/key"
docHas cli Deployment devpod-gateway "- {name: cli-key, mountPath: /etc/devpod/cli, readOnly: true}"
docHas cli Deployment devpod-gateway "secretName: devpod-cli-key"
docHas cli Deployment devpod-webui "- --cli-api-key-file=/etc/devpod/cli/key"
docHas cli Deployment devpod-webui "secretName: devpod-cli-key"
lacks defaults "--cli-"
if helm template devpod "$CHART" --set gateway.cli.enabled=true >/dev/null 2>&1; then
  echo "FAIL [cli]: gateway.cli without webui.enabled rendered"; fail=1
fi

# HTTP proxy: DevPod ports as {id}-{port} hostnames; a kept key shared by
# the gateway and the webui, a Service and the wildcard route.
render http "${WEBUI[@]}" --set webui.baseURL=https://console.test --set gateway.http.enabled=true \
  --set gateway.http.baseDomain=apps.test --set gateway.http.suffix=-devpod \
  --set gateway.http.route.enabled=true --set gateway.http.route.gateway.name=gw --set gateway.http.route.httpsListener=https
docHas http Secret devpod-http-key '"helm.sh/resource-policy": keep'
docHas http Deployment devpod-gateway "- --http-listen=:8090"
docHas http Deployment devpod-gateway "- --http-base-domain=apps.test"
docHas http Deployment devpod-gateway "- --http-subdomain-suffix=-devpod"
docHas http Deployment devpod-gateway "- --http-auth-key-file=/etc/devpod/http/key"
docHas http Deployment devpod-gateway "- --http-login-url=https://console.test/http-auth"
docHas http Deployment devpod-gateway "- {name: http, containerPort: 8090}"
docHas http Deployment devpod-gateway "secretName: devpod-http-key"
docHas http Deployment devpod-webui "- --http-proxy-host={id}-{port}-devpod.apps.test"
docHas http Deployment devpod-webui "- --http-proxy-key-file=/etc/devpod/http/key"
docHas http Deployment devpod-webui "secretName: devpod-http-key"
docHas http Service devpod-gateway-http "targetPort: http"
docHas http HTTPRoute devpod-gateway-http '- "*.apps.test"'
docHas http HTTPRoute devpod-gateway-http "sectionName: https"

# web terminal (UI spec §5.12): a kept SSH key for the webui, wired into
# the Deployment; its public half becomes a trusted proxy on the gateway.
render webshell "${WEBUI[@]}" --set webui.webshell.enabled=true \
  --set-json 'gateway.trustedProxyKeys=[{"alias":"openng","pubkey":"ssh-ed25519 AAAAopenng"}]'
docHas webshell Secret devpod-webshell-key '"helm.sh/resource-policy": keep'
docHas webshell Secret devpod-webshell-key "id_ed25519: "
docHas webshell Deployment devpod-webui "- --webshell-key-file=/etc/devpod/webshell/id_ed25519"
docHas webshell Deployment devpod-webui "- --webshell-gateway=devpod-gateway.devpod-system.svc:22"
docHas webshell Deployment devpod-webui "secretName: devpod-webshell-key"
docHas webshell GatewayConfig default "alias: openng"
lacks webshell "alias: webui"
render webshellkey "${WEBUI[@]}" --set webui.webshell.enabled=true --set "webui.webshell.publicKey=ssh-ed25519 AAAAwebui" \
  --set-json 'gateway.trustedProxyKeys=[{"alias":"openng","pubkey":"ssh-ed25519 AAAAopenng"}]'
docHas webshellkey GatewayConfig default "alias: openng"
docHas webshellkey GatewayConfig default "alias: webui"
docHas webshellkey GatewayConfig default "pubkey: ssh-ed25519 AAAAwebui"

# (b) bundled Silo + ClickHouse.
render bundled "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true
has bundled "name: devpod-audit-s3"
has bundled "kind: StatefulSet"
has bundled "name: devpod-audit-clickhouse"
# the console and the bundled ClickHouse placed on chosen nodes
render placed "${WEBUI[@]}" --set audit.enabled=true --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true \
  --set-string webui.nodeSelector.storage=true --set-string audit.clickhouse.bundled.nodeSelector.storage=true \
  --set-json 'webui.tolerations=[{"key":"dedicated","operator":"Exists"}]'
docHas placed Deployment devpod-webui 'storage: "true"'
docHas placed Deployment devpod-webui "key: dedicated"
docHas placed StatefulSet devpod-audit-clickhouse 'storage: "true"'
lacks bundled "nodeSelector"

# the bundled store routed for gateways in other clusters: ClickHouse's
# HTTP port and S3, each at its hostname; the proxy's namespace admitted
render bundledroute "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true \
  --set audit.route.enabled=true --set audit.route.clickhouseHostname=ch.example.org --set audit.route.s3Hostname=s3.example.org \
  --set audit.route.gateway.name=gw --set audit.route.gateway.namespace=gw-ns --set audit.route.httpsListener=https
docHas bundledroute HTTPRoute devpod-audit-clickhouse "- ch.example.org"
docHas bundledroute HTTPRoute devpod-audit-clickhouse "port: 8123"
docHas bundledroute HTTPRoute devpod-audit-s3 "- s3.example.org"
docHas bundledroute HTTPRoute devpod-audit-s3 "sectionName: https"
docHas bundledroute NetworkPolicy devpod-audit-clickhouse "kubernetes.io/metadata.name: gw-ns"
docHas bundledroute NetworkPolicy devpod-audit-s3 "kubernetes.io/metadata.name: gw-ns"
lacks bundled "kind: HTTPRoute"
has bundled "name: devpod-audit-migrate"
has bundled '"helm.sh/hook": post-install,post-upgrade'
has bundled "- --create-bucket=true"
has bundled "- --manage-lifecycle=true"
has bundled "- --retention-days=180"
has bundled "- --audit-clickhouse-url=clickhouse://devpod-audit-clickhouse.devpod-system.svc:9000"
has bundled "- --audit-s3-endpoint=http://devpod-audit-s3.devpod-system.svc:9000"
# The spool budget leaves headroom below the emptyDir limit: kubelet counts
# disk blocks, the spool counts bytes, and exceeding sizeLimit evicts the pod.
has bundled "- --audit-spool-max-bytes=17408Mi"
# ~2 MiB per audited session (measured): 1Gi covers ~300 sessions on one
# replica, and GOMEMLIMIT (90%) makes the GC work before the OOM killer.
has bundled "memory: 1Gi"
has bundled "- {name: GOMEMLIMIT, value: 921MiB}"
has bundled "terminationGracePeriodSeconds: 300"
has bundled "sizeLimit: 20Gi"
has bundled "secretName: devpod-audit-ch-gateway"
has bundled "secretName: devpod-audit-ch-webui"
has bundled "secretName: devpod-audit-ch-migrate"
has bundled "GRANT INSERT ON devpod_audit.*"
# live session viewing (audit spec §16): a kept shared key, the gateway's
# internal endpoint, the webui relay and its EndpointSlice read access
docHas bundled Secret devpod-live-key '"helm.sh/resource-policy": keep'
docHas bundled Deployment devpod-gateway "- --audit-live-listen=:8082"
docHas bundled Deployment devpod-gateway "- --audit-live-key-file=/etc/devpod/live/key"
docHas bundled Deployment devpod-gateway "- {name: live, containerPort: 8082}"
docHas bundled Deployment devpod-gateway "secretName: devpod-live-key"
docHas bundled Deployment devpod-webui "- --audit-live-key-file=/etc/devpod/live/key"
docHas bundled Deployment devpod-webui "- --audit-live-gateway-namespace=devpod-system"
docHas bundled Deployment devpod-webui "secretName: devpod-live-key"
docHas bundled Role devpod-webui-live "endpointslices"
docHas bundled RoleBinding devpod-webui-live "name: devpod-webui-live"
has bundled "audit:"
has bundled "enabled: true"
has bundled "docker.io/pgsty/silo:RELEASE.2026-10-26T00-00-00Z"
has bundled "clickhouse/clickhouse-server:26.8.36.3"
has bundled "app.kubernetes.io/name: devpod-audit-migrate"
has bundled "resources: [\"gatewayconfigs\"]"

render small "${WEBUI[@]}" --set audit.enabled=true --set audit.spool.sizeLimit=2Gi \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true
has small "- --audit-spool-max-bytes=1740Mi"
has small "sizeLimit: 2Gi"

# (c) external storage: no StatefulSets, the given Secrets are mounted.
render external "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.endpoint=https://s3.example.com --set audit.s3.credentialsSecret=my-s3 \
  --set audit.clickhouse.url=clickhouse://ch.example.com:9000 \
  --set audit.clickhouse.credentialsSecrets.gateway=ch-gw \
  --set audit.clickhouse.credentialsSecrets.webui=ch-ui \
  --set audit.clickhouse.credentialsSecrets.migrate=ch-mig
lacks external "kind: StatefulSet"
has external "secretName: my-s3"
has external "secretName: ch-gw"
has external "secretName: ch-ui"
has external "secretName: ch-mig"
has external "- --audit-clickhouse-url=clickhouse://ch.example.com:9000"
has external "- --create-bucket=false"
has external "- --manage-lifecycle=false"

# (e) site settings a production install needs (were hand-edited on zjusct).
render site "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true \
  --set 'imagePullSecrets[0].name=harbor-pull' --set controller.homeHostPath=/tank/home \
  --set-json 'gateway.trustedProxyKeys=[{"alias":"openng","pubkey":"ssh-ed25519 AAAAtest openng"}]'
has site "- --home-host-path=/tank/home"
has site "trustedProxyKeys:"
has site "alias: openng"
has site "pubkey: ssh-ed25519 AAAAtest openng"
n=$(grep -c "name: harbor-pull" "$OUT/site.yaml" || true)
[ "$n" -ge 4 ] || { echo "FAIL [site]: imagePullSecrets on $n pod specs, want controller, gateway, webui, migrate"; fail=1; }
lacks defaults "imagePullSecrets"
lacks defaults "home-host-path"

# (f) webui behind a Gateway API HTTPRoute: hostnames[0] is served over the
# HTTPS listener; plain HTTP and the other hostnames 301 to it.
ROUTE=(--set webui.httpRoute.enabled=true --set webui.httpRoute.gateway.name=gw
  --set webui.httpRoute.gateway.namespace=gw-ns --set webui.httpRoute.httpsListener=https-wild
  --set webui.httpRoute.httpListener=http
  --set-json 'webui.httpRoute.hostnames=["devpod.example.org","devpod.alt.example.org"]')
render route "${WEBUI[@]}" --set webui.baseURL=https://devpod.example.org "${ROUTE[@]}"
n=$(grep -c "^kind: HTTPRoute" "$OUT/route.yaml" || true)
[ "$n" -eq 3 ] || { echo "FAIL [route]: $n HTTPRoutes, want 3 (backend, http redirect, alias redirect)"; fail=1; }
has route "sectionName: https-wild"
has route "sectionName: http"
has route "- devpod.alt.example.org"
has route "hostname: devpod.example.org"
has route "scheme: https"
has route "statusCode: 301"
has route "- --external-url=https://devpod.example.org"
lacks defaults "kind: HTTPRoute"
if helm template devpod "$CHART" "${WEBUI[@]}" "${ROUTE[@]}" >/dev/null 2>"$OUT/route-nourl.err"; then
  echo "FAIL [route]: rendered without webui.baseURL"; fail=1
fi

# (g) telemetry: the leader publishes itself behind a selectorless Service;
# the shared ingest key reaches the controller (tokens) and the webui.
AUDIT=(--set audit.enabled=true --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true)
render telemetry "${WEBUI[@]}" "${AUDIT[@]}" --set telemetry.enabled=true
has telemetry "name: devpod-webui-ingest"
svc=$(doc telemetry Service devpod-webui-ingest)
[ -n "$svc" ] || { echo "FAIL [telemetry]: no devpod-webui-ingest Service"; fail=1; }
if grep -q "selector:" <<<"$svc"; then echo "FAIL [telemetry]: the ingest Service must be selectorless"; fail=1; fi
grep -qF "name: ingest" <<<"$svc" || { echo "FAIL [telemetry]: ingest Service port must be named ingest (matches the EndpointSlice)"; fail=1; }
has telemetry "containerPort: 8081"
has telemetry "- --ingest-key-file=/etc/devpod/ingest/key"
has telemetry "- --leader-election-namespace=devpod-system"
has telemetry "- --ingest-service=devpod-webui-ingest"
has telemetry "- --idle-cpu-pct=5"
has telemetry "- --agent-silent-after=3m"
has telemetry "fieldPath: status.podIP"
# the leader decodes and writes every DevPod's telemetry: sized resources
docHas telemetry Deployment devpod-webui "cpu: \"2\""
docHas telemetry Deployment devpod-webui "memory: 1Gi"
docHas telemetry Deployment devpod-webui "- {name: GOGC, value: \"200\"}"
docHas telemetry Deployment devpod-webui "- {name: GOMEMLIMIT, value: 921MiB}"
docHas telemetry Role devpod-webui-leader "namespace: devpod-system"
docHas telemetry Role devpod-webui-leader 'resources: ["endpointslices"]'
docHas telemetry Role devpod-webui-leader 'resources: ["leases"]'
docHas telemetry RoleBinding devpod-webui-leader "name: devpod-webui"
docHas telemetry NetworkPolicy devpod-webui-ingest "port: 8081"
# hostNetwork DevPods post from their node's IP: node CIDRs are a knob
render telenodes "${WEBUI[@]}" "${AUDIT[@]}" --set telemetry.enabled=true --set-json 'telemetry.ingestFromCIDRs=["10.10.0.0/16"]'
docHas telenodes NetworkPolicy devpod-webui-ingest "cidr: 10.10.0.0/16"
docHas telemetry NetworkPolicy devpod-webui-ingest "kubernetes.io/metadata.name: devpods"
docHas telemetry NetworkPolicy devpod-webui-ingest "app.kubernetes.io/name: devpod-webui"
docHas telemetry Role devpod-webui 'verbs: ["create", "patch"]'
has telemetry "- --ingest-url=http://devpod-webui-ingest.devpod-system.svc:8081/ingest/v1/batch"
has telemetry "- --telemetry-metrics-interval=1s"
has telemetry "- --metrics-retention-1s-days=7"
has telemetry "- --process-retention-hours=48"
has telemetry "name: devpod-ingest-key"
lacks defaults "devpod-webui-ingest"
lacks defaults "--ingest-"
lacks defaults "devpod-ingest-key"
lacks defaults "name: GOGC"
lacks bundled "devpod-webui-ingest"
lacks bundled "--ingest-"
if helm template devpod "$CHART" "${WEBUI[@]}" --set telemetry.enabled=true >/dev/null 2>"$OUT/tele-noaudit.err"; then
  echo "FAIL [telemetry]: rendered without audit"; fail=1
elif ! grep -qF "audit.enabled" "$OUT/tele-noaudit.err"; then
  echo "FAIL [telemetry]: unhelpful error: $(cat "$OUT/tele-noaudit.err")"; fail=1
fi

# (d) `helm upgrade --reuse-values` from v0.1.x: the old release's values
# replace the chart defaults, so audit.* and gatewayConfigName are absent.
OLD="$OUT/chart-v0.1"
cp -R "$CHART" "$OLD"
cp "$ROOT/deploy/testdata/chart-values-v0.1.yaml" "$OLD/values.yaml"
if helm template devpod "$OLD" "${WEBUI[@]}" >"$OUT/reuse.yaml" 2>"$OUT/reuse.err"; then
  lacks reuse "--audit-"
  has reuse "- --gateway-config-name=default"
else
  echo "FAIL [reuse]: old values do not render: $(cat "$OUT/reuse.err")"; fail=1
fi
# Enabling audit on top of old values must say how to fix it.
if helm template devpod "$OLD" "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true >/dev/null 2>"$OUT/reuse-audit.err"; then
  echo "FAIL [reuse-audit]: rendered with incomplete audit values"; fail=1
elif ! grep -qF -- "--reset-then-reuse-values" "$OUT/reuse-audit.err"; then
  echo "FAIL [reuse-audit]: unhelpful error: $(cat "$OUT/reuse-audit.err")"; fail=1
fi

# node reservations: off by default; on, the controller runs the
# reservation timeline with the shared rules and may walk Reservations.
lacks defaults "--reservation"
lacks defaults '"reservations"'
render reservation "${WEBUI[@]}" --set reservation.enabled=true --set reservation.clearNotice=10m
docHas reservation Deployment devpod-controller "- --reservations=true"
docHas reservation Deployment devpod-controller "- --reservation-clear-notice=10m"
docHas reservation Deployment devpod-controller "- --reservation-ending-notice=30m,5m"
docHas reservation Deployment devpod-controller "- --reservation-retention=720h"
docHas reservation ClusterRole devpod-controller 'resources: ["reservations"]'
docHas reservation ClusterRole devpod-controller 'resources: ["reservations/status"]'
docHas reservation Deployment devpod-webui "- --reservations=true"
docHas reservation Deployment devpod-webui "- --reservation-granularity=30m"
docHas reservation Deployment devpod-webui "- --reservation-max-duration=24h"
docHas reservation Deployment devpod-webui "- --reservation-max-advance=336h"
docHas reservation ClusterRole devpod-webui 'resources: ["reservations"]'
docHas reservation ClusterRole devpod-webui 'verbs: ["patch"]'
lacks defaults "--reservation"

# Several clusters. Nothing of it renders unless asked for: a release of
# one cluster with its own console is what it was.
lacks defaults "devpod-agent"
lacks defaults "--clusters"
lacks defaults 'resources: ["clusters"'

# (hub) the console of several clusters: agents connect to every replica.
render hub "${WEBUI[@]}" --set webui.clusters.enabled=true
docHas hub Deployment devpod-webui "- --clusters=true"
docHas hub Deployment devpod-webui "- --instance-selector=app.kubernetes.io/name=devpod-webui"
docHas hub Deployment devpod-webui "- --instance-namespace=devpod-system"
docHas hub ClusterRole devpod-webui 'resources: ["clusters", "clusters/status"]'
docHas hub Role devpod-webui-instances 'resources: ["pods"]'
lacks hub "name: devpod-agent"

# (hub, telemetry, route) the other clusters' telemetry comes in at the
# console's address, to the leader's receiver, which admits the proxy
render hubtel "${WEBUI[@]}" "${AUDIT[@]}" "${ROUTE[@]}" --set webui.baseURL=https://devpod.example.org --set webui.clusters.enabled=true --set telemetry.enabled=true
docHas hubtel HTTPRoute devpod-webui "value: /ingest/v1/batch"
docHas hubtel HTTPRoute devpod-webui "name: devpod-webui-ingest"
docHas hubtel NetworkPolicy devpod-webui-ingest "kubernetes.io/metadata.name: gw-ns"
# one cluster: its DevPods reach the receiver inside, nothing outside does
render onetel "${WEBUI[@]}" "${AUDIT[@]}" "${ROUTE[@]}" --set webui.baseURL=https://devpod.example.org --set telemetry.enabled=true
lacks onetel "value: /ingest/v1/batch"
lacks onetel "kubernetes.io/metadata.name: gw-ns"
# (console with its own cluster's agent) the DevPods post to the receiver; with
# agent.telemetryOverLink to the agent, as any other cluster's do (DevPods on
# the host network, which the receiver's policy does not admit, reach it too).
# The receiver stays: every agent's telemetry comes to it.
HUBAGENT=(--set webui.clusters.enabled=true --set telemetry.enabled=true --set agent.enabled=true --set agent.cluster=st --set agent.console=http://devpod-webui.devpod-system.svc --set agent.joinToken=dpj_x)
render hubown "${WEBUI[@]}" "${AUDIT[@]}" "${HUBAGENT[@]}"
docHas hubown Deployment devpod-controller "- --ingest-url=http://devpod-webui-ingest.devpod-system.svc:8081/ingest/v1/batch"
lacks hubown "--telemetry-listen"
render hublink "${WEBUI[@]}" "${AUDIT[@]}" "${HUBAGENT[@]}" --set agent.telemetryOverLink=true
docHas hublink Deployment devpod-controller "- --ingest-url=http://devpod-agent-ingest.devpod-system.svc:8081/ingest/v1/batch"
docHas hublink Deployment devpod-agent "- --telemetry-listen=:8081"
docHas hublink Service devpod-agent-ingest "targetPort: telemetry"
docHas hublink Service devpod-webui-ingest "8081"
docHas hublink Deployment devpod-webui "- --ingest-listen=:8081"

# (member) a cluster managed from elsewhere: controller and agent, no
# console. The agent holds what the console may do here, and no more.
MEMBER=(--set agent.enabled=true --set agent.cluster=mulan --set agent.console=https://console.example.com --set agent.joinToken=dpj_x)
render member "${MEMBER[@]}" --set reservation.enabled=true
docHas member Deployment devpod-agent "- --cluster=mulan"
docHas member Deployment devpod-agent "- --console=https://console.example.com"
docHas member Deployment devpod-agent "- --namespace=devpod-system"
docHas member Deployment devpod-agent "- --join-token-file=/etc/devpod/agent/join/token"
docHas member Deployment devpod-agent "- --capabilities=reservations"
docHas member Deployment devpod-agent 'image: "ghcr.io/mrhaoxx/devpod-agent:dev"'
docHas member Deployment devpod-agent "serviceAccountName: devpod-agent"
docHas member Secret devpod-agent-join "token: \"ZHBqX3g=\""
docHas member Role devpod-agent 'resources: ["devpods", "devpodsnapshots"]'
docHas member ClusterRole devpod-agent 'resources: ["nodes"]'
# every pod, read only: the agent tells the console what other workloads hold per node
docHas member ClusterRole devpod-agent 'resources: ["pods"]'
docHas member ClusterRole devpod-agent 'resources: ["reservations"]'
# its own key: one Secret by name, and making it
docHas member Role devpod-agent-identity 'resourceNames: ["devpod-agent-identity"]'
docHas member Role devpod-agent-identity 'verbs: ["create"]'
# never Secrets at large, never the users' keys or passwords from elsewhere
if doc member ClusterRole devpod-agent | grep -q 'secrets'; then echo "FAIL [member]: the agent's ClusterRole names secrets"; fail=1; fi
if doc member Role devpod-agent | grep -q 'secrets'; then echo "FAIL [member]: the agent's Role in the DevPod namespace names secrets"; fail=1; fi
lacks member "name: devpod-webui"
docHas member Deployment devpod-controller "name: controller"
# an existing Secret instead of a token in the values
render membersecret --set agent.enabled=true --set agent.cluster=mulan --set agent.console=https://c.example.com --set agent.joinTokenSecret=my-join
docHas membersecret Deployment devpod-agent "secretName: my-join"
lacks membersecret "name: devpod-agent-join"
# once joined, no token at all: the agent proves its own key
render memberjoined --set agent.enabled=true --set agent.cluster=mulan --set agent.console=https://c.example.com
lacks memberjoined "--join-token-file"
lacks memberjoined "--capabilities"
for missing in "agent.console=https://c.example.com" "agent.cluster=mulan"; do
  if helm template devpod "$CHART" --set agent.enabled=true --set "$missing" >/dev/null 2>&1; then
    echo "FAIL: an agent without a cluster name or a console address rendered"; fail=1
  fi
done

# (member telemetry) DevPods of a managed cluster send telemetry to the
# agent, which passes it on; no receiver of its own, and the key is the
# console's (a Secret the operator brings).
# — over the agent's link: no address of the console's receiver to give
render membertel "${MEMBER[@]}" --set telemetry.enabled=true
docHas membertel Deployment devpod-agent "- --telemetry-listen=:8081"
lacks membertel "--telemetry-url"
docHas membertel Service devpod-agent-ingest "targetPort: telemetry"
docHas membertel Deployment devpod-controller "- --ingest-url=http://devpod-agent-ingest.devpod-system.svc:8081/ingest/v1/batch"
docHas membertel Deployment devpod-controller "- --ingest-key-file=/etc/devpod/ingest/key"
lacks membertel "name: devpod-webui-ingest"
lacks member "--telemetry-listen"
lacks member "devpod-agent-ingest"
if doc membertel Secret devpod-ingest-key | grep -q .; then echo "FAIL [membertel]: a member cluster made an ingest key of its own"; fail=1; fi

# (agent beside another controller) what the agent says its cluster can do is
# that controller's, when given, not what this release turns on
render agentcaps "${MEMBER[@]}" --set snapshot.enabled=true --set reservation.enabled=true --set 'agent.capabilities={}'
lacks agentcaps "--capabilities"
render agentcaps1 "${MEMBER[@]}" --set snapshot.enabled=true --set reservation.enabled=true --set 'agent.capabilities={reservations}'
docHas agentcaps1 Deployment devpod-agent "- --capabilities=reservations"
render agentcaps0 "${MEMBER[@]}" --set snapshot.enabled=true --set reservation.enabled=true
docHas agentcaps0 Deployment devpod-agent "- --capabilities=snapshots,reservations"

# (gateway hub) a gateway of several clusters: it asks the console and
# is reached by the clusters' agents.
lacks defaults "--console="
lacks defaults "devpod-gateway-agents"
HUBGW=(--set gateway.hub.enabled=true --set gateway.hub.name=b300 --set gateway.hub.console=https://console.example.com --set gateway.hub.joinToken=dpj_g)
render hubgw "${WEBUI[@]}" --set webui.clusters.enabled=true "${HUBGW[@]}"
# the agents' endpoint published for clusters outside this one
render hubgwroute "${WEBUI[@]}" --set webui.clusters.enabled=true "${HUBGW[@]}" --set gateway.hub.route.enabled=true \
  --set gateway.hub.route.hostname=gw.example.org --set gateway.hub.route.gateway.name=gw --set gateway.hub.route.gateway.namespace=gw-ns \
  --set gateway.hub.route.httpsListener=https
docHas hubgwroute HTTPRoute devpod-gateway-agents "- gw.example.org"
docHas hubgwroute HTTPRoute devpod-gateway-agents "name: devpod-gateway-agents"
docHas hubgwroute HTTPRoute devpod-gateway-agents "sectionName: https"
lacks hubgw "kind: HTTPRoute"
docHas hubgw Deployment devpod-gateway "- --console=https://console.example.com"
docHas hubgw Deployment devpod-gateway "- --gateway-name=b300"
docHas hubgw Deployment devpod-gateway "- --agent-listen=:8443"
docHas hubgw Deployment devpod-gateway "- --gateway-namespace=devpod-system"
docHas hubgw Deployment devpod-gateway "- --instance-selector=app.kubernetes.io/name=devpod-gateway"
docHas hubgw Deployment devpod-gateway "- --join-token-file=/etc/devpod/gateway/join/token"
docHas hubgw Deployment devpod-gateway "{name: agents, containerPort: 8443}"
docHas hubgw Service devpod-gateway-agents "targetPort: agents"
docHas hubgw Secret devpod-gateway-join "token: \"ZHBqX2c=\""
docHas hubgw Role devpod-gateway-hub 'resourceNames: ["devpod-gateway-identity"]'
docHas hubgw Role devpod-gateway-hub 'resources: ["pods"]'
# the console knows gateways when it manages clusters
docHas hubgw ClusterRole devpod-webui 'resources: ["gateways", "gateways/status"]'
docHas hub ClusterRole devpod-webui 'resources: ["gateways", "gateways/status"]'
for missing in "gateway.hub.console=https://c.example.com" "gateway.hub.name=b300"; do
  if helm template devpod "$CHART" --set gateway.hub.enabled=true --set "$missing" >/dev/null 2>&1; then
    echo "FAIL: a gateway of several clusters without a name or a console rendered"; fail=1
  fi
done

# A gateway under another name (beside an older install's devpod-gateway):
# every object, label, selector and reference of it follows the name
render gwname "${WEBUI[@]}" --set webui.clusters.enabled=true "${HUBGW[@]}" --set gateway.name=devpod-gateway-hub \
  --set webui.webshell.enabled=true --set gatewayConfigName=hub --set audit.enabled=true --set audit.clickhouse.bundled.enabled=true --set audit.s3.bundled.enabled=true
docHas gwname Deployment devpod-gateway-hub "app.kubernetes.io/name: devpod-gateway-hub"
docHas gwname Deployment devpod-gateway-hub "serviceAccountName: devpod-gateway-hub"
docHas gwname Deployment devpod-gateway-hub "secretName: devpod-gateway-hub-host-key"
docHas gwname Deployment devpod-gateway-hub "secretName: devpod-gateway-hub-internal-key"
docHas gwname Deployment devpod-gateway-hub "- --instance-selector=app.kubernetes.io/name=devpod-gateway-hub"
docHas gwname Deployment devpod-gateway-hub "- --gateway-identity-secret=devpod-gateway-hub-identity"
docHas gwname Service devpod-gateway-hub "app.kubernetes.io/name: devpod-gateway-hub"
docHas gwname Service devpod-gateway-hub-agents "app.kubernetes.io/name: devpod-gateway-hub"
docHas gwname ClusterRoleBinding devpod-gateway-hub "name: devpod-gateway-hub"
docHas gwname Role devpod-gateway-hub-secrets "devpod-gateway-hub-host-key"
docHas gwname Role devpod-gateway-hub-hub 'resourceNames: ["devpod-gateway-hub-identity"]'
docHas gwname GatewayConfig hub "name: devpod-gateway-hub-host-key"
docHas gwname Deployment devpod-webui "- --webshell-gateway=devpod-gateway-hub.devpod-system.svc:22"
docHas gwname NetworkPolicy devpod-audit-clickhouse "devpod-gateway-hub"
if grep -qE '(name|serviceAccountName): devpod-gateway$|app.kubernetes.io/name=devpod-gateway$|secretName: devpod-gateway-(host|internal)-key$' "$OUT/gwname.yaml"; then
  echo "FAIL [gwname]: something still named devpod-gateway"; fail=1
fi

# The agent's rights, as rendered, are what internal/e2e runs an agent
# with: a console that needs more than the chart grants fails there.
# UPDATE=1 rewrites the copy after a deliberate change.
RBAC="$ROOT/internal/e2e/testdata/agent-rbac.yaml"
helm template devpod "$CHART" --set agent.enabled=true --set agent.cluster=b --set agent.console=http://console \
  -s templates/agent-rbac.yaml >"$OUT/agent-rbac.yaml"
if [ "${UPDATE:-}" = 1 ]; then cp "$OUT/agent-rbac.yaml" "$RBAC"; fi
cmp -s "$OUT/agent-rbac.yaml" "$RBAC" || { echo "FAIL: internal/e2e/testdata/agent-rbac.yaml is not what the chart renders (UPDATE=1 bash deploy/test-chart.sh)"; fail=1; }

[ "$fail" -eq 0 ] && echo "chart: OK"
exit "$fail"
