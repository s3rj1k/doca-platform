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
	"context"
	"fmt"
	"strings"
	"text/template"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"

	"github.com/Masterminds/sprig/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// JoinScriptTemplateKey is the ConfigMap key a scriptTemplateRef reads when it names none.
	JoinScriptTemplateKey = "JOIN_SCRIPT_TEMPLATE"

	// maxJoinScriptSize bounds a rendered script, so a runaway template is reported here rather
	// than written to a DPU.
	maxJoinScriptSize = 128 * 1024
)

// JoinScriptData is what every join script template can reference, whatever mechanism renders
// it. A mechanism with more to offer embeds this and adds its own fields.
type JoinScriptData struct {
	// Config is joinToken.config merged over the mechanism defaults, every value shell safe.
	Config map[string]string
	// JoinToken is the credential this DPU presents, in whatever shape its mechanism uses.
	JoinToken string
	// NodeName is the name the node has to register under for DPF to see it.
	NodeName string
	// DPUName and DPUNamespace name the DPU the script was rendered for.
	DPUName      string
	DPUNamespace string
	// ClusterName and ClusterNamespace name the DPUCluster being joined.
	ClusterName      string
	ClusterNamespace string
}

// JoinScriptTemplate returns the template to render, which is the one this build ships unless
// the cluster names a ConfigMap holding its own. A named template that cannot be read is an
// error rather than a fallback, since a wrong join shape fails on the card.
func JoinScriptTemplate(ctx context.Context, c client.Client, dc *provisioningv1.DPUCluster, shipped string) (string, error) {
	if dc.Spec.JoinToken == nil || dc.Spec.JoinToken.ScriptTemplateRef == nil {
		return shipped, nil
	}
	ref := dc.Spec.JoinToken.ScriptTemplateRef
	key := ref.Key
	if key == "" {
		key = JoinScriptTemplateKey
	}

	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: dc.Namespace, Name: ref.Name}, configMap); err != nil {
		return "", fmt.Errorf("reading join script template ConfigMap %s/%s: %w", dc.Namespace, ref.Name, err)
	}
	script, ok := configMap.Data[key]
	if !ok {
		return "", fmt.Errorf("join script template ConfigMap %s/%s has no key %q", dc.Namespace, ref.Name, key)
	}
	if strings.TrimSpace(script) == "" {
		return "", fmt.Errorf("join script template ConfigMap %s/%s has an empty key %q", dc.Namespace, ref.Name, key)
	}

	return script, nil
}

// RenderJoinScript executes a join script template. A key with no value is an error, since the
// result runs as root on the DPU and an empty variable there is worse than a failure here.
func RenderJoinScript(name, script string, data any) (string, error) {
	// Hermetic, because the non hermetic map reaches env and getHostByName, and a namespace
	// scoped author would then render the controller's own environment into a root script.
	tmpl, err := template.New(name).Funcs(sprig.HermeticTxtFuncMap()).Option("missingkey=error").Parse(script)
	if err != nil {
		return "", fmt.Errorf("parsing the %s join script: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering the %s join script: %w", name, err)
	}
	if buf.Len() == 0 {
		return "", fmt.Errorf("the %s join script rendered empty", name)
	}
	if buf.Len() > maxJoinScriptSize {
		return "", fmt.Errorf("the %s join script rendered %d bytes, over the %d limit", name, buf.Len(), maxJoinScriptSize)
	}

	return buf.String(), nil
}

// ShellSafe reports whether a value can be substituted into a join script without changing what
// it does. Every value is assigned inside single quotes, so a quote is what breaks out.
func ShellSafe(value string) bool {
	for _, r := range value {
		switch {
		// Closes the assignment, which is the only way to reach a new command.
		case r == '\'':
			return false
		// A newline ends the assignment and a NUL makes the whole script unexecutable.
		case r < 0x20 || r == 0x7f:
			return false
		// A value expanded unquoted is subject to word splitting and to globbing.
		case r == '*' || r == '?' || r == '[':
			return false
		}
	}

	return true
}
