# Troubleshooting

This page is for bqto users whose session fails. It lists the tunnel errors that
bqto recognizes, and tells you how to get more detail when a `Fix:` line is not
enough.

## Tunnel errors

When the tunnel fails, bqto recognizes these errors and prints the cause and the
fix:

| What you see | Cause | Fix |
|---|---|---|
| `4033` | IAP refused the tunnel | check `roles/iap.tunnelResourceAccessor`, and that the bastion is still running |
| `4003` | IAP could not reach port 22 on the bastion | allow ingress on tcp:22 from `35.235.240.0/20` to the bastion |
| `… API has not been used in project … or it is disabled` under `last stderr:` | gcloud needs an API that is off | enable the API that line names, for example `gcloud services enable oslogin.googleapis.com`. [permissions.md](permissions.md#apis) lists the APIs to enable |
| `4047` | IAP could not find the bastion | it is still starting or was stopped; run `bqto connect` again |
| `Permission denied (publickey)` | OS Login refused the SSH certificate. A new bastion can refuse it while it boots, so bqto retries for up to a minute before it reports this | grant `roles/compute.osLogin` on the project |
| `SSH using certificates is not yet generally available` | gcloud is older than 550.0.0 | `gcloud components update`, or your package manager (`apt`, `dnf`, `brew`) if you installed gcloud with one |
| `Address already in use`, `Cannot bind port` | another program took the tunnel's port while the tunnel started | run `bqto connect` again |
| `ADC account (…) does not match the active gcloud account (…)` | the two logins use different accounts | run the command bqto prints; both logins must be the same account |
| `x509: certificate signed by unknown authority` | this machine has no CA certificates (common in slim containers), or a TLS-inspecting proxy shows its own certificate | install them (`apt-get install ca-certificates` on Debian or Ubuntu), or add the proxy's CA |

Every other failure says what bqto was doing and why it failed. It ends with a
`Fix:` line, such as the permission to ask for, the quota to raise or the
command to run. [permissions.md](permissions.md) lists every role.

## Debug output

If the `Fix:` line is not enough, run the same command with `--debug`. Put the
flag after `connect` or before it, as in `bqto connect --debug ...` or
`bqto --debug connect ...`. For the wizard, run `bqto --debug`. You can also set
`BQTO_DEBUG=1`.

bqto then writes these lines to stderr, each on a line of its own that starts
with `debug:`:

- The Google API calls and how long they took.
- Each retry.
- Each `gcloud` command.
- The bastion that bqto creates and deletes.
- Each new try to open the tunnel, with its number and gcloud's last lines from
  the try before.
- Each tunnel reconnect, with gcloud's last output.

The debug output leaves out tokens and the arguments of the command after `--`,
which can hold passwords. It does show project, instance, network and bastion
names. Read it before you paste it into a public bug report.
