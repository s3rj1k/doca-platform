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

#### Using the k0smotron Cluster Manager

k0smotron hosts the control plane as pods in the management cluster, like Kamaji does, but runs
k0s rather than upstream Kubernetes. It is not installed by DPF, so install it first, then enable
the manager, which is off by default:

```yaml
apiVersion: operator.dpu.nvidia.com/v1alpha1
kind: DPFOperatorConfig
metadata:
  name: dpfoperatorconfig
  namespace: dpf-operator-system
spec:
  k0smotronClusterManager:
    disable: false
    ## Backs each hosted control plane's etcd volume. Only needed where the management cluster
    ## has no default StorageClass, otherwise the etcd PVC never binds.
    etcdStorageClassName: local-path
```

The chart also needs telling, for the same reason the static manager does:

```shell
helm upgrade --install -n dpf-operator-system dpf-operator dpf-repository/dpf-operator \
  --set clusterManager=k0smotron.io/k0smotron
```

The DPUCluster will look like:

```yaml
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUCluster
metadata:
  name: dpu-cluster-1
  namespace: dpf-operator-system
spec:
  type: k0smotron.io/k0smotron
  maxNodes: 1000
  ## No clusterEndpoint. k0smotron exposes the control plane through a NodePort Service of its
  ## own, so keepalived is not involved and the field is ignored for this type.
```

##### Configuring the hosted control plane

`.spec.clusterManagerConfig` is a partial [k0smotron
ClusterSpec](https://docs.k0smotron.io/stable/resource-reference/) merged over the one the cluster
manager builds. Every field k0smotron accepts can be set this way, including ones DPF sets itself:

```yaml
spec:
  type: k0smotron.io/k0smotron
  clusterManagerConfig:
    replicas: 1
    controlPlaneFlags:
      - --enable-metrics-scraper=true
    resources:
      requests:
        cpu: 500m
        memory: 512Mi
    ## Node ports are cluster scoped, so a second k0smotron DPUCluster in the same management
    ## cluster has to move them or its Service will fail to allocate.
    service:
      type: NodePort
      apiPort: 31443
      konnectivityPort: 31132
```

Three things are worth knowing before using it:

* **Arrays replace, they do not join.** Setting `patches` or `k0sConfig` discards what DPF put
  there rather than adding to it.
* **An unknown field is an error**, reported on the DPUCluster rather than ignored, so a typo
  does not look like a setting that quietly did nothing.
* **`storage` and `persistence` apply only when the control plane is first created.** A bound
  PVC's class and size cannot change, so an edit to either is reported on the DPUCluster as a
  `K0smotronSpecApplied` condition instead of being retried forever.

The manager refuses a configuration no DPU could join, reporting `K0smotronSpecApplied` with
reason `InvalidClusterManagerConfig`. It requires a non-empty `version`, a `service.type` of
`NodePort` or `LoadBalancer`, a `dpu` entry in `k0sConfig.spec.workerProfiles` (the profile the
join script names), and a non-zero etcd volume size.

`K0smotronSpecApplied` is reported on every pass, so it reads `True` with reason `Applied` once
the control plane carries what was asked for:

```shell
kubectl get dpucluster dpu-cluster-1 -n dpf-operator-system \
  -o jsonpath='{.status.conditions[?(@.type=="K0smotronSpecApplied")]}'
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
