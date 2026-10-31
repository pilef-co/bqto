# bqto in GitHub Actions

This page is for teams that want a GitHub Actions job to reach a private
database through bqto, for example to run a migration on deploy or a smoke test
after it. The job runs on a GitHub-hosted runner, with no service account key
and no runner inside the VPC. The session lasts as long as the command.

The quickest way is [pilef-co/bqto-action](https://github.com/pilef-co/bqto-action),
which ships bqto and runs your command through the tunnel. This page shows
the setup behind it (Workload Identity Federation, roles and the firewall rule)
and an example workflow that runs bqto directly.

## The workflow

Save the workflow as `.github/workflows/deploy.yml`. The setup further down
trusts that file name. Replace the example values with yours: the project
`my-project`, the instance `sql-main`, the service account
`migrations@my-project`, the `production` environment, the `DB_PASSWORD`
secret and the `./migrate up` command.

```yaml
name: deploy

on:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  migrate:
    runs-on: ubuntu-24.04
    environment: production
    permissions:
      contents: read
      id-token: write
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
        with:
          persist-credentials: false
      - uses: google-github-actions/auth@7c6bc770dae815cd3e89ee6cdf493a5fab2cc093 # v3.0.0
        with:
          workload_identity_provider: ${{ vars.WIF_PROVIDER }}
          service_account: migrations@my-project.iam.gserviceaccount.com
      - uses: google-github-actions/setup-gcloud@aa5489c8933f4cc7a4f7d45035b3b1440c9c10db # v3.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version: "1.26.x"
          cache: false
      - name: install bqto
        run: go install github.com/pilef-co/bqto/cmd/bqto@COMMIT
      - name: migrate
        env:
          PGPASSWORD: ${{ secrets.DB_PASSWORD }}
        run: |
          exec bqto connect --no-input --duration 15m \
            --service cloudsql --project my-project --instance sql-main \
            -- ./migrate up
```

Notes on the workflow:

- Every action is pinned to a commit, with its version in the comment, as
  bqto's own workflows are. The job holds a token for the service account and
  the database password, so a moved tag must not change what runs.
- bqto is pinned to a commit. Replace `COMMIT` with the full hash of a commit of
  `main`, or of a release tag such as `v0.16.0`. Go checks what it downloads
  against its checksum database, so a moved tag cannot change the code.
- The command gets the address from bqto. bqto sets `PGHOST` and `PGPORT` for
  it, so the job does not pick a port.
- The exit code is the command's, so a failed migration fails the job. bqto
  deletes the bastion either way.
- If someone cancels the job, the runner sends a signal. `exec` makes bqto the
  step's process, so the signal reaches bqto and it deletes the bastion. If the
  runner kills bqto first, `--duration` is the backstop: Compute Engine deletes
  the bastion when it runs out. Keep it as short as the job allows.
- The database login is separate from bqto. Use a password from the
  environment's secrets, as above, or an IAM token from
  `gcloud sql generate-login-token`.

## Setting it up

Someone who can grant IAM in the project does this once. Workload Identity
Federation signs the job in as the service account, for gcloud and for
Application Default Credentials alike, so bqto runs as that account.

The provider below hands out a token only to the `production` environment of
`deploy.yml`, run from `main` of your repository. It refuses a pull request,
another workflow or `deploy.yml` on another branch.

The script enables the APIs in [permissions.md](permissions.md#apis), with
`sqladmin.googleapis.com` for Cloud SQL. CI also needs
`iamcredentials.googleapis.com` and `sts.googleapis.com`, because Workload Identity Federation uses them to exchange the GitHub token for
a token of the service account.

```sh
PROJECT=my-project
NUMBER=$(gcloud projects describe $PROJECT --format='value(projectNumber)')
REPO=my-org/my-app
read -r REPO REPO_ID < <(gh api repos/$REPO --jq '"\(.full_name) \(.id)"')
SA=migrations@$PROJECT.iam.gserviceaccount.com

gcloud services enable compute.googleapis.com iap.googleapis.com oslogin.googleapis.com \
  cloudresourcemanager.googleapis.com sqladmin.googleapis.com \
  iamcredentials.googleapis.com sts.googleapis.com --project $PROJECT

gcloud iam service-accounts create migrations --project $PROJECT

gcloud iam workload-identity-pools create github-deploy --project $PROJECT --location global
gcloud iam workload-identity-pools providers create-oidc github --project $PROJECT \
  --location global --workload-identity-pool github-deploy \
  --issuer-uri https://token.actions.githubusercontent.com \
  --attribute-mapping google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_id=assertion.repository_id,attribute.ref=assertion.ref,attribute.environment=assertion.environment,attribute.job_workflow_ref=assertion.job_workflow_ref \
  --attribute-condition "assertion.repository == '$REPO' && assertion.repository_id == '$REPO_ID' && assertion.ref == 'refs/heads/main' && assertion.environment == 'production' && assertion.job_workflow_ref == '$REPO/.github/workflows/deploy.yml@refs/heads/main'"

gcloud iam service-accounts add-iam-policy-binding $SA --project $PROJECT \
  --role roles/iam.workloadIdentityUser \
  --member "principalSet://iam.googleapis.com/projects/$NUMBER/locations/global/workloadIdentityPools/github-deploy/attribute.repository/$REPO"
```

The condition compares the names case-sensitively, which is why the script
reads `REPO` back from GitHub with its exact case. `repository_id` refuses a
repository that someone deletes and creates again with the same name. The
provider also refuses a renamed repository, until you update the condition and
the binding with the new name.

### Roles

Use the [minimal custom role](permissions.md#a-minimal-custom-role), with the
Cloud SQL lines, limited to VMs and disks named `bqto-*` by the condition in
[Narrowing the grant](permissions.md#narrowing-the-grant). Save the two files
from there as `bqto-role.yaml` and `bqto-condition.yaml`. In the condition,
replace `P` with your project ID and list the zones of your region:

```sh
gcloud iam roles create bqtoUser --project=$PROJECT --file=bqto-role.yaml
gcloud projects add-iam-policy-binding $PROJECT --member=serviceAccount:$SA \
  --role=projects/$PROJECT/roles/bqtoUser --condition-from-file=bqto-condition.yaml
```

Without Shared VPC, that is all bqto needs. With Shared VPC, add the network project roles from
[permissions.md](permissions.md#how-to-grant-it). Do not give the account
`roles/editor`: it cannot open the tunnel, and it can change almost anything
else in the project.

The project-wide predefined roles in
[permissions.md](permissions.md#how-to-grant-it) also work. But
`roles/compute.instanceAdmin.v1` lets the account manage every VM in the project
and gives it sudo on all of them through OS Login. Avoid them for an identity
that no one watches.

### Repository settings

Last, set these in the repository's Settings:

- Environments → New environment `production`, with required reviewers, and
  deployment branches limited to `main`. Put `DB_PASSWORD` in its secrets,
  not in the repository's, so only an approved run of this job can read it.
- Secrets and variables → Actions → Variables: `WIF_PROVIDER` set to
  `projects/NUMBER/locations/global/workloadIdentityPools/github-deploy/providers/github`.
