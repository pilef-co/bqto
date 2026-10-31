# Security

This page is for the team that decides whether developers may use bqto. It says what
bqto does with your identity, your network and your data, and what it does not do.
Each section links to the code, the documentation or the live test behind it. The
roles and the commands to grant them are in [permissions.md](permissions.md).

## Data flows

```text
developer's laptop ──HTTPS── Google APIs (Compute, the service's API, IAM, Resource Manager)
developer's laptop ──WSS──── IAP (tunnel.cloudproxy.app) ──tcp:22── bastion ──── the service's private IP
```

- **bqto calls Google's APIs, and nothing else, except when you ask for the newest
  version.** No telemetry and no server run by Pilef. Outside a
  Google client library, the only HTTP calls go to
  `oauth2.googleapis.com/tokeninfo`, to read the email of your own login, and to
  GitHub, from `bqto version` and `bqto update` only, as the next point says.
- **`bqto version` and `bqto update` ask for the newest release, and nothing else
  does.** The free bqto sends one unauthenticated `GET` to
  `api.github.com/repos/pilef-co/bqto/releases/latest`, with the `User-Agent`
  `bqto` and nothing about you, your version or your projects; GitHub sees your IP
  address. `bqto update` then downloads the archive and `checksums.txt` from the
  release. bqto Pro asks Artifact Registry instead, through your own gcloud
  login. No other command looks, and nothing runs in the background. To never
  contact GitHub, do not run these two commands.
- **`bqto update` checks what it downloads.** It installs only an archive whose
  SHA-256 matches the release's `checksums.txt`, writes the new binary next to
  the old one and renames it over it, so a failed update leaves the old binary.
  When you cannot write to the binary's folder, it downloads and checks the
  release as you, and only then runs one short root shell through `sudo`. That
  shell makes a temporary file in the binary's folder, copies the checked
  binary into it with `install`, keeping the old binary's owner, group and
  mode, and renames it over the old one with `mv`. A refused or failed `sudo`
  leaves the old binary.
  `checksums.txt` comes from the same release, so it guards against a broken
  download, not against a compromised release. It follows only HTTPS
  redirects, and never replaces a development build (a `go build` from a
  checkout). Installed with `go install`, it runs
  `go install github.com/pilef-co/bqto/cmd/bqto@latest` instead, and Go reaches
  its own servers (the Go module proxy or GitHub). Installed with Homebrew, it
  runs `brew upgrade bqto` instead, and Homebrew downloads and checks the
  release itself.
- **The tunnel is gcloud's**: `gcloud compute ssh --tunnel-through-iap`, an SSH
  session through Identity-Aware Proxy to port 22 of the bastion, with port
  forwards to the service. From your laptop to the bastion, the traffic is inside
  that SSH session. From the bastion to the service it is plain VPC traffic,
  encrypted only if the service itself uses TLS.
- **Everything bqto creates stays in your project**: the bastion VM and its boot
  disk. It creates no service account and no IAM binding.
- **It creates no firewall rule, but it needs one**: an ingress rule for tcp:22
  from `35.235.240.0/20`. bqto copies that rule's target tags onto the bastion
  (`evalVPCFirewalls` and `instanceResource` in `internal/bastion/bastion.go`), so
  every other rule that uses those tags applies to the bastion too. Watch those
  tags.

### Data flows in bqto Pro

The free binary refuses the services and the flag in this section.

- **Cloud Run, Managed Kafka and Redis or Valkey clusters** go through a SOCKS
  proxy on the bastion (`ssh -D`). That proxy reaches anything the bastion can
  reach, not only the service. For Cloud Run it uses Private Google Access. With
  `--web`, a proxy on `127.0.0.1` adds your identity token to each request.
- **The `--web` proxy asks for the session secret.** bqto makes one secret per
  session and prints the link to open with it. A request with neither the cookie
  that the link sets nor the `X-Bqto-Token` header gets 403, so another account on
  the machine cannot use the proxy as you. Two limits remain: browsers send that
  cookie to any server on `127.0.0.1`, whatever its port, and the browser history
  keeps the first link, secret included, until the session ends and the secret
  stops working.

## Credentials

| Credential | Where it lives | For how long |
|---|---|---|
| Your gcloud login and Application Default Credentials | your laptop, where gcloud keeps them | until you revoke them; bqto only reads them |
| The SSH key for the tunnel | `~/.ssh/google_compute_engine`, created by `gcloud compute ssh --quiet` with no passphrase; gcloud gets a short-lived OS Login certificate for it for each bastion, which takes only the certificate | the key has no expiry; each certificate is short-lived |
| Cloud Run identity tokens (`--web`, bqto Pro) | in memory, from `gcloud auth print-identity-token` | about an hour, refreshed by the proxy |
| The bastion's own credentials | none: the VM is created with no service account | — |
| Database passwords and service tokens | never seen by bqto: you type them into your own client | — |
| An access token used as a password (IAM auth) | your client sends it, as bqto's hints suggest (`gcloud auth print-access-token`, cloud-platform scope) | about an hour. Memorystore: from the bastion to the instance it travels in cleartext unless transit encryption is on. Managed Kafka (bqto Pro): in cleartext only to `kafka-proxy` on `127.0.0.1`, which sends it to the cluster over TLS |

A service account key is never needed: bqto runs as a service account through
impersonation or Workload Identity Federation, with no key
([permissions.md](permissions.md#how-to-grant-it)).

## The bastion

- `e2-micro`, Shielded VM (secure boot, vTPM, integrity monitoring), from the public
  `debian-cloud` image, or the image that bqto Pro's `--image` names. The boot disk uses Google's
  default encryption.
- No external IP and no service account. OS Login is on. It can still reach the
  internet if its subnet has Cloud NAT: bqto does not check for it.
- It deletes itself when `--duration` ends, even if bqto was killed: Compute
  Engine enforces the deadline, not bqto. The developer picks the duration, 30
  minutes by default and up to 120 days, and nothing lets an admin cap it. It is
  a convenience, not an admin control, and a `bqto-*` VM made by hand needs no
  deadline at all.
- The free binary puts it in the first subnet, by name order, of the service's
  VPC network and region that can host a VM. It has no flag to pick another.
- bqto Pro puts it in the subnet that `--subnet` names. Without the flag, it
  uses the subnet that the service names, as Cloud Run with Direct VPC egress set
  by subnet and Managed Kafka do. For Cloud Run, it takes the first subnet with
  Private Google Access. Otherwise, it takes the same subnet as the free binary.
- It carries the label `bqto=session` and a random name, `bqto-` plus eight hex
  characters. Nothing on it names the person who created it; the audit logs do.

Source: `internal/bastion/bastion.go`, `instanceResource`.

## What a developer can do

bqto adds no power a developer did not have: the roles it needs let them create a
VM and open a tunnel by hand. The table comparing a bqto session with the same
developer's own VM, and which control closes each row, is in
[permissions.md](permissions.md#what-the-developer-can-do-with-a-bastion). In short:

- **Other addresses in the VPC:** a bastion reaches what any VM there reaches. bqto
  does not limit it. To limit it, give bastions their own subnet with egress
  firewall rules, and grant `compute.subnetworks.use` on that subnet only. Then a
  bastion in any other subnet fails to create. This setup needs bqto Pro: its
  `--subnet` flag, or a profile, puts the sessions in that subnet. The free
  binary has no `--subnet`. It takes the first subnet in the region that can host
  a VM, by name order, so it works only when that subnet is the one for bastions.
- **Root on the bastion, a colleague's included:** with `roles/compute.osAdminLogin`
  (bqto warns when the session has it), or through `compute.instances.setMetadata`
  on any `bqto-*` VM. `compute.requireOsLogin` does not close the second path: a
  `startup-script` in the metadata plus a reset (`compute.instances.reset`, in
  `instanceAdmin.v1`) still gives root. From there a developer can read a
  colleague's traffic from the bastion to a service without TLS. Creating a
  bastion needs `setMetadata`, so the fix is the minimal custom role, which holds
  no `reset`, `stop` or `start`, together with `compute.requireOsLogin`. With
  `instanceAdmin.v1`, accept the risk.
- **Other ports of the bastion:** closed by granting `roles/iap.tunnelResourceAccessor`
  with the condition `destination.port == 22`, tested live.
- **Other VMs:** closed by the IAM condition that limits the role to `bqto-*`, tested
  live.

## The audit trail

| Question | Where the answer is | Cost | Can it be turned off |
|---|---|---|---|
| Who created and deleted the bastion | Admin Activity, `compute.instances.insert` and `delete` | free | no |
| Who logged in, to which bastion, with or without sudo | Data Access, `oslogin.googleapis.com` `CheckPolicy` | Data Access volume, inside the 50 GiB free tier for normal use | yes, it is a project setting |
| Whether a session ended cleanly | a `delete` by the person, or `compute.instances.deferredDelete` by `system@google.com` when bqto was not there | free | no |
| When the tunnel opened | nowhere: IAP writes nothing for TCP forwarding, even with its Data Access logs on | — | — |
| What was typed on the bastion | nowhere | — | — |
| What was done in the service | the service's own connection logs, which see the bastion's IP | per service | yes, they are off by default |

Tested live on 2026-09-24 and 2026-09-25. The queries for each row, including
the one that finds orphaned bastions, are in
[permissions.md](permissions.md#what-a-session-leaves-behind).

## What bqto assumes

- **One person per machine.** Every local port bqto opens listens on `127.0.0.1`,
  which keeps the network out but not other accounts on the same host. On a shared
  Linux host or a jump box, another user could use your session.
  bqto's hints keep tokens out of command lines, where `ps` shows them to every
  user. A port forward has no login: anyone on the machine can use it while the
  session is up. In bqto Pro, the `--web` proxy asks for the session secret, but
  the SOCKS proxy has no login either.
- **In CI, a job has the machine to itself.** GitHub-hosted runners give each job
  its own VM and delete it after the job. A self-hosted runner is as safe when it
  runs one job per VM and throws the VM away after it (an ephemeral runner). A
  self-hosted runner shared by several jobs or repositories at the same time is a
  risk: another job there can use your session, and with it your identity on the
  service.
- **The services still check who connects.** bqto gets a developer to the port; the
  database, cache or API decides whether they get in.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md).
