# Skill: Cluster Pool Troubleshooting on hosted-mgmt

## Purpose

Debug Hive-managed cluster pools running on the `hosted-mgmt` OpenShift cluster.
All commands use `oc --context hosted-mgmt` — never assume the current context is correct.

---

## Step 0 — Identify the pool and its namespace

Pool definitions live in the `release` repo under:

```
clusters/hosted-mgmt/hive/pools/<owner-namespace>/
```

The `namespace:` field in each `*_clusterpool.yaml` is the namespace on hosted-mgmt where
Hive creates ClusterDeployments, provision pods, and related resources.

Common pool namespaces:

| Owner directory      | Namespace on hosted-mgmt    |
|----------------------|-----------------------------|
| `openshift-ci/`      | `ci-cluster-pool`           |
| `cvp/`               | `cvp-cluster-pool`          |
| `serverless/`        | `serverless-cluster-pool`   |
| `rhdh/`              | `rhdh-cluster-pool`         |
| `konflux/`           | `konflux-cluster-pool`      |
| `openshift-observability/` | `obs-cluster-pool`    |
| `rh-openshift-ecosystem/`  | `rhoe-cluster-pool`   |
| *(others)*           | Check the YAML's `namespace:` field |

To find the namespace for an unknown pool, run from the `release` repo root:

```bash
grep -r "namespace:" clusters/hosted-mgmt/hive/pools/<owner-dir>/
```

Set a shell variable for convenience throughout the debug session:

```bash
NS=ci-cluster-pool   # replace with the actual namespace
CTX=hosted-mgmt
```

---

## Step 1 — Inspect the ClusterPool

```bash
# List all pools in the namespace
oc --context $CTX -n $NS get clusterpool

# Describe the specific pool to check status conditions and ready/standby counts
oc --context $CTX -n $NS describe clusterpool <pool-name>
```

Key fields to check in the output:

- `Status.Ready` — number of clusters ready to claim.
- `Status.Standby` — clusters installed but hibernating.
- `Status.Size` — current total managed by the pool.
- `Conditions` — look for `CapacityAvailable: False`, `AllClustersCurrentImages: False`, or `MissingDependencies: True`.

---

## Step 2 — List ClusterDeployments in the namespace

```bash
# Overview of all ClusterDeployments: installed state and provision status
oc --context $CTX -n $NS get clusterdeployment \
  -o custom-columns="NAME:.metadata.name,INSTALLED:.spec.installed,STAGE:.status.installRestarts,PROVISION:.status.provisionRef.name"

# Find specifically un-installed or failed deployments
oc --context $CTX -n $NS get clusterdeployment \
  -o jsonpath='{range .items[?(@.spec.installed==false)]}{.metadata.name}{"\n"}{end}'
```

For a suspicious deployment:

```bash
oc --context $CTX -n $NS describe clusterdeployment <name>
```

Look for:

- `Conditions` block — `ProvisionFailed`, `DNSNotReady`, `AuthenticationCertificateNotAvailable`.
- `Status.InstallRestarts` — high count means repeated failures.
- `Status.ProvisionRef` — points to the active ClusterProvision.

---

## Step 3 — Check ClusterProvisions

```bash
# List provisions in the namespace
oc --context $CTX -n $NS get clusterprovision

# Describe a specific provision
oc --context $CTX -n $NS describe clusterprovision <provision-name>
```

Key fields:

- `Stage` — `Provisioning`, `Failed`, `Complete`.
- `Conditions` — `JobCreated`, `Initialized`, `Succeeded`.
- `Status.AdminKubeconfigSecret` / `Status.AdminPasswordSecret` — populated only on success.

---

## Step 4 — Find provision and deprovision pods

Hive spawns `hive-install-*` pods for provisioning and `hive-deprovision-*` pods for deprovisioning.

```bash
# All install/deprovision pods — check phase at a glance
oc --context $CTX -n $NS get pod \
  -l hive.openshift.io/job-type \
  --sort-by='.status.startTime'

# Filter to only failing pods
oc --context $CTX -n $NS get pod \
  -l hive.openshift.io/job-type \
  --field-selector=status.phase!=Succeeded,status.phase!=Running
```

Common pod label selectors:

```bash
# Provision pods for a specific ClusterDeployment
oc --context $CTX -n $NS get pod \
  -l hive.openshift.io/cluster-deployment-name=<cd-name>

# Deprovision pods
oc --context $CTX -n $NS get pod \
  -l hive.openshift.io/cluster-deployment-name=<cd-name>,hive.openshift.io/job-type=deprovision
```

---

## Step 5 — Get error messages from provision/deprovision logs

```bash
# Full logs from a failing install pod (hiveutil container has the installer output)
oc --context $CTX -n $NS logs <pod-name> -c hiveutil --tail=100

# If the pod has multiple containers, list them first
oc --context $CTX -n $NS get pod <pod-name> -o jsonpath='{.spec.containers[*].name}'

# Then fetch logs per container
oc --context $CTX -n $NS logs <pod-name> -c <container-name> --tail=200

# Previous run logs (useful if the pod restarted)
oc --context $CTX -n $NS logs <pod-name> -c <container-name> --previous

# Grep for known error patterns
oc --context $CTX -n $NS logs <pod-name> -c hiveutil 2>&1 | grep -iE "error|fail|timeout|denied|quota"
```

For deprovision pods the main container is usually named `deprovision`:

```bash
oc --context $CTX -n $NS logs <deprovision-pod-name> -c deprovision --tail=200
```

---

## Step 6 — Check all other pods in the namespace for issues

```bash
# Overview of pod health in the namespace
oc --context $CTX -n $NS get pod --sort-by='.status.startTime'

# Pods not in Running or Succeeded state
oc --context $CTX -n $NS get pod \
  --field-selector=status.phase!=Running,status.phase!=Succeeded

# Describe a troubled pod
oc --context $CTX -n $NS describe pod <pod-name>
```

In `describe` output look for:

- `Events` section — `FailedScheduling`, `BackOff`, `OOMKilled`, `Evicted`.
- `State.Waiting.Reason` — `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`.
- `Last State.Terminated.Reason` and `Exit Code`.

```bash
# Quickly surface CrashLoopBackOff or OOMKilled pods
oc --context $CTX -n $NS get pod -o json | \
  jq -r '.items[] | select(.status.containerStatuses[]?.state.waiting.reason == "CrashLoopBackOff") | .metadata.name'
```

---

## Step 7 — Check Hive controller pods (hive namespace)

Problems are sometimes in the Hive controller itself, not the pool namespace.

```bash
# Hive runs in the hive namespace by default
oc --context $CTX -n hive get pod

# Controller logs (filter for the pool/CD name you are debugging)
oc --context $CTX -n hive logs -l control-plane=hive-controllers --tail=200 | grep <cd-name>

# HiveConfig status
oc --context $CTX get hiveconfig hive -o yaml | grep -A 20 "conditions:"
```

---

## Step 8 — Check recent events in the pool namespace

```bash
# All events sorted by time — fast way to see what went wrong recently
oc --context $CTX -n $NS get events \
  --sort-by='.lastTimestamp' | tail -40

# Filter to Warning events only
oc --context $CTX -n $NS get events \
  --field-selector=type=Warning \
  --sort-by='.lastTimestamp' | tail -20
```

---

## Quick-reference: common failure patterns

| Symptom | Where to look | Likely cause |
|---------|---------------|--------------|
| Pool `Ready` count stuck at 0 | `describe clusterpool` conditions | Quota exhaustion, cloud creds invalid, image pull failure |
| `ProvisionFailed` on ClusterDeployment | `describe clusterdeployment` + provision pod logs | Cloud API error, DNS failure, machine quota |
| Install pod `OOMKilled` | `describe pod` + previous logs | Insufficient node resources on hosted-mgmt |
| Deprovision pod stuck | Deprovision pod logs | Cloud resource still exists, creds issue |
| `ImagePullBackOff` on install pod | `describe pod` events | Hive installer image not accessible |
| High `InstallRestarts` | ClusterDeployment status | Transient cloud errors or a persistent config problem |

---

## References and further reading

- **Hive troubleshooting guide** (official):
  <https://github.com/openshift/hive/blob/master/docs/troubleshooting.md>

- **Hive ClusterPool documentation**:
  <https://github.com/openshift/hive/blob/master/docs/clusterpools.md>

- **Hive ClusterDeployment documentation**:
  <https://github.com/openshift/hive/blob/master/docs/using-hive.md>

- **Hive API types** (ClusterPool, ClusterDeployment, ClusterProvision conditions):
  <https://github.com/openshift/hive/tree/master/apis/hive/v1>

- **Pool definitions in the `release` repo** (namespace mapping source of truth):
  `clusters/hosted-mgmt/hive/pools/`

- **Test Platform SOP — cluster pool issues** (internal):
  <https://docs.google.com/document/d/1bBkqR1kMmulGSVbbRv2EQ4T86H7oMWNn4y_AKDpDlzE> *(check #forum-testplatform for the current link)*

---

## Tips

- Always confirm you are talking to the right cluster before running write operations:
  ```bash
  oc --context $CTX whoami --show-server
  ```
- The `oc --context hosted-mgmt` context must be present in your kubeconfig.
  Refresh it with the cluster-login skill if you get auth errors.
- Pool namespaces follow the pattern `<owner>-cluster-pool` but confirm from the YAML —
  some owners (e.g. `openshift-ci`) use `ci-cluster-pool` instead.
