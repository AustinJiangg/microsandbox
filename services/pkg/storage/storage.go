// Package storage abstracts where a template's built artifacts live -- the rootfs + warm
// snapshot the orchestrator boots a sandbox from. It mirrors E2B's StorageProvider seam
// (Local now, object storage later).
//
// On one machine the artifacts are published *in place* at vendor/templates/<name>/ (where
// pkg/template.Resolve looks), not staged under a build id and moved: the Firecracker
// snapshot bakes in its rootfs's absolute path (see fc.Restore + scripts/build-snapshot.sh),
// so moving artifacts after the snapshot is built would break from_snapshot. The build_id
// is the async-job handle, not a storage key. A future object-storage impl would materialize
// artifacts to a local path before boot (firecracker reads local files, and the snapshot
// bakes a local path) -- which is exactly why a single machine's storage differs from E2B's
// build-id-keyed object store. See docs/STAGE10_DESIGN.md, Decision 2.
package storage

import (
	"fmt"
	"path/filepath"

	"microsandbox/services/pkg/template"
)

// StorageProvider resolves where a template's published artifacts live.
type StorageProvider interface {
	// TemplateDir returns the directory holding a template's rootfs.ext4 + snapshot/, i.e.
	// where pkg/template.Resolve(name) looks. The default template and invalid names are
	// rejected: "default" is the stock image baked into vendorDir, not built via the API.
	TemplateDir(name string) (string, error)
}

// Local stores template artifacts on the local filesystem under root (the orchestrator's
// vendorDir), at root/templates/<name>/.
type Local struct {
	root string
}

// NewLocal returns a Local provider rooted at root (the orchestrator's vendorDir).
func NewLocal(root string) *Local { return &Local{root: root} }

// TemplateDir computes the published artifact dir for name (pure path computation + name
// validation; existence is checked at boot by fc.Spawn / fc.Restore). The name rule is
// shared with pkg/template, so a name that builds is a name that resolves.
func (l *Local) TemplateDir(name string) (string, error) {
	if name == "" || name == template.DefaultTemplate {
		return "", fmt.Errorf("the default template is the baked stock image; it cannot be built via the API")
	}
	if !template.ValidName(name) {
		return "", fmt.Errorf("invalid template name %q: must match [a-z0-9][a-z0-9_-]* (max 64 chars)", name)
	}
	return filepath.Join(l.root, "templates", name), nil
}
