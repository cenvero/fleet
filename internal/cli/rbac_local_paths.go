// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/webui"
	"github.com/spf13/cobra"
)

// refuseScopedProtectedPath stops a scoped RBAC token from using the local side
// of a transfer to read or write the controller's own protected files: its
// configuration directory (token store, secrets, policies, automations, server
// records), private keys, known_hosts, data, logs and databases. Uploading the
// controller key to an in-scope server would hand out access to every server;
// downloading over tokens.json or the secret store would rewrite the token's
// own scope. These are exactly the locations the web UI's Local source
// refuses. Invocations without a token, or with an unscoped admin-equivalent
// token, are unaffected. With tree set, a directory that contains a protected
// location is refused too, since its whole tree is read, written or mirrored.
func refuseScopedProtectedPath(cmd *cobra.Command, configDir string, app *core.App, local string, tree bool) error {
	tok, err := currentVerifiedToken(cmd, configDir)
	if err != nil {
		return err
	}
	if tok == nil || !tok.IsScoped() {
		return nil
	}
	if err := webui.CheckLocalPath(app, local, tree); err != nil {
		core.AuditDeniedAccess(configDir, "token:"+tok.Name,
			fmt.Sprintf("%s local path %q: protected controller location", commandSecurityPath(cmd), local))
		return fmt.Errorf("denied: a scoped token cannot read or write %s: it is, or contains, the controller's configuration directory or key files", local)
	}
	return nil
}

// downloadLocalTargets lists the local paths a single-file download writes:
// the destination as given and, when that is an existing directory (or empty,
// meaning the working directory), the file inside it named after the remote.
func downloadLocalTargets(remote, local string) []string {
	base := path.Base(strings.ReplaceAll(remote, `\`, "/"))
	if local == "" {
		return []string{base}
	}
	targets := []string{local}
	if info, err := os.Stat(local); err == nil && info.IsDir() {
		targets = append(targets, filepath.Join(local, base))
	}
	return targets
}
