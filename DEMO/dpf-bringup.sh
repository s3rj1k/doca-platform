#!/usr/bin/env bash
# DPF bring up. The DPUSet is applied last because creating it makes
# the BMC fetch the BFB, and the BMC gets one attempt at it.
set -uo pipefail

echo "== operator config and discovery"
kubectl apply -f - <<'PHASE1' || exit 1
---
apiVersion: operator.dpu.nvidia.com/v1alpha1
kind: DPFOperatorConfig
metadata:
  name: dpfoperatorconfig
  namespace: dpf-operator-system
spec:
  provisioningController:
    dmsTimeout: 900
    ## bfbPVCName stays unset, single node cannot strand the BFB cache.
    ## Flash over BMC Redfish, host rshim is disabled so host-agent cannot work.
    installInterface:
      installViaRedfish:
        ## Let discovery create the DPUNode too, not just the DPUDevice.
        skipDPUNodeDiscovery: false

    ## Where the BMC fetches the BFB and the generated bf.cfg from.
    ## loadBalancerAddress is the only field that pins a stable host and port,
    ## everything else leaves DPF advertising nodeIP plus a random NodePort.
    ## It must start with http://. The Caddy proxy k0s-nv.yml deploys on port 80
    ## is the load balancer this points at, it forwards to bfb-registry:8082.
    registry:
      loadBalancerAddress: "http://172.20.136.101"

  ## Zero-trust bf.cfg needs the API server so the DPU can join k0s over its OOB.
  overrides:
    kubernetesAPIServerVIP: "172.20.136.101"
    kubernetesAPIServerPort: 6443

  ## Adopt the k0s control plane. Disabled by default, so the key has to appear.
  staticClusterManager: {}

  ## Only one cluster manager may run.
  kamajiClusterManager:
    disable: true

  ## The DPU gets its networking from k0s, so nothing DPUService backed is wanted.
  ## Each of these would otherwise pull in argo cd.
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
  ovsCNI:
    disable: true
  nvipam:
    disable: true
  sriovDevicePlugin:
    disable: true
  monitoring:
    disable: true

  ## dpuDetector disabled: with Redfish install the DPUDiscovery object below finds
  ## the DPU through its BMC and creates the DPUDevice/DPUNode with bmcIp populated.
  dpuDetector:
    disable: true
  ## nodeSRIOVDevicePluginController is off by default and needs no entry.

---
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUCluster
metadata:
  name: dpu-cluster-1
  namespace: dpf-operator-system
spec:
  type: static
  maxNodes: 10
  kubeconfig: dpu-cluster-1-admin-kubeconfig

  joinToken:
    type: k0s
    ## Must cover minting, BFB flashing and the first join attempt.
    ttl: 4h
    config:
      ## A BFB never ships k0s, with no version the join script fetches nothing.
      version: "1.36.3+k0s.2"
      ## Must exist in workerProfiles or the kubelet never starts.
      profile: dpu
      extraArgs: "--labels dpu=true"

      ## Defaults, spelled out so they are easy to find.
      criSocket: "remote:unix:///run/containerd/containerd.sock"
      kubeletRootDir: "/var/lib/kubelet"
      readyFile: "/var/lib/k0s/kubelet.conf"

      ## Optional, nothing here is validated on admission.
      ## sha256 "<64 lower case hex>" and url "https://<mirror>/k0s-arm64"

---
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: BFB
metadata:
  name: bf-bundle-3-4-0
  namespace: dpf-operator-system
spec:
  url: https://content.mellanox.com/BlueField/BFBs/Ubuntu24.04/bf-bundle-3.4.0-92_26.04_ubuntu-24.04_64k_prod.bfb

---
## From test/objects/infrastructure/dpuflavor.yaml. dpuAgentConfig is the k0s part.
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUFlavor
metadata:
  name: k0s-flavor
  namespace: dpf-operator-system
spec:
  dpuMode: dpu
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
        - PF_BAR2_ENABLE=0
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
        ovs-vsctl --timeout 15 "$@"
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
      # Lab console login. Known password on every DPU, keep it off routable networks.
      echo -e "ubuntu\nubuntu" | passwd ubuntu

      # br-comm-ch takes the oob_net0 MAC so it keeps the same lease.
      # Do not retype this block, the macaddress line is what makes it work.
      oob_mac=$(cat /sys/class/net/oob_net0/address)
      cat <<EOF > /etc/netplan/99-dpf-comm-ch.yaml
      network:
          renderer: networkd
          version: 2
          ethernets:
            pf0vf0:
              mtu: 1500
          bridges:
            br-comm-ch:
              macaddress: $oob_mac
              dhcp4: yes
              mtu: 1500
              interfaces:
                - pf0vf0
      EOF

  ## configureKubelet stays enabled, that step reads the join Secret and runs it.
  dpuAgentConfig:
    skipOperations:
      ## Points the stock kubelet at kubeadm credentials k0s never writes.
      kubeletSystemdDropIn: true
      ## Needs a kubelet config file k0s does not produce.
      kubeletCustomizedConfig: true
      ## Required, otherwise the BFB kubelet starts after k0s joined and both
      ## fight over the same root dir and port.
      startKubelet: true
      ## Uncomment if reboot method discovery misreports on this hardware.
      ## rebootMethodDiscovery: true

---
## Shared root password DPF uses to reach every DPU BMC over Redfish.
## CHANGEME to your BMC password. Plaintext here, keep this file access controlled.
apiVersion: v1
kind: Secret
metadata:
  name: bmc-shared-password
  namespace: dpf-operator-system
type: Opaque
stringData:
  password: "CHANGEME-bmc-root-password"

---
## Scans DPU BMCs over Redfish, creates the DPUDevice/DPUNode with bmcIp set.
## Auth uses the bmc-shared-password Secret. Range is the BMC IP, not the DPU OS.
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUDiscovery
metadata:
  name: dpu-discovery
  namespace: dpf-operator-system
## One IP on purpose. The BMC is pinned static at .245 and a dead IP costs a
## 30s TCP timeout, so a wide range takes minutes and loses to the host path.
##
## The host path creates a DPUDevice with no bmcIp about 40s after apply, which
## is why the DPUDevice below is declared rather than left to discovery.
spec:
  ipRangeSpec:
    ipRange:
      startIP: "172.20.137.245"
      endIP: "172.20.137.245"
      port: 443
PHASE1

# Probe the URL the BMC will use, not pod readiness.
echo "== waiting for bfb-registry"
for i in $(seq 1 120); do
  curl -sf -o /dev/null -m 5 \
    http://172.20.136.101/bfb/dpf-operator-system-bf-bundle-3-4-0.bfb && break
  [ "$i" = 120 ] && { echo "registry never came up"; exit 1; }
  sleep 5
done

# Discovery has to create the DPUNode. The host path leaves hostAgent set,
# which needs rshim, and rshim is not installed here.
echo "== waiting for a redfish DPUNode"
for i in $(seq 1 120); do
  node=$(kubectl -n dpf-operator-system get dpunode -o name 2>/dev/null | grep dpu-node- | head -1)
  if [ -n "$node" ]; then
    iface=$(kubectl -n dpf-operator-system get "$node" -o jsonpath='{.status.dpuInstallInterface}')
    echo "   $node iface=$iface"
    [ "$iface" = redfish ] && break
  fi
  [ "$i" = 120 ] && {
    kubectl -n dpf-operator-system get dpunode
    echo "host path won, rshim would be used"
    exit 1
  }
  sleep 5
done

echo "== DPUSet"
kubectl apply -f - <<'PHASE2' || exit 1
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUSet
metadata:
  name: k0s-test
  namespace: dpf-operator-system
spec:
  strategy:
    type: RollingUpdate

  ## Discovery-created DPUNodes carry this label, not kubernetes.io/hostname.
  ## Every discovered DPUNode gets it, so on its own this matches the whole lab.
  dpuNodeSelector:
    matchLabels:
      feature.node.kubernetes.io/dpu-enabled: "true"

  ## Pins the set to this one card, and only when discovery created the DPUNode.
  ## Discovery names it dpu-node-<serial>, the host path names it after the host.
  ##
  ## So a host-path DPUNode selects nothing and the DPUSet stays idle, which is
  ## wanted. That node carries hostAgent and rshim is not installed here.
  ##
  ## CHANGEME to the serial of your card. The DPUSet matches nothing until then.
  dpuDeviceSelector:
    matchLabels:
      provisioning.dpu.nvidia.com/dpunode-name: dpu-node-CHANGEME

  dpuTemplate:
    spec:
      dpuFlavor: k0s-flavor
      bfb:
        name: bf-bundle-3-4-0

      ## host is the only control plane node and must not be drained. Leaving
      ## nodeEffect unset lets DPF fall back to draining the host.
      nodeEffect:
        noEffect: true

      cluster:
        ## Applied by DPF with its own credentials, which is why the restricted
        ## node-role prefix is allowed. A kubelet cannot self assign it.
        nodeLabels:
          node-role.kubernetes.io/dpu: ""
PHASE2

echo "== kubectl get bfb,dpuset,dpu,dpunode -n dpf-operator-system -w"
