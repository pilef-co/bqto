# Permissions

bqto acts as you: it uses your `gcloud` login and your Application Default
Credentials, never a service account of its own. This page lists what your
account needs.

Two projects matter:

- **Service project:** where the instance lives. bqto creates the bastion here.
- **Network project:** the project that owns the VPC network. It is the same
  project unless you use Shared VPC, where it is the host project.

## Roles

| Where | Role | Used for |
|---|---|---|
| Service project | `roles/compute.instanceAdmin.v1` | Creating and deleting the bastion |
| Service project | `roles/iap.tunnelResourceAccessor` | Opening the IAP tunnel |
| Service project | `roles/compute.osLogin` | Logging in to the bastion with OS Login |
| Service project, as the quota project | `roles/serviceusage.serviceUsageConsumer` | Letting Google check API enablement and quota there ([below](#which-project-google-checks-apis-and-quota-in)) |
| Service project | The viewer role of the service: `roles/cloudsql.viewer`, `roles/alloydb.viewer`, `roles/redis.viewer` or `roles/memorystore.viewer` (Valkey) | Finding the instance and its private IP |
| Network project | `roles/compute.networkViewer` | Choosing a subnet and checking the firewall |
| Network project, on the subnet | `roles/compute.networkUser` | Placing the bastion in a Shared VPC subnet |
| Organization (optional) | `roles/browser` | The wizard's list of organizations and folders |

With bqto Pro, these roles are also needed:

| Where | Role | Used for |
|---|---|---|
| Service project | `roles/managedkafka.viewer` or `roles/run.viewer` | Finding the Managed Kafka cluster or the Cloud Run service |
| Cloud Run service | `roles/run.invoker` | Calling a service that requires authentication |
| Image project, only with `--image` | `roles/compute.imageUser` | Creating the bastion from an image in another project |

**`roles/editor` is not enough.** It does not include
`iap.tunnelInstances.accessViaIAP`, so the tunnel fails. Add
`roles/iap.tunnelResourceAccessor`. bqto checks this before it creates the
bastion and prints the command to grant it.

In the same call, bqto checks that the project exists. It asks
`testIamPermissions`, which needs no role, for `resourcemanager.projects.get`
too. When the answer does not include it, bqto reads the project, which needs
`resourcemanager.projects.get`: `roles/compute.instanceAdmin.v1` and the custom
role below hold it. When the project does not exist or the account cannot see
it, bqto stops with `gcloud projects list` as the fix.

## APIs

bqto calls these APIs, so they must be enabled in the service project:

| API | Used by |
|---|---|
| `compute.googleapis.com` | bqto: the bastion VM, subnets and firewall checks |
| `cloudresourcemanager.googleapis.com` | bqto: the project lookup and the IAP permission check, and the wizard's project list |
| `sqladmin.googleapis.com`, `alloydb.googleapis.com`, `redis.googleapis.com` or `memorystore.googleapis.com` | bqto: finding the Cloud SQL, AlloyDB, Redis or Valkey instance |

If one of them is disabled, bqto prints the `gcloud services enable` command as
its `Fix:` line.

Also enable `iap.googleapis.com` and `oslogin.googleapis.com`. gcloud uses IAP
for the tunnel and the OS Login API for the short-lived SSH certificate. Google's
setup pages for
[IAP TCP forwarding](https://docs.cloud.google.com/iap/docs/using-tcp-forwarding)
and [OS Login](https://docs.cloud.google.com/compute/docs/oslogin/set-up-oslogin)
do not list them. When one of them is disabled and gcloud needs it, the tunnel
fails, and bqto shows gcloud's line under `last stderr:`.

```sh
gcloud services enable compute.googleapis.com cloudresourcemanager.googleapis.com \
  iap.googleapis.com oslogin.googleapis.com sqladmin.googleapis.com --project=PROJECT
```

Replace `sqladmin.googleapis.com` with the API of your service.

## Which project Google checks APIs and quota in

Google checks that an API is enabled, and counts its quota, in a **quota
project**. As `gcloud` does by default, bqto uses the project of each call:

- The lookup of the service, the IAP check and the bastion use the service
  project, also for the calls on a Shared VPC host project.
- In a bqto Pro stack, each service uses its own project for its lookup. The
  IAP check and the bastion use the first profile's project.
- The wizard's list of organizations, folders and projects uses your gcloud
  project (`gcloud config get project`), and falls back to your ADC quota
  project. When both refuse it, the list is sent with no quota project, as
  `gcloud projects list` does. Only when that fails too does the wizard warn
  and ask you to type the project.

That project needs the Compute Engine API, the Cloud Resource Manager API and
the service's API enabled, and you need `roles/serviceusage.serviceUsageConsumer`
on it (Owner and Editor include it).

When that project refuses a call, because you lack that role or the API is
disabled there, bqto sends the call again with the quota project of your ADC
login (set by `gcloud auth application-default login`). It works when that
project has the API and you hold the role on it. When both refuse, the error
names the project of the call and the fix.

Two cases skip this:

- `GOOGLE_CLOUD_QUOTA_PROJECT` is set: every call uses that project. It always
  wins.
- ADC is a service account (a key, impersonation or the metadata server): the
  Google client libraries' default applies, the quota project in the ADC file if
  it has one. gcloud also leaves service accounts alone.

## How to grant it

Set the project and who gets the roles: a developer or a service account.

```sh
PROJECT=my-project
MEMBER=user:dev@example.com
# or: MEMBER=serviceAccount:bqto@my-project.iam.gserviceaccount.com
```

Service project. Use the viewer role of your service:

```sh
gcloud projects add-iam-policy-binding $PROJECT --member=$MEMBER --role=roles/compute.instanceAdmin.v1 --condition=None
gcloud projects add-iam-policy-binding $PROJECT --member=$MEMBER --role=roles/iap.tunnelResourceAccessor --condition=None
gcloud projects add-iam-policy-binding $PROJECT --member=$MEMBER --role=roles/compute.osLogin --condition=None
gcloud projects add-iam-policy-binding $PROJECT --member=$MEMBER --role=roles/cloudsql.viewer --condition=None
```

`--condition=None` makes the grant unconditional. gcloud asks for it once the
project has a conditional grant.

Network project, which is the same project unless you use Shared VPC:

```sh
NETWORK_PROJECT=my-host-project
gcloud projects add-iam-policy-binding $NETWORK_PROJECT --member=$MEMBER --role=roles/compute.networkViewer --condition=None
```

Shared VPC only, on the subnet where bqto places the bastion:

```sh
gcloud compute networks subnets add-iam-policy-binding SUBNET --region=REGION --project=$NETWORK_PROJECT --member=$MEMBER --role=roles/compute.networkUser
```

bqto Pro, Cloud Run, only for a service that requires authentication:

```sh
gcloud run services add-iam-policy-binding SERVICE --region=REGION --project=$PROJECT --member=$MEMBER --role=roles/run.invoker
```

bqto Pro, only with `--image`, when the image lives in a project of your own (the
default Debian image is public and needs nothing):

```sh
gcloud projects add-iam-policy-binding IMAGE_PROJECT --member=$MEMBER --role=roles/compute.imageUser --condition=None
```

The quota project, which is the service project unless bqto falls back to
your ADC quota project or you set `GOOGLE_CLOUD_QUOTA_PROJECT`:

```sh
gcloud projects add-iam-policy-binding QUOTA_PROJECT --member=$MEMBER --role=roles/serviceusage.serviceUsageConsumer --condition=None
```

Organization, optional. Without it, the wizard still lists your projects, but
shows folder numbers instead of names. For a project in a folder it cannot
list, the wizard reads the project's ancestry (`projects.getAncestry`, which
needs `resourcemanager.projects.get` on the project, the permission that lists
it) to place it under its organization. When that fails too, the project goes
under "No organization".

```sh
gcloud organizations add-iam-policy-binding ORG_ID --member=$MEMBER --role=roles/browser --condition=None
```

bqto checks that ADC and `gcloud` use the same account. To run as a service
account without a key, impersonate it in both; you need
`roles/iam.serviceAccountTokenCreator` on it:

```sh
SA=bqto@my-project.iam.gserviceaccount.com
gcloud config set auth/impersonate_service_account $SA
gcloud auth application-default login --impersonate-service-account $SA
```

A new grant of that role can take a few minutes to apply; until then bqto says
your account cannot impersonate the service account. The `gcloud config` line
applies to every gcloud command, not only bqto, and
`gcloud config unset auth/impersonate_service_account` goes back to your own
account. With a key file instead, run `gcloud auth activate-service-account
--key-file=KEY_FILE` and point `GOOGLE_APPLICATION_CREDENTIALS` at the same file.
Workload Identity Federation, as a GitHub Actions workflow gets it, works the
same way as impersonation.

For Cloud Run, bqto Pro mints the identity token as that service account, for the
service's URL (`gcloud auth print-identity-token --impersonate-service-account=$SA
--audiences=URL`, without the first flag for a key file), in `--web` and in the
`try:` line. The account needs `roles/run.invoker` on the service.

## A minimal custom role

`roles/compute.instanceAdmin.v1` is broad: it can manage every VM in the
project and gives sudo on all of them through OS Login. This custom role for
the service project holds only what bqto uses. Save it as `bqto-role.yaml`, and
keep the lines of the services you connect to.

```yaml
title: bqto user
description: What bqto needs in the service project
stage: GA
includedPermissions:
- resourcemanager.projects.get
- compute.projects.get
- compute.zones.list
- compute.networks.list
- compute.zoneOperations.get
- compute.instances.get
- compute.instances.list
- compute.instances.create
- compute.instances.delete
- compute.disks.create
- compute.instances.setMetadata
- compute.instances.setLabels
- compute.instances.setTags
- compute.instances.osLogin
- iap.tunnelInstances.accessViaIAP
# Cloud SQL
- cloudsql.instances.list
- cloudsql.instances.get
# AlloyDB
- alloydb.instances.list
- alloydb.instances.get
- alloydb.clusters.get
# Memorystore for Redis
- redis.instances.list
- redis.instances.get
# Memorystore for Valkey
- memorystore.instances.list
- memorystore.instances.get
# bqto Pro, Memorystore for Redis Cluster
- redis.clusters.list
- redis.clusters.get
# bqto Pro, Managed Kafka
- managedkafka.clusters.list
- managedkafka.clusters.get
# bqto Pro, Cloud Run
- run.services.list
- run.services.get
# bqto Pro, only with --image, when the image is in this project:
- compute.images.get
- compute.images.getFromFamily
- compute.images.useReadOnly
# Only when the network is in this project (no Shared VPC):
- compute.networks.get
- compute.networks.getEffectiveFirewalls
- compute.networks.getRegionEffectiveFirewalls
- compute.subnetworks.list
- compute.subnetworks.get
- compute.subnetworks.use
```

```sh
gcloud iam roles create bqtoUser --project=$PROJECT --file=bqto-role.yaml
gcloud projects add-iam-policy-binding $PROJECT --member=$MEMBER --role=projects/$PROJECT/roles/bqtoUser --condition=None
```

Without Shared VPC, this role alone replaces the service-project and
network-project roles in the table, IAP and OS Login included. A project-level
custom role can only be granted in its own project. With Shared VPC, keep the
predefined roles in the network project.

Bastions set `enable-oslogin-certificates=TRUE`. So `gcloud compute ssh` asks
the OS Login API for a short-lived certificate (`signSshPublicKey`) instead of
adding a key to your OS Login profile. Google's OS Login audit-log page lists
`compute.instances.osLogin` (or `compute.instances.osAdminLogin`) as the
permission that call checks, on the bastion. `roles/compute.osLogin` and this
custom role already hold it, so certificates need nothing more. The bastion has
no service account, so `roles/iam.serviceAccountUser` is not needed either.

gcloud also reads your OS Login profile (`getLoginProfile`), and creates it on
your first login. Those calls act on your own profile and need no role. After
the session, bqto reads the bastion's numeric ID to remove its host key and
certificate from `~/.ssh`. That read uses `compute.instances.get`, which the
session already needs to check the bastion's status.

These permissions were checked against Google's documentation on 2026-10-16
([audit logs](https://cloud.google.com/compute/docs/oslogin/view-audit-logs),
[roles](https://cloud.google.com/iam/docs/roles-permissions/compute)), not with
a live test of a role without them.

## What a session leaves behind

This table shows what the audit trail records for a bqto session, with nothing
installed and no service account on the bastion. A security team usually asks
for it before it grants anything.

| Question | Where the answer is | Cost | Can it be turned off |
|---|---|---|---|
| Who created and deleted the bastion | Admin Activity, `compute.instances.insert` and `delete` | free | no, they are always written |
| Who logged in, and to which bastion | Data Access, `oslogin.googleapis.com`, method `CheckPolicy`, naming the person and the instance | Data Access volume | yes, it is a project setting |
| Whether the session had sudo | the same entry, `policy: ADMIN_LOGIN` | — | — |
| When the tunnel opened | nowhere. IAP writes nothing for TCP forwarding, even with its Data Access logs on | — | — |
| What was typed on the bastion | nowhere; see below | — | — |
| What was done in the service | the service's own connection logs, which see the bastion's IP | per service | yes, they are off by default |

The bastion's private IP ties the service's logs back to a session. The VM exists
for minutes and forwards to one endpoint, so every connection from that address
in that window came through it. The OS Login entry says who was on it. That can
be more than one person. Anyone with the same roles can log in to a colleague's
bastion while it lives, because IAM grants them every `bqto-*` VM, not only
their own. The trail still names them.

Who created a `bqto-*` VM:

```sh
gcloud logging read \
  'logName:"cloudaudit.googleapis.com%2Factivity"
   AND protoPayload.methodName="v1.compute.instances.insert"
   AND protoPayload.resourceName:"instances/bqto-"' \
  --project $PROJECT --freshness 7d --limit 20 \
  --format='table(timestamp, protoPayload.authenticationInfo.principalEmail, protoPayload.resourceName.scope(instances))'
```

Who logged in to one:

```sh
gcloud logging read \
  'protoPayload.serviceName="oslogin.googleapis.com"
   AND protoPayload.methodName:"CheckPolicy"' \
  --project $PROJECT --freshness 7d --limit 20 \
  --format='table(timestamp, protoPayload.authenticationInfo.principalEmail, protoPayload.request.instance, protoPayload.request.policy)'
```

How a session ended is in the same logs. Every delete a person causes carries
their email, and the user agent says which path they took: `Go-http-client/2.0`
is bqto cleaning up, `google-cloud-sdk …` is gcloud by hand, `Mozilla/…` is the
console. Reaching the end of `--duration` is not by itself the odd case: if bqto
is still running it deletes the bastion like any other exit. The odd case is a
bqto that is no longer running, because it was killed, the laptop slept or the
terminal closed. Then the server-side deadline closes it instead, as
`compute.instances.deferredDelete` by `system@google.com`. So an orphan is an
`insert` by a person with no matching `delete`:

```sh
gcloud logging read \
  'protoPayload.methodName="compute.instances.deferredDelete"
   AND protoPayload.resourceName:"instances/bqto-"' \
  --project $PROJECT --freshness 7d --limit 20 \
  --format='table(timestamp, protoPayload.resourceName.scope(instances))'
```

Turning on the service's own connection log, so the two can be joined:

```sh
# Cloud SQL for PostgreSQL, and AlloyDB
gcloud sql instances patch INSTANCE --project $PROJECT --database-flags=log_connections=on
gcloud alloydb instances update INSTANCE --cluster CLUSTER --region REGION --project $PROJECT \
  --database-flags=log_connections=on

# Cloud SQL for MySQL keeps it in the general log
gcloud sql instances patch INSTANCE --project $PROJECT --database-flags=general_log=on,log_output=FILE
```

Each of these writes to the instance's own logs in Cloud Logging, and each has a
cost in log volume and, for MySQL, in instance performance. These flags come from
Google's documentation and were not tested live. This page does not cover the
connection logs of Memorystore, Managed Kafka or Cloud Run.

You can record what is typed on the bastion, but it weakens the bastion. It
needs a logging agent on the VM, and the agent needs a service account with
`roles/logging.logWriter`. Anyone with a shell can read that token from the
metadata server. So the person being watched can write into the log that watches
them, and the bastion stops being a machine that holds no credential. If that
trade is worth it in your organization, you need these pieces:

- A service account on the instance.
- Private Google Access on the subnet, so that the agent can reach Cloud Logging
  without internet.
- The agent baked into your own image.

Enabling Data Access logs for `iap.googleapis.com` is not worth it: tested on
2026-09-25, a full session produced no entries under that service.

## What the developer can do with a bastion

This section answers whether bqto lets a developer do more than they could
already. It compares a bqto session with a VM that the developer can create by
hand with the roles bqto needs.

| What a developer could do | Through a bqto session | Through a VM of their own | What closes it |
|---|---|---|---|
| Reach the service's port | yes, that is the product: one local port per service | yes | the service's own credentials; bqto carries none |
| Reach any other address in the VPC | yes, like any VM | yes | the service's own authentication, or the network's egress rules |
| Reach the internet from the VM | no external IP, but yes if its subnet has Cloud NAT, which bqto does not check | yes, with an external IP or through Cloud NAT | grant `compute.subnetworks.use` only on a subnet without Cloud NAT, and pick it with bqto Pro's `--subnet` |
| Call a Google API as the VM | no: the bastion has no service account | yes, if they attach one | the instance is created without `serviceAccounts` |
| Run commands as root on the VM | through OS Login, only with `roles/compute.osAdminLogin` or `instanceAdmin.v1`, and bqto warns when the session has it. Also through metadata: `compute.instances.setMetadata`, which every role that creates a bastion holds, can turn OS Login off on any `bqto-*` VM, a colleague's included, and add an SSH key with sudo, or add a `startup-script` that runs as root at the next boot | yes, on their own VM | `roles/compute.osLogin`, the organization policy `compute.requireOsLogin`, which keeps OS Login on whatever the metadata says, and no `compute.instances.reset`, `stop` or `start`, as in the minimal custom role. `instanceAdmin.v1` holds `reset` |
| SSH in from inside the VPC | only if an ingress rule allows it. bqto warns when the rule that lets IAP in is wider than `35.235.240.0/20`; it does not look at other rules, such as `default-allow-internal` | yes | ingress rules limited to the IAP range |
| Open the tunnel to another port of the VM | no, with the `destination.port == 22` condition on `roles/iap.tunnelResourceAccessor` | — | the IAM condition in "Narrowing the grant" |
| Create a VM that is not a bastion | no, with the `bqto-*` IAM condition | yes, without it | the IAM condition in "Narrowing the grant" |
| Leave a machine running | up to 120 days: the VM deletes itself at the deadline, but the developer picks it with `--duration` | yes | none in bqto: an admin cannot cap `--duration` |
| Do all this without being named | no: creation, login and deletion are in the audit logs | the same | Admin Activity and OS Login logs, above |

## What a bastion can reach

A bastion reaches what any VM in the VPC reaches: by default, every private
address. bqto does not limit that, because it adds no power the developer did not
have: the same roles let them create a VM and open a tunnel by hand. What decides
what they can use is the service's own authentication, or egress firewall rules
that the network admin may already have.

## Narrowing the grant

- **Only bastions.** An IAM condition can limit `instanceAdmin.v1` (or the
  custom role) to VMs and disks whose names start with `bqto-`. List every zone
  of the region, and cover both instances and disks, because the boot disk takes
  the VM's name. Save it as `bqto-condition.yaml`, with your project ID in place
  of `P`:

  ```yaml
  title: bqto-bastions-only
  expression: >-
    (resource.type == 'compute.googleapis.com/Instance' && (
      resource.name.startsWith('projects/P/zones/europe-west1-b/instances/bqto-') ||
      resource.name.startsWith('projects/P/zones/europe-west1-c/instances/bqto-') ||
      resource.name.startsWith('projects/P/zones/europe-west1-d/instances/bqto-'))) ||
    (resource.type == 'compute.googleapis.com/Disk' && (
      resource.name.startsWith('projects/P/zones/europe-west1-b/disks/bqto-') ||
      resource.name.startsWith('projects/P/zones/europe-west1-c/disks/bqto-') ||
      resource.name.startsWith('projects/P/zones/europe-west1-d/disks/bqto-'))) ||
    (resource.type != 'compute.googleapis.com/Instance' && resource.type != 'compute.googleapis.com/Disk')
  ```

  Then grant the role with it:

  ```sh
  gcloud projects add-iam-policy-binding $PROJECT --member=$MEMBER --role=projects/$PROJECT/roles/bqtoUser --condition-from-file=bqto-condition.yaml
  ```

  A condition allows at most 12 logical operators. This one has 9, and a region
  with four zones needs 11. With predefined roles, put the same condition on
  `roles/compute.osLogin` too, so that logins are limited to bastions.

  Tested on 2026-09-23 with the custom role (Cloud SQL lines) granted to a
  service account, in a project without Shared VPC:

  - bqto created the bastion, opened the tunnel and deleted the bastion. The
    boot disk was deleted with it, without `compute.disks.delete`.
  - Creating a VM with any other name failed with
    `Required 'compute.instances.create' permission`.
  - SSH to a VM with any other name failed with `Permission denied (publickey)`,
    even with `roles/compute.viewer` added: OS Login applies the condition. The
    IAP tunnel to that VM still opened, since IAP cannot filter by VM name.
- **Only SSH through IAP.** IAP conditions cannot match VM names, but they can
  match the port: grant `roles/iap.tunnelResourceAccessor` with the condition
  `destination.port == 22`. Tested on 2026-09-29: bqto works, and a tunnel to any
  other port of the bastion fails with `4033: 'not authorized'`. Google cannot check
  a conditional grant in advance, so bqto prints a warning that the permission may
  be missing, and goes on.
- **No root through metadata.** Creating a VM with metadata needs
  `compute.instances.setMetadata`. With it, anyone can set
  `enable-oslogin=FALSE` and an SSH key on a bastion, and the guest agent gives
  that key sudo. They can also set a `startup-script` that runs as root when the
  VM boots again. The organization policy `compute.requireOsLogin` makes OS Login
  apply whatever the instance metadata says. The startup script needs a reboot,
  so also leave out `compute.instances.reset`, `stop` and `start`, as the
  minimal custom role does:

  ```sh
  gcloud resource-manager org-policies enable-enforce compute.requireOsLogin --project $PROJECT
  ```

  This comes from Google's documentation and was not tested live.
- **Only for a while.** Privileged Access Manager can grant the role for the
  length of a session. It works with predefined and custom roles, not with
  Owner or Editor.
