# secret-shatterer

Tool for managing secret collections in OpenShift CI by extracting secrets from OCP clusters and generating member templates from Vault.

## Prerequisites

- `oc` CLI configured with access to the target OCP cluster
- `vault` CLI installed and authenticated (`vault login -method=oidc`)
- `RELEASE_REPO` environment variable set (or use `--release-repo` flag)

## Modes

### 1. Generate Member Template

Generates a YAML file mapping secret collections to their Vault group members.

```bash
./secret-shatterer --generate-member-template
```

**What it does:**
- Extracts all secrets from OCP cluster (default: `test-credentials` namespace, `app.ci` context)
- Matches them against Vault secrets in `kv/selfservice`
- For each matched secret, extracts the namespace from its Vault path
- Queries Vault identity group `secret-collection-manager-managed-{namespace}` for members
- Generates `secret-collections-members.yaml` with collection names and member lists
- Generates `obsolete-secrets.txt` with OCP secrets that don't have Vault backing

**Outputs:**
- `secret-collections-members.yaml` - Collection membership template
- `obsolete-secrets.txt` - List of obsolete secrets in OCP

### 2. Secret Sharding (Mass Migration)

Shards secrets from monolithic collections into individual GSM secrets.

```bash
./secret-shatterer
```

**What it does:**
- Extracts all secrets from OCP cluster
- Converts each secret into GSM format (creates individual secrets per key)
- Creates secrets in Google Secret Manager using `secret-manager.sh`
- Updates CI configurations to reference the sharded secrets

**Options:**
- `--dry-run` - Preview changes without creating secrets
- `--ocp-context` - Override OCP context (default: `app.ci`)
- `--ocp-namespace` - Override namespace (default: `test-credentials`)

### 3. Targeted Mode

Process only specific org/repo credentials instead of all secrets.

```bash
./secret-shatterer --targeted-mode --org=openshift --repo=origin
```

**What it does:**
- Scans CI configurations for the specified org/repo
- Extracts only the credentials used by that org/repo
- Shards those specific secrets to GSM

**Options:**
- `--org` - GitHub organization (required in targeted mode)
- `--repo` - GitHub repository (required in targeted mode)

## Common Options

- `--release-repo` - Path to local openshift/release repository
- `--log-level` - Set logging level (debug, info, warn, error) (default: `debug`)
- `--gsm-project-name` - GSM project name (default: `openshift-ci-secrets`)
- `--gsm-project-number` - GSM project number (default: `384486694155`)

## Examples

```bash
# Generate member template with info logging
./secret-shatterer --generate-member-template --log-level=info

# Dry run for specific org/repo
./secret-shatterer --targeted-mode --org=kubernetes --repo=kubernetes --dry-run

# Full migration with custom release repo path
./secret-shatterer --release-repo=/path/to/release
```
