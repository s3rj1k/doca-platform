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
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	cutil "github.com/nvidia/doca-platform/internal/provisioning/controllers/util"
	"github.com/nvidia/doca-platform/pkg/dpucluster"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed join_k0s.sh.tmpl
var k0sJoinScript string

const (
	// k0sContextName is the cluster and context name k0s writes into its own join tokens.
	k0sContextName = "k0s"
	// k0sWorkerAuthName is the authinfo name a k0s worker install requires.
	k0sWorkerAuthName = "kubelet-bootstrap"
	// k0sBinPath is where the join script puts the k0s binary and looks for an existing one.
	k0sBinPath = "/usr/local/bin/k0s"
)

// k0sConfigKeys are the keys the join script reads. Anything else in the block is passed to
// the template untouched, so a script change needs no API change.
const (
	k0sKeyVersion        = "version"
	k0sKeyURL            = "url"
	k0sKeyCRISocket      = "criSocket"
	k0sKeyProfile        = "profile"
	k0sKeyKubeletRootDir = "kubeletRootDir"
	k0sKeyExtraArgs      = "extraArgs"
	k0sKeyReadyFile      = "readyFile"
	k0sKeySHA256         = "sha256"
)

// k0sConfigDefaults are the values the bring up proved. A block that names none of them still
// renders a working script.
var k0sConfigDefaults = map[string]string{
	k0sKeyCRISocket:      "remote:unix:///run/containerd/containerd.sock",
	k0sKeyProfile:        "dpu",
	k0sKeyKubeletRootDir: "/var/lib/kubelet",
	k0sKeyExtraArgs:      "",
	// Empty means the download is trusted on TLS alone. Set it to verify what runs as root.
	k0sKeySHA256: "",
	// Where k0s writes the worker kubeconfig under its default data dir. An extraArgs that moves
	// the data dir has to name the new path here, or the wait below never sees the credentials.
	k0sKeyReadyFile: "/var/lib/k0s/kubelet.conf",
}

// k0sVersionPattern accepts a k0s release such as 1.36.3+k0s.2. The block is not validated on
// admission, so a typo has to be caught here rather than becoming a 404 during flashing.
var k0sVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\+k0s\.[0-9]+$`)

// k0sSHA256Pattern accepts a lower case hex sha256. A malformed digest would otherwise fail the
// verification on the card rather than here.
var k0sSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// k0sScriptData is what a join script template can reference. A custom template is written
// against these fields, so removing or renaming one breaks templates that are already deployed.
type k0sScriptData struct {
	// Config is joinToken.config merged over the defaults, with every value shell safe.
	Config map[string]string
	// JoinToken is the encoded worker token this DPU presents.
	JoinToken string
	// NodeName is the name the node has to register under for DPF to see it.
	NodeName string
	// DPUName and DPUNamespace name the DPU the script was rendered for.
	DPUName      string
	DPUNamespace string
	// ClusterName and ClusterNamespace name the DPUCluster being joined.
	ClusterName      string
	ClusterNamespace string
	// K0sBinPath is where the script installs k0s and looks for an existing one.
	K0sBinPath string
	// DownloadURL is empty when no version was configured, meaning k0s has to be in the image.
	DownloadURL string
}

// K0sJoinTokenGenerator mints a k0s worker token and returns the script that joins with it.
type K0sJoinTokenGenerator struct {
	client.Client
}

// GenerateJoinCommand mints a worker token in the DPU cluster and renders the join script that
// presents it. The DPU names the node, which has to match the DPU object or DPF never sees it.
func (s *K0sJoinTokenGenerator) GenerateJoinCommand(ctx context.Context, dc *provisioningv1.DPUCluster, dpu *provisioningv1.DPU) (JoinCommand, error) {
	config, err := k0sConfig(dc)
	if err != nil {
		return JoinCommand{}, err
	}
	downloadURL, err := k0sDownloadURL(config)
	if err != nil {
		return JoinCommand{}, err
	}

	clusterConfig := dpucluster.NewConfig(s, dc)
	server, err := clusterConfig.Server(ctx)
	if err != nil {
		return JoinCommand{}, err
	}
	caCert, err := k0sCACert(ctx, clusterConfig)
	if err != nil {
		return JoinCommand{}, err
	}

	id, secret, err := GenerateBootstrapToken()
	if err != nil {
		return JoinCommand{}, err
	}
	expiresAt := time.Now().Add(JoinTokenTTL(dc))

	// Encoded before the Secret is created, so a failure here cannot leave a live token whose
	// id the caller never learns and so can never revoke.
	encoded, err := k0sEncodeToken(server, caCert, id+"."+secret)
	if err != nil {
		return JoinCommand{}, err
	}

	scriptTemplate, err := JoinScriptTemplate(ctx, s, dc, k0sJoinScript)
	if err != nil {
		return JoinCommand{}, err
	}

	// Rendered before the Secret too, for the same reason.
	script, err := RenderJoinScript("join_k0s.sh", scriptTemplate, &k0sScriptData{
		Config:           config,
		JoinToken:        encoded,
		NodeName:         cutil.GenerateNodeName(dpu),
		DPUName:          dpu.Name,
		DPUNamespace:     dpu.Namespace,
		ClusterName:      dc.Name,
		ClusterNamespace: dc.Namespace,
		K0sBinPath:       k0sBinPath,
		DownloadURL:      downloadURL,
	})
	if err != nil {
		return JoinCommand{}, err
	}

	dpuClusterClient, err := clusterConfig.Client(ctx)
	if err != nil {
		return JoinCommand{}, err
	}
	if err := dpuClusterClient.Create(ctx, k0sBootstrapSecret(id, secret, expiresAt)); err != nil {
		return JoinCommand{}, fmt.Errorf("failed to create bootstrap token secret: %w", err)
	}

	return JoinCommand{Command: script, TokenID: id, ExpiresAt: expiresAt}, nil
}

// k0sConfig merges the cluster's join config over the proven defaults. Every value has to be a
// string, since the script substitutes them into shell.
func k0sConfig(dc *provisioningv1.DPUCluster) (map[string]string, error) {
	config := make(map[string]string, len(k0sConfigDefaults))
	for key, value := range k0sConfigDefaults {
		config[key] = value
	}

	if dc.Spec.JoinToken == nil || dc.Spec.JoinToken.Config == nil || len(dc.Spec.JoinToken.Config.Raw) == 0 {
		return config, nil
	}

	raw := map[string]any{}
	if err := json.Unmarshal(dc.Spec.JoinToken.Config.Raw, &raw); err != nil {
		return nil, fmt.Errorf("joinToken.config is not an object: %w", err)
	}
	for key, value := range raw {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("joinToken.config.%s has to be a string, got %T", key, value)
		}
		// The script quotes every value, so one carrying a quote would escape it and run.
		if !ShellSafe(text) {
			return nil, fmt.Errorf("joinToken.config.%s contains a character a shell would read", key)
		}
		config[key] = text
	}

	return config, nil
}

// k0sDownloadURL reports where the join script should fetch k0s from, or the empty string when
// the DPU is expected to have it already.
func k0sDownloadURL(config map[string]string) (string, error) {
	version, override := config[k0sKeyVersion], config[k0sKeyURL]
	if digest := config[k0sKeySHA256]; digest != "" && !k0sSHA256Pattern.MatchString(digest) {
		return "", fmt.Errorf("joinToken.config.%s has to be a lower case hex sha256, got %q", k0sKeySHA256, digest)
	}
	if version == "" {
		if override != "" {
			return "", fmt.Errorf("joinToken.config.%s needs %s, since there is nothing to download without one", k0sKeyURL, k0sKeyVersion)
		}
		if config[k0sKeySHA256] != "" {
			return "", fmt.Errorf("joinToken.config.%s needs %s, since there is nothing to verify without one", k0sKeySHA256, k0sKeyVersion)
		}
		return "", nil
	}
	if !k0sVersionPattern.MatchString(version) {
		return "", fmt.Errorf("joinToken.config.%s %q is not a k0s release such as 1.36.3+k0s.2", k0sKeyVersion, version)
	}
	if override != "" {
		// The script installs whatever this points at as an executable and runs it as root,
		// so a local path or a plaintext fetch is not something to accept.
		parsed, err := url.Parse(override)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return "", fmt.Errorf("joinToken.config.%s has to be an https URL, got %q", k0sKeyURL, override)
		}
		return override, nil
	}

	// The plus in a k0s release has to be encoded, or the release is not found. The pattern
	// above admits nothing else that needs escaping.
	tag := "v" + strings.ReplaceAll(version, "+", "%2B")
	return fmt.Sprintf("https://github.com/k0sproject/k0s/releases/download/%s/k0s-%s-arm64", tag, tag), nil
}

// k0sBootstrapSecret builds the Secret that makes the token authenticate. k0s grants its
// bootstrappers group the rights it needs, so unlike kubeadm no extra group is set here.
func k0sBootstrapSecret(id, secret string, expiresAt time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BootstrapTokenSecretName(id),
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretTypeBootstrapToken,
		StringData: map[string]string{
			"token-id":                       id,
			"token-secret":                   secret,
			"expiration":                     expiresAt.Format(time.RFC3339),
			"usage-bootstrap-authentication": "true",
			"description":                    "Worker bootstrap token for a DPF provisioned DPU",
		},
	}
}

// k0sEncodeToken renders the bootstrap kubeconfig and packs it the way k0s expects. The three
// names below are what a k0s worker install looks for and must not be changed.
func k0sEncodeToken(apiServerURL string, caCert []byte, token string) (string, error) {
	kubeconfig, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{k0sContextName: {
			Server:                   apiServerURL,
			CertificateAuthorityData: caCert,
		}},
		Contexts: map[string]*clientcmdapi.Context{k0sContextName: {
			Cluster:  k0sContextName,
			AuthInfo: k0sWorkerAuthName,
		}},
		CurrentContext: k0sContextName,
		AuthInfos: map[string]*clientcmdapi.AuthInfo{k0sWorkerAuthName: {
			Token: token,
		}},
	})
	if err != nil {
		return "", fmt.Errorf("writing bootstrap kubeconfig: %w", err)
	}

	var out bytes.Buffer
	gz, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(gz, bytes.NewReader(kubeconfig)); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// k0sCACert reads the cluster CA the bootstrap kubeconfig has to trust.
func k0sCACert(ctx context.Context, clusterConfig *dpucluster.Config) ([]byte, error) {
	kubeconfig, err := clusterConfig.Kubeconfig(ctx)
	if err != nil {
		return nil, err
	}
	for _, cluster := range kubeconfig.Clusters {
		if len(cluster.CertificateAuthorityData) > 0 {
			return cluster.CertificateAuthorityData, nil
		}
	}

	return nil, fmt.Errorf("no certificate authority in the kubeconfig of DPUCluster %s", clusterConfig.ClusterNamespaceName())
}
