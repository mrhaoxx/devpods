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
| `audit:read` | Sessions, commands, file records, replays (your own) |
| `keys:read` / `keys:write` | SSH public keys |
| `admin:read` | Act as admin for reads (everyone's DevPods, nodes, users, audit). Admins only |
| `admin:write` | Act as admin for changes (other users' DevPods, users, policies, disconnect). Admins only |

An admin's token **without** admin scopes behaves like an ordinary user's
token. `PATCH /api/devpods/{name}` needs `devpods:power` for `{"running": …}`
and `devpods:update` for anything else. Tokens can never change the password
or manage tokens.

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

## Without a token: the CLI over SSH

`ssh devpod@<gateway> <command>` runs the same operations authenticated by
your SSH key — see [cli.md](cli.md).
