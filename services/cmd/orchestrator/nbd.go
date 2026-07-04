package main

import (
	"context"
	"fmt"
	"io"

	"microsandbox/services/pkg/block"
	"microsandbox/services/pkg/fc"
	"microsandbox/services/pkg/nbd"
	"microsandbox/services/pkg/storage"
	"microsandbox/services/pkg/template"
	"microsandbox/services/pkg/uffd"
)

// buildRootfsBacking prepares this VM's rootfs to be served over NBD (Stage 21c): it builds a read-only
// COW base that resolves each block through the rootfs header to its owning build's object in the bucket
// (streamed lazily, never assembled whole), binds it to a free /dev/nbdX, and returns an fc.RootfsBacking
// the VM boots against. The returned Close (run on Destroy) disconnects the device, closes the base's
// readers, and returns the device to the pool.
//
// It applies only in --nbd s3 mode; local-fs mode (storage == nil) and non-NBD keep the legacy
// materialized-file rootfs (a zero RootfsBacking), so those paths are unchanged.
func (s *server) buildRootfsBacking(tmpl template.Template) (fc.RootfsBacking, error) {
	if !s.useNBD || s.storage == nil {
		return fc.RootfsBacking{}, nil // legacy: the drive/snapshot uses the materialized file at tmpl.Rootfs
	}
	ctx := context.Background()
	buildID, err := storage.ResolveAlias(ctx, s.storage, tmpl.Name)
	if err != nil {
		return fc.RootfsBacking{}, err
	}
	// A Stage-20 layered child's snapshot is a re-snapshot of its base, so it bakes the base template's
	// rootfs path (recorded at {buildID}/rootfs.path). Bind the device over THAT path, not tmpl.Rootfs, so
	// firecracker opens what the vmstate references. Absent (non-layered / pre-Stage-20) => empty => Restore
	// binds over tmpl.Rootfs as before.
	bakedPath, err := storage.OpenRootfsBakedPath(ctx, s.storage, buildID)
	if err != nil {
		return fc.RootfsBacking{}, err
	}
	// The bind target must exist as a file for `mount --bind` (prepareRestore only ensures tmpl.Rootfs,
	// which for a layered child is NOT the baked path the device binds over). In --nbd mode the base's
	// rootfs is never materialized there, so create an empty placeholder the bind then shadows.
	if bakedPath != "" {
		if err := ensureFile(bakedPath); err != nil {
			return fc.RootfsBacking{}, err
		}
	}
	base, err := s.openRootfsBase(ctx, buildID)
	if err != nil {
		return fc.RootfsBacking{}, err
	}

	// Bind a read-only provider (Stage 21c serves ro) to a pooled device. On any failure past Get, put
	// the device back and close the base so nothing leaks.
	provider := block.NewReadOnly(base)
	idx, err := s.nbdPool.Get(ctx)
	if err != nil {
		_ = provider.Close()
		return fc.RootfsBacking{}, fmt.Errorf("acquire nbd device: %w", err)
	}
	exp, err := nbd.Bind(idx, provider)
	if err != nil {
		_ = provider.Close()
		s.nbdPool.Put(idx)
		return fc.RootfsBacking{}, fmt.Errorf("bind nbd%d: %w", idx, err)
	}
	return fc.RootfsBacking{
		Device:    nbd.DevicePath(idx),
		BakedPath: bakedPath, // "" for non-layered builds -> Restore binds over tmpl.Rootfs
		Close: func() error {
			derr := exp.Close()      // disconnect + stop the Dispatch goroutines (no more reads of provider)
			cerr := provider.Close() // close the base's per-owner bucket readers
			s.nbdPool.Put(idx)       // hand the device back to the pool
			if derr != nil {
				return derr
			}
			return cerr
		},
	}, nil
}

// openRootfsBase builds the read-only COW base for buildID: a layered source over the rootfs header's
// owner mapping (each run read from {owner}/rootfs.ext4), or -- for a non-layered build with no header --
// one run covering the whole {buildID}/rootfs.ext4. It reuses the exact per-owner chunked reader the
// memfile UFFD path uses (block.NewLayeredBase -> uffd.NewLayeredSource); this is the disk-side of the
// Stage-20a layered read, addressed by an NBD block read instead of a page fault.
func (s *server) openRootfsBase(ctx context.Context, buildID string) (block.ReadSource, error) {
	h, err := storage.OpenRootfsHeader(ctx, s.storage, buildID)
	if err != nil {
		return nil, err
	}
	var extents []uffd.Extent
	var size int64
	if h != nil {
		extents = make([]uffd.Extent, len(h.Mapping))
		for i, m := range h.Mapping {
			extents[i] = uffd.Extent{
				Logical:  int64(m.Offset),
				Length:   int64(m.Length),
				Physical: int64(m.BuildStorageOffset),
				Owner:    m.Owner,
			}
		}
		size = int64(h.Metadata.Size)
	} else {
		// Non-layered build (e.g. the default template): one run owned by this build over the whole object.
		size, err = storage.ObjectSize(ctx, s.storage, storage.ArtifactKey(buildID, storage.RootfsName))
		if err != nil {
			return nil, err
		}
		extents = []uffd.Extent{{Logical: 0, Length: size, Physical: 0, Owner: buildID}}
	}
	// open resolves an owner to its {owner}/rootfs.ext4 object; the layered source opens each once, lazily,
	// and closes them on Close. Background ctx: these readers live for the VM's whole life.
	open := func(owner string) (io.ReaderAt, func() error, error) {
		rr, err := s.storage.OpenReaderAt(context.Background(), storage.ArtifactKey(owner, storage.RootfsName))
		if err != nil {
			return nil, nil, err
		}
		return rr, rr.Close, nil
	}
	return block.NewLayeredBase(extents, size, 0, open), nil
}
