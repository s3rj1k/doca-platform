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
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	"github.com/nvidia/doca-platform/pkg/dpucluster"
	"github.com/nvidia/doca-platform/test/utils"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// k0sTestDownloadURL is what a version of 1.36.3+k0s.2 resolves to.
const k0sTestDownloadURL = "https://github.com/k0sproject/k0s/releases/download/v1.36.3%2Bk0s.2/k0s-v1.36.3%2Bk0s.2-arm64"

// clusterWithConfig builds a static DPUCluster carrying the given join config.
func clusterWithConfig(t *testing.T, config map[string]any) *provisioningv1.DPUCluster {
	t.Helper()

	spec := &provisioningv1.JoinTokenSpec{Type: provisioningv1.JoinTokenK0s}
	if config != nil {
		raw, err := json.Marshal(config)
		NewWithT(t).Expect(err).NotTo(HaveOccurred())
		spec.Config = &runtime.RawExtension{Raw: raw}
	}

	return &provisioningv1.DPUCluster{Spec: provisioningv1.DPUClusterSpec{
		Type:      string(provisioningv1.StaticCluster),
		JoinToken: spec,
	}}
}

// decodeK0sToken reverses the encoding, which is how a k0s worker reads its token.
func decodeK0sToken(t *testing.T, encoded string) *clientcmdapi.Config {
	t.Helper()
	g := NewWithT(t)

	raw, err := base64.StdEncoding.DecodeString(encoded)
	g.Expect(err).NotTo(HaveOccurred())
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = gz.Close() }()
	kubeconfig, err := io.ReadAll(gz)
	g.Expect(err).NotTo(HaveOccurred())

	config, err := clientcmd.Load(kubeconfig)
	g.Expect(err).NotTo(HaveOccurred())

	return config
}

// TestK0sEncodeToken pins the three names a k0s worker install looks for. Renaming any of them
// produces a token that decodes cleanly and then fails on the card.
func TestK0sEncodeToken(t *testing.T) {
	g := NewWithT(t)
	caCert := []byte("-----BEGIN CERTIFICATE-----\nnot a real one\n-----END CERTIFICATE-----\n")

	encoded, err := k0sEncodeToken("https://192.0.2.1:6443", caCert, "abcdef.0123456789abcdef")
	g.Expect(err).NotTo(HaveOccurred())

	config := decodeK0sToken(t, encoded)
	g.Expect(config.CurrentContext).To(Equal("k0s"))
	g.Expect(config.Clusters).To(HaveKey("k0s"))
	g.Expect(config.Contexts).To(HaveKey("k0s"))
	g.Expect(config.AuthInfos).To(HaveKey("kubelet-bootstrap"))

	g.Expect(config.Clusters["k0s"].Server).To(Equal("https://192.0.2.1:6443"))
	g.Expect(config.Clusters["k0s"].CertificateAuthorityData).To(Equal(caCert))
	g.Expect(config.Contexts["k0s"].AuthInfo).To(Equal("kubelet-bootstrap"))
	g.Expect(config.AuthInfos["kubelet-bootstrap"].Token).To(Equal("abcdef.0123456789abcdef"))
}

// TestK0sBootstrapSecret pins the shape k0s needs, which is not the shape kubeadm needs.
func TestK0sBootstrapSecret(t *testing.T) {
	g := NewWithT(t)
	expiresAt := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	secret := k0sBootstrapSecret("abcdef", "0123456789abcdef", expiresAt)

	g.Expect(secret.Name).To(Equal("bootstrap-token-abcdef"))
	g.Expect(secret.Namespace).To(Equal(metav1.NamespaceSystem))
	g.Expect(secret.Type).To(Equal(corev1.SecretTypeBootstrapToken))
	g.Expect(secret.StringData).To(HaveKeyWithValue("token-id", "abcdef"))
	g.Expect(secret.StringData).To(HaveKeyWithValue("token-secret", "0123456789abcdef"))
	g.Expect(secret.StringData).To(HaveKeyWithValue("expiration", "2026-08-25T12:00:00Z"))
	g.Expect(secret.StringData).To(HaveKeyWithValue("usage-bootstrap-authentication", "true"))

	// k0s grants its own bootstrappers group, unlike the kamaji flavored kubeadm token.
	g.Expect(secret.StringData).NotTo(HaveKey("auth-extra-groups"))
	g.Expect(secret.StringData).NotTo(HaveKey("usage-bootstrap-signing"))
}

func TestGenerateBootstrapToken(t *testing.T) {
	g := NewWithT(t)

	id, secret, err := GenerateBootstrapToken()
	g.Expect(err).NotTo(HaveOccurred())
	// The bootstrap token API fixes both lengths, and a wrong one is rejected at join time.
	g.Expect(id).To(HaveLen(6))
	g.Expect(secret).To(HaveLen(16))

	otherID, _, err := GenerateBootstrapToken()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(otherID).NotTo(Equal(id))
}

func TestK0sConfig(t *testing.T) {
	t.Run("nothing set yields the proven defaults", func(t *testing.T) {
		g := NewWithT(t)

		config, err := k0sConfig(clusterWithConfig(t, nil))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(config).To(HaveKeyWithValue("criSocket", "remote:unix:///run/containerd/containerd.sock"))
		g.Expect(config).To(HaveKeyWithValue("profile", "dpu"))
		g.Expect(config).To(HaveKeyWithValue("kubeletRootDir", "/var/lib/kubelet"))
		g.Expect(config).To(HaveKeyWithValue("extraArgs", ""))
		g.Expect(config).To(HaveKeyWithValue("readyFile", "/var/lib/k0s/kubelet.conf"))
		g.Expect(config).To(HaveKeyWithValue("sha256", ""))
	})

	t.Run("a partial block overrides only what it names", func(t *testing.T) {
		g := NewWithT(t)

		config, err := k0sConfig(clusterWithConfig(t, map[string]any{"profile": "default"}))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(config).To(HaveKeyWithValue("profile", "default"))
		g.Expect(config).To(HaveKeyWithValue("kubeletRootDir", "/var/lib/kubelet"))
	})

	// The block is opaque to the API server, so the script can grow keys without one.
	t.Run("a key the script does not know is passed through", func(t *testing.T) {
		g := NewWithT(t)

		config, err := k0sConfig(clusterWithConfig(t, map[string]any{"somethingNew": "value"}))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(config).To(HaveKeyWithValue("somethingNew", "value"))
	})

	t.Run("a non string is rejected and named", func(t *testing.T) {
		g := NewWithT(t)

		_, err := k0sConfig(clusterWithConfig(t, map[string]any{"profile": 3}))
		g.Expect(err).To(MatchError(ContainSubstring("joinToken.config.profile")))
		g.Expect(err).To(MatchError(ContainSubstring("has to be a string")))
	})

	t.Run("a block that is not an object is rejected", func(t *testing.T) {
		g := NewWithT(t)
		dc := clusterWithConfig(t, nil)
		dc.Spec.JoinToken.Config = &runtime.RawExtension{Raw: []byte(`["not", "an", "object"]`)}

		_, err := k0sConfig(dc)
		g.Expect(err).To(MatchError(ContainSubstring("not an object")))
	})

	// Every value is assigned inside single quotes, so a quote is what reaches a new command.
	// This is the check that matters most now the block is untyped and runs as root.
	t.Run("a value that would change what the script does is rejected", func(t *testing.T) {
		for name, injection := range map[string]string{
			"closes the quote":    `a'; rm -rf /; echo '`,
			"newline ends it":     "a\nid",
			"NUL breaks exec":     "a\x00b",
			"star globs":          "--labels dpu=*",
			"question mark globs": "dpu-?",
			"bracket globs":       "dpu-[ab]",
		} {
			g := NewWithT(t)

			_, err := k0sConfig(clusterWithConfig(t, map[string]any{"extraArgs": injection}))
			g.Expect(err).To(MatchError(ContainSubstring("joinToken.config.extraArgs")), "should reject %s", name)
		}
	})

	// Inert in every position the template uses, since an unquoted expansion is not rescanned
	// for expansions. Rejecting them would turn away legitimate kubelet arguments.
	t.Run("a value a shell would not act on is accepted", func(t *testing.T) {
		for name, value := range map[string]string{
			"dollar":      "--labels cost=$5",
			"backtick":    "--labels a=`b`",
			"backslash":   `--node-labels=a\,b`,
			"doublequote": `--labels a="b"`,
			"semicolon":   "--labels a=b;c",
		} {
			g := NewWithT(t)

			config, err := k0sConfig(clusterWithConfig(t, map[string]any{"extraArgs": value}))
			g.Expect(err).NotTo(HaveOccurred(), "should accept %s", name)
			g.Expect(config).To(HaveKeyWithValue("extraArgs", value))
		}
	})
}

func TestK0sDownloadURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  map[string]string
		want    string
		wantErr string
	}{
		{
			name:   "no version means the DPU already has k0s",
			config: map[string]string{},
			want:   "",
		},
		{
			name:   "a version derives the GitHub release URL",
			config: map[string]string{"version": "1.36.3+k0s.2"},
			want: "https://github.com/k0sproject/k0s/releases/download/v1.36.3%2Bk0s.2/" +
				"k0s-v1.36.3%2Bk0s.2-arm64",
		},
		{
			name:   "an explicit url wins",
			config: map[string]string{"version": "1.36.3+k0s.2", "url": "https://mirror.example.com/k0s-arm64"},
			want:   "https://mirror.example.com/k0s-arm64",
		},
		{
			name:    "a url without a version is rejected",
			config:  map[string]string{"url": "https://mirror.example.com/k0s-arm64"},
			wantErr: "joinToken.config.url",
		},
		{
			name:    "a local path is rejected, since the script would run it as root",
			config:  map[string]string{"version": "1.36.3+k0s.2", "url": "file:///bin/busybox"},
			wantErr: "has to be an https URL",
		},
		{
			name:    "plaintext is rejected, since the binary is not checksummed",
			config:  map[string]string{"version": "1.36.3+k0s.2", "url": "http://mirror.example.com/k0s"},
			wantErr: "has to be an https URL",
		},
		{
			name:    "a url with no host is rejected",
			config:  map[string]string{"version": "1.36.3+k0s.2", "url": "https:///k0s"},
			wantErr: "has to be an https URL",
		},
		{
			name:    "a version that is not a k0s release is rejected",
			config:  map[string]string{"version": "1.36.3"},
			wantErr: "is not a k0s release",
		},
		{
			name:    "a version carrying a path is rejected",
			config:  map[string]string{"version": "../../etc/passwd"},
			wantErr: "is not a k0s release",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := k0sDownloadURL(tc.config)
			if tc.wantErr != "" {
				g.Expect(err).To(MatchError(ContainSubstring(tc.wantErr)))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
}

// TestK0sJoinScriptFlagOrder pins the one flag a cluster must not be able to displace. k0s takes
// the last occurrence, so an extraArgs naming kubelet-extra-args would otherwise drop the hostname
// override, the worker would join under its real hostname and DPF would never see the node.
func TestK0sJoinScriptFlagOrder(t *testing.T) {
	g := NewWithT(t)

	config := maps.Clone(k0sConfigDefaults)
	config["extraArgs"] = "--kubelet-extra-args=--node-ip=192.0.2.10"

	script, err := RenderJoinScript("join_k0s.sh", k0sJoinScript, &k0sScriptData{
		Config:       config,
		JoinToken:    "c3RhbmRJblRva2Vu",
		NodeName:     "dpu-node-1",
		DPUNamespace: "dpf-operator-system",
		K0sBinPath:   k0sBinPath,
	})
	g.Expect(err).NotTo(HaveOccurred())

	theirs := strings.Index(script, "--kubelet-extra-args=--node-ip=192.0.2.10")
	ours := strings.Index(script, `--kubelet-extra-args="--hostname-override=$NODE_NAME"`)
	g.Expect(theirs).To(BeNumerically(">", -1), "the configured extraArgs should still be rendered")
	g.Expect(ours).To(BeNumerically(">", theirs), "DPF's hostname override has to come last to win")
}

// TestK0sDownloadSHA256 covers the digest, which is the only thing standing between a mirror and
// root code execution once a cluster asks for verification.
func TestK0sDownloadSHA256(t *testing.T) {
	const digest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	t.Run("a digest rides along with a version", func(t *testing.T) {
		g := NewWithT(t)

		url, err := k0sDownloadURL(map[string]string{"version": "1.36.3+k0s.2", "sha256": digest})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(url).To(ContainSubstring("k0s-v1.36.3%2Bk0s.2-arm64"))
	})

	t.Run("a digest with nothing to download is refused", func(t *testing.T) {
		g := NewWithT(t)

		_, err := k0sDownloadURL(map[string]string{"sha256": digest})
		g.Expect(err).To(MatchError(ContainSubstring("nothing to verify")))
	})

	for name, bad := range map[string]string{
		"too short":  "9f86d081",
		"upper case": "9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08",
		"not hex":    "zzzzd081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	} {
		t.Run("a malformed digest is refused, "+name, func(t *testing.T) {
			g := NewWithT(t)

			_, err := k0sDownloadURL(map[string]string{"version": "1.36.3+k0s.2", "sha256": bad})
			g.Expect(err).To(MatchError(ContainSubstring("lower case hex sha256")))
		})
	}
}

// TestRenderK0sJoinScript compares against checked in goldens, since the result runs as root on
// the card and a diff is the only readable way to review a change to it.
func TestRenderK0sJoinScript(t *testing.T) {
	base := func() *k0sScriptData {
		return &k0sScriptData{
			Config: map[string]string{
				"criSocket":      "remote:unix:///run/containerd/containerd.sock",
				"profile":        "dpu",
				"kubeletRootDir": "/var/lib/kubelet",
				"extraArgs":      "--labels dpu=true",
				"readyFile":      "/var/lib/k0s/kubelet.conf",
				"sha256":         "",
			},
			JoinToken:    "c3RhbmRJblRva2Vu",
			NodeName:     "dpu-node-1",
			DPUNamespace: "dpf-operator-system",
			K0sBinPath:   "/usr/local/bin/k0s",
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(*k0sScriptData)
		golden string
	}{
		{
			name: "a version downloads it",
			mutate: func(d *k0sScriptData) {
				d.DownloadURL = k0sTestDownloadURL
			},
			golden: "join_k0s_download.golden",
		},
		{
			name:   "no version expects k0s to be there",
			mutate: func(d *k0sScriptData) {},
			golden: "join_k0s_preinstalled.golden",
		},
		{
			// What a cluster naming only a version gets, which is the common case.
			name: "the default config",
			mutate: func(d *k0sScriptData) {
				d.Config = maps.Clone(k0sConfigDefaults)
				d.DownloadURL = k0sTestDownloadURL
			},
			golden: "join_k0s_defaults.golden",
		},
		{
			// A cluster that will not trust the download on TLS alone.
			name: "a sha256 is verified before the binary is installed",
			mutate: func(d *k0sScriptData) {
				d.Config = maps.Clone(k0sConfigDefaults)
				d.Config["sha256"] = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
				d.DownloadURL = k0sTestDownloadURL
			},
			golden: "join_k0s_verified.golden",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			data := base()
			tc.mutate(data)

			script, err := RenderJoinScript("join_k0s.sh", k0sJoinScript, data)
			g.Expect(err).NotTo(HaveOccurred())

			path := filepath.Join("testdata", tc.golden)
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				g.Expect(os.WriteFile(path, []byte(script), 0o644)).To(Succeed())
				t.Skip("golden updated, rerun without UPDATE_GOLDEN to check it")
			}
			want, err := os.ReadFile(path)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(script).To(Equal(string(want)))

			parsed := filepath.Join(t.TempDir(), "join.sh")
			g.Expect(os.WriteFile(parsed, []byte(script), 0o600)).To(Succeed())
			out, err := exec.Command("bash", "-n", parsed).CombinedOutput()
			g.Expect(err).NotTo(HaveOccurred(), "rendered script is not valid bash: %s", out)
		})
	}

	t.Run("a template key with no value fails rather than rendering empty", func(t *testing.T) {
		g := NewWithT(t)
		data := base()
		delete(data.Config, "profile")

		_, err := RenderJoinScript("join_k0s.sh", k0sJoinScript, data)
		g.Expect(err).To(HaveOccurred())
	})
}

// TestK0sGenerateJoinCommand covers the generator itself, since the helper tests cannot see a
// caller that hands them the wrong thing. It is the test that catches a stripped URL scheme.
func TestK0sGenerateJoinCommand(t *testing.T) {
	g := NewWithT(t)

	namespace := metav1.NamespaceDefault
	dpuCluster := utils.GetTestDPUCluster(namespace, "test-cluster-k0s")
	dpuCluster.Spec.Type = string(provisioningv1.StaticCluster)
	dpuCluster.Spec.JoinToken = &provisioningv1.JoinTokenSpec{
		Type:   provisioningv1.JoinTokenK0s,
		Config: &runtime.RawExtension{Raw: []byte(`{"version":"1.36.3+k0s.2"}`)},
	}
	secret, err := utils.GetFakeKamajiClusterSecretFromEnvtest(dpuCluster, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(testClient.Create(ctx, &dpuCluster)).To(Succeed())
	g.Expect(testClient.Create(ctx, secret)).To(Succeed())
	defer func() {
		g.Expect(utils.CleanupAndWait(ctx, testClient, &dpuCluster)).To(Succeed())
		g.Expect(utils.CleanupAndWait(ctx, testClient, secret)).To(Succeed())
	}()

	dpu := &provisioningv1.DPU{ObjectMeta: metav1.ObjectMeta{Name: "dpu-k0s-1", Namespace: namespace}}
	generator := &K0sJoinTokenGenerator{testClient}
	cmd, err := generator.GenerateJoinCommand(ctx, &dpuCluster, dpu)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cmd.TokenID).NotTo(BeEmpty())
	g.Expect(cmd.ExpiresAt).To(BeTemporally("~", time.Now().Add(DefaultJoinTokenTTL), time.Minute))

	// The node the script names has to be the one DPF looks for, or the join goes unnoticed.
	g.Expect(cmd.Command).To(ContainSubstring("NODE_NAME='dpu-k0s-1'"))
	g.Expect(cmd.Command).To(ContainSubstring("k0s-v1.36.3%2Bk0s.2-arm64"))

	// The real token, not the id and not a placeholder, and it has to decode to a kubeconfig
	// whose server keeps its scheme. A stripped scheme is silently useless to a k0s worker.
	token := regexp.MustCompile(`JOIN_TOKEN='([^']+)'`).FindStringSubmatch(cmd.Command)
	g.Expect(token).To(HaveLen(2))
	config := decodeK0sToken(t, token[1])
	g.Expect(config.Clusters["k0s"].Server).To(HavePrefix("https://"))
	g.Expect(config.AuthInfos["kubelet-bootstrap"].Token).To(HavePrefix(cmd.TokenID + "."))

	// The Secret that makes the token authenticate has to exist in the DPU cluster.
	dpuClusterClient, err := dpucluster.NewConfig(testClient, &dpuCluster).Client(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	minted := &corev1.Secret{}
	g.Expect(dpuClusterClient.Get(ctx, types.NamespacedName{
		Namespace: metav1.NamespaceSystem, Name: BootstrapTokenSecretName(cmd.TokenID),
	}, minted)).To(Succeed())
	g.Expect(minted.Type).To(Equal(corev1.SecretTypeBootstrapToken))
}

// templateConfigMap creates a ConfigMap holding a join script template and removes it after.
func templateConfigMap(t *testing.T, name string, data map[string]string) {
	t.Helper()
	g := NewWithT(t)

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: metav1.NamespaceDefault},
		Data:       data,
	}
	g.Expect(testClient.Create(ctx, configMap)).To(Succeed())
	t.Cleanup(func() {
		g.Expect(utils.CleanupAndWait(ctx, testClient, configMap)).To(Succeed())
	})
}

// clusterWithTemplateRef builds a static k0s DPUCluster naming a script template.
func clusterWithTemplateRef(name, key string) *provisioningv1.DPUCluster {
	dc := &provisioningv1.DPUCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-with-template", Namespace: metav1.NamespaceDefault},
		Spec: provisioningv1.DPUClusterSpec{
			Type: string(provisioningv1.StaticCluster),
			JoinToken: &provisioningv1.JoinTokenSpec{
				Type:              provisioningv1.JoinTokenK0s,
				ScriptTemplateRef: &provisioningv1.ScriptTemplateRef{Name: name, Key: key},
			},
		},
	}

	return dc
}

// TestK0sJoinTemplate covers which template gets rendered. A cluster that names one and does not
// get it has to fail, since falling back would silently run a different script as root.
func TestK0sJoinTemplate(t *testing.T) {
	t.Run("no reference uses the template this build ships", func(t *testing.T) {
		g := NewWithT(t)

		script, err := JoinScriptTemplate(ctx, testClient, clusterWithConfig(t, nil), k0sJoinScript)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(script).To(Equal(k0sJoinScript))
	})

	t.Run("a reference with no key reads K0S_JOIN_TEMPLATE", func(t *testing.T) {
		g := NewWithT(t)
		templateConfigMap(t, "tmpl-default-key", map[string]string{
			JoinScriptTemplateKey: "#!/usr/bin/env bash\necho default-key\n",
		})

		script, err := JoinScriptTemplate(ctx, testClient, clusterWithTemplateRef("tmpl-default-key", ""), k0sJoinScript)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(script).To(ContainSubstring("echo default-key"))
	})

	t.Run("a named key wins over the default one", func(t *testing.T) {
		g := NewWithT(t)
		templateConfigMap(t, "tmpl-named-key", map[string]string{
			JoinScriptTemplateKey: "#!/usr/bin/env bash\necho wrong\n",
			"JOIN_SCRIPT":         "#!/usr/bin/env bash\necho named\n",
		})

		script, err := JoinScriptTemplate(ctx, testClient, clusterWithTemplateRef("tmpl-named-key", "JOIN_SCRIPT"), k0sJoinScript)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(script).To(ContainSubstring("echo named"))
		g.Expect(script).NotTo(ContainSubstring("echo wrong"))
	})

	t.Run("a missing ConfigMap is an error naming it", func(t *testing.T) {
		g := NewWithT(t)

		_, err := JoinScriptTemplate(ctx, testClient, clusterWithTemplateRef("tmpl-absent", ""), k0sJoinScript)
		g.Expect(err).To(MatchError(ContainSubstring("default/tmpl-absent")))
	})

	t.Run("a missing key is an error naming the key", func(t *testing.T) {
		g := NewWithT(t)
		templateConfigMap(t, "tmpl-wrong-key", map[string]string{"SOMETHING_ELSE": "echo hi"})

		_, err := JoinScriptTemplate(ctx, testClient, clusterWithTemplateRef("tmpl-wrong-key", ""), k0sJoinScript)
		g.Expect(err).To(MatchError(ContainSubstring(JoinScriptTemplateKey)))
	})

	t.Run("an empty key is an error rather than an empty script", func(t *testing.T) {
		g := NewWithT(t)
		templateConfigMap(t, "tmpl-empty", map[string]string{JoinScriptTemplateKey: "   \n\t"})

		_, err := JoinScriptTemplate(ctx, testClient, clusterWithTemplateRef("tmpl-empty", ""), k0sJoinScript)
		g.Expect(err).To(MatchError(ContainSubstring("empty")))
	})
}

// TestRenderK0sCustomTemplate covers what a custom template may rely on, which is the contract
// a deployed template is written against.
func TestRenderK0sCustomTemplate(t *testing.T) {
	data := func() *k0sScriptData {
		return &k0sScriptData{
			Config:           map[string]string{"myOwnKey": "mine"},
			JoinToken:        "c3RhbmRJblRva2Vu",
			NodeName:         "dpu-node-1",
			DPUName:          "dpu-node-1",
			DPUNamespace:     "dpf-operator-system",
			ClusterName:      "cluster-1",
			ClusterNamespace: "dpf-operator-system",
			K0sBinPath:       k0sBinPath,
			DownloadURL:      "",
		}
	}

	// The whole point of the override, since a key the shipped script never reads is usable.
	t.Run("a config key only the custom template knows is substituted", func(t *testing.T) {
		g := NewWithT(t)

		script, err := RenderJoinScript("join_k0s.sh", "echo {{ .Config.myOwnKey }}", data())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(script).To(Equal("echo mine"))
	})

	t.Run("the DPU and cluster identity is available", func(t *testing.T) {
		g := NewWithT(t)

		script, err := RenderJoinScript("join_k0s.sh", "{{ .DPUNamespace }}/{{ .DPUName }} {{ .ClusterName }}", data())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(script).To(Equal("dpf-operator-system/dpu-node-1 cluster-1"))
	})

	// The non hermetic sprig map reaches os.Getenv, which would let a namespace scoped author
	// render the controller's own environment into a script that runs as root on the DPU.
	t.Run("the non hermetic sprig functions are not available", func(t *testing.T) {
		g := NewWithT(t)

		for _, fn := range []string{`{{ env "PATH" }}`, `{{ expandenv "$PATH" }}`, `{{ getHostByName "localhost" }}`} {
			_, err := RenderJoinScript("join_k0s.sh", fn, data())
			g.Expect(err).To(HaveOccurred(), "%s must not be callable from a custom template", fn)
		}
	})

	t.Run("sprig functions are available", func(t *testing.T) {
		g := NewWithT(t)

		script, err := RenderJoinScript("join_k0s.sh", `{{ upper .NodeName }}`, data())
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(script).To(Equal("DPU-NODE-1"))
	})

	t.Run("a template that does not parse is an error", func(t *testing.T) {
		g := NewWithT(t)

		_, err := RenderJoinScript("join_k0s.sh", "{{ .Config.myOwnKey", data())
		g.Expect(err).To(MatchError(ContainSubstring("parsing")))
	})

	t.Run("a key the template names but the config lacks is an error", func(t *testing.T) {
		g := NewWithT(t)

		_, err := RenderJoinScript("join_k0s.sh", "echo {{ .Config.notSet }}", data())
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("a template rendering past the size limit is refused", func(t *testing.T) {
		g := NewWithT(t)

		_, err := RenderJoinScript("join_k0s.sh", `{{ repeat 200000 "x" }}`, data())
		g.Expect(err).To(MatchError(ContainSubstring("over the")))
	})
}

// TestK0sGenerateJoinCommandWithTemplate proves the override reaches the script the DPU runs, and
// that the checks guarding the shipped script still apply to a cluster that replaces it.
func TestK0sGenerateJoinCommandWithTemplate(t *testing.T) {
	g := NewWithT(t)

	namespace := metav1.NamespaceDefault
	templateConfigMap(t, "tmpl-generator", map[string]string{
		JoinScriptTemplateKey: "#!/usr/bin/env bash\n# {{ .ClusterName }}\necho {{ .Config.myOwnKey }} {{ .NodeName }}\n",
	})

	dpuCluster := utils.GetTestDPUCluster(namespace, "test-cluster-k0s-tmpl")
	dpuCluster.Spec.Type = string(provisioningv1.StaticCluster)
	dpuCluster.Spec.JoinToken = &provisioningv1.JoinTokenSpec{
		Type:              provisioningv1.JoinTokenK0s,
		Config:            &runtime.RawExtension{Raw: []byte(`{"myOwnKey":"from-config"}`)},
		ScriptTemplateRef: &provisioningv1.ScriptTemplateRef{Name: "tmpl-generator"},
	}
	secret, err := utils.GetFakeKamajiClusterSecretFromEnvtest(dpuCluster, cfg)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(testClient.Create(ctx, &dpuCluster)).To(Succeed())
	g.Expect(testClient.Create(ctx, secret)).To(Succeed())
	defer func() {
		g.Expect(utils.CleanupAndWait(ctx, testClient, &dpuCluster)).To(Succeed())
		g.Expect(utils.CleanupAndWait(ctx, testClient, secret)).To(Succeed())
	}()

	dpu := &provisioningv1.DPU{ObjectMeta: metav1.ObjectMeta{Name: "dpu-k0s-tmpl", Namespace: namespace}}
	generator := &K0sJoinTokenGenerator{testClient}
	cmd, err := generator.GenerateJoinCommand(ctx, &dpuCluster, dpu)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(cmd.Command).To(ContainSubstring("echo from-config dpu-k0s-tmpl"))
	g.Expect(cmd.Command).To(ContainSubstring("# test-cluster-k0s-tmpl"))
	// The shipped script is gone, so nothing it did should still be there.
	g.Expect(cmd.Command).NotTo(ContainSubstring("k0s install worker"))
	// A token is still minted and revocable, since that is not the template's job.
	g.Expect(cmd.TokenID).NotTo(BeEmpty())

	// The value checks run before the template is read, so replacing the script does not buy a
	// way to reach a new command through joinToken.config.
	unsafe := dpuCluster.DeepCopy()
	unsafe.Spec.JoinToken.Config = &runtime.RawExtension{Raw: []byte(`{"myOwnKey":"a'; id; echo '"}`)}
	_, err = generator.GenerateJoinCommand(ctx, unsafe, dpu)
	g.Expect(err).To(MatchError(ContainSubstring("character a shell would read")))
}
