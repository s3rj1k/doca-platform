#!/usr/bin/env bash
# DPF bring up on cz9121, host-trusted over PCIe rshim.
# Assumes a clean cluster after k0smotron-nv.yml. Run host-preflight.sh if unsure.
set -uo pipefail

# Config comes from demo.env, source it from any location beforehand.
: "${HOST_NODE:?source demo.env first, for example  set -a && source demo.env && set +a}"

# The host agent names its DPUNode after the HOST, not the DPU serial.
DPUNODE_NAME="$HOST_NODE"

echo "== operator config"
kubectl apply -f - <<PHASE1 || exit 1
---
apiVersion: operator.dpu.nvidia.com/v1alpha1
kind: DPFOperatorConfig
metadata:
  name: dpfoperatorconfig
  namespace: ${NS}
spec:
  ## Immutable. host-trusted is what permits installViaHostAgent, and a CEL rule
  ## rejects installViaRedfish in this mode.
  deploymentMode: host-trusted

  provisioningController:
    dmsTimeout: 900

    ## The rshim channel. Exactly one install interface may be set, and
    ## installViaGNOI is deprecated in favour of this one.
    installInterface:
      installViaHostAgent: {}

  ## Still required. The dpu agent reads its join payload from THIS cluster,
  ## which is unrelated to how the BFB arrived.
  overrides:
    kubernetesAPIServerVIP: "${MGMT_IP}"
    kubernetesAPIServerPort: 6443

  ## networking.dpuNodeOOBBridgeName defaults to br-dpu, already this host's
  ## bridge, and is configurable only in host-trusted mode.

  ## Hosted DPU control plane. The cluster manager does not care how the DPU
  ## was provisioned, so this is unchanged by the move to rshim.
  k0smotronClusterManager:
    etcdStorageClassName: local-path
    replicas: 1

  ## Kamaji is the one cluster manager enabled by default, so omitting this key
  ## deploys it. staticClusterManager needs no entry, it is disabled by default.
  kamajiClusterManager:
    disable: true

  ## Kamaji Keepalived bit, on by default, unused by k0smotron. Left enabled it
  ## makes a DPUService that blocks DPFOperatorConfig Ready until a DPU joins.
  coreDNS:
    disable: true

  ## The DPU gets its networking from k0s, so nothing DPUService backed is
  ## wanted. Each of these would otherwise pull in argo cd.
  dpuServiceController:
    disable: true
  serviceSetController:
    disable: true
  sfcController:
    disable: true
  cniInstaller:
    disable: true
  multus:
    disable: true
  flannel:
    disable: true
  nvipam:
    disable: true
  sriovDevicePlugin:
    disable: true
  monitoring:
    disable: true

  ## The HostAgent Pod registers the DPUDevice and DPUNode over rshim, so the
  ## detector DaemonSet has nothing to add.
  dpuDetector:
    disable: true
PHASE1

echo "== waiting for the operator config"
for i in $(seq 1 120); do
  ready=$(kubectl -n "$NS" get dpfoperatorconfig dpfoperatorconfig \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  echo "   dpfoperatorconfig Ready=${ready:-none}"
  [ "$ready" = "True" ] && break
  [ "$i" = 120 ] && {
    kubectl -n "$NS" get dpfoperatorconfig -o yaml | sed -n '/status:/,$p' | head -40
    echo "operator config never became Ready"
    exit 1
  }
  sleep 5
done

# Supplementary RBAC for a fork bug. The k0smotron join generator can create and
# delete JoinTokenRequests but cannot read or watch them.

# So the controller cache never syncs, no token is minted, and the DPU hangs at
# Prepare BFB. The real fix is read verbs on the join_command_k0smotron.go marker.
echo "== jointokenrequest RBAC (fork workaround)"
kubectl apply -f - <<RBAC || exit 1
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: dpf-provisioning-jointokenrequest-read
rules:
  - apiGroups: ["k0smotron.io"]
    resources: ["jointokenrequests"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: dpf-provisioning-jointokenrequest-read
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: dpf-provisioning-jointokenrequest-read
subjects:
  - kind: ServiceAccount
    name: dpf-provisioning-controller-manager
    namespace: ${NS}
RBAC

echo "== cluster, BFB and flavor"
kubectl apply -f - <<PHASE2 || exit 1
---
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUCluster
metadata:
  name: ${DPUCLUSTER}
  namespace: ${NS}
spec:
  type: k0smotron.io/k0smotron
  maxNodes: 10
  clusterManagerConfig:
    ## Restated because DPF defines the dpu profile empty and k0sConfig is
    ## replaced not merged. The name matters, k0s install worker --profile dpu.
    k0sConfig:
      apiVersion: k0s.k0sproject.io/v1beta1
      kind: ClusterConfig
      spec:
        workerProfiles:
          - name: dpu
            values:
              topologyManagerPolicy: "best-effort"
              cpuManagerPolicy: "static"
              cpuManagerPolicyOptions:
                full-pcpus-only: "true"
                distribute-cpus-across-numa: "true"
              reservedSystemCPUs: "0"
              reservedMemory:
                - limits:
                    memory: 2Gi
                  numaNode: 0
              kubeReservedCgroup: ""
    ## k0smotron requires kine for more than one replica, and this control plane
    ## is etcd backed, so 1 is the only valid value here.
    replicas: 1
    ## Stated because the CRD default type is ClusterIP, which the manager
    ## refuses. The ports are cluster scoped, so a second cluster must move them.
    service:
      type: NodePort
      apiPort: ${API_PORT}
      konnectivityPort: ${KONNECTIVITY_PORT}
    ## Immutable once the PVC binds, storage is dropped from the reconciled set
    ## even while named here. Oversized on purpose for one DPU.
    storage:
      type: etcd
      etcd:
        persistence:
          size: 4Gi
    resources:
      requests:
        cpu: 200m
        memory: 256Mi

---
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: BFB
metadata:
  name: ${BFB_NAME}
  namespace: ${NS}
spec:
  url: ${BFB_URL}

---
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUFlavor
metadata:
  name: k0s-flavor
  namespace: ${NS}
spec:
  dpuMode: dpu
  ## This card sits behind a PCIe switch and cannot do the level 3 reset MFT
  ## discovery picks, so stand discovery down and keep the agent on warm reboot.
  dpuAgentConfig:
    skipOperations:
      rebootMethodDiscovery: true
  ## The DPU joins the HCP over its OOB port, which routes to the control plane.
  ## The agent writes this and stands its default pf0vf0 comm channel down.
  provisioningNetwork:
    netplan: |
      network:
        version: 2
        renderer: networkd
        ethernets:
          oob_net0:
            dhcp4: true
            dhcp6: false
            accept-ra: false
            dhcp-identifier: mac
  bfcfgParameters:
    - UPDATE_ATF_UEFI=yes
    - UPDATE_DPU_OS=yes
    - WITH_NIC_FW_UPDATE=no
    - UPDATE_BMC_FW=no
    - UPDATE_CEC_FW=no
  configFiles:
    - operation: override
      path: /etc/mellanox/mlnx-bf.conf
      permissions: "0644"
      raw: |
        ALLOW_SHARED_RQ="no"
        IPSEC_FULL_OFFLOAD="no"
        ENABLE_ESWITCH_MULTIPORT="yes"
    - operation: override
      path: /etc/mellanox/mlnx-ovs.conf
      permissions: "0644"
      raw: |
        CREATE_OVS_BRIDGES="no"
        OVS_DOCA="yes"
    - operation: override
      path: /etc/mellanox/mlnx-sf.conf
      permissions: "0644"
      raw: ""
  grub:
    kernelParameters:
      - console=hvc0
      - console=ttyAMA0
      - earlycon=pl011,0x13010000
      - fixrttc
      - net.ifnames=0
      - biosdevname=0
      - iommu.passthrough=1
      - cgroup_no_v1=net_prio,net_cls
      - hugepagesz=2048kB
      - hugepages=250
  nvconfig:
    - device: '*'
      parameters:
        ## 1, not 0. This is the value that keeps rshim alive. Setting it to 0
        ## removes BAR2 and leaves the card reachable only through the BMC.
        - PF_BAR2_ENABLE=1
        - PER_PF_NUM_SF=1
        - PF_TOTAL_SF=16
        - PF_SF_BAR_SIZE=10
        - INTERNAL_CPU_MODEL=1
        - INTERNAL_CPU_OFFLOAD_ENGINE=0
        - SRIOV_EN=1
        - NUM_OF_VFS=8
        - LAG_RESOURCE_ALLOCATION=1
        - LINK_TYPE_P1=ETH
        - LINK_TYPE_P2=ETH
  ovs:
    rawConfigScript: |
      _ovs-vsctl() {
        ovs-vsctl --timeout 15 "\$@"
      }

      # Clear the stock config and any leftovers on the kernel datapath.
      _ovs-vsctl --if-exists del-br ovsbr1
      _ovs-vsctl --if-exists del-br ovsbr2
      ovs-appctl --timeout 15 dpctl/del-dp system@ovs-system || true

      _ovs-vsctl set Open_vSwitch . other_config:doca-init=true
      _ovs-vsctl set Open_vSwitch . other_config:dpdk-max-memzones=50000
      _ovs-vsctl set Open_vSwitch . other_config:hw-offload=true
      _ovs-vsctl set Open_vSwitch . other_config:pmd-quiet-idle=true
      _ovs-vsctl set Open_vSwitch . other_config:max-idle=20000
      _ovs-vsctl set Open_vSwitch . other_config:max-revalidator=5000
      _ovs-vsctl set Open_vSwitch . other_config:doca-congestion-threshold=60
      _ovs-vsctl set Open_vSwitch . other_config:flow-limit=500000
      _ovs-vsctl set Open_vSwitch . other_config:hw-offload-ct-unidir-udp-enabled=true
      _ovs-vsctl remove Open_vSwitch . other_config default-datapath-type || true

      if systemctl list-unit-files openvswitch-switch.service &>/dev/null; then
        systemctl restart openvswitch-switch
      elif systemctl list-unit-files openvswitch.service &>/dev/null; then
        systemctl restart openvswitch
      fi
      _ovs-vsctl --may-exist add-br br-sfc
      _ovs-vsctl set bridge br-sfc datapath_type=netdev
      _ovs-vsctl set bridge br-sfc fail_mode=secure
      _ovs-vsctl --may-exist add-port br-sfc p0
      _ovs-vsctl set Interface p0 type=dpdk
      _ovs-vsctl set Interface p0 mtu_request=9216
      _ovs-vsctl set Port p0 external_ids:dpf-type=physical
      # Dual port card on this host, so p1 is wired up too.
      _ovs-vsctl --may-exist add-port br-sfc p1
      _ovs-vsctl set Interface p1 type=dpdk
      _ovs-vsctl set Interface p1 mtu_request=9216
      _ovs-vsctl set Port p1 external_ids:dpf-type=physical
      # Lab console login on the DPU. The rshim console needs a password that
      # works, so set a known one for debugging.
      echo -e "ubuntu\nubuntu" | passwd ubuntu
PHASE2

# The HostAgent registers this over rshim, reading the DPU mode off the card.

# Gated on the DPUDevice Ready condition, NOT dpuType. dpuType comes from a
# Redfish chassis query that host-trusted never runs, so Unknown is expected.
echo "== waiting for the host agent to register the DPU"
for i in $(seq 1 120); do
  dev=$(kubectl -n "$NS" get dpudevice -o name 2>/dev/null | head -1)
  if [ -n "$dev" ]; then
    ready=$(kubectl -n "$NS" get "$dev" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
    mode=$(kubectl -n "$NS" get "$dev" -o jsonpath='{.status.dpuMode}' 2>/dev/null)
    serial=$(kubectl -n "$NS" get "$dev" -o jsonpath='{.spec.serialNumber}' 2>/dev/null)
    echo "   ${dev#*/} serial=${serial:-?} mode=${mode:-none} Ready=${ready:-none}"
    [ "$ready" = True ] && [ "$mode" = dpu ] && break
  else
    echo "   no DPUDevice yet"
  fi
  [ "$i" = 120 ] && {
    kubectl -n "$NS" get dpudevice -o yaml 2>&1 | sed -n '/status:/,$p' | head -30
    kubectl -n "$NS" logs "${HOST_NODE}-dms" -c hostagent --tail=30 2>&1
    echo "host agent never brought the DPUDevice Ready"
    exit 1
  }
  sleep 5
done

# hostAgent, not external. DPF reboots the host itself over the agent, so no
# manual power cycle and nothing to release afterwards.
echo "== reboot method"
kubectl -n "$NS" get dpunode -o jsonpath='{range .items[*]}   {.metadata.name} rebootMethod={.spec.nodeRebootMethod}{"\n"}{end}' 2>&1

echo "== waiting for the hosted control plane"
for i in $(seq 1 120); do
  phase=$(kubectl -n "$NS" get dpucluster "$DPUCLUSTER" -o jsonpath='{.status.phase}' 2>/dev/null)
  echo "   dpucluster=${phase:-none}"
  [ "$phase" = Ready ] && break
  if [ "$phase" = Failed ]; then
    kubectl -n "$NS" get dpucluster "$DPUCLUSTER" -o yaml
    echo "DPUCluster failed"
    exit 1
  fi
  [ "$i" = 120 ] && {
    kubectl -n "$NS" get dpucluster "$DPUCLUSTER" -o yaml
    kubectl -n "$NS" get pods,pvc
    echo "control plane never became Ready"
    exit 1
  }
  sleep 5
done

# Published by the cluster manager itself. Nothing here creates it, so its
# absence means the manager never got far enough.
echo "== waiting for the published kubeconfig"
for i in $(seq 1 60); do
  kubectl -n "$NS" get secret "${DPUCLUSTER}-admin-kubeconfig" \
    -o jsonpath='{.data.super-admin\.conf}' >/dev/null 2>&1 && break
  [ "$i" = 60 ] && {
    echo "${DPUCLUSTER}-admin-kubeconfig was never published"
    exit 1
  }
  sleep 5
done

echo "== DPUSet"
# Captured so the webhook can be probed with the real object before the real
# apply, since the operator config above may have only just rolled.
dpuset_yaml=$(cat <<PHASE3
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUSet
metadata:
  name: k0s-test
  namespace: ${NS}
spec:
  strategy:
    type: RollingUpdate

  ## Both selectors name the HOST, not the DPU serial. A dpu-node-<serial>
  ## selector matches nothing and the DPUSet then sits idle with no error.
  dpuNodeSelector:
    matchLabels:
      feature.node.kubernetes.io/dpu-enabled: "true"

  dpuDeviceSelector:
    matchLabels:
      provisioning.dpu.nvidia.com/dpunode-name: ${DPUNODE_NAME}

  dpuTemplate:
    spec:
      dpuFlavor: k0s-flavor
      bfb:
        name: ${BFB_NAME}

      ## cz9121 is the only control plane node and must not be drained. Leaving
      ## nodeEffect unset lets DPF fall back to draining the host.
      nodeEffect:
        noEffect: true

      cluster:
        ## Applied by DPF with its own credentials, which is why the restricted
        ## node-role prefix is allowed. A kubelet cannot self assign it.
        nodeLabels:
          node-role.kubernetes.io/dpu: ""
PHASE3
)

# mdpuset.kb.io is served by the provisioning controller, which may have only
# just rolled. A server dry run exercises that webhook without creating anything.
echo "== waiting for the DPUSet webhook"
for i in $(seq 1 60); do
  err=$(printf '%s\n' "$dpuset_yaml" | kubectl apply --dry-run=server -f - 2>&1) && break
  case "$err" in
    *'connection refused'*|*'no endpoints available'*|*'i/o timeout'*|\
    *'failed calling webhook'*|*'EOF'*)
      [ "$i" = 60 ] && {
        echo "$err"
        echo "webhook never came up"
        exit 1
      }
      sleep 5
      ;;
    *)
      # A real rejection of the object, which retrying cannot fix.
      echo "$err"
      exit 1
      ;;
  esac
done

printf '%s\n' "$dpuset_yaml" | kubectl apply -f - || exit 1

echo
echo "== watch    kubectl get bfb,dpuset,dpu,dpunode,dpudevice -n ${NS} -w"
echo "== hosted   kubectl -n ${NS} get clusters.k0smotron.io,pods"
echo "== install  kubectl -n ${NS} logs ${HOST_NODE}-dms -c dms -f"
echo "== console  sudo cat /dev/rshim0/console"

# Hand off to the live dashboard, it inherits the already sourced env.
echo
echo "== launching dashboard, Ctrl-C to exit"
sleep 3
exec python3 "$(dirname "$0")/dpf-dashboard.py"
