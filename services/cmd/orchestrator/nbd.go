package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"microsandbox/services/pkg/block"
	"microsandbox/services/pkg/fc"
	"microsandbox/services/pkg/nbd"
	"microsandbox/services/pkg/storage"
	"microsandbox/services/pkg/template"
	"microsandbox/services/pkg/uffd"
)

// buildRootfsBacking prepares this VM's rootfs to be served over NBD as a per-VM writable overlay (Stage
// 21c + 22b): it builds a read-only COW base that resolves each block through the rootfs header to its
// owning build's object in the bucket (streamed lazily, never assembled whole), layers a private writable
// cache over it (block.Overlay -- so the guest mounts root rw and its writes land in that cache, the shared
// base objects staying immutable), binds it to a free /dev/nbdX, and returns an fc.RootfsBacking the VM
// boots against. The returned Close (run on Destroy) disconnects the device, closes the base's readers +
// the cache, and returns the device to the pool.
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

	// Stage 22b: give this VM a private writable overlay over the shared read-only base (E2B's model), so
	// the guest mounts root rw and its writes land in a per-VM sparse cache -- the shared base objects stay
	// immutable, and the layer producer can export the dirtied blocks as a rootfs diff. The cache file is
	// removed on Destroy.
	cachePath, cache, err := newRootfsCache(buildID, base.Size())
	if err != nil {
		_ = base.Close()
		return fc.RootfsBacking{}, err
	}
	provider, err := block.NewOverlay(base, cache)
	if err != nil {
		_ = cache.Close()
		_ = base.Close()
		_ = os.Remove(cachePath)
		return fc.RootfsBacking{}, err
	}
	// Bind the writable overlay to a pooled device. On any failure past Get, put the device back, close the
	// provider (base readers + cache handle), and remove the cache file so nothing leaks.
	idx, err := s.nbdPool.Get(ctx)
	if err != nil {
		_ = provider.Close()
		_ = os.Remove(cachePath)
		return fc.RootfsBacking{}, fmt.Errorf("acquire nbd device: %w", err)
	}
	exp, err := nbd.Bind(idx, provider)
	if err != nil {
		_ = provider.Close()
		_ = os.Remove(cachePath)
		s.nbdPool.Put(idx)
		return fc.RootfsBacking{}, fmt.Errorf("bind nbd%d: %w", idx, err)
	}
	return fc.RootfsBacking{
		Device:    nbd.DevicePath(idx),
		BakedPath: bakedPath, // "" for non-layered builds -> Restore binds over tmpl.Rootfs
		Close: func() error {
			derr := exp.Close()      // disconnect + stop the Dispatch goroutines (no more reads of provider)
			cerr := provider.Close() // close the base's per-owner bucket readers + the cache handle
			_ = os.Remove(cachePath) // remove this VM's sparse cache file
			s.nbdPool.Put(idx)       // hand the device back to the pool
			if derr != nil {
				return derr
			}
			return cerr
		},
	}, nil
}

// newRootfsCache creates this VM's private writable overlay cache: a sparse temp file sized to the device,
// wrapped as a block.Cache. It returns the file path (removed on Destroy) and the cache. The file is sparse
// (block.NewCache truncates to size without allocating), so an untouched overlay costs almost nothing.
func newRootfsCache(buildID string, size int64) (string, *block.Cache, error) {
	f, err := os.CreateTemp("", "msb-rootfs-cache-"+buildID+"-")
	if err != nil {
		return "", nil, fmt.Errorf("create rootfs cache: %w", err)
	}
	path := f.Name()
	_ = f.Close() // block.NewCache reopens it O_RDWR|O_TRUNC
	cache, err := block.NewCache(path, size)
	if err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	return path, cache, nil
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
