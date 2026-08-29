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
	"fmt"
	"testing"
)

// TestKubeadmTemplateMatchesTheOldFormatter is the property the whole change rests on. The old
// path built the command with fmt.Sprintf; the template has to produce the same bytes.
func TestKubeadmTemplateMatchesTheOldFormatter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server string
		id     string
		secret string
		hashes []string
	}{
		{"no hashes", "192.0.2.1:6443", "abcdef", "0123456789abcdef", nil},
		{"one hash", "192.0.2.1:6443", "abcdef", "0123456789abcdef", []string{"sha256:aaa"}},
		{"two hashes", "10.0.0.1:6443", "112233", "445566778899aabb", []string{"sha256:aaa", "sha256:bbb"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Verbatim from the pre-template implementation.
			want := fmt.Sprintf("kubeadm join %s --token %s.%s --v=5", tc.server, tc.id, tc.secret)
			for _, h := range tc.hashes {
				want += fmt.Sprintf(" --discovery-token-ca-cert-hash %s", h)
			}

			got, err := RenderJoinScript("join_kubeadm.sh", kubeadmJoinScript, &kubeadmScriptData{
				JoinScriptData: JoinScriptData{JoinToken: tc.id + "." + tc.secret},
				APIServer:      tc.server,
				CACertHashes:   tc.hashes,
			})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != want {
				t.Fatalf("template drifted from the old formatter\n got: %q\nwant: %q", got, want)
			}
		})
	}
}
