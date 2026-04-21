
# Why do we need this?

## Workload segmentation
We have several workload types: builds, short-running tests, long-running tests, and prowjobs. They differ in duration and resource shape, so we segment them.

### RuntimeClass and kubelet overhead
Builds stress the container runtime in ways the pod autoscaler does not fully model. You can address reserve CPU with `KubeletConfig` on self-managed OpenShift; we also use **`RuntimeClass`** so admission applies **overhead** CPU and memory per pool. That keeps build and test pods on different overhead profiles (for example, extra CPU headroom for builds) without relying on a single global kubelet recipe.

Without enough CPU, the runtime can surface errors such as `context deadline exceeded` and builds can fail.

### IOPS, instance types, analysis
Segmentation lets us tune IOPS and machine types per class and reason about capacity without mixing unrelated workloads on the same node pool.

## Kubernetes scheduling and the cluster autoscaler
By default the scheduler tends to **spread** pods (least-allocated style). Our CI load is often bursty: many pods, then scale-out, then relatively sparse placement—so spread scheduling interacts badly with **machine** scale-out/scale-in.

The **cluster autoscaler** scales **MachineSets up** when there are **unschedulable** pods that match a pool. It is conservative about evicting workloads that are not clearly safe to move; many CI pods are not owned by ReplicaSets in the usual way, which can leave nodes busy longer than we want.

**This webhook does not replace the cluster autoscaler for scale-up.** It coordinates **scale-down** and placement pressure so we can reclaim nodes on a useful timeline (see [Scale-down control loop](#scale-down-control-loop)). In operation, the **ci-scheduling-webhook** service account may **patch `MachineSet` and node objects** as part of that path—distinct from the machine-controller’s behavior when **you** change `MachineSet` replicas.

Tight **scheduling** vs **kubelet** resource accounting was a recurring issue in older Kubernetes releases ([kubernetes#106884](https://github.com/kubernetes/kubernetes/issues/106884#issuecomment-1005074672)). On **current** OpenShift/Kubernetes versions, treat that history as **background**: validate tight packing under your real jobs instead of assuming the same failure mode as pre–1.23-era clusters.

## Cluster autoscaler, DNS (`dns-default`), and `enable-ds-eviction`
**DaemonSet eviction for DNS** is a **cluster-autoscaler** concern when it **removes or drains nodes**. OpenShift DNS pods can use annotations such as **`cluster-autoscaler.kubernetes.io/enable-ds-eviction`** so eviction goes through paths that honor graceful shutdown. That is **not** something this **admission webhook** applies to arbitrary pods—it is part of how **platform DNS / DaemonSets** interact with **node** lifecycle and CA.

**This webhook is not the right lever to “make DNS respect `enable-ds-eviction`”**—that belongs to DNS/operator/CA configuration for the relevant **MachineSet** and DaemonSet, not to mutating CI workload pods here.

# Design

## Workload classes
Classes include tests, builds, longtests, and prowjobs. Each class has its own **MachineSet** (and usually **MachineAutoscaler** for scale-out). The webhook classifies pods and applies a **`RuntimeClass`** so they land on the right pool.

## Cluster autoscaler scales up
The cluster autoscaler increases **MachineSet** replicas when **Pending** pods need capacity—normal behavior.

## Scale-down control loop
Per workload class, the controller runs a **reconciliation loop about once per minute** (`pollNodeClassForScaleDown` in `prioritization.go`: initial evaluation, then `time.Tick(time.Minute)`). Each loop:

1. **Cordoned candidates:** For nodes already in **NoSchedule** avoidance (cordoned), only consider nodes **at least ~15 minutes old** so initialization cordons are not mistaken for scale-down targets. If the node still has **no** class pods (DaemonSets ignored), spawn work to **actually scale down** the Machine / MachineSet; otherwise wait.
2. **Avoidance set:** Among schedulable workload nodes, mark roughly **the top 25%** (`ceil(n/4)`) for **PreferNoSchedule**-style avoidance so new CI work tends to land elsewhere; clear avoidance on nodes past that budget.
3. **When a node is empty** under the avoidance / cordon policy, a later pass **cordons** (NoSchedule) and, once still empty, triggers **machine removal** via the scale-down path (with checks that the MachineSet is reconciled).

So “useful timeline” is **minute-scale loops** plus **15-minute minimum node age** for one part of the decision—not real-time per pod. The same path adjusts **nodes** and **MachineSet** objects so capacity is not left stranded longer than necessary.

## Avoidance states
States include **none**, **PreferNoSchedule** (taint), and **NoSchedule** (cordon). When a node has no running class pods (DaemonSets ignored for this check), the webhook can cordon and eventually trigger removal of that machine.

## Pod node affinity
Incoming pods can be given affinity that **excludes** a specific node the webhook is trying to empty, so scheduling pressure favors reclaiming that node.

# Deploying
1. Create **MachineSet** and **MachineAutoscaler** per class; they are **cluster- and cloud-specific**. Copy from existing build-farm YAML in **`openshift/release`** (for example under `clusters/build-clusters/buildNN/ci-scheduling-webhook/`). Set **`minReplicas` / `maxReplicas`** per pool from those examples—there is **no** single fixed maximum (older docs mentioning a flat max were wrong).
2. Apply `cmd/ci-scheduling-webhook/res/admin.yaml`.
3. Apply `cmd/ci-scheduling-webhook/res/rbac.yaml`.
4. Apply `cmd/ci-scheduling-webhook/res/deployment.yaml`.
5. Apply `cmd/ci-scheduling-webhook/res/dns.yaml` so cluster DNS DaemonSets can schedule on **tainted** worker nodes where required.
6. Verify the deployment in namespace `ci-scheduling-webhook`.
7. Confirm **MachineSets** have at least one node.
8. Apply `cmd/ci-scheduling-webhook/res/webhook.yaml`.

# Hack

## Manual image builds
```shell
[ci-tools]$ CGO_ENABLED=0 go build -ldflags="-extldflags=-static" github.com/openshift/ci-tools/cmd/ci-scheduling-webhook
[ci-tools]$ podman build -t quay.io/jupierce/ci-scheduling-webhook:latest -f images/ci-scheduling-webhook/Dockerfile .
[ci-tools]$ podman push quay.io/jupierce/ci-scheduling-webhook:latest
```

## Local test
```shell
[ci-tools]$ export KUBECONFIG=~/.kube/config
[ci-tools]$ go run github.com/openshift/ci-tools/cmd/ci-scheduling-webhook --as system:admin --port 8443
[ci-tools]$ cmd/ci-scheduling-webhook/testing/post-pods.sh
```

## Pushing to prod
```shell
[ci-tools]$ podman build -t quay.io/openshift/ci:ci_ci-scheduling-webhook_latest -f images/ci-scheduling-webhook/Dockerfile .
[ci-tools]$ podman push quay.io/openshift/ci:ci_ci-scheduling-webhook_latest
```

Restart or roll `ci-scheduling-webhook` pods on build farms so they pick up the new image.
