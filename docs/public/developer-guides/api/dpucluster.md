---
title: "DPUCluster"
---

[[_TOC_]]

The DPUCluster is a Kubernetes CRD which managed the control plane of a DPUCluster in DPF. The DPUCluster can be backed
by different implementations.

Two implementations are included in this repo:

* Kamaji cluster manager which creates Kamaji TenantControlPlanes to back the DPUCluster
* Static cluster manager which transforms an existing Kubernetes control plane into a DPUCluster control plane

## DPUCluster Usage

A DPUCluster is a user API and the usage will differ depending on the implementation.

#### Using the Static Cluster Manager

The static cluster manager controller should be enabled first. It is enabled by adding staticClusterManager field in the
DPUOperatorConfig CR:

```yaml
apiVersion: operator.dpu.nvidia.com/v1alpha1
kind: DPFOperatorConfig
metadata:
  name: dpfoperatorconfig
  namespace: dpf-operator-system
spec:
  staticClusterManager: {}
```

The dpf-operator chart also needs telling, since it is rendered before the DPFOperatorConfig
exists and defaults to Kamaji:

```shell
helm upgrade --install -n dpf-operator-system dpf-operator dpf-repository/dpf-operator \
  --set clusterManager=static
```

Without it the chart creates the Kamaji etcd defrag CronJob, which waits forever in
`ContainerCreating` for certificates a static install never produces.

Then create a secret for storing the kubeconfig of the existing Kubernetes control plane. For example, the kubeconfig is
under the home directory:

```shell
TENANT_KUBE_CONFIG=`cat ~/.kube/config | base64 -w 0`

cat <<EOF | kubectl apply -f -
apiVersion: v1
data:
  super-admin.conf: ${TENANT_KUBE_CONFIG}
kind: Secret
metadata:
  name: dpu-cluster-1-admin-kubeconfig
  namespace: dpf-operator-system
type: Opaque
EOF
```

the DPUCluster will look like:

```yaml
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUCluster
metadata:
  name: dpu-cluster-1
  namespace: dpf-operator-system
spec:
  ## type signals which controller implementation should take responsibility for the DPUCluster.
  type: static
  ## Max nodes is the maximum number of nodes supported by the DPUCluster implementation.
  maxNodes: 1000
  ## Kubeconfig is the name of a secret in the same namespace as the DPUCluster object.
  ## Note: This field is supplied by the user in the static cluster manager - but this may not be the case for other implementations.
  kubeconfig: dpu-cluster-1-admin-kubeconfig
```

##### Replacing the join script

Every join mechanism renders its command from a template, and the one DPF ships is the one the
mechanism named by `type` was proven with. A cluster whose control plane differs can replace it with
`joinToken.scriptTemplateRef`, naming a ConfigMap in the same namespace as the DPUCluster. `key` is
optional and `JOIN_SCRIPT_TEMPLATE` is read when it is not set:

```yaml
spec:
  joinToken:
    type: kubeadm
    scriptTemplateRef:
      name: kubeadm-join-template
      # key: JOIN_SCRIPT
    config:
      ## Any key here reaches the template, so a replacement can read ones DPF never defines.
      myOwnKey: something
```

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kubeadm-join-template
  namespace: dpf-operator-system
data:
  JOIN_SCRIPT_TEMPLATE: |
    #!/usr/bin/env bash
    set -euo pipefail
    echo "joining {{ .ClusterNamespace }}/{{ .ClusterName }} as {{ .NodeName }}"
```

The template is Go `text/template` with the hermetic [sprig](https://masterminds.github.io/sprig/)
functions, so `env`, `expandenv` and `getHostByName` are not available. It may reference:

| Field | What it is |
| --- | --- |
| `.Config` | `joinToken.config` merged over the defaults, see the note on quoting below |
| `.JoinToken` | the credential this DPU presents, in the shape its mechanism uses |
| `.NodeName` | the name the node has to register under for DPF to see it |
| `.DPUName`, `.DPUNamespace` | the DPU the script was rendered for |
| `.ClusterName`, `.ClusterNamespace` | the DPUCluster being joined |

A mechanism may offer more. The kubeadm template also has `.APIServer` and `.CACertHashes`.

Naming a key the config does not define is an error rather than an empty substitution, and a
ConfigMap or key that cannot be read is an error rather than a fall back to the shipped script,
since running a different script as root is worse than not joining.

Values in `.Config` are checked for what would break out of a single quoted assignment, which is a
quote, a control character or a glob metacharacter. `$`, a backtick, `;` and `|` all pass, because
the shipped script assigns every value inside single quotes where none of them mean anything. A
replacement has to do the same. Writing `FOO="{{ .Config.x }}"` with double quotes, or expanding a
value unquoted, hands command substitution and word splitting to whoever can edit the DPUCluster.

Replacing the script moves three responsibilities to whoever wrote it. The agent reruns the whole
script every 30s, so every step has to be idempotent. The script must not exit 0 before the node
holds its kubelet credentials, or DPF reports a DPU that never joined. And the node has to register
under `.NodeName`, or DPF never sees it.

The template is rendered inside the provisioning controller, with a cap on the size of the result but
none on the work spent producing it, and the script itself then runs as root on the DPU. Write access
to this ConfigMap is therefore worth treating as equivalent to write access to the provisioning
controller's workload, and should be granted accordingly.


##### Joining a k0s cluster

A static DPUCluster chooses how its nodes authenticate when they join. The default is `kubeadm`,
which suits a control plane built with kubeadm. For a k0s cluster set `joinToken.type` to `k0s`
and describe the worker under `joinToken.config`:

```yaml
spec:
  type: static
  maxNodes: 1000
  kubeconfig: dpu-cluster-1-admin-kubeconfig
  joinToken:
    type: k0s
    ## Read by the mechanism named above, so these keys belong to k0s rather than to DPF.
    ## None of them is validated when the object is applied. A bad value is reported on the
    ## DPU as BFBPrepared=False instead.
    config:
      ## The release to download from GitHub. Leave it out when k0s is already in the BFB,
      ## in which case the join fails on the DPU if it turns out not to be.
      version: "1.36.3+k0s.2"
      ## Optional, takes the binary from somewhere other than GitHub. Needs version set.
      ## Glob metacharacters are rejected in every value, so a URL carrying one, such as an S3
      ## presigned URL or an IPv6 literal host, has to be fronted by a mirror without them.
      # url: https://mirror.example.com/k0s-arm64
      ## Optional. The downloaded binary is installed and run as root, and without this it is
      ## trusted on TLS alone. Needs version set.
      # sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
      criSocket: remote:unix:///run/containerd/containerd.sock
      profile: dpu
      kubeletRootDir: /var/lib/kubelet
      extraArgs: "--labels dpu=true"
      ## The file the join waits for to prove the worker got its credentials. Name the new path
      ## here when extraArgs moves the k0s data dir, otherwise the join reports a failure.
      readyFile: /var/lib/k0s/kubelet.conf
```

The DPUFlavor used with a k0s cluster needs three skips. The DPU agent runs the join script as
part of its ConfigureKubelet step, so that step must be left enabled, while the parts of the
agent's kubelet handling that would fight k0s are turned off:

```yaml
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUFlavor
spec:
  dpuAgentConfig:
    skipOperations:
      ## Not configureKubelet, which is what runs the join script.
      ## The agent would otherwise write a drop-in for a kubelet k0s supervises itself.
      kubeletSystemdDropIn: true
      ## The agent would otherwise parse a config file k0s does not write.
      kubeletCustomizedConfig: true
      ## Required. The agent starts the stock kubelet after the join, which would then fight
      ## the one k0s supervises over the same root directory and port.
      startKubelet: true
```

Leaving `startKubelet` unset is the mistake that is hardest to spot, because provisioning
reports the DPU ready and the node only afterwards begins flapping between Ready and NotReady.

#### Using the Kamaji Cluster Manager

The DPUCluster will look like:

```yaml
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUCluster
metadata:
  name: dpu-cluster-1
  namespace: dpf-operator-system
spec:
  ## type signals which controller implementation should take responsibility for the DPUCluster.
  type: kamaji
  ## Max nodes is the maximum number of nodes supported by the DPUCluster implementation.
  maxNodes: 1000
  ## Cluster endpoint is supplied by the user and provides and IP and other details to make the APIServer available.
  clusterEndpoint:
    # deploy keepalived instances on the nodes that match the given nodeSelector.
    keepalived:
      # interface on which keepalived will listen. Should be the oob interface of the control plane node.
      interface: interface_one
      # vip is the Virtual IP reserved for the DPU Cluster load balancer. Must not be allocatable by DHCP.
      vip: dpucluster_vip
      # virtualRouterID must be in range [1,255], make sure the given virtualRouterID does not duplicate with any existing
      # keepalived process running on the host
      virtualRouterID: 126
      # nodeSelector selects which nodes the keepalived pods will be scheduled to.
      nodeSelector:
        node-role.kubernetes.io/control-plane: ""
```

### Multiple DPUClusters

DPF supports running multiple DPUClusters simultaneously within a single management cluster. Each DPUCluster is an
independent Kubernetes control plane that manages a subset of DPU nodes. This enables horizontal scaling beyond the
limits of a single cluster.

When allocating DPUs to DPUClusters, DPF uses a bin packing algorithm that selects the cluster with the highest ratio
of assigned DPUs to `maxNodes`, filling clusters before spilling into new ones.

DPF enables directing workloads to specific DPUClusters as an advanced use case. To do so, apply labels to the DPUCluster
objects. Other DPF resources expose a `dpuClusterSelector` field that uses those labels to target a subset of clusters.
When `dpuClusterSelector` is omitted, the resource applies to all DPUClusters.

### DPUCluster Implementation

A DPUCluster implementation is a Kubernetes controller which operates on the DPF DPUCluster object. It should:

* only operate on a DPUCluster which has a `type` it is responsible for.
* be the only DPUCluster controller implementation in a cluster.
* provide an admin Kubeconfig to a functioning Kubernetes cluster as a Kubernetes Secret.
* ensure the name of that Secret is available in the `.spec.kubeconfig` of the DPUCluster object.

The Kubeconfig provided by the DPUCluster should have the following format:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: dpu-cluster-1
  namespace: dpf-operator-system
type: Opaque
data:
  super-admin.conf: $KUBECONFIG_DATA
```

## Troubleshooting

### DPUCluster Status

Inspect the `DPUCluster` status conditions to understand the current state of the cluster:

```shell
kubectl describe dpucluster <dpucluster-name> -n dpf-operator-system
```

Key status fields:

| Field | Description |
|---|---|
| `.status.phase` | Overall lifecycle phase: `Pending`, `Creating`, `Ready`, `NotReady`, `Failed` |
| `.status.conditions` | Detailed conditions with `Reason` and `Message` fields |
| `.status.version` | Kubernetes control-plane version running in the DPU cluster |
| `.status.nodesCount` | Number of DPU nodes currently joined to the cluster |

For Kamaji-backed clusters, the underlying `TenantControlPlane` resource may provide additional detail:

```shell
kubectl get tenantcontrolplane <dpucluster-name> -n dpf-operator-system -o yaml
```

If direct access to the DPU cluster is needed for further investigation, see
[Accessing the Kamaji DPU Cluster](../../operational-readiness/troubleshooting/kamaji-cluster-access.md).
