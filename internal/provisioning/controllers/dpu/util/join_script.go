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
	"fmt"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// maxJoinScriptSize bounds a rendered script, so a runaway template is reported here
// rather than written to a DPU.
const maxJoinScriptSize = 128 * 1024

// RenderJoinScript executes a join script template. A key with no value is an error, since
// the result runs as root on the DPU and an empty variable there is worse than a failure.
func RenderJoinScript(name, script string, data any) (string, error) {
	// Hermetic, because the non hermetic map reaches env and getHostByName, which would
	// render the controller's own environment into a script that runs as root.
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

// ShellSafe reports whether a value can be substituted into a join script without changing
// what it does. Every value is assigned inside single quotes, so a quote is what breaks out.
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
