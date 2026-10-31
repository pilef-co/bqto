# bqto

[![CI](https://github.com/pilef-co/bqto/actions/workflows/ci.yml/badge.svg)](https://github.com/pilef-co/bqto/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/pilef-co/bqto)](https://github.com/pilef-co/bqto/releases/latest)
[![License](https://img.shields.io/github/license/pilef-co/bqto)](LICENSE)

bqto connects your laptop or a CI job to a private Google Cloud service, such as
Cloud SQL, AlloyDB or Memorystore. It creates a short-lived bastion VM, forwards
a local port to the service through Identity-Aware Proxy (IAP), and deletes the
bastion VM when the session ends. You do not need a VPN or a bastion that runs
all the time.

## What it looks like

```text
$ bqto connect --project my-project --service cloudsql --instance orders-db
checking auth…
resolving endpoint…
checking firewall…
finding zones…
finding subnet…
creating bastion from debian-13-trixie-v20261027…
bastion bqto-410ded5f is in us-east1-b
opening tunnel…
ready: 127.0.0.1:15432 → 10.0.0.5:5432 (instance orders-db), bastion bqto-410ded5f, expires at 00:38 (in 29m)
try: psql "host=127.0.0.1 port=15432 user=postgres"
^C
deleting bastion…
bastion bqto-410ded5f is being deleted (about 2 minutes)
```

Point `psql` or your app at the local port while the session runs.
Press Ctrl-C to end the session. bqto then deletes the bastion VM.

## Quickstart

1. Install bqto:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/pilef-co/bqto/main/install.sh | sh
   ```

   Or, with Homebrew: `brew install pilef-co/tap/bqto`.

2. Sign in to Google Cloud twice, with the same account:

   ```sh
   gcloud auth login
   gcloud auth application-default login
   ```

3. Prepare the project once. Ask for the roles in [Permissions](#permissions).
   Make sure that the APIs in [Requirements](#requirements) are enabled, and
   that a firewall rule lets IAP reach the bastion VM on port 22, as
   Requirements shows.

4. Connect:

   ```sh
   bqto connect --project my-project --service cloudsql --instance my-db
   ```

   When the `ready:` line appears, copy the `try:` line below it into another
   terminal. For Cloud SQL for PostgreSQL, it is:

   ```sh
   psql "host=127.0.0.1 port=15432 user=postgres"
   ```

   For MySQL or SQL Server, the `try:` line gives the `mysql` or `sqlcmd`
   command instead.

bqto finds the instance's private IP, creates a small bastion VM next to it and
opens an IAP tunnel to `127.0.0.1:15432`. Without flags, bqto starts an
interactive wizard, which asks for each value in turn.

You can also let a coding agent do this. Install the skill below, then ask it to
count the rows of a table in `my-db`.

## Use with a coding agent

An [agent skill](https://agentskills.io) teaches Claude Code, Codex, Cursor and
other coding agents to open a bqto session without prompts, run their command
through the tunnel and read the `Fix:` lines. Install it for every agent on
this machine:

```sh
npx skills add pilef-co/bqto -g
```

[docs/usage.md](docs/usage.md#coding-agents) explains what the command installs
and how to copy the skill by hand.

## Use in GitHub Actions

[pilef-co/bqto-action](https://github.com/pilef-co/bqto-action) runs a step
against a private service from a workflow, for example a database migration.
After `google-github-actions/auth` with a `service_account` and
`google-github-actions/setup-gcloud`, it needs no service account key and no
runner inside the VPC:

```yaml
- uses: pilef-co/bqto-action@v1
  with:
    project: my-project
    instance: my-db
    run: ./migrate up
```

[The action's README](https://github.com/pilef-co/bqto-action#inputs) lists the
inputs, and [docs/github-actions.md](docs/github-actions.md)
shows a full workflow.

## Services

| Service | `--service` | Plan |
|---|---|---|
| Cloud SQL, including read replicas | `cloudsql` | Free |
| AlloyDB, including read pools | `alloydb` | Free |
| Memorystore for Redis, Basic and Standard tiers | `redis` | Free |
| Memorystore for Valkey, cluster mode disabled | `valkey` | Free |
| Memorystore for Redis Cluster | `redis` | [bqto Pro](#bqto-pro) |
| Memorystore for Valkey in cluster mode | `valkey` | [bqto Pro](#bqto-pro) |
| Managed Kafka | `kafka` | [bqto Pro](#bqto-pro) |
| Cloud Run, internal ingress | `cloudrun` | [bqto Pro](#bqto-pro) |

Read replicas and read pools work like any other instance, and the wizard marks
them.

## Requirements

- macOS or Linux, on amd64 or arm64. Windows is not supported yet: bqto relies
  on Unix process groups, signals and file locks, and does not build for
  Windows. WSL 2 with the Linux binary is untested.
- The [Google Cloud CLI](https://cloud.google.com/sdk/docs/install) on `PATH`,
  version 550.0.0 or later.
- An SSH client (`ssh`) on `PATH`. Slim Linux images need `openssh-client` and
  `ca-certificates`.
- Two logins with the same account: `gcloud auth login` (used by the tunnel)
  and `gcloud auth application-default login`. The second sets up Application
  Default Credentials (ADC), which bqto uses for discovery and the bastion VM.
- Outbound HTTPS with WebSockets to `tunnel.cloudproxy.app`.
- An instance with a private IP, through private services access (PSA) or
  Private Service Connect (PSC). bqto refuses an instance that has only a public
  IP.
- A subnet in the instance's region and VPC network where a VM can run.
- These APIs enabled in the service's project: `compute.googleapis.com`,
  `cloudresourcemanager.googleapis.com`, `iap.googleapis.com`,
  `oslogin.googleapis.com` and the API of the service. That is
  `sqladmin.googleapis.com` for Cloud SQL, `alloydb.googleapis.com` for AlloyDB,
  `redis.googleapis.com` for Redis or `memorystore.googleapis.com` for Valkey.
  [docs/permissions.md](docs/permissions.md#apis) says what each one is for.
- A firewall rule or policy that allows ingress on tcp:22 from `35.235.240.0/20`
  (the IAP range) into the service's VPC network. A network admin can create a
  VPC firewall rule:

  ```sh
  gcloud compute firewall-rules create allow-iap-ssh \
    --project=NETWORK_PROJECT --network=NETWORK \
    --direction=INGRESS --action=ALLOW --rules=tcp:22 \
    --source-ranges=35.235.240.0/20
  ```

Before it creates a bastion VM, bqto checks the gcloud version, `ssh`, the two
logins and the firewall rule. If one is missing, bqto prints a `Fix:` line. bqto
does not create the firewall rule.
[docs/usage.md](docs/usage.md#requirements-in-detail) says why each requirement
exists.

## Permissions

| Where | For | Roles |
|---|---|---|
| Service project | Finding the service | The viewer role of the service, such as `roles/cloudsql.viewer` |
| Service project | Creating and deleting the bastion | `roles/compute.instanceAdmin.v1` |
| Service project | SSH through IAP | `roles/iap.tunnelResourceAccessor`, `roles/compute.osLogin` |
| Network project (Shared VPC host, or the same project) | Subnet and firewall checks | `roles/compute.networkViewer` |
| Network project, Shared VPC only | Using the subnet | `roles/compute.networkUser` on the subnet |
| Service project | API checks and quota, as gcloud does: Google checks the APIs in the service project, not in your ADC quota project | `roles/serviceusage.serviceUsageConsumer` (Owner and Editor include it). Without it, bqto falls back to your ADC quota project; `GOOGLE_CLOUD_QUOTA_PROJECT` picks another one |

`roles/compute.instanceAdmin.v1` is broad: it can manage every VM in the
project and gives sudo on all of them. The
[minimal custom role](docs/permissions.md#a-minimal-custom-role) holds only what
bqto uses.

`roles/editor` is not enough, because it does not include the IAP tunnel
permission. [docs/permissions.md](docs/permissions.md) has every role, the
commands to grant them, a minimal custom role and a tested way to limit it to
bastions. [docs/security.md](docs/security.md) is the page for your security
team: data flows, credentials, what a developer can do and the audit trail.

## Cost

A 30-minute session, the default, costs about half a cent in `us-central1`: one
`e2-micro` VM at about 0.0084 USD an hour, and a 10 GB standard boot disk at
under 0.001. IAP TCP forwarding is free. Prices vary by region. A bastion VM
left behind by `kill -9` keeps costing until its `--duration` ends.

## Build and install

Each release has a binary for macOS and Linux, on amd64 and arm64,
and a `checksums.txt` with their SHA-256. [install.sh](install.sh) downloads the
newest one for your machine, checks its SHA-256 and puts `bqto` in
`/usr/local/bin`. It uses `sudo` only when that folder is not yours. You do not
need Go:

```sh
curl -fsSL https://raw.githubusercontent.com/pilef-co/bqto/main/install.sh | sh
```

`BQTO_VERSION=v0.16.0` installs that release, and `BQTO_INSTALL_DIR=~/.local/bin`
installs into another folder, as in `curl … | BQTO_INSTALL_DIR=~/.local/bin sh`.
If GitHub limits the requests from your address, give the script a GitHub
token, as in `curl … | GITHUB_TOKEN=$(gh auth token) sh`.

With Homebrew:

```sh
brew install pilef-co/tap/bqto
```

To install by hand, download the archive for your machine from the release
page, check it and put `bqto` on your `PATH`:

```sh
gh release download --repo pilef-co/bqto --pattern 'bqto_*_darwin_arm64.tar.gz' --pattern checksums.txt
shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf bqto_*_darwin_arm64.tar.gz bqto && sudo mv bqto /usr/local/bin/
```

On an Intel Mac, use `darwin_amd64`. On Linux, use `linux_amd64` or
`linux_arm64`.

### Upgrade

`bqto version` tells you when a newer release exists, and `bqto update`
installs it. If you installed bqto with Homebrew, `bqto update` runs
`brew upgrade bqto`, and you can run that yourself. bqto looks for updates only
when you run one of these two commands. [docs/usage.md](docs/usage.md#update) has the details.

### Uninstall

Delete the `bqto` binary. [docs/usage.md](docs/usage.md#cleanup-and-uninstall)
lists the files that a session can leave behind.

### Shell completion

Add the line for your shell to its startup file, then open a new terminal.

Bash:

```sh
echo 'eval "$(bqto completion bash)"' >> ~/.bashrc
```

Zsh:

```sh
echo 'source <(bqto completion zsh)' >> ~/.zshrc
```

Fish:

```sh
echo 'bqto completion fish | source' >> ~/.config/fish/config.fish
```

[docs/usage.md](docs/usage.md#shell-completion) has the details for macOS and
zsh.

### Build from source

With Go 1.26 or later:

```sh
go install github.com/pilef-co/bqto/cmd/bqto@latest
```

Or, from a checkout:

```sh
go build -o bqto ./cmd/bqto
./bqto version
```

To work on bqto itself, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Cleanup

A session deletes its bastion VM when it ends, and the bastion VM deletes itself
when `--duration` ends. [docs/usage.md](docs/usage.md#cleanup-and-uninstall)
lists what else can stay behind.

### Who created a bastion

The project's Admin Activity audit logs name the person.
[docs/usage.md](docs/usage.md#who-created-a-bastion) has the query.

## Troubleshooting

When a check or the tunnel fails, bqto prints the cause and a `Fix:` line. The
usual causes are a missing IAP or OS Login role, no firewall rule for
`35.235.240.0/20`, a gcloud older than 550.0.0, and two logins with different
accounts. [docs/troubleshooting.md](docs/troubleshooting.md) matches each tunnel
error to its fix, and explains `--debug`.
[docs/usage.md](docs/usage.md#exit-codes) lists the exit codes.

## Getting help

Open an issue at https://github.com/pilef-co/bqto/issues. Run the failing
command again with `--debug` and attach the output, after you read it as
[docs/troubleshooting.md](docs/troubleshooting.md#debug-output) says. To report
a vulnerability, follow [SECURITY.md](SECURITY.md) instead.

## Documentation

- [docs/usage.md](docs/usage.md): how a session works, flags, shell completion,
  scripts and CI, exit codes, cleanup and updates.
- [docs/github-actions.md](docs/github-actions.md): an example GitHub Actions
  workflow that reaches a private database with no key.
- [docs/troubleshooting.md](docs/troubleshooting.md): tunnel errors and debug
  output.
- [docs/permissions.md](docs/permissions.md): every role, and how to grant it.
- [docs/security.md](docs/security.md): data flows, credentials and the audit
  trail.
- [docs/design.md](docs/design.md): the problem, why a VM hop is required and
  where bqto stops being the right tool.
- [CHANGELOG.md](CHANGELOG.md): what changed in each release.

## bqto Pro

bqto Pro is the paid build of bqto, now in beta. To try it, join the bqto Pro
beta at https://bqto.dev/#waitlist. It is one `bqto` binary that does everything
this one does, built from a private repository on top of this one. It adds:

- Cloud Run services with internal ingress, through a SOCKS5 proxy or a local
  web proxy (`--web`).
- Managed Kafka, Memorystore for Redis Cluster and Memorystore for Valkey in
  cluster mode, through the same SOCKS5 proxy.
- Profiles and stacks: saved targets, and several services in one session on one
  bastion (`bqto profile`, `bqto stack`, `--profile`, `--stack`, `--save`,
  `--replace`).
- `--spot`, `--subnet`, `--machine-type` and `--image`, for organizations that
  restrict what a VM may be.
- `--endpoint IP:PORT`, to connect to a private IP and port instead of a named
  instance.

Profiles and stacks in bqto Pro, as `bqto -h` and `bqto profile -h` show them:

```sh
bqto connect --project P --service cloudsql --instance orders --save db
bqto profile create cache --project P --service redis --instance cache
bqto connect db
bqto stack create work --profile db --profile cache
bqto connect --stack work
```

The first line connects and saves the session as the profile `db`. The second
saves a profile without connecting. `bqto connect db` opens one profile by
name, and `bqto connect --stack work` opens both through one bastion.

This binary marks the bqto Pro flags `(bqto Pro)` in its help. Given one of
them, it exits 2 with a `Fix:` line and creates nothing.
[docs/usage.md](docs/usage.md#bqto-pro-features-in-this-binary) has the details,
and [Services](#services) says which plan has each service.

## License

bqto is licensed under the [Apache License 2.0](LICENSE). bqto Pro is a
separate product with its own terms.
