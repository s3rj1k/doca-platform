/*
Copyright 2026 NVIDIA

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

// Package k0smotron manages a DPU control plane that k0smotron hosts. It owns the
// k0smotron Cluster behind a DPUCluster and republishes its kubeconfig for DPF.
package k0smotron

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"github.com/nvidia/doca-platform/internal/clustermanager/controller"
	dutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/dpu/util"
	cutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/util"
	k0smotronv1 "github.com/nvidia/doca-platform/third_party/forked/github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// RegisterWatches watches the k0smotron Cluster, so a starting control plane does not wait out
// the error backoff. Skipped without the CRD, since that watch would fail informer startup.
func RegisterWatches(mapper meta.RESTMapper, log logr.Logger) {
	gvk := k0smotronv1.GroupVersion.WithKind("Cluster")
	if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		log.Info("k0smotron CRDs are not installed, continuing without the Cluster watch",
			"gvk", gvk.String(), "reason", err.Error())
		return
	}

	controller.RegisterWatch(&k0smotronv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(clusterToDPUCluster))
}

// clusterToDPUCluster maps a k0smotron Cluster to the DPUCluster behind it. The two share a name
// and namespace, which is how this handler creates it.
func clusterToDPUCluster(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: cutil.GetNamespacedName(o)}}
}

const (
	// DefaultK0sVersion is the version a control plane runs and its workers download to match.
	// Its Kubernetes version tracks util.KubernetesVersion, so a DPU cluster matches what DPF ships.
	DefaultK0sVersion = "v1.35.6+k0s.0"

	// k0smotronKubeconfigKey is where k0smotron writes the admin kubeconfig.
	k0smotronKubeconfigKey = "value"

	// dpfKubeconfigKey is where DPF expects to read it, per pkg/dpucluster.
	dpfKubeconfigKey = "super-admin.conf"

	// workerProfile is the k0s worker profile a DPU joins under. The join script names it,
	// so the control plane has to define it or k0s rejects the join.
	workerProfile = "dpu"

	// etcdVolumeSize matches the CRD default, which our explicit spec would otherwise defeat.
	etcdVolumeSize = "1Gi"

	// ConditionSpecApplied reports whether the running control plane carries what
	// spec.clusterManagerConfig asked for. Declared here, since only this manager sets it.
	ConditionSpecApplied = "K0smotronSpecApplied"
)

// dpfOwnedSpecFields are reconciled even when the overlay says nothing about them. Everything
// else is reconciled only if the overlay names it, so a field k0smotron defaulted is left alone.
var dpfOwnedSpecFields = []string{"version", "k0sConfig", "patches", "service"}

// immutableSpecFields cannot be changed under a running control plane. A bound PVC's class and
// size are fixed, so patching them fails server side and the reconcile would retry it forever.
var immutableSpecFields = []string{"storage", "persistence"}

// +kubebuilder:rbac:groups=k0smotron.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

type clusterHandler struct {
	client.Client
	// k0sVersion is the version given to every cluster this handler creates.
	k0sVersion string
	// etcdStorageClass names the class backing every hosted control plane's etcd volume.
	etcdStorageClass string
}

// Option configures the handler.
type Option func(*clusterHandler)

// WithK0sVersion overrides the k0s version hosted control planes run.
func WithK0sVersion(version string) Option {
	return func(ch *clusterHandler) {
		ch.k0sVersion = version
	}
}

// WithEtcdStorageClass names the class backing a hosted control plane's etcd volume. Empty
// leaves k0smotron on the cluster default, which a cluster without one does not have.
func WithEtcdStorageClass(storageClass string) Option {
	return func(ch *clusterHandler) {
		ch.etcdStorageClass = storageClass
	}
}

// NewHandler returns a ClusterHandler backed by k0smotron.
func NewHandler(c client.Client, options ...Option) controller.ClusterHandler {
	ch := &clusterHandler{Client: c, k0sVersion: DefaultK0sVersion}
	for _, option := range options {
		option(ch)
	}

	return ch
}

func (ch *clusterHandler) Type() string {
	return dutil.K0smotronClusterType
}

// DPFOperatorConfigToDPUClusters returns nothing, since no field of the DPFOperatorConfig
// reaches a hosted control plane.
func (ch *clusterHandler) DPFOperatorConfigToDPUClusters(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{}
}

// ReconcileCluster brings up the k0smotron Cluster for a DPUCluster and hands back the name
// of the Secret holding its kubeconfig, in the shape DPF reads.
func (ch *clusterHandler) ReconcileCluster(ctx context.Context, dc *provisioningv1.DPUCluster) (string, []metav1.Condition, error) {
	conds, err := ch.reconcileK0smotronCluster(ctx, dc)
	if err != nil {
		cond := cutil.NewCondition(string(provisioningv1.ConditionCreated), err, "CreateK0smotronClusterError", err.Error())
		return "", append(conds, *cond), err
	}

	// The kubeconfig only exists once a control plane replica has served it, so a miss here
	// is the normal path on the first passes rather than a failure.
	secretName, err := ch.reconcileKubeconfig(ctx, dc)
	if err != nil {
		cond := cutil.NewCondition(string(provisioningv1.ConditionCreated), err, "KubeconfigNotReady", err.Error())
		return "", append(conds, *cond), err
	}

	cond := cutil.NewCondition(string(provisioningv1.ConditionCreated), nil, "Created", "")

	return secretName, append(conds, *cond), nil
}

// CleanUpCluster deletes the hosted control plane. Its kubeconfig Secret is owned by the
// DPUCluster, so it goes with the DPUCluster rather than here.
func (ch *clusterHandler) CleanUpCluster(ctx context.Context, dc *provisioningv1.DPUCluster) (bool, error) {
	cluster := &k0smotronv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: dc.Name, Namespace: dc.Namespace},
	}

	// Deleting by name alone would take a Cluster of the same name that belongs to something
	// else. Nothing to clean up if it is not ours.
	key := client.ObjectKeyFromObject(cluster)
	existing := &k0smotronv1.Cluster{}
	switch err := ch.Get(ctx, key, existing); {
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("failed to get k0smotron Cluster %s: %w", key, err)
	case !ownedBy(existing, dc):
		return true, nil
	}

	if err := ch.Delete(ctx, cluster); err != nil {
		// A missing CRD means k0smotron itself is gone, which took the control plane with
		// it. Reporting that as an error would hold the DPUCluster on its finalizer.
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return true, nil
		}
		return false, fmt.Errorf("failed to delete k0smotron Cluster %s/%s: %w", dc.Namespace, dc.Name, err)
	}

	// Reporting unfinished would hold the DPUCluster on its finalizer until the next resync,
	// minutes later. The Cluster is owned by the DPUCluster, so it goes either way.
	return true, nil
}

// ownedBy reports whether the object is controlled by the given owner. Mirrors the check the
// kamaji handler makes before it touches anything it did not create.
func ownedBy(obj metav1.Object, owner metav1.Object) bool {
	ref := metav1.GetControllerOf(obj)

	return ref != nil && ref.UID == owner.GetUID()
}

// reconcileK0smotronCluster creates the hosted control plane, then keeps the fields DPF owns
// and the fields the overlay names in step. It returns any condition the caller should report.
func (ch *clusterHandler) reconcileK0smotronCluster(ctx context.Context, dc *provisioningv1.DPUCluster) ([]metav1.Condition, error) {
	cluster := &k0smotronv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: dc.Name, Namespace: dc.Namespace},
	}
	key := client.ObjectKeyFromObject(cluster)

	desired, err := ch.desiredCluster(dc)
	if err != nil {
		cond := cutil.NewCondition(ConditionSpecApplied, err, "InvalidClusterManagerConfig", err.Error())
		return []metav1.Condition{*cond}, err
	}

	err = ch.Get(ctx, key, cluster)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(dc, desired, ch.Scheme()); err != nil {
			return nil, fmt.Errorf("failed to set owner on k0smotron Cluster %s: %w", key, err)
		}
		// Everything applies at creation, including the fields that cannot be changed later.
		if err := ch.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to create k0smotron Cluster %s: %w", key, err)
		}

		return []metav1.Condition{*specAppliedCondition(nil)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get k0smotron Cluster %s: %w", key, err)
	}

	// Refuse a Cluster this DPUCluster does not own. Patching it would reconfigure someone
	// else's control plane, and CleanUpCluster would later delete it by name.
	if !ownedBy(cluster, dc) {
		return nil, fmt.Errorf("k0smotron Cluster %s exists but is not owned by this DPUCluster", key)
	}

	drifted := immutableDrift(&cluster.Spec, &desired.Spec, dc.Spec.ClusterManagerConfig)
	conds := []metav1.Condition{*specAppliedCondition(drifted)}

	patch, err := specMergePatch(&cluster.Spec, &desired.Spec, dc.Spec.ClusterManagerConfig)
	if err != nil {
		return conds, fmt.Errorf("failed to build the patch for k0smotron Cluster %s: %w", key, err)
	}
	if patch == nil {
		return conds, nil
	}

	// A merge patch of named keys only, so a field k0smotron defaulted and nobody asked about
	// is left alone rather than reset by a full spec write.
	if err := ch.Patch(ctx, cluster, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return conds, fmt.Errorf("failed to patch k0smotron Cluster %s: %w", key, err)
	}

	return conds, nil
}

// desiredCluster builds the hosted control plane for a DPUCluster, with the user's overlay
// merged over it, and refuses one that could not serve a DPU cluster.
func (ch *clusterHandler) desiredCluster(dc *provisioningv1.DPUCluster) (*k0smotronv1.Cluster, error) {
	cluster := ch.baseCluster(dc)

	if err := mergeClusterManagerConfig(&cluster.Spec, dc.Spec.ClusterManagerConfig); err != nil {
		return nil, err
	}
	if err := validateClusterSpec(&cluster.Spec); err != nil {
		return nil, err
	}

	return cluster, nil
}

// mergeClusterManagerConfig applies a partial k0smotron ClusterSpec over the one DPF builds.
// Decoding into the populated struct merges per field, and replaces arrays rather than joining.
func mergeClusterManagerConfig(spec *k0smotronv1.ClusterSpec, raw *runtime.RawExtension) error {
	if raw == nil || len(raw.Raw) == 0 {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw.Raw))
	// A misspelled field would otherwise be dropped in silence, which is the failure a
	// passthrough like this one produces most often.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(spec); err != nil {
		return fmt.Errorf("spec.clusterManagerConfig is not a valid k0smotron ClusterSpec: %w", err)
	}

	return nil
}

// validateClusterSpec refuses a control plane no DPU could join. The overlay can set anything,
// including the few things the join path depends on.
func validateClusterSpec(spec *k0smotronv1.ClusterSpec) error {
	if spec.Version == "" {
		return fmt.Errorf("version is empty, and a worker downloads the version it reads back off it")
	}

	// k0smotron defaults this to ClusterIP, which nothing outside the host cluster can reach.
	switch spec.Service.Type {
	case corev1.ServiceTypeNodePort, corev1.ServiceTypeLoadBalancer:
	default:
		return fmt.Errorf("service.type %q does not expose the control plane, so no DPU could reach it", spec.Service.Type)
	}

	if !hasWorkerProfile(spec.K0sConfig, workerProfile) {
		return fmt.Errorf("k0sConfig does not define the %q worker profile the join script names", workerProfile)
	}

	if spec.Storage.Type == k0smotronv1.StorageTypeEtcd && spec.Storage.Etcd.Persistence.Size.IsZero() {
		return fmt.Errorf("storage.etcd.persistence.size is zero, which no PersistentVolumeClaim accepts")
	}

	return nil
}

// hasWorkerProfile reports whether the k0s config defines the named worker profile.
func hasWorkerProfile(config *unstructured.Unstructured, name string) bool {
	if config == nil {
		return false
	}

	profiles, found, err := unstructured.NestedSlice(config.Object, "spec", "workerProfiles")
	if err != nil || !found {
		return false
	}
	for _, profile := range profiles {
		entry, ok := profile.(map[string]any)
		if ok && entry["name"] == name {
			return true
		}
	}

	return false
}

// reconciledSpecFields is the set of top level spec keys to keep in step, being the ones DPF
// decides plus the ones the overlay names, less the ones a running control plane cannot change.
func reconciledSpecFields(raw *runtime.RawExtension) (map[string]bool, error) {
	fields := make(map[string]bool, len(dpfOwnedSpecFields))
	for _, field := range dpfOwnedSpecFields {
		fields[field] = true
	}

	if raw != nil && len(raw.Raw) > 0 {
		overlay := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw.Raw, &overlay); err != nil {
			return nil, fmt.Errorf("failed to read the fields spec.clusterManagerConfig sets: %w", err)
		}
		for field := range overlay {
			fields[field] = true
		}
	}

	for _, field := range immutableSpecFields {
		delete(fields, field)
	}

	return fields, nil
}

// specMergePatch returns a merge patch carrying the reconciled fields that differ, or nil when
// the running control plane already matches.
func specMergePatch(existing, desired *k0smotronv1.ClusterSpec, raw *runtime.RawExtension) ([]byte, error) {
	fields, err := reconciledSpecFields(raw)
	if err != nil {
		return nil, err
	}

	existingFields, err := specFields(existing)
	if err != nil {
		return nil, err
	}
	desiredFields, err := specFields(desired)
	if err != nil {
		return nil, err
	}

	changed := map[string]json.RawMessage{}
	for field := range fields {
		want, ok := desiredFields[field]
		if !ok {
			continue
		}
		if !bytes.Equal(existingFields[field], want) {
			changed[field] = want
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}

	return json.Marshal(map[string]any{"spec": changed})
}

// specFields renders a spec as its top level keys, so they can be compared and patched by name.
func specFields(spec *k0smotronv1.ClusterSpec) (map[string]json.RawMessage, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the k0smotron ClusterSpec: %w", err)
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, fmt.Errorf("failed to read back the k0smotron ClusterSpec: %w", err)
	}

	return fields, nil
}

// specAppliedCondition reports whether the running control plane carries what was asked for.
// Always returned, so a drift reported once clears as soon as the overlay stops asking for it.
func specAppliedCondition(drifted []string) *metav1.Condition {
	if len(drifted) == 0 {
		return cutil.NewCondition(ConditionSpecApplied, nil, "Applied", "")
	}

	err := fmt.Errorf("%s cannot change under a running control plane, so the cluster still runs what it was created with", strings.Join(drifted, " and "))

	return cutil.NewCondition(ConditionSpecApplied, err, "ImmutableFieldChanged", err.Error())
}

// immutableDrift names the fields an overlay asks to change that a running control plane cannot.
// Silently ignoring them would leave the user reading a spec that is not what is running.
func immutableDrift(existing, desired *k0smotronv1.ClusterSpec, raw *runtime.RawExtension) []string {
	if raw == nil || len(raw.Raw) == 0 {
		return nil
	}

	overlay := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw.Raw, &overlay); err != nil {
		return nil
	}

	existingFields, err := specFields(existing)
	if err != nil {
		return nil
	}
	desiredFields, err := specFields(desired)
	if err != nil {
		return nil
	}

	drifted := make([]string, 0, len(immutableSpecFields))
	for _, field := range immutableSpecFields {
		if _, asked := overlay[field]; !asked {
			continue
		}
		if !bytes.Equal(existingFields[field], desiredFields[field]) {
			drifted = append(drifted, field)
		}
	}

	return drifted
}

// baseCluster is the hosted control plane DPF builds on its own, before any overlay.
func (ch *clusterHandler) baseCluster(dc *provisioningv1.DPUCluster) *k0smotronv1.Cluster {
	return &k0smotronv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dc.Name,
			Namespace: dc.Namespace,
			Labels:    map[string]string{provisioningv1.DPUClusterNameLabelKey: dc.Name},
		},
		Spec: k0smotronv1.ClusterSpec{
			// Set explicitly rather than left to k0smotron, because a worker downloads the
			// version it reads back off this field.
			Version: ch.k0sVersion,
			// Restates the CRD defaults. Node ports are cluster scoped, so a second
			// DPUCluster has to move them through spec.clusterManagerConfig.
			Service: k0smotronv1.ServiceSpec{
				Type:             corev1.ServiceTypeNodePort,
				APIPort:          30443,
				KonnectivityPort: 30132,
			},
			K0sConfig: workerProfileConfig(),
			Patches:   controlPlaneTolerationPatches(),
			// Set explicitly. The vendored types omit `omitempty`, so a zero value is sent
			// and defaulting never fills it, and a size of 0 no PersistentVolumeClaim accepts.
			Storage: k0smotronv1.StorageSpec{
				Type: k0smotronv1.StorageTypeEtcd,
				Etcd: k0smotronv1.EtcdSpec{
					Image: k0smotronv1.DefaultEtcdImage,
					Persistence: k0smotronv1.StoragePersistenceSpec{
						StorageClass: ch.etcdStorageClass,
						Size:         resource.MustParse(etcdVolumeSize),
					},
					// Otherwise the volume outlives the DPUCluster, and one recreated under
					// the same name mounts stale etcd data against freshly issued certificates.
					AutoDeletePVCs: true,
				},
			},
		},
	}
}

// controlPlaneTolerationPatches lets the hosted control plane run on a tainted control plane
// node. k0smotron tolerates nothing, so a host cluster with no plain workers cannot place it.
func controlPlaneTolerationPatches() []k0smotronv1.ComponentPatch {
	// Matches the tolerations the operator gives every component it deploys itself.
	tolerations := `spec:
  template:
    spec:
      tolerations:
        - key: node-role.kubernetes.io/master
          operator: Exists
          effect: NoSchedule
        - key: node-role.kubernetes.io/control-plane
          operator: Exists
          effect: NoSchedule
`

	patches := make([]k0smotronv1.ComponentPatch, 0, 2)
	// The two StatefulSets k0smotron generates, matched by their component label.
	for _, component := range []string{"control-plane", "etcd"} {
		patches = append(patches, k0smotronv1.ComponentPatch{
			Target: k0smotronv1.PatchTarget{Kind: "StatefulSet", Component: component},
			Patch: k0smotronv1.PatchSpec{
				Type:    k0smotronv1.StrategicMergePatchType,
				Content: tolerations,
			},
		})
	}

	return patches
}

// workerProfileConfig returns the k0s config carrying the profile a DPU joins under. The join
// script names it, and its values are where DPU specific kubelet settings belong.
func workerProfileConfig() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k0s.k0sproject.io/v1beta1",
			"kind":       "ClusterConfig",
			"spec": map[string]any{
				"workerProfiles": []any{
					map[string]any{
						"name":   workerProfile,
						"values": map[string]any{},
					},
				},
			},
		},
	}
}

// reconcileKubeconfig republishes k0smotron's admin kubeconfig under the name and key DPF
// reads, and returns that Secret's name.
func (ch *clusterHandler) reconcileKubeconfig(ctx context.Context, dc *provisioningv1.DPUCluster) (string, error) {
	source := &corev1.Secret{}
	sourceKey := types.NamespacedName{Namespace: dc.Namespace, Name: fmt.Sprintf("%s-kubeconfig", dc.Name)}
	if err := ch.Get(ctx, sourceKey, source); err != nil {
		return "", fmt.Errorf("failed to get the k0smotron kubeconfig Secret %s: %w", sourceKey, err)
	}

	kubeconfig, ok := source.Data[k0smotronKubeconfigKey]
	if !ok || len(kubeconfig) == 0 {
		return "", fmt.Errorf("the k0smotron kubeconfig Secret %s has no %q yet", sourceKey, k0smotronKubeconfigKey)
	}

	target := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-admin-kubeconfig", dc.Name),
			Namespace: dc.Namespace,
		},
	}
	if _, err := ctrl.CreateOrUpdate(ctx, ch.Client, target, func() error {
		target.Type = corev1.SecretTypeOpaque
		target.Data = map[string][]byte{dpfKubeconfigKey: kubeconfig}

		return controllerutil.SetControllerReference(dc, target, ch.Scheme())
	}); err != nil {
		return "", fmt.Errorf("failed to publish the kubeconfig Secret for DPUCluster %s/%s: %w", dc.Namespace, dc.Name, err)
	}

	return target.Name, nil
}
