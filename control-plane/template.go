package main

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// A template is a named custom image: the (rootfs, snapshot) pair the control plane
// boots a sandbox from. Stage 6 generalizes the single image baked into vendor/ into
// named ones, so a sandbox can carry user-installed packages/files (E2B's headline
// feature). See docs/STAGE6_DESIGN.md.
//
// The "registry" is just the filesystem: a name resolves to fixed artifact paths
// under vendorDir, and existence is checked at spawn/restore time -- the same
// "artifact present == capability available" rule checkHostArtifacts already uses.
// The reserved name "default" maps to the legacy top-level vendor/ paths, so every
// pre-Stage-6 artifact, test and script keeps working unchanged (no file moved, no
// snapshot rebuilt).
type template struct {
	name        string // resolved name ("default" for the stock image)
	rootfs      string // path to the ext4 rootfs the VM boots
	snapshotDir string // dir holding the snapshot (vmstate / memfile) for from_snapshot
}

// defaultTemplate is the stock image; an absent/empty request template means this.
const defaultTemplate = "default"

// templateNameRE constrains a template name to a single safe path component. It
// forbids '/', '.', '..' and leading separators, so a name can never escape
// vendor/templates/<name>/ (e.g. "../../etc"); lowercase-only keeps names canonical.
var templateNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// resolveTemplate maps a requested name to its artifact paths. It is pure (path
// computation + name validation only) -- existence is checked later by
// spawnMicroVM / restoreMicroVM -- so it is unit-testable without a filesystem or a
// VM. "" and "default" resolve to the legacy vendor/ paths; any other name must be a
// valid component and resolves under vendor/templates/<name>/.
func resolveTemplate(vendorDir, name string) (template, error) {
	if name == "" || name == defaultTemplate {
		return template{
			name:        defaultTemplate,
			rootfs:      filepath.Join(vendorDir, "rootfs.ext4"),
			snapshotDir: filepath.Join(vendorDir, "snapshot"),
		}, nil
	}
	if !templateNameRE.MatchString(name) {
		return template{}, fmt.Errorf(
			"invalid template name %q: must match [a-z0-9][a-z0-9_-]* (max 64 chars)", name)
	}
	base := filepath.Join(vendorDir, "templates", name)
	return template{
		name:        name,
		rootfs:      filepath.Join(base, "rootfs.ext4"),
		snapshotDir: filepath.Join(base, "snapshot"),
	}, nil
}
