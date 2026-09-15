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

package util

import (
	"context"
	"strings"
	"testing"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	k0smotronv1 "github.com/nvidia/doca-platform/third_party/forked/github.com/k0sproject/k0smotron/api/k0smotron.io/v1beta2"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	k0sTestNamespace = "dpf-operator-system"
	k0sTestCluster   = "dpu-cplane"
	k0sTestDPU       = "dpu-1"
)

func k0sTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(provisioningv1.AddToScheme(s))
	utilruntime.Must(k0smotronv1.AddToScheme(s))
	return s
}

func k0sTestDPUCluster() *provisioningv1.DPUCluster {
	return &provisioningv1.DPUCluster{
		ObjectMeta: metav1.ObjectMeta{Name: k0sTestCluster, Namespace: k0sTestNamespace},
		Spec:       provisioningv1.DPUClusterSpec{Type: K0smotronClusterType},
	}
}

func k0sTestDPUObject() *provisioningv1.DPU {
	return &provisioningv1.DPU{
		ObjectMeta: metav1.ObjectMeta{Name: k0sTestDPU, Namespace: k0sTestNamespace},
	}
}

// k0smotronCluster builds the Cluster the generator reads the version from.
func k0smotronCluster(version string) *k0smotronv1.Cluster {
	return &k0smotronv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: k0sTestCluster, Namespace: k0sTestNamespace},
		Spec:       k0smotronv1.ClusterSpec{Version: version},
	}
}

func k0sTokenSecret(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      K0smotronJoinTokenRequestName(k0sTestDPUObject()),
			Namespace: k0sTestNamespace,
		},
		Data: map[string][]byte{"token": []byte(token)},
	}
}

func k0sTestClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(k0sTestScheme()).WithObjects(objects...).Build()
}

func TestK0smotronGenerateJoinCommand(t *testing.T) {
	ctx := context.Background()

	t.Run("renders the worker script and mints the request", func(t *testing.T) {
		g := NewWithT(t)
		c := k0sTestClient(k0smotronCluster("v1.33.1+k0s.0"), k0sTokenSecret("a-worker-token"))
		generator := &K0smotronJoinTokenGenerator{Client: c}

		script, err := generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).NotTo(HaveOccurred())

		g.Expect(script).To(ContainSubstring("JOIN_TOKEN='a-worker-token'"))
		g.Expect(script).To(ContainSubstring("K0S_VERSION='v1.33.1+k0s.0'"))
		g.Expect(script).To(ContainSubstring("K0S_PROFILE='dpu'"))
		g.Expect(script).To(ContainSubstring("NODE_NAME='" + k0sTestDPU + "'"))
		g.Expect(script).To(ContainSubstring("--profile \"$K0S_PROFILE\""))
		g.Expect(script).To(ContainSubstring("install worker"))

		// The request is what k0smotron turns into a token, and deleting it revokes.
		request := &k0smotronv1.JoinTokenRequest{}
		key := client.ObjectKey{
			Namespace: k0sTestNamespace,
			Name:      K0smotronJoinTokenRequestName(k0sTestDPUObject()),
		}
		g.Expect(c.Get(ctx, key, request)).To(Succeed())
		g.Expect(request.Spec.ClusterName).To(Equal(k0sTestCluster))
		g.Expect(request.Spec.Role).To(Equal("worker"))
		g.Expect(request.Spec.Expiry).To(Equal(k0smotronTokenExpiry))
	})

	t.Run("is idempotent when the request already exists", func(t *testing.T) {
		g := NewWithT(t)
		existing := &k0smotronv1.JoinTokenRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      K0smotronJoinTokenRequestName(k0sTestDPUObject()),
				Namespace: k0sTestNamespace,
			},
			Spec: k0smotronv1.JoinTokenRequestSpec{
				ClusterName: k0sTestCluster,
				Expiry:      k0smotronTokenExpiry,
				Role:        k0smotronWorkerRole,
			},
			Status: k0smotronv1.JoinTokenRequestStatus{TokenID: "abcdef"},
		}

		c := k0sTestClient(k0smotronCluster("v1.33.1+k0s.0"), k0sTokenSecret("a-worker-token"), existing)
		generator := &K0smotronJoinTokenGenerator{Client: c}

		_, err := generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).NotTo(HaveOccurred())
	})

	t.Run("replaces a request whose token secret has gone", func(t *testing.T) {
		g := NewWithT(t)
		// A token past its expiry presents as a vanished Secret. Reusing the request would
		// republish a dead token with no way out.
		stale := &k0smotronv1.JoinTokenRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      K0smotronJoinTokenRequestName(k0sTestDPUObject()),
				Namespace: k0sTestNamespace,
			},
			Spec: k0smotronv1.JoinTokenRequestSpec{
				ClusterName: k0sTestCluster,
				Role:        "worker",
			},
			// k0smotron minted this one, so a missing Secret really is an expiry rather than
			// a Secret that has not been written yet.
			Status: k0smotronv1.JoinTokenRequestStatus{TokenID: "abcdef"},
		}
		c := k0sTestClient(k0smotronCluster("v1.35.6+k0s.0"), stale)
		gen := &K0smotronJoinTokenGenerator{Client: c}

		// The stale request is retired, and deliberately not recreated in the same pass.
		_, err := gen.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("retired the stale JoinTokenRequest"))

		// The next pass finds it gone and mints a fresh one.
		_, err = gen.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).To(HaveOccurred())

		got := &k0smotronv1.JoinTokenRequest{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(stale), got)).To(Succeed())
		g.Expect(got.Spec.Expiry).To(Equal(k0smotronTokenExpiry), "should be the freshly minted request")
	})

	t.Run("replaces a request that asks for the wrong role", func(t *testing.T) {
		g := NewWithT(t)
		// The spec is not editable, and the CRD permits controller. Reusing it would join
		// the DPU as a control plane node.
		wrong := &k0smotronv1.JoinTokenRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      K0smotronJoinTokenRequestName(k0sTestDPUObject()),
				Namespace: k0sTestNamespace,
			},
			Spec: k0smotronv1.JoinTokenRequestSpec{ClusterName: k0sTestCluster, Role: "controller"},
		}
		c := k0sTestClient(k0smotronCluster("v1.35.6+k0s.0"), wrong, k0sTokenSecret("a-worker-token"))
		gen := &K0smotronJoinTokenGenerator{Client: c}

		// A wrong spec is retired regardless of status, and recreated on the next pass.
		_, err := gen.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("retired the stale JoinTokenRequest"))

		_, err = gen.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).NotTo(HaveOccurred())

		got := &k0smotronv1.JoinTokenRequest{}
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(wrong), got)).To(Succeed())
		g.Expect(got.Spec.Role).To(Equal("worker"))
	})

	t.Run("fails when the cluster reports no version", func(t *testing.T) {
		g := NewWithT(t)
		c := k0sTestClient(k0smotronCluster(""), k0sTokenSecret("a-worker-token"))
		generator := &K0smotronJoinTokenGenerator{Client: c}

		_, err := generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("no spec.version"))
	})

	t.Run("fails while the token secret has not appeared", func(t *testing.T) {
		g := NewWithT(t)
		c := k0sTestClient(k0smotronCluster("v1.33.1+k0s.0"))
		generator := &K0smotronJoinTokenGenerator{Client: c}

		_, err := generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to get the join token Secret"))
	})

	t.Run("trims the newline k0s emits with the token", func(t *testing.T) {
		g := NewWithT(t)
		// k0s token create ends its output with a newline and k0smotron stores the Secret
		// verbatim. Untrimmed it fails ShellSafe, so no DPU could ever join.
		c := k0sTestClient(k0smotronCluster("v1.35.6+k0s.0"), k0sTokenSecret("H4sIAAAAAAAC/2yVy5Kr+/abc=\n"))
		gen := &K0smotronJoinTokenGenerator{Client: c}

		script, err := gen.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(script).To(ContainSubstring("JOIN_TOKEN='H4sIAAAAAAAC/2yVy5Kr+/abc='"))
		g.Expect(script).NotTo(ContainSubstring("JOIN_TOKEN='H4sIAAAAAAAC/2yVy5Kr+/abc=\n"))
	})

	t.Run("reports a token that is only whitespace", func(t *testing.T) {
		g := NewWithT(t)
		c := k0sTestClient(k0smotronCluster("v1.35.6+k0s.0"), k0sTokenSecret("\n\n"))
		gen := &K0smotronJoinTokenGenerator{Client: c}

		_, err := gen.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("empty"))
	})

	t.Run("refuses a token a shell would read", func(t *testing.T) {
		g := NewWithT(t)
		c := k0sTestClient(k0smotronCluster("v1.33.1+k0s.0"), k0sTokenSecret("tok'; rm -rf /; #"))
		generator := &K0smotronJoinTokenGenerator{Client: c}

		_, err := generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("a shell would read"))
	})

	t.Run("needs the DPU it is for", func(t *testing.T) {
		g := NewWithT(t)
		generator := &K0smotronJoinTokenGenerator{Client: k0sTestClient()}

		_, err := generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), nil)
		g.Expect(err).To(HaveOccurred())
	})
}

func TestDeleteK0smotronJoinToken(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes the request that minted the token", func(t *testing.T) {
		g := NewWithT(t)
		request := &k0smotronv1.JoinTokenRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      K0smotronJoinTokenRequestName(k0sTestDPUObject()),
				Namespace: k0sTestNamespace,
			},
		}
		c := k0sTestClient(request)

		g.Expect(DeleteK0smotronJoinToken(ctx, c, k0sTestDPUCluster(), k0sTestDPUObject())).To(Succeed())

		remaining := &k0smotronv1.JoinTokenRequest{}
		key := client.ObjectKey{Namespace: k0sTestNamespace, Name: request.Name}
		g.Expect(c.Get(ctx, key, remaining)).NotTo(Succeed())
	})

	t.Run("tolerates a request that is already gone", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(DeleteK0smotronJoinToken(ctx, k0sTestClient(), k0sTestDPUCluster(), k0sTestDPUObject())).To(Succeed())
	})
}

func TestJoinCommandGeneratorsDispatch(t *testing.T) {
	ctx := context.Background()

	t.Run("routes a k0smotron cluster to its own generator", func(t *testing.T) {
		g := NewWithT(t)
		c := k0sTestClient(k0smotronCluster("v1.33.1+k0s.0"), k0sTokenSecret("a-worker-token"))
		generators := NewJoinCommandGenerators(c)

		script, err := generators.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(script).To(ContainSubstring("install worker"))
	})

	t.Run("falls back to kubeadm for the types DPF ships", func(t *testing.T) {
		g := NewWithT(t)
		generators := NewJoinCommandGenerators(k0sTestClient())
		for _, clusterType := range []string{
			string(provisioningv1.KamajiCluster),
			string(provisioningv1.StaticCluster),
		} {
			dc := k0sTestDPUCluster()
			dc.Spec.Type = clusterType

			// The kubeadm generator needs a reachable child cluster, which the fake has
			// none of. Reaching its failure proves the dispatch, without standing one up.
			_, err := generators.GenerateJoinCommand(ctx, dc, k0sTestDPUObject())
			g.Expect(err).To(HaveOccurred())
			g.Expect(strings.Contains(err.Error(), "install worker")).To(BeFalse())
		}
	})
}

// k0sJoinRequestKey names the JoinTokenRequest the generator mints for the test DPU.
func k0sJoinRequestKey() types.NamespacedName {
	return types.NamespacedName{
		Namespace: k0sTestNamespace,
		Name:      K0smotronJoinTokenRequestName(k0sTestDPUObject()),
	}
}

// TestK0smotronJoinTokenRequestSurvivesMinting guards a race that revoked the token as it was made.
// A Secret that has not appeared yet used to look exactly like one whose token had expired.
func TestK0smotronJoinTokenRequestSurvivesMinting(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	c := k0sTestClient(k0sTestDPUCluster(), k0smotronCluster("v1.35.6+k0s.0"))
	generator := &K0smotronJoinTokenGenerator{Client: c}

	// First pass creates the request. No Secret yet, which is reported so the caller comes back.
	_, err := generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
	g.Expect(err).To(HaveOccurred())

	created := &k0smotronv1.JoinTokenRequest{}
	g.Expect(c.Get(ctx, k0sJoinRequestKey(), created)).To(Succeed())
	// Marks this exact object, so a delete and recreate is visible rather than silent.
	created.Annotations = map[string]string{"test.dpf.nvidia.com/generation": "first"}
	g.Expect(c.Update(ctx, created)).To(Succeed())

	// Second pass, arriving the way the 5ms backoff does, long before k0smotron has minted.
	_, err = generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
	g.Expect(err).To(HaveOccurred())

	survived := &k0smotronv1.JoinTokenRequest{}
	g.Expect(c.Get(ctx, k0sJoinRequestKey(), survived)).To(Succeed())
	g.Expect(survived.Annotations).To(HaveKeyWithValue("test.dpf.nvidia.com/generation", "first"),
		"the request was replaced while k0smotron was still minting, which revokes the token")
}

// TestK0smotronJoinTokenRequestReplaced covers the cases that must still retire a request, so the
// fix above does not turn into "never replace anything".
func TestK0smotronJoinTokenRequestReplaced(t *testing.T) {
	ctx := context.Background()

	// minted carries a token ID, which is what k0smotron sets once it has run.
	minted := func(mutate func(*k0smotronv1.JoinTokenRequest)) *k0smotronv1.JoinTokenRequest {
		request := &k0smotronv1.JoinTokenRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:        k0sJoinRequestKey().Name,
				Namespace:   k0sJoinRequestKey().Namespace,
				Annotations: map[string]string{"test.dpf.nvidia.com/generation": "first"},
			},
			Spec: k0smotronv1.JoinTokenRequestSpec{
				ClusterName: k0sTestCluster,
				Expiry:      k0smotronTokenExpiry,
				Role:        k0smotronWorkerRole,
			},
			Status: k0smotronv1.JoinTokenRequestStatus{TokenID: "abcdef"},
		}
		if mutate != nil {
			mutate(request)
		}
		return request
	}

	tests := []struct {
		name    string
		request *k0smotronv1.JoinTokenRequest
	}{
		{
			name:    "a minted token whose Secret has gone",
			request: minted(nil),
		},
		{
			name: "a request that asks for another cluster",
			request: minted(func(r *k0smotronv1.JoinTokenRequest) {
				r.Spec.ClusterName = "somewhere-else"
			}),
		},
		{
			name: "a request that asks for the controller role",
			request: minted(func(r *k0smotronv1.JoinTokenRequest) {
				r.Spec.Role = "controller"
			}),
		},
		{
			// k0smotron never ran, so waiting on it forever would strand the DPU.
			name: "a request k0smotron never minted, past the window",
			request: minted(func(r *k0smotronv1.JoinTokenRequest) {
				r.Status.TokenID = ""
				r.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * joinTokenMintTimeout))
			}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			c := k0sTestClient(k0sTestDPUCluster(), k0smotronCluster("v1.35.6+k0s.0"), tc.request)
			generator := &K0smotronJoinTokenGenerator{Client: c}

			_, err := generator.GenerateJoinCommand(ctx, k0sTestDPUCluster(), k0sTestDPUObject())
			g.Expect(err).To(HaveOccurred())

			// Retired, and deliberately not recreated in the same pass. k0smotron finalizes the
			// request before it goes, so a create here would collide with one still terminating.
			replaced := &k0smotronv1.JoinTokenRequest{}
			err = c.Get(ctx, k0sJoinRequestKey(), replaced)
			if err == nil {
				g.Expect(replaced.Annotations).NotTo(HaveKey("test.dpf.nvidia.com/generation"),
					"the stale request was left in place")
			} else {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		})
	}
}

func TestUnmintedTooLong(t *testing.T) {
	g := NewWithT(t)

	fresh := &k0smotronv1.JoinTokenRequest{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Now())},
	}
	g.Expect(unmintedTooLong(fresh)).To(BeFalse())

	stale := &k0smotronv1.JoinTokenRequest{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * joinTokenMintTimeout))},
	}
	g.Expect(unmintedTooLong(stale)).To(BeTrue())
}
