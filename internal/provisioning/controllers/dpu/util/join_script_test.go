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
	"os/exec"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

func TestShellSafe(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		// A k0s worker token is standard base64, so the alphabet it uses has to pass.
		// Tightening this function until it rejects one of these would break every join.
		{"a base64 token with the whole alphabet", "aGVsbG8rd29ybGQvYWJj+/Yz==", true},
		{"a k0s version", "v1.35.6+k0s.0", true},
		{"an absolute path", "/usr/local/bin", true},
		{"a DPU name", "dpu-node-1.example.com", true},
		{"empty", "", true},

		// Inside single quotes nothing else is special, so these stay legal rather than
		// being rejected out of caution.
		{"a dollar sign, inert inside single quotes", "tok$HOME", true},
		{"a backtick, inert inside single quotes", "tok`id`", true},
		{"a double quote, inert inside single quotes", `tok"x`, true},
		{"a semicolon, inert inside single quotes", "tok;id", true},
		{"a backslash, inert inside single quotes", `tok\x`, true},

		// The quote closes the assignment, which is the only way to reach a new command.
		{"a single quote", "tok'", false},
		{"a quote opening a command", "tok'; rm -rf /; #", false},

		// A newline ends the assignment and a NUL makes the script unexecutable.
		{"a newline", "tok\nid", false},
		{"a carriage return", "tok\rid", false},
		{"a tab", "tok\tx", false},
		{"a NUL", "tok\x00x", false},
		{"a delete character", "tok\x7fx", false},

		// Globs, rejected so a value cannot expand against the DPU filesystem.
		{"a star", "tok*", false},
		{"a question mark", "tok?", false},
		{"an opening bracket", "tok[a]", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(ShellSafe(tc.value)).To(Equal(tc.want))
		})
	}
}

func TestRenderJoinScript(t *testing.T) {
	t.Run("renders the data it is given", func(t *testing.T) {
		g := NewWithT(t)
		out, err := RenderJoinScript("test", `token={{ .Token }}`, map[string]any{"Token": "abc"})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal("token=abc"))
	})

	t.Run("refuses a key with no value", func(t *testing.T) {
		g := NewWithT(t)
		// An empty variable in a script that runs as root is worse than a failure, so
		// missingkey=error has to stay on.
		_, err := RenderJoinScript("test", `token={{ .Absent }}`, map[string]any{})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("rendering the test join script"))
	})

	t.Run("reports a template that does not parse", func(t *testing.T) {
		g := NewWithT(t)
		_, err := RenderJoinScript("test", `{{ .Unclosed`, map[string]any{})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("parsing the test join script"))
	})

	t.Run("refuses to hand back an empty script", func(t *testing.T) {
		g := NewWithT(t)
		_, err := RenderJoinScript("test", "", map[string]any{})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("rendered empty"))
	})

	t.Run("refuses a script over the size limit", func(t *testing.T) {
		g := NewWithT(t)
		_, err := RenderJoinScript("test", `{{ .Big }}`, map[string]any{
			"Big": strings.Repeat("a", maxJoinScriptSize+1),
		})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("over the"))
	})

	t.Run("keeps the sprig map hermetic", func(t *testing.T) {
		g := NewWithT(t)
		// The non hermetic map reaches env and getHostByName, which would render the
		// controller's own environment into a script that runs as root on the DPU.
		for _, fn := range []string{`{{ env "HOME" }}`, `{{ expandenv "$HOME" }}`, `{{ getHostByName "localhost" }}`} {
			_, err := RenderJoinScript("test", fn, map[string]any{})
			g.Expect(err).To(HaveOccurred(), "expected %s to be unavailable", fn)
			g.Expect(err.Error()).To(ContainSubstring("not defined"))
		}
	})

	t.Run("still offers the hermetic sprig helpers", func(t *testing.T) {
		g := NewWithT(t)
		out, err := RenderJoinScript("test", `{{ "abc" | upper }}`, map[string]any{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal("ABC"))
	})
}

// TestK0sJoinScriptIsValidBash checks the rendered script parses as bash. Nothing else does,
// so a template edit that breaks the syntax would otherwise only fail on a DPU.
func TestK0sJoinScriptIsValidBash(t *testing.T) {
	g := NewWithT(t)

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}

	script, err := RenderJoinScript("k0s", k0sJoinScript, k0sJoinScriptData{
		JoinToken:      "a-worker-token",
		NodeName:       "dpu-1",
		K0sVersion:     "v1.35.6+k0s.0",
		K0sInstallPath: k0sInstallPath,
		K0sProfile:     k0sWorkerProfile,
		DPUName:        "dpu-1",
		DPUNamespace:   "dpf-operator-system",
	})
	g.Expect(err).NotTo(HaveOccurred())

	cmd := exec.Command(bash, "-n")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), "bash -n rejected the rendered script: %s", out)
}
