# Local testing example

A minimal config for exercising the provider against a **local** Barndoor
platform (`make tilt`) using a locally-built binary — no registry, no
`terraform init`.

Full walkthrough: [`../../CONTRIBUTING.md`](../../CONTRIBUTING.md).

Quick version (after `make tilt` + `make trust-certs` in your `bdai-platform`
checkout, and `make dev-install` in this repo):

```bash
export BARNDOOR_CLIENT_ID=...        # see CONTRIBUTING.md section 3
export BARNDOOR_CLIENT_SECRET=...
export BARNDOOR_ORGANIZATION_ID=...

# dev_overrides scoped to this dir (alternative to editing ~/.terraformrc):
export TF_CLI_CONFIG_FILE=$PWD/dev.tfrc   # edit the path inside dev.tfrc first

terraform plan
terraform apply
terraform state show barndoor_log_export.audit   # verify
terraform destroy
```

`main.tf` uses `enabled = false` and a dummy S3 destination, so it stores config
without any S3 connectivity probe — safe to run with no real bucket.
