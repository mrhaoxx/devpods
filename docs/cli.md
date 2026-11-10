# CLI over SSH

`ssh devpod@<gateway> <command>` manages DevPods from a terminal, a script or an agent. There is nothing to install and no token to keep: the gateway knows who you are from your SSH key and runs each command against the console's REST API as you, so the CLI, the console and API tokens follow the same rules (ownership, quota, admin-only operations).

```console
$ ssh devpod@gateway.example.com list
NAME      OWNER  PHASE    NODE  IMAGE         CPU  MEMORY  GPU  AGE
alice-a   alice  Running  g206  ubuntu:24.04  4    8Gi     -    3d
alice-b   alice  Stopped  -     pytorch:2     8    32Gi    1    12d
```

With a terminal and no command (`ssh devpod@gateway`) you get a prompt (`devpod>`) that takes the same commands; `exit` leaves.

## Who you are

| Login | Meaning |
|---|---|
| `devpod` | The user whose public keys (the ones registered in the console) contain the key you offered. |
| `devpod+<user>` | That user. The key must be theirs; identity sources are checked in the usual order (User, then LDAP). Use this form when your key comes from LDAP, when a key is registered for more than one user, or through an SSH front proxy. |

A trusted SSH front proxy must use `devpod+<user>`: the name is how it says whom it acts for, and the user must exist.

The login name `devpod` is configurable (`--cli-user`, chart value `gateway.cli.user`). Whatever it is, it cannot be used as a DevPod user name any more.

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
| `ports <name>` | The HTTP address pattern and the public ports. |
| `ports <name> url <port>` | One port's address. |
| `ports <name> public <port>…` / `private <port>…` | Open a port to people who are not signed in, or close it again. |
| `ports <name> reset` | Draw a new random address; every old link stops working. |
| `ssh <name>` | Print the `ssh` command that logs into the DevPod. |
| `templates` | List DevPod templates. |
| `nodes`, `users` | List nodes / users (admin). |
| `migrate <name> <node\|->` | Pin a DevPod to a node; `-` lets the scheduler choose again (admin). |

`<name>` is the DevPod's full name as `list` shows it (`alice-a`), except in `create`, where it is the part after your user name.

Commands that take several names (`start`, `stop`, `redeploy`, `delete`) try every one and report each; one failure does not stop the rest.

The command line is split like a shell splits plain words and quotes (`'…'`, `"…"`, `\`), and nothing is expanded: no variables, globs, pipes or substitutions run on the gateway.

## Output and exit status

- Default output is text: aligned tables and `key: value` lines on stdout.
- `--json` (or `-o json`) on any command prints the REST API's JSON answer on stdout, unchanged. Commands that take several names print `[{"name": …, "ok": true}, {"name": …, "ok": false, "code": …, "error": …}]`. `ports … url` prints `{"url", "port", "public"}`; `ssh` prints `{"command", "login", "host"}`.
- Errors go to stderr as `error: <message> (<CODE>)`, where `<CODE>` is the API's error code (`NOT_FOUND`, `FORBIDDEN`, `QUOTA_EXCEEDED`, …).
- Exit status: `0` success, `1` the API refused or failed (for several names: at least one failed), `2` the command line is wrong (nothing was sent).

## For scripts and agents

```sh
# everything about my DevPods, as JSON
ssh devpod@gateway list --json | jq -r '.items[] | "\(.metadata.name) \(.status.phase)"'

# create, wait until it runs, then log in
ssh devpod@gateway create train1 --image pytorch/pytorch:2.4.0-cuda12.4-cudnn9-runtime --cpu 8 --memory 32Gi --gpu 1
until [ "$(ssh devpod@gateway show alice-train1 --json | jq -r .devpod.status.phase)" = Running ]; do sleep 2; done
$(ssh devpod@gateway ssh alice-train1)

# publish a port and get its address
ssh devpod@gateway ports alice-train1 public 8888
ssh devpod@gateway ports alice-train1 url 8888

# stop everything of mine
ssh devpod@gateway stop $(ssh devpod@gateway list --json | jq -r '.items[].metadata.name')
```

An agent needs only an SSH key registered for its user: give it `ssh devpod@gateway help` and `--json`. For finer-grained rights than "everything this user may do", use an API token with the REST API instead.

## What a CLI connection is not

It only runs commands. Port forwarding, SFTP / scp, agent forwarding and X11 are refused, and it does not put you inside a DevPod: use the command `ssh <name>` prints for that.

## Limits

- 64 CLI connections at once per gateway replica (`--cli-max-conns`); beyond that a command fails with `too many DevPod CLI sessions`.
- 8 sessions at once on one connection (SSH multiplexing).
- One command may take 60 s; one API request 10 s; an API answer 8 MiB.
- A connection with no command running is closed after 5 minutes.

## Operations

- Chart: `gateway.cli.enabled: true` (needs `webui.enabled`), `gateway.cli.user` (default `devpod`). The chart creates the Secret `devpod-cli-key` once and keeps it; the gateway reads it at `/etc/devpod/cli/key` and the web UI must be given the same key.
- Gateway flags: `--cli-user`, `--cli-api-url` (the web UI Service), `--cli-api-key-file`, `--cli-max-conns`. Without `--cli-api-url` the CLI is off and the login name is an ordinary one.
- The gateway sends `Authorization: Bearer gwk_<key>` and `X-DevPod-User: <user>`; the web UI treats such a request as that user. Anyone holding the key can act as any user: it must stay inside the cluster.
- Every command is logged by the gateway as `cli_command` (user, command, arguments, exit status, duration, client address, direct or proxy). CLI connections are not SSH sessions into a DevPod, so they are not in the session recordings.
