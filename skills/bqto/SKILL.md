---
name: bqto
description: Reach a private Google Cloud service (Cloud SQL, AlloyDB, Memorystore for Redis or Valkey; with bqto Pro also Cloud Run, Managed Kafka and Memorystore clusters) from this machine through bqto, which opens a short-lived bastion and an IAP tunnel and deletes them when the command ends. Use when a task needs a query, a command or an HTTP call against a service that has no public address, or when the user wants to save such a target as a bqto profile. If unsure whether bqto fits, ask the user.
---

# bqto

bqto creates a bastion VM in the service's VPC, opens an IAP tunnel to it and
deletes the VM when the session ends. Run it without prompts, one command per
session.

## Before you start

- `bqto version`, `gcloud version`: both must be on PATH, gcloud 550.0.0 or later
  (bqto checks it and prints the update command).
- `bqto version` also looks for a newer bqto. When stderr says
  `bqto X.Y.Z is available` or `fixes a security issue`, tell the user; do not
  run `bqto update` unless they ask. `bqto version --output json` gives
  `version`, `latest`, `update_available` and `security`.
- Application Default Credentials and gcloud must be the same account, with the
  roles in https://github.com/pilef-co/bqto/blob/main/docs/permissions.md. If not, bqto stops and prints a `Fix:` line.
- The client for the service (the table below) must be on this machine. Check
  with `command -v psql` (or the client you need) before you open a session: a
  missing client wastes the bastion. If it is missing, ask the user how to install
  it; on macOS, `brew install libpq` puts `psql` in `$(brew --prefix libpq)/bin`.
- You need the project, the service and the instance name. If the user does not
  give them, ask.
- If the service needs a password and none is set (`PGPASSWORD`, `MYSQL_PWD`,
  `SQLCMDPASSWORD`, `REDISCLI_AUTH`), ask the user. Do not list secrets or
  enable APIs to find one.
- Pass anything longer than a short query on stdin or from a file, not inside
  nested quotes. bqto hands its stdin to the command:
  `bqto connect … -- sh -c 'psql "…"' <<'SQL'`.

## Run one command through a session

Always pass `--no-input`, a short `--duration` and the command after `--`. bqto
opens the session, runs the command with the address in its environment
(`BQTO_HOST`, `BQTO_PORT`, `BQTO_REMOTE` for the service's own address,
`BQTO_TLS`, `BQTO_AUTH`, and `PGHOST`/`PGPORT` for Cloud SQL and AlloyDB),
deletes the bastion when the command exits, and exits with the command's code.

```sh
bqto connect --no-input --duration 15m --project P --service cloudsql --instance NAME \
  -- sh -c 'psql "host=$PGHOST port=$PGPORT user=USER dbname=DB sslmode=require" -c "select 1"'
```

A session takes about a minute to open. If the default local port is taken, bqto
uses the next free one and puts it in `$BQTO_PORT`, so always read the port from
the environment. A port asked for with `-p` is never moved: if it is taken, bqto
stops with "local port … is taken" before it creates anything.

## What each service supports

The free bqto has only what this table lists. `bqto version` says `bqto Pro`
when the binary is bqto Pro.

| Service | `--service`, `--instance` | What the session gives | Client |
|---|---|---|---|
| Cloud SQL for PostgreSQL | `cloudsql`, `NAME` | a port forward | `psql` with `$PGHOST`/`$PGPORT` |
| Cloud SQL for MySQL or SQL Server | `cloudsql`, `NAME` | a port forward | `mysql`/`sqlcmd` with `$BQTO_HOST`/`$BQTO_PORT` |
| AlloyDB | `alloydb`, `REGION/CLUSTER/INSTANCE` | a port forward | `psql` with `$PGHOST`/`$PGPORT` |
| Memorystore for Redis, Valkey | `redis` or `valkey`, `REGION/NAME` | a port forward | `redis-cli -h $BQTO_HOST -p $BQTO_PORT`, with `--tls` and the CA when `$BQTO_TLS` is set and `REDISCLI_AUTH` when `$BQTO_AUTH` is set, below |

Cloud Run, Managed Kafka, Redis Cluster, clustered Valkey, profiles, stacks and
the flags marked `(bqto Pro)` in `bqto connect -h` need bqto Pro. The free bqto
refuses them with exit code 2 and a `Fix:` line; tell the user. In bqto Pro they
work: `bqto profile -h` shows profiles and stacks with examples, `bqto connect -h`
lists every flag in one alphabetical list and ends each bqto Pro flag with
`(P)`, and bqto Pro's documentation has an example for each.

When the user wants to reach a target again later, save it as a profile (bqto
Pro): `bqto profile create NAME --project P --service S --instance I`, or add
`--save NAME` to a `bqto connect` that works. Several profiles that open
together make a stack: `bqto stack create NAME --profile A --profile B`. Then
the user runs `bqto connect NAME` or `bqto connect --stack NAME`. Ask for the
names; `bqto profile list` and `bqto stack list` show the ones that exist. A
name in use is refused; add `--replace` only when the user wants the old one
overwritten. To change a name,
`bqto profile rename OLD NEW` (the stacks that list it follow) or
`bqto stack rename OLD NEW`; both refuse a name that is already taken.

`BQTO_TLS` is `1` when your client must use TLS, and empty otherwise.
`BQTO_AUTH` names the credential the service takes: `password` (an AUTH string),
`iam` (an access token), or empty for none.

Redis and Valkey need a few lines, so put them in a script and run it with
`-- sh client.sh`. Outside `-- CMD` (a longer session, below), set `BQTO_HOST`,
`BQTO_PORT`, `BQTO_TLS` and `BQTO_AUTH` from the ready event's `host`, `port`,
`tls` (`1` when true) and `auth` first.

A Redis or Valkey instance. With TLS, the client needs the instance's CA in
`server-ca.pem`: bqto prints the gcloud command that writes it on its `CA:` line.
With `BQTO_AUTH=password`, start bqto with `REDISCLI_AUTH` already set to the
instance's AUTH string, which the user gives you:

```sh
set --
if [ -n "$BQTO_TLS" ]; then set -- --tls --cacert server-ca.pem; fi
if [ "$BQTO_AUTH" = iam ]; then REDISCLI_AUTH="$(gcloud auth print-access-token)"; export REDISCLI_AUTH; fi
redis-cli -h "$BQTO_HOST" -p "$BQTO_PORT" "$@" PING
```

Passwords, IAM tokens and auth strings come from the user, a secret the user
names, or `gcloud auth print-access-token`. Never print them, and pass them
through the environment or a private file, as above, never as command-line
arguments.

## A longer session

When several commands need the same tunnel, start bqto in the background with
`--output json` and read stdout: once ready, it prints one JSON object per
service, such as

```json
{"event":"ready","service":"cloudsql","instance":"sql","host":"127.0.0.1","port":15432,"remote":"10.0.0.5:5432","bastion":"bqto-1a2b3c4d","expires":"2026-10-14T12:30:00Z"}
```

Use `host` and `port`; `remote` is the service's own address. `tls: true` means
your client must use TLS. `auth` says which credential: `password` (an AUTH
string) or `iam` (an access token). When you
are done, stop bqto with SIGINT (Ctrl-C) or SIGTERM: it deletes the bastion. The
bastion deletes itself at `expires` in any case.

If your process may die without stopping bqto, add `--end-on-stdin-close` and
start bqto with a pipe on its stdin that you keep open and never write to: when
the pipe closes, bqto ends the session and deletes the bastion as on SIGTERM.
It does not go with `-- CMD`, and stdin must not be a terminal.

With `--web` (bqto Pro, Cloud Run), the ready event also has `web`, the local
proxy's address, and `web_token`, the session secret. Under `-- CMD` they are
`$BQTO_WEB_URL` and `$BQTO_WEB_TOKEN`. Send the secret as the `X-Bqto-Token`
header without putting it in argv:
`curl -H @<(printf 'X-Bqto-Token: %s\n' "$BQTO_WEB_TOKEN") "$BQTO_WEB_URL/"`
(bash or zsh). A request without it gets 403.

## When it fails

Read bqto's `error:` line and the `Fix:` line under it on stderr; the fix is a
command or a change the user can make. With `-- CMD`, bqto exits with the
command's own code once the command has run, so a code alone does not say whether
bqto or the command failed: look for bqto's `error:` line first. bqto's own codes,
before the command starts:

| Code | Meaning |
|---|---|
| 0 | The session ended normally, including SIGINT, SIGTERM, SIGHUP (the terminal closed) or the end of stdin with `--end-on-stdin-close` once the session is ready |
| 1 | An error; the message says which |
| 2 | A bad flag, a missing value or a bqto Pro feature; nothing was created |
| 126 | The command after `--` exists but cannot run |
| 127 | The command after `--` was not found |
| 130 | Interrupted (SIGINT, SIGTERM, SIGHUP or the end of stdin with `--end-on-stdin-close`) while bqto was still working, before the session was ready |

With no `error:` line from bqto, the code is the command's, or 128 + N when a signal N killed the
command (143 for SIGTERM). The exception is 130 right after you interrupted bqto (SIGINT, SIGTERM
or SIGHUP) before the session was ready. Do not retry a bqto usage error (code 2 with an `error:`
line) with the same flags. On code 1, follow the `Fix:` line or show it to the user. Do not change
their IAM or their firewall yourself.
