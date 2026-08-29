/*
Copyright 2024 NVIDIA

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"github.com/nvidia/doca-platform/pkg/conditions"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	DPUClusterKind = "DPUCluster"
)

var DPUClusterGroupVersionKind = GroupVersion.WithKind(DPUClusterKind)

type ClusterType string

type ConditionType string

const (
	StaticCluster ClusterType = "static"
	KamajiCluster ClusterType = "kamaji"
)

const (
	ConditionCreated  ConditionType = "Created"
	ConditionReady    ConditionType = "Ready"
	ConditionUpgraded ConditionType = "Upgraded"
)

// ClusterPhase describes current state of DPUCluster.
// Only one of the following state may be specified.
// Default is Pending.
// +kubebuilder:validation:Enum="Pending";"Creating";"Ready";"NotReady";"Failed"
type ClusterPhase string

const (
	PhasePending  ClusterPhase = "Pending"
	PhaseCreating ClusterPhase = "Creating"
	PhaseReady    ClusterPhase = "Ready"
	PhaseNotReady ClusterPhase = "NotReady"
	PhaseFailed   ClusterPhase = "Failed"
)

var _ conditions.GetSet = &DPUCluster{}

// GetConditions returns the conditions of the DPUService.
func (s *DPUCluster) GetConditions() []metav1.Condition {
	return s.Status.Conditions
}

// SetConditions sets the conditions of the DPUService.
func (s *DPUCluster) SetConditions(conditions []metav1.Condition) {
	s.Status.Conditions = conditions
}

const (
	FinalizerCleanUp         = "provisioning.dpu.nvidia.com/cluster-manager-clean-up"
	FinalizerInternalCleanUp = "provisioning.dpu.nvidia.com/cluster-manager-internal-clean-up"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// DPUClusterSpec defines the desired state of DPUCluster
type DPUClusterSpec struct {
	// Type of the cluster with few supported values
	// static - existing cluster that is deployed by user. For DPUCluster of this type, the kubeconfig field must be set.
	// kamaji - DPF managed cluster. The kamaji-cluster-manager will create a DPU cluster on behalf of this CR.
	// $(others) - any string defined by ISVs, such type names must start with a prefix.
	// +kubebuilder:validation:Pattern="kamaji|static|[^/]+/.*"
	// +required
	Type string `json:"type"`

	// MaxNodes is the max amount of node in the cluster
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=1000
	// +optional
	MaxNodes int `json:"maxNodes,omitempty"`

	// Kubeconfig is the secret that contains the admin kubeconfig
	// +kubebuilder:validation:XValidation:rule="oldSelf==\"\"||self==oldSelf",message="kubeconfig is immutable"
	// +optional
	Kubeconfig string `json:"kubeconfig,omitempty"`

	// ClusterEndpoint contains configurations of the cluster entry point
	// +optional
	ClusterEndpoint *ClusterEndpointSpec `json:"clusterEndpoint,omitempty"`

	// JoinToken configures the bootstrap token minted for nodes joining this cluster.
	// Only read for clusters of type static, since a kamaji cluster is managed by DPF.
	// +optional
	JoinToken *JoinTokenSpec `json:"joinToken,omitempty"`
}

// JoinTokenType selects how a node authenticates when it joins the cluster.
// +kubebuilder:validation:Enum=kubeadm
type JoinTokenType string

const (
	// JoinTokenKubeadm mints a kubeadm bootstrap token and emits a kubeadm join command.
	JoinTokenKubeadm JoinTokenType = "kubeadm"
)

// JoinTokenSpec configures the bootstrap token minted for nodes joining a DPUCluster.
type JoinTokenSpec struct {
	// Type selects how nodes join this cluster. Only read for clusters of type static,
	// and kubeadm is assumed when it is not set.
	// +kubebuilder:default=kubeadm
	// +optional
	Type JoinTokenType `json:"type,omitempty"`

	// TTL is how long a minted join token authenticates for. It has to cover minting
	// through BFB flashing and the DPU agent's first join attempt.
	// +kubebuilder:default="2h"
	// +kubebuilder:validation:Type=string
	// Six digits per component keeps the hour count inside the int64 the bounds are evaluated in,
	// and repeating components keep a compound duration such as 1h30m valid.
	// +kubebuilder:validation:Pattern=`^([0-9]{1,6}(h|m|s|ms|us|µs|ns))+$`
	// +kubebuilder:validation:MaxLength=10
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10m')",message="ttl has to be at least 10m so a token outlives BFB flashing"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('24h')",message="ttl has to be at most 24h so a leaked token ages out"
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// Config is read by the join mechanism Type names, so its keys belong to that mechanism
	// rather than to this API, and they reach its join script template.
	// Nothing here is validated on admission, so a bad value is reported on the DPU instead.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`

	// ScriptTemplateRef replaces the join script the mechanism named by Type ships with. Read
	// from the namespace of this DPUCluster, so it cannot reach a ConfigMap the author cannot.
	// +optional
	ScriptTemplateRef *ScriptTemplateRef `json:"scriptTemplateRef,omitempty"`
}

// ScriptTemplateRef names a ConfigMap holding a join script template. The ConfigMap is read from
// the namespace of the DPUCluster that names it, so it cannot reach one the author cannot read.
type ScriptTemplateRef struct {
	// Name is the ConfigMap holding the template.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// Key is the ConfigMap key holding the template. JOIN_SCRIPT_TEMPLATE is read when unset.
	// +optional
	Key string `json:"key,omitempty"`
}

// DPUClusterStatus defines the observed state of DPUCluster
type DPUClusterStatus struct {
	// +kubebuilder:validation:Enum=Pending;Creating;Ready;NotReady;Failed
	// +kubebuilder:default="Pending"
	Phase ClusterPhase `json:"phase"`

	// Version is the K8s control-plane version of the cluster
	// +optional
	Version string `json:"version"`

	// NodesCount is the number of DPUs assigned to the cluster
	// +kubebuilder:validation:Minimum=0
	// +optional
	NodesCount int `json:"nodesCount"`

	// +optional
	Conditions []metav1.Condition `json:"conditions"`
}

type ClusterEndpointSpec struct {
	// Keepalived configures the keepalived that will be deployed for the cluster control-plane
	// +optional
	Keepalived *KeepalivedSpec `json:"keepalived,omitempty"`
}

type KeepalivedSpec struct {
	// VIP is the virtual IP owned by the keepalived instances
	VIP string `json:"vip"`

	// VirtualRouterID is the virtual_router_id in keepalived.conf
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=255
	VirtualRouterID int `json:"virtualRouterID"`

	// Interface specifies on which interface the VIP should be assigned
	// +kubebuilder:validation:MinLength=1
	Interface string `json:"interface"`

	// NodeSelector is used to specify a subnet of control plane nodes to deploy keepalived instances.
	// Note: keepalived instances are always deployed on control plane nodes
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:metadata:annotations=helm.sh/resource-policy=keep
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="phase of the cluster"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type",description="type of the cluster"
// +kubebuilder:printcolumn:name="MaxNodes",type="integer",JSONPath=".spec.maxNodes",description="max amount of nodes"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.version",description="Kubernetes control-plane version"
// +kubebuilder:printcolumn:name="NodesCount",type="integer",JSONPath=".status.nodesCount",description="number of assigned DPUs"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63", message="name length can't be bigger than 63 chars"
// The DPUCluster name is used as a prefix for Service names created for its DPUServices, which must be valid
// DNS-1035 labels (start with a lowercase letter). This is enforced on create only. Clusters that already
// exist with a non-compliant name are exempt (the name is immutable), so their status and finalizers can
// still be updated.
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$') || (oldSelf.hasValue() && oldSelf.value().metadata.name == self.metadata.name)",optionalOldSelf=true,message="name must be a DNS-1035 label: start with a lowercase letter, contain only lowercase alphanumerics or '-', and end with an alphanumeric"

// DPUCluster is the Schema for the dpuclusters API
type DPUCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec DPUClusterSpec `json:"spec,omitempty"`

	// +kubebuilder:default={phase: Pending}
	// +optional
	Status DPUClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DPUClusterList contains a list of DPUCluster
type DPUClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DPUCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DPUCluster{}, &DPUClusterList{})
}

const (
	// DPUClusterNameLabelKey is the key of the label linking objects to a specific DPU Cluster. The value should be the
	// namespace of the DPUCluster.
	DPUClusterNameLabelKey = "dpu.nvidia.com/cluster"
	// DPUClusterNamespaceLabelKey is the key of the label linking objects to a specific DPU Cluster. The value should
	// be the namespace of the DPUCluster.
	DPUClusterNamespaceLabelKey = "dpu.nvidia.com/cluster-namespace"
)
