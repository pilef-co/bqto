# bqto

bqto reaches a private Google Cloud service from a laptop or a CI job, through a short-lived
bastion and an IAP tunnel.

## Language

**Service**:
The kind of private Google Cloud product bqto connects to, picked with `--service`:
`cloudsql`, `alloydb`, `redis` or `valkey`. bqto Pro adds `kafka` and `cloudrun`.
_Avoid_: service for one instance of it; that is a target.

**Target**:
One instance of a service that a session forwards a local port to, named by project, service
and instance. In tests, a target is a test instance that `scripts/testenv.sh` creates,
named `bqto-test-*`.

**Profile** (bqto Pro):
A named, saved target with its session settings (duration, local port, zone and others), kept
in `config.toml`. It stores the target's identity, never its resolved address.

**Stack** (bqto Pro):
A named list of profiles that `bqto connect --stack` opens in one session, on one bastion.
The settings of that bastion come from the first profile.

**Bastion**:
The VM with no external IP that bqto creates in the target's VPC for one session, labelled
`bqto=session`. bqto deletes it when the session ends, and a server-side deadline deletes it if
bqto cannot.
_Avoid_: proxy, jump host.

**Session**:
One run of `bqto connect`: one bastion, an IAP tunnel to it and one local port per target
that bqto holds for the whole session, from the bastion's creation to its deletion. It ends on
Ctrl-C or another signal, when `-- CMD` exits, when `--duration` runs out or when the bastion
goes away.
