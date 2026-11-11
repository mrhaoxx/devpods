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
| `POST /api/devpods/{name}/snapshots` | Save a running DevPod. `{name, tag?, pause?}` goes to the platform registry (the server builds the address); `{image, pushAuth: {username, password}, pause?}` goes to an address you name — credentials are required unless the address is your own place in the platform registry. `400 PUSH_AUTH_REQUIRED`, `409 NOT_RUNNING` / `IMAGE_EXISTS` / `SNAPSHOT_QUOTA` |
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
```

Errors are JSON: `{"code": "SCOPE", "message": "…", "detail": …}` with the
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
one or take it off the public list (`PATCH {"public": false}`), nothing else.

| Variable | Meaning |
|---|---|
| `${VAR}` | must be given |
| `${VAR:-default}` | optional; the default ends at the first `}` |
| `$${` | a literal `${` |
| `${OWNER}` | whoever launches (filled by the server) |
| `${NAME}` | the DevPod's full name: you give the short name, your prefix is added |
| `${NODE}` | a node; given as `nodes`, one DevPod per node |
| `${INDEX}` | the DevPod's number in the launch, from 1 (filled by the server) |

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

# launch: one DevPod per node, named <me>-sg-1, <me>-sg-2
curl … -d '{"vars":{"NAME":"sg"},"nodes":["m602","m603"]}' $DEVPOD/api/configs/star-sglang/launch
# a config without ${NODE} takes a count instead (default 1, at most 32)
curl … -d '{"vars":{"NAME":"w","IMAGE":"debian:12"},"count":3}' $DEVPOD/api/configs/star-plain/launch
```

Launching needs `configs:read` and `devpods:create`. Each DevPod is created
exactly as `POST /api/devpods` with that YAML would create it, as you: the same
quota, node and manifest rules. With several, the name gets `-<index>` unless
the config uses `${INDEX}` itself.

The launch is checked as a whole first. The answer is `{"items": [{index, name,
node, ok, code, message}]}` with:

| Status | Meaning |
|---|---|
| `200` | every DevPod was created (or, with `"dryRun": true`, would be — each item then carries its `yaml`) |
| `422` `LAUNCH_REJECTED` | at least one could not be created; **nothing was**. The failing items say why (`QUOTA_EXCEEDED`, `NAME_TAKEN`, `DUPLICATE_NAME`, `BAD_NAME`, `NODE_RESERVED`, `BAD_REQUEST` for YAML the values broke, …) and carry their `yaml` |
| `207` | the check passed but a write failed meanwhile; the others were created and are not undone |
| `400` `BAD_VARIABLES` | a variable is missing or unknown (`detail` names them), or a value is not single-line text |

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
