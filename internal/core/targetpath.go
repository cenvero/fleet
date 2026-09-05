// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// TargetPathStyle describes the path syntax used by a managed node. It must be
// selected from the target's OS, never from the controller's runtime OS.
type TargetPathStyle string

const (
	TargetPathPOSIX   TargetPathStyle = "posix"
	TargetPathWindows TargetPathStyle = "windows"
)

// TargetPathStyleForOS returns the lexical path style for a GOOS value.
func TargetPathStyleForOS(goos string) TargetPathStyle {
	if strings.EqualFold(strings.TrimSpace(goos), "windows") {
		return TargetPathWindows
	}
	return TargetPathPOSIX
}

// NativePathStyle returns the controller-local path style.
func NativePathStyle() TargetPathStyle { return TargetPathStyleForOS(runtime.GOOS) }

// LocalPathCaseInsensitive reports whether the filesystem that would contain p
// aliases names by case. It probes the nearest existing directory with a private
// temporary file and removes it before returning.
func LocalPathCaseInsensitive(p string) (bool, error) {
	dir := filepath.Clean(p)
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			break
		}
		if !os.IsNotExist(err) {
			return false, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, fmt.Errorf("no existing parent for local path %q", p)
		}
		dir = parent
	}
	probe, err := os.CreateTemp(dir, ".fleet-case-probe-a-*")
	if err != nil {
		return false, fmt.Errorf("probe local path case sensitivity: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return false, err
	}
	defer os.Remove(probePath)
	alternate := filepath.Join(dir, strings.ToUpper(filepath.Base(probePath)))
	alternateInfo, err := os.Stat(alternate)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe local path case sensitivity: %w", err)
	}
	originalInfo, err := os.Stat(probePath)
	if err != nil {
		return false, err
	}
	return os.SameFile(originalInfo, alternateInfo), nil
}

// TargetPathStyleForPath infers Windows syntax only when a drive or UNC volume
// is present. It is useful as a compatibility fallback for old server records
// that predate persisted OS observations.
func TargetPathStyleForPath(p string) TargetPathStyle {
	if volume, _, _ := splitWindowsPath(p); volume != "" {
		return TargetPathWindows
	}
	return TargetPathPOSIX
}

// TargetPathStyleForServer determines path syntax from target metadata. A
// configured Windows remote directory remains a safe fallback when an old
// record has not yet persisted Observed.OS.
func TargetPathStyleForServer(server ServerRecord) TargetPathStyle {
	if TargetPathStyleForOS(server.Observed.OS) == TargetPathWindows {
		return TargetPathWindows
	}
	if TargetPathStyleForPath(server.FileTransfer.RemoteDir) == TargetPathWindows ||
		TargetPathStyleForPath(server.Metrics.DiskPath) == TargetPathWindows {
		return TargetPathWindows
	}
	return TargetPathPOSIX
}

// InitialRemotePath returns a valid initial browse path for a server. An
// operator-configured remote directory takes precedence, followed by the
// target-advertised file root on every OS. Windows targets then use the reported
// disk root when available and otherwise fall back to C:\.
func InitialRemotePath(server ServerRecord) string {
	style := TargetPathStyleForServer(server)
	advertised := strings.TrimSpace(server.Observed.FileRoot)
	if configured := strings.TrimSpace(server.FileTransfer.RemoteDir); configured != "" {
		configured = style.Clean(configured)
		if !style.IsAbs(advertised) {
			return configured
		}
		advertised = style.Clean(advertised)
		if _, err := style.Relative(advertised, configured); err == nil {
			return configured
		}
		return advertised
	}
	if style.IsAbs(advertised) {
		return style.Clean(advertised)
	}
	if style == TargetPathWindows {
		if disk := strings.TrimSpace(server.Metrics.DiskPath); style.IsAbs(disk) {
			volume, _, _ := splitWindowsPath(style.Clean(disk))
			if volume != "" {
				return volume + `\`
			}
		}
	}
	return style.DefaultRoot()
}

// RemotePathBoundary returns the highest path the file-manager UI should expose.
// A current agent advertises its native system root when unrestricted and its
// first configured file root when sandboxed. Old records fall back to the
// observed Windows disk volume or the native target root.
func RemotePathBoundary(server ServerRecord) string {
	style := TargetPathStyleForServer(server)
	if advertised := strings.TrimSpace(server.Observed.FileRoot); style.IsAbs(advertised) {
		return style.Clean(advertised)
	}
	if style == TargetPathWindows {
		if configured := strings.TrimSpace(server.FileTransfer.RemoteDir); style.IsAbs(configured) {
			volume, _, _ := splitWindowsPath(style.Clean(configured))
			if volume != "" {
				return volume + `\`
			}
		}
		if disk := strings.TrimSpace(server.Metrics.DiskPath); style.IsAbs(disk) {
			volume, _, _ := splitWindowsPath(style.Clean(disk))
			if volume != "" {
				return volume + `\`
			}
		}
	}
	return style.DefaultRoot()
}

func (s TargetPathStyle) IsWindows() bool { return s == TargetPathWindows }

func (s TargetPathStyle) Separator() string {
	if s.IsWindows() {
		return `\`
	}
	return "/"
}

func (s TargetPathStyle) DefaultRoot() string {
	if s.IsWindows() {
		return `C:\`
	}
	return "/"
}

// HasTrailingSeparator reports directory intent without cleaning it away.
func (s TargetPathStyle) HasTrailingSeparator(p string) bool {
	if p == "" {
		return false
	}
	if s.IsWindows() {
		return strings.HasSuffix(p, `\`) || strings.HasSuffix(p, "/")
	}
	return strings.HasSuffix(p, "/")
}

// ValidateTargetPathComponent rejects a value that cannot safely remain one
// filename component when used by a target-style path operation. Both separator
// forms are refused to keep cross-target transfers unambiguous. Windows targets
// additionally enforce Win32-invalid characters, trailing dot/space rules, and
// DOS device-name reservations.
func ValidateTargetPathComponent(style TargetPathStyle, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("name must not be %q", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("name must be one path component")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("name must not contain control characters")
		}
	}
	if !style.IsWindows() {
		return nil
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return fmt.Errorf("name contains a character invalid on Windows")
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("windows names must not end in a dot or space")
	}
	stem := name
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " ."))
	if windowsReservedComponent(stem) {
		return fmt.Errorf("%q is a reserved Windows name", name)
	}
	return nil
}

// ValidateTargetPath validates every filename component of p according to the
// destination style. Windows drive letters and UNC server/share volume syntax
// are excluded; all descendants, including the final top-level name, are
// checked. The path may be absolute or relative.
func ValidateTargetPath(style TargetPathStyle, p string) error {
	clean := style.Clean(strings.TrimSpace(p))
	var raw string
	if style.IsWindows() {
		_, rest, _ := splitWindowsPath(clean)
		raw = strings.Trim(rest, `\`)
	} else {
		raw = strings.Trim(clean, "/")
	}
	if raw == "" || raw == "." {
		return nil
	}
	separator := "/"
	if style.IsWindows() {
		separator = `\`
	}
	for _, component := range strings.Split(raw, separator) {
		if component == "" || component == "." {
			continue
		}
		if err := ValidateTargetPathComponent(style, component); err != nil {
			return fmt.Errorf("invalid destination path %q: %w", p, err)
		}
	}
	return nil
}

func windowsReservedComponent(stem string) bool {
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	runes := []rune(stem)
	if len(runes) != 4 || (string(runes[:3]) != "COM" && string(runes[:3]) != "LPT") {
		return false
	}
	return (runes[3] >= '1' && runes[3] <= '9') || runes[3] == '¹' || runes[3] == '²' || runes[3] == '³'
}

// Clean lexically normalizes a target path without consulting the controller
// filesystem. Windows cleaning is therefore identical on Linux, macOS, and
// Windows controllers.
func (s TargetPathStyle) Clean(p string) string {
	if !s.IsWindows() {
		return path.Clean(p)
	}
	return cleanWindowsPath(p)
}

// Join joins target-native path components. A later absolute/volume-qualified
// component replaces the earlier path instead of creating a mixed-style path.
func (s TargetPathStyle) Join(elements ...string) string {
	if !s.IsWindows() {
		current := ""
		for _, element := range elements {
			if element == "" {
				continue
			}
			if path.IsAbs(element) {
				current = element
			} else if current == "" {
				current = element
			} else {
				current = strings.TrimRight(current, "/") + "/" + strings.TrimLeft(element, "/")
			}
		}
		return path.Clean(current)
	}

	current := ""
	for _, element := range elements {
		if element == "" {
			continue
		}
		volume, _, _ := splitWindowsPath(element)
		if s.IsAbs(element) || volume != "" {
			current = element
			continue
		}
		if current == "" {
			current = element
			continue
		}
		current = strings.TrimRight(current, `\/`) + `\` + strings.TrimLeft(element, `\/`)
	}
	return cleanWindowsPath(current)
}

func (s TargetPathStyle) Dir(p string) string {
	if !s.IsWindows() {
		return path.Dir(p)
	}
	clean := cleanWindowsPath(p)
	if s.IsRoot(clean) {
		return clean
	}
	volume, rest, rooted := splitWindowsPath(clean)
	rest = strings.TrimRight(rest, `\`)
	idx := strings.LastIndex(rest, `\`)
	if idx < 0 {
		if volume != "" {
			if rooted {
				return volume + `\`
			}
			return volume
		}
		return "."
	}
	parent := strings.TrimRight(rest[:idx], `\`)
	if rooted {
		if parent == "" {
			return volume + `\`
		}
		return volume + `\` + strings.TrimLeft(parent, `\`)
	}
	if parent == "" {
		if volume != "" {
			return volume
		}
		return "."
	}
	return volume + parent
}

func (s TargetPathStyle) Base(p string) string {
	if !s.IsWindows() {
		return path.Base(p)
	}
	clean := cleanWindowsPath(p)
	if s.IsRoot(clean) {
		return `\`
	}
	_, rest, _ := splitWindowsPath(clean)
	rest = strings.TrimRight(rest, `\`)
	if idx := strings.LastIndex(rest, `\`); idx >= 0 {
		return rest[idx+1:]
	}
	return rest
}

func (s TargetPathStyle) IsAbs(p string) bool {
	if !s.IsWindows() {
		return path.IsAbs(p)
	}
	volume, _, rooted := splitWindowsPath(p)
	return volume != "" && rooted
}

func (s TargetPathStyle) IsRoot(p string) bool {
	clean := s.Clean(p)
	if !s.IsWindows() {
		return clean == "/"
	}
	volume, rest, rooted := splitWindowsPath(clean)
	return volume != "" && rooted && strings.Trim(rest, `\`) == ""
}

// Relative returns target as a slash-separated relative path beneath root.
// Slash-separated relative keys are an internal interchange form only; callers
// convert them back with style.Join when addressing the target or filepath when
// addressing the controller. Paths outside root are rejected.
func (s TargetPathStyle) Relative(root, target string) (string, error) {
	rootVolume, rootParts, rootAbs := s.absoluteParts(root)
	targetVolume, targetParts, targetAbs := s.absoluteParts(target)
	if !rootAbs || !targetAbs {
		return "", fmt.Errorf("target paths must be absolute: root=%q target=%q", root, target)
	}
	if s.IsWindows() {
		if !strings.EqualFold(rootVolume, targetVolume) {
			return "", fmt.Errorf("path %q is on a different volume than %q", target, root)
		}
	} else if rootVolume != targetVolume {
		return "", fmt.Errorf("path %q is outside %q", target, root)
	}
	if len(targetParts) < len(rootParts) {
		return "", fmt.Errorf("path %q is outside %q", target, root)
	}
	for i := range rootParts {
		equal := rootParts[i] == targetParts[i]
		if s.IsWindows() {
			equal = strings.EqualFold(rootParts[i], targetParts[i])
		}
		if !equal {
			return "", fmt.Errorf("path %q is outside %q", target, root)
		}
	}
	rel := strings.Join(targetParts[len(rootParts):], "/")
	if rel == "" {
		return ".", nil
	}
	return rel, nil
}

func (s TargetPathStyle) absoluteParts(p string) (volume string, parts []string, absolute bool) {
	clean := s.Clean(p)
	if !s.IsWindows() {
		if !path.IsAbs(clean) {
			return "", nil, false
		}
		return "/", splitNonEmpty(strings.TrimPrefix(clean, "/"), "/"), true
	}
	volume, rest, rooted := splitWindowsPath(clean)
	if volume == "" || !rooted {
		return volume, nil, false
	}
	return volume, splitNonEmpty(strings.Trim(rest, `\`), `\`), true
}

func splitNonEmpty(value, separator string) []string {
	if value == "" {
		return nil
	}
	raw := strings.Split(value, separator)
	out := raw[:0]
	for _, part := range raw {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
}

func cleanWindowsPath(p string) string {
	if p == "" {
		return "."
	}
	p = strings.ReplaceAll(p, "/", `\`)
	volume, rest, rooted := splitWindowsPath(p)
	restSlash := strings.ReplaceAll(rest, `\`, "/")
	cleaned := path.Clean(restSlash)
	if rooted {
		cleaned = strings.TrimPrefix(cleaned, "/")
		if cleaned == "." || cleaned == "" {
			if volume != "" {
				return volume + `\`
			}
			return `\`
		}
		if volume != "" {
			return volume + `\` + strings.ReplaceAll(cleaned, "/", `\`)
		}
		return `\` + strings.ReplaceAll(cleaned, "/", `\`)
	}
	if cleaned == "." {
		if volume != "" {
			return volume
		}
		return "."
	}
	return volume + strings.ReplaceAll(cleaned, "/", `\`)
}

// splitWindowsPath returns the volume, remainder, and whether the remainder is
// rooted. It recognizes drive paths and UNC shares without relying on the host
// implementation of filepath.VolumeName.
func splitWindowsPath(p string) (volume, rest string, rooted bool) {
	p = strings.ReplaceAll(p, "/", `\`)
	if len(p) >= 2 && p[1] == ':' && unicode.IsLetter(rune(p[0])) {
		volume = p[:2]
		rest = p[2:]
		rooted = strings.HasPrefix(rest, `\`)
		return volume, rest, rooted
	}
	if !strings.HasPrefix(p, `\\`) {
		return "", p, strings.HasPrefix(p, `\`)
	}

	// A UNC volume consists of exactly the leading server and share components.
	tail := strings.TrimLeft(p, `\`)
	first := strings.Index(tail, `\`)
	if first <= 0 {
		return "", p, true
	}
	server := tail[:first]
	tail = strings.TrimLeft(tail[first+1:], `\`)
	second := strings.Index(tail, `\`)
	share := tail
	rest = ""
	if second >= 0 {
		share = tail[:second]
		rest = tail[second:]
	}
	if share == "" {
		return "", p, true
	}
	return `\\` + server + `\` + share, rest, true
}
