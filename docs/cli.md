# CLI over SSH

`ssh <user>+devpod@<gateway> <command>` manages DevPods from a terminal, a script or an agent. There is nothing to install and no token to keep: the gateway knows who you are from your SSH key and runs each command against the console's REST API as you, so the CLI, the console and API tokens follow the same rules (ownership, quota, admin-only operations).

```console
$ ssh alice+devpod@gateway.example.com list
NAME      OWNER  PHASE    NODE  IMAGE         CPU  MEMORY  GPU  AGE
alice-a   alice  Running  g206  ubuntu:24.04  4    8Gi     -    3d
alice-b   alice  Stopped  -     pytorch:2     8    32Gi    1    12d
```

With a terminal and no command (`ssh alice+devpod@gateway`) you get a prompt (`devpod>`) that takes the same commands; `exit` leaves.

## Who you are

The login is `<user>+devpod` — the same shape as a DevPod login (`<user>+<devpod>`), with the reserved name `devpod` where the DevPod's name would be. The key you offer must belong to that user; identity sources are checked in the usual order (User, then LDAP), exactly as for a DevPod login.

A trusted SSH front proxy uses the same form: the name is how it says whom it acts for, and the user must exist.

The login name `devpod` is configurable (`--cli-user`, chart value `gateway.cli.user`). Whatever it is, a DevPod with that short name cannot be reached by SSH while the CLI is on.

## Commands

| Command | What it does |
|---|---|
| `help [command]` | The command list, or one command's help. |
| `whoami` | Your user, admin flag, usage and quota. |
| `list` (`ls`) `[--all]` | Your DevPods; `--all` lists everyone's (admin). |
| `show <name>` (`status`, `get`) | One DevPod in detail. |
| `create <name> --image IMG [--cpu N] [--memory SIZE] [--gpu N] [--template T] [--shell S] [--collab a,b]` | Create `<you>-<name>`. Non-admins must set `--cpu` and `--memory` (or use a template that does). |
| `start <name>…` / `stop <name>…` | Wake / hibernate. The home volume stays. |
| `redeploy <name>…` | Recreate the Pod (sessions drop, the home volume stays). |
| `delete <name>… --yes` (`rm`) | Delete DevPods and their data. `--yes` is required. |
| `set <name> [--image IMG] [--cpu N] [--memory SIZE] [--gpu N] [--shell S]` | Change a DevPod; a running one is rebuilt. |
| `collab <name> [add\|remove\|set user…]` | Show or change collaborators. |
| `ports <name>` | The HTTP address pattern and each access level's ports. |
| `ports <name> url <port>` | One port's address. |
| `ports <name> users <port>…` | Open ports to anyone signed in to the console. |
| `ports <name> public <port>…` / `private <port>…` | Open a port to anyone with the link, or back to owner, collaborators and admins only. |
| `ports <name> reset` | Draw a new random address; every old link stops working. |
| `ssh <name>` | Print the `ssh` command that logs into the DevPod. |
| `templates` | List DevPod templates. |
| `nodes`, `users` | List nodes / users (admin). |
| `migrate <name> <node\|->` | Pin a DevPod to a node; `-` lets the scheduler choose again (admin). |
| `snapshot <devpod> [--name N] [--tag T] [--no-pause] [--detach]` | Save the DevPod's container as an image and follow the progress. **Ctrl-C or disconnecting cancels it**; `--detach` starts it and returns. See [Snapshots](#snapshots). |
| `snapshot <devpod> --image REF [--username U] [--password-stdin]` | The same, to an image address of your choice with that registry's credentials. |
| `snapshots` (`images`) `[--all] [--devpod D]` | List snapshots; `--all` lists everyone's (admin). |
| `snapshot show <id>` / `follow <id>` / `cancel <id>` / `rm <id>` | One snapshot in detail / re-attach to its progress / stop one in progress / remove a finished one's record. |
| `registry` (`registries`) `[list]` | Your saved registry logins (host, user name, whether it has taken effect yet). |
| `registry add <host> [--username U] [--password-stdin]` | Save or replace the login for one registry. The password is asked for without echo, or read from standard input; never on the command line. |
| `registry rm <host>` | Remove a saved login. |
| `config` (`configs`) `[list] [--mine\|--shared\|--public]` | Saved configs you may use: your own, those shared with you, the public ones. See [Saved configs](#saved-configs). |
| `config show <name>` | A config's details, variables and YAML. |
| `config save <id> [--title T] [--description D] [--public]` | Save the YAML on standard input as `<you>-<id>`; an existing one gets the new YAML. |
| `config share <name> [user…]` / `config public <name> on\|off` | Who may use it besides you (no user: nobody) / open it to everyone signed in. |
| `config delete <name>` | Delete a config. |
| `config launch <name> [--set VAR=value]… [--node N]… [--count N] [--dry-run]` | Create DevPods from a config, one per `--node` or `--count` of them. |

`<name>` is the DevPod's full name as `list` shows it (`alice-a`), except in `create`, where it is the part after your user name.

Commands that take several names (`start`, `stop`, `redeploy`, `delete`) try every one and report each; one failure does not stop the rest.

The command line is split like a shell splits plain words and quotes (`'…'`, `"…"`, `\`), and nothing is expanded: no variables, globs, pipes or substitutions run on the gateway.

## Snapshots

`snapshot <devpod>` commits the DevPod's container to an image, pushes it, and shows what is happening until it ends:

```
$ ssh -t alice+devpod@gateway snapshot alice-train1
快照 snap-7f3k → harbor.example.com/devpod-users/alice/train1:20261111-1530
committing container 3f9c…
layer sha256:1c2d… pushed
[推送镜像] 48s 1.2 GiB            ← redrawn in place on a terminal
完成：harbor.example.com/devpod-users/alice/train1:20261111-1530@sha256:ab12…（1.2 GiB，50s）
```

- **Stages**: 等待开始 → 提交容器 → 推送镜像, then 完成 or 失败. On a terminal (`ssh -t`) one status line with the stage, the elapsed time and the image size is redrawn in place, with the tools' log lines above it. Without a terminal each stage is printed once and log lines are appended; there are no control characters.
- **Cancelling**: while the command follows a snapshot it started, **Ctrl-C cancels the snapshot**, and so does an SSH signal or the connection going away (closing your laptop cancels it). The command prints `已取消` and exits 130. Closing standard input alone (`ssh -n`, an agent without stdin) is not a disconnect. To start a snapshot that survives you leaving, use `--detach`.
- **Re-attaching**: `snapshot follow <id>` shows a running snapshot again. There Ctrl-C only stops following; `snapshot cancel <id>` stops the snapshot.
- **cancel vs rm**: `cancel` only acts on a snapshot in progress and `rm` only on a finished one (it removes the record; the image stays in the registry), so neither can do the other's job by accident.
- **What is in the image**: the container's own filesystem. Volumes — the home directory — are not. The container is paused while it is committed (seconds to tens of seconds); `--no-pause` skips that at the risk of an inconsistent image.
- **Names**: `--name` defaults to the DevPod's name without your user-name prefix, `--tag` to a timestamp. The words `create`, `show`, `follow`, `cancel` and `rm` after `snapshot` are sub-commands; a DevPod that has one of those names is reached with `snapshot create <devpod>`.
- **Your own image address**: `--image registry/path:tag` pushes somewhere other than your default address. That needs the registry's credentials: give `--username`; the password is asked for without echo on a terminal, or read from the first line of standard input (`--password-stdin` forces that). There is no `--password` flag: the password never goes on a command line, is not logged and never appears in the output. If you leave the credentials out and the address needs them, a terminal is asked for them; without a terminal the command fails with `PUSH_AUTH_REQUIRED`.
- **Giving up following**: if no progress arrives for 60 s, or after 60 minutes, the command stops following (exit 1) and says how to re-attach. The snapshot itself goes on.

## Output and exit status

- Default output is text: aligned tables and `key: value` lines on stdout.
- `--json` (or `-o json`) on any command prints the REST API's JSON answer on stdout, unchanged. Commands that take several names print `[{"name": …, "ok": true}, {"name": …, "ok": false, "code": …, "error": …}]`. `ports … url` prints `{"url", "port", "public"}`; `ssh` prints `{"command", "login", "host"}`.
- Errors go to stderr as `error: <message> (<CODE>)`, where `<CODE>` is the API's error code (`NOT_FOUND`, `FORBIDDEN`, `QUOTA_EXCEEDED`, …).
- Exit status: `0` success, `1` the API refused or failed (for several names: at least one failed; for a snapshot: it failed or could no longer be followed), `2` the command line is wrong (nothing was sent), `130` you interrupted a snapshot follow.
- Following a snapshot with `--json` prints one JSON object per line: the API's progress object (`phase`, `stage` = `pending` / `commit` / `push` / `done` / `error`, `elapsedSeconds`, `sizeBytes`, `digest`, `message`, `lines`) plus `id` and `image`. The last line has `stage` `done` (with `digest`) or `error` (with `message`). `snapshot … --detach --json` prints the snapshot itself.


## Registry logins

A saved login for an image registry does two things: it lets the DevPods you start **pull private images** from that registry, and it is what `snapshot --image` **pushes** with when the address is on that registry — no credentials to type each time.

```console
$ ssh -t alice+devpod@gateway registry add harbor.example.com
仓库用户名: bob
仓库密码（不回显）:
saved the login for harbor.example.com (user bob)

$ ssh alice+devpod@gateway registry list
HOST                USERNAME  STATUS
harbor.example.com  bob       ready

# without a terminal (scripts, agents): the password is the first line of standard input
$ printf '%s\n' "$REGISTRY_PASSWORD" | ssh alice+devpod@gateway registry add harbor.example.com --username bob --json
{"host":"harbor.example.com","saved":true,"username":"bob"}
```

- **The password** is never an argument (there is no `--password`), is not logged and is not shown again. `--password-stdin` forces reading it from standard input even on a terminal and needs `--username`.
- **The host** is `registry[:port]`; a pasted `https://…/path` is reduced to the host.
- **Replacing**: `registry add` for a host that is already saved replaces its login.
- **`pending`** in the list means the login has been accepted but is not written to the cluster yet; it turns `ready` within seconds.
- **Effect**: adding or removing a login never restarts a running DevPod; it applies to DevPods started afterwards.
- When `snapshot --image` answers that the address needs credentials (`PUSH_AUTH_REQUIRED`), the message names the `registry add <host>` that would save them.

## Saved configs

A config is a DevPod manifest saved as text, with variables — `${NAME}`, `${NODE}`, `${OWNER}`, `${INDEX}` and any `${VAR}` or `${VAR:-default}` of your own (the full rules are in [the API notes](api.md#saved-configs)). You save it once and create DevPods from it; you can share it with named users or make it public. Whoever launches a config creates DevPods **as themselves**: their quota, their node rules.

```console
$ ssh alice+devpod@gateway config save sglang --title "SGLang 8 GPUs" < sglang.yaml
saved alice-sglang (3 variables)

$ ssh alice+devpod@gateway config share sglang bob carol
alice-sglang is shared with bob, carol

$ ssh alice+devpod@gateway config list
NAME          OWNER  ACCESS    VARIABLES        TITLE
alice-sglang  alice  shared:2  NAME,NODE,IMAGE  SGLang 8 GPUs
bob-k3s       bob    public    NAME             k3s

# one DevPod per node, named alice-sg-1 and alice-sg-2
$ ssh alice+devpod@gateway config launch sglang --set NAME=sg --node m602 --node m603
ok   alice-sg-1  m602
ok   alice-sg-2  m603
```

- `<name>` is the config's full name as `list` shows it (`bob-k3s`); your own may be given without your user name (`sglang`).
- `--set NAME=…` is the part of the DevPod's name after your user name. `OWNER` and `INDEX` are filled in for you; nodes are given with `--node`, not `--set NODE=`.
- A config with `${NODE}` creates one DevPod per `--node`, in the order given. One without takes `--count N` (default 1, at most 32). Several get `-1`, `-2`, … after the name, unless the config places `${INDEX}` itself.
- The launch is checked as a whole first. If any DevPod cannot be created — quota, a name already taken, a reserved node, a value that breaks the YAML — **none is**: each line says `pass` or `FAIL … CODE: message`, and the exit status is `1`.
- If the check passed and a write still failed (someone took the name in between), the others are created and stay: `ok` and `FAIL` lines, exit status `1`.
- `--dry-run` only checks, and prints `--- <name>` and the YAML each DevPod would be created from.
- `config save` reads the whole of standard input (at most 256 KiB). Saving an id you already have replaces its YAML and changes only what the flags say.
- With `--json`, `launch` prints the API's answer (`{"items": [{index, name, node, ok, code, message, yaml}]}`), also when the launch was rejected.

## For scripts and agents

```sh
# everything about my DevPods, as JSON
ssh alice+devpod@gateway list --json | jq -r '.items[] | "\(.metadata.name) \(.status.phase)"'

# create, wait until it runs, then log in
ssh alice+devpod@gateway create train1 --image pytorch/pytorch:2.4.0-cuda12.4-cudnn9-runtime --cpu 8 --memory 32Gi --gpu 1
until [ "$(ssh alice+devpod@gateway show alice-train1 --json | jq -r .devpod.status.phase)" = Running ]; do sleep 2; done
$(ssh alice+devpod@gateway ssh alice-train1)

# publish a port and get its address
ssh alice+devpod@gateway ports alice-train1 public 8888
ssh alice+devpod@gateway ports alice-train1 url 8888

# save an environment as an image and wait for the result (cancelled if this connection dies)
ssh alice+devpod@gateway snapshot alice-train1 --name train-env --json | tail -n 1 | jq -r 'select(.stage == "done") | "\(.image)@\(.digest)"'

# or start it and poll
id=$(ssh alice+devpod@gateway snapshot alice-train1 --detach --json | jq -r .id)
until ssh alice+devpod@gateway snapshot show "$id" --json | jq -e '.phase == "Succeeded" or .phase == "Failed"' >/dev/null; do sleep 5; done

# push to a registry of your own; the password comes from standard input
printf '%s\n' "$REGISTRY_PASSWORD" | ssh alice+devpod@gateway snapshot alice-train1 --image registry.example.com/me/env:v1 --username me --password-stdin

# launch three workers from a saved config; non-zero exit if any was not created
ssh alice+devpod@gateway config launch workers --set NAME=w --count 3 --json

# stop everything of mine
ssh alice+devpod@gateway stop $(ssh alice+devpod@gateway list --json | jq -r '.items[].metadata.name')
```

An agent needs only an SSH key registered for its user: give it `ssh alice+devpod@gateway help` and `--json`. For finer-grained rights than "everything this user may do", use an API token with the REST API instead.

## What a CLI connection is not

It only runs commands. Port forwarding, SFTP / scp, agent forwarding and X11 are refused, and it does not put you inside a DevPod: use the command `ssh <name>` prints for that.

## Limits

- 64 CLI connections at once per gateway replica (`--cli-max-conns`); beyond that a command fails with `too many DevPod CLI sessions`.
- 8 sessions at once on one connection (SSH multiplexing).
- One command may take 60 s; one API request 10 s; an API answer 8 MiB. Following a snapshot is the exception: up to 60 minutes, and it gives up after 60 s without any progress event.
- A connection with no command running is closed after 5 minutes.

## Operations

- Chart: `gateway.cli.enabled: true` (needs `webui.enabled`), `gateway.cli.user` (default `devpod`). The chart creates the Secret `devpod-cli-key` once and keeps it; the gateway reads it at `/etc/devpod/cli/key` and the web UI must be given the same key.
- Gateway flags: `--cli-user`, `--cli-api-url` (the web UI Service), `--cli-api-key-file`, `--cli-max-conns`. Without `--cli-api-url` the CLI is off and the login name is an ordinary one.
- The gateway sends `Authorization: Bearer gwk_<key>` and `X-DevPod-User: <user>`; the web UI treats such a request as that user. Anyone holding the key can act as any user: it must stay inside the cluster.
- Every command is logged by the gateway as `cli_command` (user, command, arguments, exit status, duration, client address, direct or proxy). CLI connections are not SSH sessions into a DevPod, so they are not in the session recordings.
