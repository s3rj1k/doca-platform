#!/usr/bin/env bash

#  2026 NVIDIA CORPORATION & AFFILIATES
#
#  Licensed under the Apache License, Version 2.0 (the License);
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an AS IS BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.

# Installs k0smotron in standalone mode. Invoked from the Makefile rather than as a helmfile
# hook, because helmfile skips releases that did not change and a hook there would not run.

set -euo pipefail

VERSION="${1:-}"

if [[ "${K0SMOTRON_ENABLED:-false}" != "true" ]]; then
	echo "K0SMOTRON_ENABLED is not true, skipping k0smotron"
	exit 0
fi

if [[ -z "$VERSION" ]]; then
	echo "usage: $0 <k0smotron-version>" >&2
	exit 1
fi

MANIFEST="https://docs.k0smotron.io/${VERSION}/install-standalone.yaml"
NAMESPACE="k0smotron"

echo "Applying k0smotron ${VERSION} from ${MANIFEST}"
# Server side, because the Cluster CRD is past the annotation size limit that a
# client side apply would try to record.
kubectl apply --server-side=true -f "$MANIFEST"

echo "Waiting for the k0smotron CRDs to be established"
kubectl wait --for=condition=Established --timeout=60s \
	crd/clusters.k0smotron.io crd/jointokenrequests.k0smotron.io

# The upstream manifest tolerates nothing, so it will not schedule on a host cluster whose
# only nodes are tainted control planes. These are the tolerations every DPF component gets.
echo "Tolerating control plane taints, as DPF does for its own components"
kubectl patch --namespace "$NAMESPACE" deployment/k0smotron-controller-manager --type=merge -p '{
  "spec": {"template": {"spec": {"tolerations": [
    {"key": "node-role.kubernetes.io/master", "operator": "Exists", "effect": "NoSchedule"},
    {"key": "node-role.kubernetes.io/control-plane", "operator": "Exists", "effect": "NoSchedule"}
  ]}}}
}'

echo "Waiting for the k0smotron controller to become available"
kubectl wait --for=condition=Available --timeout=300s \
	--namespace "$NAMESPACE" deployment/k0smotron-controller-manager

# Both webhooks fail closed and the Cluster CRD converts through one, so until cert-manager has
# injected the bundle even reading a Cluster fails. Available says nothing about that.
echo "Waiting for cert-manager to inject the webhook CA bundles"
kubectl wait --for=jsonpath='{.webhooks[0].clientConfig.caBundle}' --timeout=300s \
	mutatingwebhookconfiguration/k0smotron-mutating-webhook-configuration
kubectl wait --for=jsonpath='{.webhooks[0].clientConfig.caBundle}' --timeout=300s \
	validatingwebhookconfiguration/k0smotron-validating-webhook-configuration
kubectl wait --for=jsonpath='{.spec.conversion.webhook.clientConfig.caBundle}' --timeout=300s \
	crd/clusters.k0smotron.io

echo "k0smotron ${VERSION} is ready"
