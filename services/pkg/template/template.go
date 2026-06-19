// Package template is the (rootfs, snapshot) registry: it resolves a template name to
// the artifact paths the orchestrator boots a sandbox from. Stage 6 generalized the
// single image baked into vendor/ into named ones, so a sandbox can carry
// user-installed packages/files (E2B's headline feature). See docs/STAGE6_DESIGN.md.
//
// The "registry" is just the filesystem: a name resolves to fixed artifact paths
// under vendorDir, and existence is checked at spawn/restore time -- the same
// "artifact present == capability available" rule fc.CheckHostArtifacts uses. The
// reserved name "default" maps to the legacy top-level vendor/ paths, so every
// pre-Stage-6 artifact, test and script keeps working unchanged (no file moved, no
// snapshot rebuilt).
//
// Ported verbatim from control-plane/template.go (Stage 8a: relocated, not changed --
// only the struct fields and the resolver are exported now that other packages use them).
package template

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Template is a named custom image: the (rootfs, snapshot) pair to boot from.
type Template struct {
	Name        string // resolved name ("default" for the stock image)
	Rootfs      string // path to the ext4 rootfs the VM boots
	SnapshotDir string // dir holding the snapshot (vmstate / memfile) for from_snapshot
}

// DefaultTemplate is the stock image; an absent/empty request template means this.
const DefaultTemplate = "default"

// templateNameRE constrains a template name to a single safe path component. It
// forbids '/', '.', '..' and leading separators, so a name can never escape
// vendor/templates/<name>/ (e.g. "../../etc"); lowercase-only keeps names canonical.
var templateNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidName reports whether name is a legal template name (a single safe path component:
// [a-z0-9][a-z0-9_-]* up to 64 chars). Shared by Resolve and the storage/build layers so
// "a name that builds is a name that resolves" stays a single rule, not a duplicated regex.
func ValidName(name string) bool { return templateNameRE.MatchString(name) }

// Resolve maps a requested name to its artifact paths. It is pure (path computation +
// name validation only) -- existence is checked later by fc.Spawn / fc.Restore -- so it
// is unit-testable without a filesystem or a VM. "" and "default" resolve to the legacy
// vendor/ paths; any other name must be a valid component and resolves under
// vendor/templates/<name>/.
func Resolve(vendorDir, name string) (Template, error) {
	if name == "" || name == DefaultTemplate {
		return Template{
			Name:        DefaultTemplate,
			Rootfs:      filepath.Join(vendorDir, "rootfs.ext4"),
			SnapshotDir: filepath.Join(vendorDir, "snapshot"),
		}, nil
	}
	if !ValidName(name) {
		return Template{}, fmt.Errorf(
			"invalid template name %q: must match [a-z0-9][a-z0-9_-]* (max 64 chars)", name)
	}
	base := filepath.Join(vendorDir, "templates", name)
	return Template{
		Name:        name,
		Rootfs:      filepath.Join(base, "rootfs.ext4"),
		SnapshotDir: filepath.Join(base, "snapshot"),
	}, nil
}
