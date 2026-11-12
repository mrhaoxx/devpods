# DevPod REST API

The console's API is open to scripts and agents through **personal access
tokens**. The machine-readable index — every endpoint, a one-line summary and
the scope it needs (`x-scope`) — is served at `GET /api/openapi.json`.

## Tokens

Create one in the console: 设置 → 访问令牌. The token (`dpt_<id>_<secret>`) is
shown once; the server keeps only a SHA-256 of the secret.

```sh
export DEVPOD=https://devpod.example.com
export TOKEN=dpt_…
curl -H "Authorization: Bearer $TOKEN" $DEVPOD/api/devpods
```

A token acts as the user who created it, narrowed three ways:

| Control | Effect |
|---|---|
| **Scopes** | Each endpoint needs one scope; anything not ticked is refused with `403 SCOPE`. |
| **DevPod list** (optional) | The token only sees and touches the DevPods named. It then cannot create DevPods or call endpoints that do not belong to one DevPod (audit, keys, …). |
| **Expiry** | 7 days to never; an expired token answers `401`. |

Revoking a token in the console takes effect at once.

### Scopes

| Scope | Allows |
|---|---|
| `devpods:read` | List and read DevPods, their events and status stream |
| `devpods:create` | Create a DevPod |
| `devpods:power` | Start, stop (hibernate), redeploy |
| `devpods:update` | Change image, CPU / memory / GPU, shell, collaborators |
| `devpods:delete` | Delete a DevPod |
| `metrics:read` | Live and historical metrics, processes, container facts |
| `shell` | Open the web terminal (WebSocket) |
| `http:read` / `http:write` / `http:connect` | See port links / make ports public, reset the address / get a ticket for a private port |
| `reservations:read` / `reservations:write` | See the node reservation timeline / reserve, move, extend, cancel |
| `configs:read` / `configs:write` | See saved configs (mine, shared with me, public) / save, change, share, delete them |
| `audit:read` | Sessions, commands, file records, replays (your own) |
| `snapshots:read` / `snapshots:write` | See saved images and follow a save / save a DevPod as an image, cancel, forget |
| `keys:read` / `keys:write` | SSH public keys |
| `registries:read` / `registries:write` | See the image registries you saved credentials for / add, replace, remove them (passwords are never returned) |
| `admin:read` | Act as admin for reads (everyone's DevPods, nodes, users, audit). Admins only |
| `admin:write` | Act as admin for changes (other users' DevPods, users, policies, disconnect). Admins only |

An admin's token **without** admin scopes behaves like an ordinary user's
token. `PATCH /api/devpods/{name}` needs `devpods:power` for `{"running": …}`
and `devpods:update` for anything else. Tokens can never change the password
or manage tokens.

## Saving a DevPod as an image

Scopes: `snapshots:read` (list, read, follow progress) and `snapshots:write`
(save, cancel, forget).

| Call | What it does |
|---|---|
| `POST /api/devpods/{name}/snapshots` | Save a running DevPod. `{name, tag?, pause?}` goes to the platform registry (the server builds the address); `{image, pushAuth?: {username, password}, pause?}` goes to an address you name. It is pushed with `pushAuth`; without it, with the credential you saved for that registry; without either, with no credentials at all (a registry that takes pushes without a login) — the platform's own credentials are only ever used for your place in the platform registry. `400 PUSH_AUTH_REQUIRED` (half a credential), `409 NOT_RUNNING` / `IMAGE_BUSY` / `SNAPSHOT_QUOTA` |
| `GET /api/snapshots[?devpod=][&scope=all]` | Your snapshots (`scope=all`: everyone's, admins) |
| `GET /api/snapshots/{id}` | One snapshot: `{id, devpod, owner, image, phase, digest, sizeBytes, message, customRegistry, createdAt, startedAt, completedAt}` |
| `GET /api/snapshots/{id}/progress` | Server-sent events until it ends; each `data:` is `{phase, stage, elapsedSeconds, sizeBytes?, digest?, message?, lines}` with `stage` one of `pending`, `commit`, `push`, `done`, `error`, and `lines` the last lines of the job's output |
| `DELETE /api/snapshots/{id}` | Cancel a running snapshot, or forget a finished one (the image stays in the registry) |

The image holds the container's own filesystem, not its persistent volume
(usually the home directory). Credentials sent in `pushAuth` are used for that
one push and are never returned by the API.

```sh
ID=$(curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"env","tag":"v1"}' $DEVPOD/api/devpods/alice-train/snapshots | jq -r .id)
curl -N -H "Authorization: Bearer $TOKEN" $DEVPOD/api/snapshots/$ID/progress
```

## Common calls

```sh
# list
curl -H "Authorization: Bearer $TOKEN" $DEVPOD/api/devpods

# create
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"train","image":"ubuntu:24.04","cpu":"4","memory":"8Gi"}' $DEVPOD/api/devpods

# stop / start
curl -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"running":false}' $DEVPOD/api/devpods/alice-train

# delete
curl -X DELETE -H "Authorization: Bearer $TOKEN" $DEVPOD/api/devpods/alice-train

# the manifest as YAML, and saving an edited one back (the spec is replaced;
# name and owner cannot change; the rules are those of creating from YAML)
curl -H "Authorization: Bearer $TOKEN" $DEVPOD/api/devpods/alice-train/yaml          # → {"yaml": "…", "running": true}
curl -X PUT … -d "$(jq -Rs '{yaml: .}' train.yaml)" $DEVPOD/api/devpods/alice-train/yaml   # → {"devpod": …, "rebuild": true}

# the Pod behind it, as `kubectl describe pod` tells it: conditions, each
# container's state and why, volumes ({"pod": null} when stopped)
curl -H "Authorization: Bearer $TOKEN" $DEVPOD/api/devpods/alice-train/pod
```

`cpu` and `memory` are optional. A DevPod created here is named
`<you>-<name>`; DevPods made with kubectl keep whatever name they have.

Errors are JSON: `{"code": "SCOPE", "message": "…", "detail": …}` (when the
cluster refuses a change, the message says why: `update rejected: <reason>`) with the
matching HTTP status (`401` bad or expired token, `403` scope or permission,
`404` not found or not yours, `409` conflict / quota).

## Nodes and reservations

`GET /api/nodes` (scope `devpods:read`) lists the nodes a DevPod can be pinned
to: capacity, whether the node is open for reservation, who holds it now
(`held`), the next reservation by someone else (`next`) and whether your own
DevPods may go there (`selectable`). Pin with `"node"` on create or
`PATCH /api/devpods/{name} {"node": "<name>"}` (`""` unpins). A node someone
else holds answers `409 NODE_RESERVED` with `detail {node, user, until}`.

```sh
# what would a reservation do? (nothing is created)
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"node":"g206","start":"2026-11-12T06:00:00Z","end":"2026-11-12T10:00:00Z"}' \
  "$DEVPOD/api/reservations?dryRun=true"        # → {"willHibernate": 2}

# reserve, extend a started one, release
curl … -d '{"node":"g206","start":"…","end":"…"}' $DEVPOD/api/reservations
curl -X PATCH … -d '{"end":"2026-11-12T12:00:00Z"}' $DEVPOD/api/reservations/<id>
curl -X DELETE … $DEVPOD/api/reservations/<id>
```

Times are RFC 3339 and must fall on the granularity from
`GET /api/reservations/rules`. A period that overlaps someone else's answers
`409 RESERVATION_CONFLICT` with the winner in `detail`.

## Saved configs

A config is a DevPod manifest kept as text, with variables. Its name is
`<owner>-<id>`. The owner changes it; users it is shared with (`sharedWith`) and
— when it is `public` — every signed-in user may read it and launch from it.
A config you may not see answers `404`. Admins see every config and may delete
one, take it off the public list (`PATCH {"public": false}`) or mark a public
one official (`PATCH {"official": true}`; official configs are listed first,
`?scope=official` lists them alone), nothing else. Taking a config off the
public list also takes away its official mark. A config's text is at most
256 KiB.

| Variable | Meaning |
|---|---|
| `${VAR}` | must be given |
| `${VAR:-default}` | optional; the default ends at the first `}` and is also used for an empty value |
| `$${` | a literal `${` |
| `${OWNER}` | whoever launches (filled by the server) |
| `${NAME}` | the DevPod's full name: you give the short name, your prefix is added |
| `${NODE}` | a node; given as `nodes`, one DevPod per node |
| `${INDEX}` | the DevPod's number in the launch, from 0 (filled by the server) |
| `${COUNT}` | how many the launch creates (filled by the server) |
| `${LEADER}` | the full name of number 0 — every DevPod reaches the others by name, so this is a `MASTER_ADDR` (filled by the server) |

A name is `[A-Z][A-Z0-9_]*`; any other `${…}` (a shell's `${home}`) is left as
it is. Values are single-line text of at most 1024 bytes.

```sh
# save (id + title + the YAML), share, publish
curl … -d '{"id":"sglang","title":"SGLang 8 GPUs","content":"apiVersion: …"}' $DEVPOD/api/configs
curl -X PATCH … -d '{"sharedWith":["alice","bob"]}' $DEVPOD/api/configs/star-sglang
curl -X PATCH … -d '{"public":true}' $DEVPOD/api/configs/star-sglang

# list (scope=mine|shared|public; default: all three), read one with its variables
curl … "$DEVPOD/api/configs?scope=public"
curl … $DEVPOD/api/configs/star-sglang

# launch: one DevPod per node, named <me>-sg-0, <me>-sg-1
curl … -d '{"vars":{"NAME":"sg"},"nodes":["m602","m603"]}' $DEVPOD/api/configs/star-sglang/launch
# a config without ${NODE} takes a count instead (default 1, at most 32)
curl … -d '{"vars":{"NAME":"w","IMAGE":"debian:12"},"count":3}' $DEVPOD/api/configs/star-plain/launch
```

Launching needs `configs:read` and `devpods:create`. Each DevPod is created
exactly as `POST /api/devpods` with that YAML would create it, as you: the same
quota, node and manifest rules. With several, the name gets `-<index>` unless
the config uses `${INDEX}` itself.

Each DevPod records where it came from: the label `devpod.io/config` (the
config's name) and the annotations `devpod.io/config-hash` (of the text it was
made from) and `devpod.io/config-vars` (the values given, and `NAME`'s). Several
at once form a group: the label `devpod.io/group` (`<you>-<short name>`, or the
name of number 0 when the config has no `${NAME}`) and the annotations
`devpod.io/group-index`, `devpod.io/group-size` and `devpod.io/group-leader`.
The answer then also carries `"group"`. A launch whose group you still have
DevPods of is refused (`GROUP_TAKEN`).

A config may say what its variables are (`"variables": [{name, description?,
type?, options?}]` when saving): `type` is `string` (the default), `int`,
`choice` (one of `options`), `node` or `image`. A value that is not of its
type fails the launch with `BAD_VALUE`.

The launch is checked as a whole first. The answer is `{"items": [{index, name,
node, ok, code, message}]}` with:

| Status | Meaning |
|---|---|
| `200` | every DevPod was created (or, with `"dryRun": true`, would be — each item then carries its `yaml`) |
| `422` `LAUNCH_REJECTED` | at least one could not be created; **nothing was**. The failing items say why (`QUOTA_EXCEEDED`, `NAME_TAKEN`, `DUPLICATE_NAME`, `BAD_NAME`, `BAD_VALUE`, `GROUP_TAKEN`, `NODE_RESERVED`, `BAD_REQUEST` for YAML the values broke, …) and carry their `yaml` |
| `207` | the check passed but a write failed meanwhile; the others were created and are not undone |
| `400` `BAD_VARIABLES` | a variable is missing or unknown (`detail` names them), or a value is not single-line text |

### Checking a config before saving it

`POST /api/configs:check` with `{content, variables?, vars?}` (scope
`configs:write`) renders the text with example values — the ones given in
`vars`, else the text's defaults, else one of each variable's type — and puts
it through what you may write in a DevPod: fields that exist, nothing that
reaches Secrets or a service account token, no Kore annotations unless you are
an admin, a valid name. It answers `200` with `{"problems": [{line?, code,
message}]}` (`BAD_VARIABLES`, `BAD_YAML`, `FORBIDDEN_CONTENT`, `BAD_NAME`); an
empty list means none. Quota, free nodes and taken names are the launch's to
check.

### Rebuilding a DevPod from its config

`POST /api/devpods/{name}/rebuild-from-config` (scopes `devpods:update` and
`configs:read`) renders the config the DevPod came from, as it is now, with the
values recorded at launch (its group number, size and leader too), and saves it
the way `PUT /api/devpods/{name}/yaml` does: the same checks, and a running
DevPod's Pod is recreated if its definition changes. `{"dryRun": true}` only
answers `{yaml, rebuild}`. `409` with `CONFIG_GONE` (no config, or it is no
longer yours to see), `CONFIG_VARS` (the recorded values no longer fit the
config's variables; `detail` names them) or `CONFIG_RENAMES` (the config now
gives another name). `GET /api/devpods/{name}` says `configOutdated: true` when
the config has changed since — only when you may see that config.

## Several clusters

A console can manage several clusters (`GET /api/clusters` lists them: name,
whether its agent is connected, what it offers). Users, tokens and saved
configs belong to the console; DevPods, nodes, reservations and snapshots
belong to a cluster, and a request says which:

```sh
# any path, in one cluster — or the plain path with ?cluster=
curl -H "$auth" $base/api/clusters/mulan/devpods
curl -H "$auth" "$base/api/nodes?cluster=mulan"
curl -H "$auth" -X POST $base/api/clusters/mulan/devpods -d '{"name":"x","image":"ubuntu:24.04"}'
curl -H "$auth" -X POST $base/api/clusters/mulan/configs/me-sg/launch -d '{"vars":{"NAME":"sg"}}'
```

- Lists without a cluster cover every cluster, each item carrying `cluster`:
  `GET /api/devpods` (and its `?watch=true` stream, whose events carry
  `cluster`; and the admin list, live rows and search), `GET /api/nodes`,
  `GET /api/admin/nodes`, `GET /api/reservations` and `GET /api/snapshots`.
  The `pods` of the DevPod list are keyed `<cluster>/<name>`. The live
  overview's nodes and attention items carry `cluster` too, and
  `GET /api/clusters/<c>/admin/live/overview` answers that cluster's own
  numbers (KPI, users, nodes, attention). Narrowing to
  one cluster is the caller's choice: name it in the path, or filter the
  items (the console does the latter, with its cluster filter).
- A path naming a DevPod (`/api/devpods/{name}…`) without a cluster finds it
  among your DevPods of every cluster. The same name in two clusters is
  `409 AMBIGUOUS_CLUSTER`, with `detail: [{cluster, name}, …]`.
- Anything else that is a cluster's and was not told one is
  `400 CLUSTER_REQUIRED`, with the cluster names in `detail`.
- Quotas are counted per cluster.
- While a cluster's agent is away, reads answer its last known state with the
  header `X-DevPod-Stale-Since` (the list also has `stale: {cluster: since}`),
  and changes are `503 CLUSTER_OFFLINE`.

With one cluster registered, or a console that manages only the cluster it
runs in, none of this is needed: the plain paths mean that cluster.

Admins register a cluster with `POST /api/admin/clusters {name, displayName?}`,
which answers a join token once (and the command to run in the new cluster);
`POST /api/admin/clusters/{name}/token` issues a new one and withdraws the
agent that had joined; `PATCH` takes `displayName`, `paused`, `defaultQuota`;
`DELETE` stops managing it (its DevPods stay).

### One gateway for several clusters

A gateway can serve every cluster the console manages (`GET /api/gateways`:
where to connect by SSH, the port-link domain). Admins register one with
`POST /api/admin/gateways {name, displayName?, sshAddress?, httpBaseDomain?,
httpSuffix?, httpPort?, agentAddress?}` (a join token, once, as for a cluster),
edit it with `PATCH` (the same fields, and `paused`), and point a cluster at it
with `PATCH /api/admin/clusters/{name} {gateway}`; a cluster that names none
uses the only gateway there is, or the console's own settings.

The login on such a gateway is `<user>+<devpod>[+<cluster>]`. The cluster can
always be written, and has to be when two clusters have a DevPod of that name
you may log in to — the gateway then says so, once your key was accepted.
`GET /api/me` gives, per cluster (`clusters.<name>.ssh`), the address and the
cluster to put in the login; port links (`…/http`) are on that gateway's domain.

## Without a token: the CLI over SSH

`ssh <user>+devpod@<gateway> <command>` runs the same operations authenticated by
your SSH key — see [cli.md](cli.md).

## Registry credentials

Save credentials for an image registry once; they are then used to pull
private images into your DevPods and to push snapshots to that registry.

| Call | Notes |
|---|---|
| `GET /api/me/registries` | `{"items":[{"host","username","pending"}]}`; `pending` = not stored yet |
| `PUT /api/me/registries/{host}` | `{"username","password"}`; `host` is `registry[:port]`. Answers the list |
| `DELETE /api/me/registries/{host}` | 204; 404 when not saved |

Docker Hub is one registry: `docker.io`, `index.docker.io` and
`registry-1.docker.io` all name it. Credentials reach Pods created after
they are saved; running DevPods are not restarted. A token limited to
specific DevPods cannot use these calls.

A DevPod written as YAML by a non-admin cannot refer to Secrets (volumes,
environment, image pull secrets — `400 SECRET_REFERENCE` with the field
path) or use a service account token (`400 SERVICE_ACCOUNT`).
