// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Re-running `fleet autocomplete install` must not touch the current block or
// the operator's own lines: the legacy marker is a prefix of the current one,
// and a substring match used to strip the block plus the line after it.
func TestRemoveMarkedBlockKeepsOperatorLines(t *testing.T) {
	cases := []struct {
		name, before, after string
		removed             bool
	}{
		{
			name:    "current block is not the legacy one",
			before:  "export A=1\n\n# cenvero-fleet:completion-file\n[ -f \"/h/.cenvero-fleet-completion.bash\" ] && source \"/h/.cenvero-fleet-completion.bash\"\nalias ll='ls -l'\n",
			after:   "export A=1\n\n# cenvero-fleet:completion-file\n[ -f \"/h/.cenvero-fleet-completion.bash\" ] && source \"/h/.cenvero-fleet-completion.bash\"\nalias ll='ls -l'\n",
			removed: false,
		},
		{
			name:    "legacy block followed directly by an operator line",
			before:  "export A=1\n\n# cenvero-fleet:completion\nsource <(fleet completion bash)\nalias ll='ls -l'\n",
			after:   "export A=1\nalias ll='ls -l'\n",
			removed: true,
		},
		{
			name:    "legacy block followed by a blank line",
			before:  "export A=1\n\n# cenvero-fleet:completion\nsource <(fleet completion bash)\n\nalias ll='ls -l'\n",
			after:   "export A=1\nalias ll='ls -l'\n",
			removed: true,
		},
		{
			name:    "legacy block at the end of the file",
			before:  "export A=1\n\n# cenvero-fleet:completion\nsource <(fleet completion bash)\n",
			after:   "export A=1\n",
			removed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc := filepath.Join(t.TempDir(), ".bashrc")
			if err := os.WriteFile(rc, []byte(c.before), 0o600); err != nil {
				t.Fatal(err)
			}
			removed, err := removeMarkedBlock(rc, "cenvero-fleet:completion")
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(rc)
			if err != nil {
				t.Fatal(err)
			}
			if removed != c.removed || string(got) != c.after {
				t.Fatalf("removed=%v, rc:\n%q\nwant removed=%v, rc:\n%q", removed, got, c.removed, c.after)
			}
		})
	}
}
