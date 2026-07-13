package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"microsandbox/services/pkg/block"
	"microsandbox/services/pkg/envdclient"
	"microsandbox/services/pkg/fc"
	"microsandbox/services/pkg/nbd"
	"microsandbox/services/pkg/network"
	"microsandbox/services/pkg/pool"
	"microsandbox/services/pkg/proxy"
	"microsandbox/services/pkg/storage"
	"microsandbox/services/pkg/storage/header"
	"microsandbox/services/pkg/template"
	"microsandbox/services/pkg/uffd"
)

// server owns the microVM fleet: it creates VMs (cold start or snapshot restore,
// optionally handed out from a warm pool), tracks them in a registry, and destroys
// them. In Stage 8b it is fronted by two listeners -- a gRPC SandboxService (grpc.go)
// for the lifecycle and an HTTP data proxy (dataproxy.go) for the TCP data path over the VM's NIC --
// but the fleet logic here is unchanged from the Stage 8a HTTP control plane: the
// handlers got thinner, the VM management did not move. See docs/STAGE8_DESIGN.md.
type server struct {
	vendorDir string
	storage   storage.StorageProvider // object store for artifacts (Stage 15); nil = local-fs: read local paths directly
	pools     map[string]*pool.Pool   // template name -> its warm pool; empty when no --pool/--pool-size
	net       *network.Manager        // Stage 12: per-sandbox netns/TAP/veth/DNAT slots (cold-start + restore paths)
	useUffd   bool                    // Stage 13b: in local-fs mode, restore over Uffd (--uffd) instead of File
	useNBD    bool                    // Stage 21c: serve the rootfs over NBD (nbdPool != nil; s3 mode only)
	nbdPool   *nbd.Pool               // Stage 21c: /dev/nbdX device pool; nil unless --nbd (s3 mode)

	mu        sync.Mutex              // guards sandboxes
	sandboxes map[string]*liveSandbox // sandbox id -> running VM + what pausing it needs
}

// liveSandbox is one running VM plus what a later per-sandbox Pause needs (Stage 26R): the
// writable rootfs overlay buildRootfsBacking bound for it (nil outside --nbd s3 mode) and the
// build id its artifacts were served from -- the diff base a pause snapshot is computed against
// ("" in local-fs / non-NBD mode, where Pause is refused anyway). Both were always created; the
// registry just used to discard them (restoreHealthy returned only the *fc.MicroVM).
type liveSandbox struct {
	vm          *fc.MicroVM
	overlay     *block.Overlay // the VM's writable rootfs overlay; owned by the VM (closed by Destroy via the backing's Close)
	baseBuildID string         // the build whose artifacts this VM booted from (ResolveAlias at create; the snapshot id after a resume)

	// bakedRootfsPath is the host rootfs path this VM's drive was configured with -- what a
	// re-snapshot of this VM bakes into its vmstate. A per-sandbox Pause records it as the
	// checkpoint's rootfs.path so Resume binds the checkpoint's NBD device over exactly the path
	// the vmstate references (Stage 26R): tmpl.Rootfs for a cold start or a template restored from
	// its own snapshot, the base's recorded baked path for a layered child.
	bakedRootfsPath string
}

// Destroy tears down the VM (satisfying pool.VM). The overlay needs no separate teardown --
// it is closed by the VM's rootfs-backing Close inside vm.Destroy; this handle is only for
// reaching it (ExportToDiff) while the VM is alive.
func (ls *liveSandbox) Destroy() { ls.vm.Destroy() }

// nbdDevices sizes the NBD device pool (Stage 21c): the max rootfs devices live at once, like the
// network slot cap. 64 is ample for one box; modprobe honors it on a fresh load (see pkg/nbd caveat).
const nbdDevices = 64

// networkSlots caps concurrent per-sandbox network slots (Stage 12a). 256 is the third-octet
// limit in pkg/network's 10.0.<i>.0/30 scheme -- ample for one machine.
const networkSlots = 256

// newServer builds the server and one warm pool per entry in poolSpecs (template name
// -> K): each pre-restores K snapshot VMs in the background, already health-probed, so
// a from_snapshot create for that template is served in ~ms. An empty poolSpecs keeps
// the original behavior of restoring on the request path. The pool's "make one VM"
// step is restoreHealthy -- the same one the unpooled from_snapshot path uses.
func newServer(vendorDir string, poolSpecs map[string]int, useUffd bool, sp storage.StorageProvider, nbdPool *nbd.Pool) *server {
	s := &server{
		vendorDir: vendorDir,
		storage:   sp,
		sandboxes: map[string]*liveSandbox{},
		pools:     map[string]*pool.Pool{},
		net:       network.NewManager(networkSlots),
		useUffd:   useUffd,
		useNBD:    nbdPool != nil, // --nbd is on iff main built a device pool (s3 mode only)
		nbdPool:   nbdPool,
	}
	for name, k := range poolSpecs {
		// name was validated by parsePoolSpecs, so resolve cannot fail. tmpl is a
		// fresh per-iteration variable, so each closure captures its own template.
		tmpl, _ := template.Resolve(vendorDir, name)
		p := pool.New(k, func() (pool.VM, error) {
			ls, err := s.restoreHealthy(tmpl)
			if err != nil {
				return nil, err
			}
			return ls, nil
		})
		s.pools[name] = p
		p.Start()
	}
	return s
}

// poolFor returns the warm pool for a template, or nil if that template isn't pooled.
func (s *server) poolFor(tmpl template.Template) *pool.Pool { return s.pools[tmpl.Name] }

// repeatedFlag collects a flag passed more than once (--pool a=1 --pool b=2).
type repeatedFlag []string

func (r *repeatedFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

// parsePoolSpecs turns the CLI pool flags into a {template name -> warm count} map.
// --pool-size K is shorthand for the default template; --pool name=K (repeatable) sets
// a named one. A bad format, a non-positive K, an invalid template name, or the same
// template named twice (including default given via both flags) is a startup error.
func parsePoolSpecs(poolFlags []string, poolSize int) (map[string]int, error) {
	out := map[string]int{}
	add := func(name string, k int) error {
		if _, dup := out[name]; dup {
			return fmt.Errorf("template %q given more than one pool size", name)
		}
		if k <= 0 {
			return fmt.Errorf("pool size for %q must be > 0, got %d", name, k)
		}
		if _, err := template.Resolve("", name); err != nil { // validate the name (path-independent)
			return err
		}
		out[name] = k
		return nil
	}
	if poolSize > 0 {
		if err := add(template.DefaultTemplate, poolSize); err != nil {
			return nil, err
		}
	}
	for _, spec := range poolFlags {
		name, val, ok := strings.Cut(spec, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --pool %q: want name=K", spec)
		}
		k, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("invalid --pool %q: K must be an integer", spec)
		}
		if err := add(name, k); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// --- VM fleet management: the verbs the gRPC SandboxService (grpc.go) calls ---

// create boots a sandbox of the given template (cold start, or snapshot restore which
// is warm-pool eligible), registers it, and returns it already health-probed. This is
// the old handleCreate switch, minus the HTTP plumbing.
func (s *server) create(fromSnapshot bool, tmpl template.Template) (*liveSandbox, error) {
	// Serve from a warm pool only if this template has one; otherwise restore/spawn
	// its own image inline. A pooled VM is always the right image -- each pool restores
	// from its template's own snapshot (newServer).
	p := s.poolFor(tmpl)
	var ls *liveSandbox
	var err error
	switch {
	case fromSnapshot && p != nil:
		var v pool.VM
		v, err = p.Get()
		if err == nil {
			ls = v.(*liveSandbox) // the pool only ever holds *liveSandbox (newServer's restore)
		}
	case fromSnapshot:
		ls, err = s.restoreHealthy(tmpl)
	default:
		ls, err = s.spawnHealthy(tmpl)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sandboxes[ls.vm.ID] = ls
	s.mu.Unlock()
	return ls, nil
}

// destroy kills a sandbox by id and drops it from the registry; it reports whether the
// id existed (a false lets the gRPC layer answer NotFound).
func (s *server) destroy(id string) bool {
	s.mu.Lock()
	ls, ok := s.sandboxes[id]
	if ok {
		delete(s.sandboxes, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	ls.Destroy()
	return true
}

// list returns the ids of all running sandboxes.
func (s *server) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.sandboxes))
	for id := range s.sandboxes {
		ids = append(ids, id)
	}
	return ids
}

// lookup returns the live sandbox for an id, if known (the data proxy reaches the VM's network
// slot through it; Pause reaches the VM + overlay + base build id).
func (s *server) lookup(id string) (*liveSandbox, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ls, ok := s.sandboxes[id]
	return ls, ok
}

// healthyOrDestroy probes a freshly created VM and returns it once its in-VM daemon
// answers /health; on failure it destroys the VM (so nothing leaks) and returns an
// error carrying the guest serial tail for diagnostics. Shared by the cold-start, the
// unpooled restore, and the warm pool's pre-warm paths -- "ready on delivery" in one
// place.
func healthyOrDestroy(vm *fc.MicroVM, err error) (*fc.MicroVM, error) {
	if err != nil {
		return nil, err
	}
	// Stage 12b-2b: the data path is TCP now, so wait for the daemon's /health over the VM's NIC
	// (the slot's routable addr), not vsock -- and poll rather than probe once, since 12a saw the
	// TCP listener bind a beat after the VM is otherwise up, which a single shot would race.
	if vm.Slot == nil {
		vm.Destroy()
		return nil, fmt.Errorf("sandbox has no network slot (cannot health-check over TCP)")
	}
	if err := proxy.TCPWaitHealthy(vm.Slot.Addr(fc.EnvdTCPPort), 10*time.Second); err != nil {
		tail := vm.ConsoleTail()
		vm.Destroy()
		return nil, fmt.Errorf("sandbox %v; %s", err, tail)
	}
	return vm, nil
}

// restoreHealthy / spawnHealthy mint an id, prepare the template's artifacts (Stage 15: materialize
// from object storage in s3 mode), create the VM (restored / cold-started), and block until healthy.
// Both return the VM as a liveSandbox: the writable overlay + base build id ride along (Stage 26R),
// so a later per-sandbox Pause can diff against them. (Stage 22's layer producer used to get the
// overlay from a separate restoreHealthyWritable; retaining it on every sandbox subsumed that.)
func (s *server) restoreHealthy(tmpl template.Template) (*liveSandbox, error) {
	memSource, err := s.prepareRestore(tmpl)
	if err != nil {
		return nil, fmt.Errorf("prepare restore artifacts for %q: %w", tmpl.Name, err)
	}
	rootfs, overlay, buildID, err := s.buildRootfsBacking(tmpl) // NBD device (Stage 21c), or a zero backing (legacy file)
	if err != nil {
		if memSource != nil {
			_ = memSource.Close() // fc.Restore would have owned it; we bailed before calling it
		}
		return nil, fmt.Errorf("prepare rootfs backing for %q: %w", tmpl.Name, err)
	}
	vm, err := healthyOrDestroy(fc.Restore(fc.NewID(), s.vendorDir, tmpl, s.net, memSource, rootfs))
	if err != nil {
		return nil, err
	}
	// The drive path the restored vmstate references: the backing's recorded baked path for a
	// layered child, else the template's own rootfs (fc.Restore binds over the same fallback).
	bakedPath := rootfs.BakedPath
	if bakedPath == "" {
		bakedPath = tmpl.Rootfs
	}
	return &liveSandbox{vm: vm, overlay: overlay, baseBuildID: buildID, bakedRootfsPath: bakedPath}, nil
}

func (s *server) spawnHealthy(tmpl template.Template) (*liveSandbox, error) {
	if err := s.prepareSpawn(tmpl); err != nil {
		return nil, fmt.Errorf("prepare spawn artifacts for %q: %w", tmpl.Name, err)
	}
	rootfs, overlay, buildID, err := s.buildRootfsBacking(tmpl)
	if err != nil {
		return nil, fmt.Errorf("prepare rootfs backing for %q: %w", tmpl.Name, err)
	}
	vm, err := healthyOrDestroy(fc.Spawn(fc.NewID(), s.vendorDir, tmpl, s.net, rootfs))
	if err != nil {
		return nil, err
	}
	// Spawn always boots path_on_host=tmpl.Rootfs (Stage 22 E1: the NBD device is bound over it),
	// so that is the path a re-snapshot of this VM would bake.
	return &liveSandbox{vm: vm, overlay: overlay, baseBuildID: buildID, bakedRootfsPath: tmpl.Rootfs}, nil
}

// LayeredSnapshot produces a layered template's snapshot by E2B's one-run-two-diffs model (Stage 22): it
// resumes the BASE over a writable rootfs overlay, runs the layer's commands IN THE GUEST, then takes one
// Full snapshot from which BOTH diffs are derived -- the memfile as a COW diff over the base
// ({childBuildID}/memfile + .header) and the rootfs as a COW diff from the overlay's dirtied blocks
// ({childBuildID}/rootfs.ext4 + .header). Because the guest itself made the changes, its RAM (page cache)
// and disk (the overlay) are one consistent state at the snapshot instant -- fixing the Stage-20 producer,
// which grafted a separately-built docker+debugfs rootfs onto the base's RAM and so left the child's disk
// change invisible at restore (the base RAM's cached /etc shadowed it; docs/STAGE22_DESIGN.md).
//
// It also stores the re-snapshotted vmstate ({childBuildID}/snapfile) and the baked rootfs path (the
// re-snapshot bakes the BASE's rootfs path, so the child's restore binds its own NBD device over exactly
// what the vmstate references). Restorable only under --nbd (the child's rootfs is served at the base's
// baked path via the per-VM bind, without clobbering the base's rootfs there). Implemented on *server so it
// can reach fc + the network manager + envd; injected into pkg/build as a build.Snapshotter.
func (s *server) LayeredSnapshot(ctx context.Context, baseName, baseBuildID, childBuildID string, commands []string) error {
	if s.storage == nil {
		return fmt.Errorf("layered snapshot needs object storage (s3 mode)")
	}
	// The child bakes its base's rootfs path (below); only NBD can then serve the child's OWN rootfs at
	// that path at restore (via the per-VM bind) without clobbering the base's rootfs there. It is also
	// what gives the producer a writable overlay to capture the in-guest command's disk writes. Without
	// --nbd the child would restore against the base's rootfs -- silently wrong -- so refuse to produce one.
	if !s.useNBD {
		return fmt.Errorf("layered snapshot requires --nbd (the child's rootfs is served over NBD at the base's baked path)")
	}
	baseTmpl, err := template.Resolve(s.vendorDir, baseName)
	if err != nil {
		return fmt.Errorf("resolve base template %q: %w", baseName, err)
	}
	// The child's diffs are computed against baseBuildID; the resume goes by template name (the alias), so
	// guard that the base's alias still points at that build -- a concurrent base rebuild mid-child-build is
	// unsupported, not silently wrong.
	if cur, err := storage.ResolveAlias(ctx, s.storage, baseName); err != nil {
		return fmt.Errorf("resolve base %q: %w", baseName, err)
	} else if cur != baseBuildID {
		return fmt.Errorf("base %q moved from %s to %s during the layered build (unsupported concurrent rebuild)", baseName, baseBuildID, cur)
	}

	// Resume the base over a WRITABLE overlay and wait for health, reusing the restore path a user create
	// takes. The producer VM is unregistered -- we own its lifecycle and Destroy it below (Destroy also
	// closes the overlay via the backing's Close, so ExportToDiff must run before then).
	ls, err := s.restoreHealthy(baseTmpl)
	if err != nil {
		return fmt.Errorf("resume base %q for re-snapshot: %w", baseName, err)
	}
	vm, overlay := ls.vm, ls.overlay
	defer vm.Destroy()

	// Run the layer's commands IN THE GUEST (E2B's model), so the one snapshot below captures a mutually
	// consistent (RAM page cache, overlay disk) pair. envd's ProcessService.Run is synchronous; a non-zero
	// exit fails the build. envd is reached over the VM's NIC at the slot's routable addr.
	envd := envdclient.New("http://" + vm.Slot.Addr(fc.EnvdTCPPort))
	for _, cmd := range commands {
		res, err := envd.Run(ctx, cmd, 0)
		if err != nil {
			return fmt.Errorf("run layer command %q in guest: %w", cmd, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("layer command %q exited %d: %s", cmd, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	}
	// sync so the writes are durable in the overlay AND settled in the page cache before the snapshot -- the
	// (disk diff, RAM diff) pair must be consistent at rest (docs/STAGE22_DESIGN.md D4).
	if res, err := envd.Run(ctx, "sync", 0); err != nil {
		return fmt.Errorf("sync guest before snapshot: %w", err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("sync guest exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	// Re-snapshot the running base to temp files. A Full snapshot faults all base RAM in over UFFD, so the
	// memfile is complete and mostly identical to the base -- the COW diff (below) is then small.
	tmp, err := os.MkdirTemp("", "msb-resnap-"+childBuildID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	newVmstate := filepath.Join(tmp, "vmstate")
	newMemfile := filepath.Join(tmp, "memfile")
	if err := vm.Snapshot(newVmstate, newMemfile); err != nil {
		return fmt.Errorf("re-snapshot base for %q: %w", childBuildID, err)
	}

	// Publish the memfile COW diff over the base, the re-snapshotted vmstate, and the baked rootfs path the
	// vmstate references (so the child's restore binds its NBD device over it).
	if err := storage.PublishMemfileDiff(ctx, s.storage, baseBuildID, newMemfile, childBuildID); err != nil {
		return fmt.Errorf("publish memfile diff for %q: %w", childBuildID, err)
	}
	if err := storage.PublishSnapfile(ctx, s.storage, newVmstate, childBuildID); err != nil {
		return fmt.Errorf("publish snapfile for %q: %w", childBuildID, err)
	}
	if err := storage.PublishRootfsBakedPath(ctx, s.storage, childBuildID, baseTmpl.Rootfs); err != nil {
		return fmt.Errorf("record baked rootfs path for %q: %w", childBuildID, err)
	}

	// Publish the rootfs COW diff from the overlay's dirtied blocks -- the disk half of the one-run producer
	// (Stage 22 E3). This REPLACES the Stage-19 docker+debugfs rootfs for the with-snapshot path: because
	// the guest itself wrote these blocks (before the snapshot above), they are consistent with the captured
	// RAM. ExportToDiff must run before the deferred Destroy closes the overlay.
	diff, err := overlay.ExportToDiff()
	if err != nil {
		return fmt.Errorf("export rootfs diff for %q: %w", childBuildID, err)
	}
	if err := storage.PublishRootfsDiffBlocks(ctx, s.storage, baseBuildID, childBuildID, storage.DiffBlocks{
		Data:      diff.Data,
		Dirty:     diff.Dirty,
		Empty:     diff.Empty,
		BlockSize: diff.BlockSize,
		Size:      overlay.Size(),
	}); err != nil {
		return fmt.Errorf("publish rootfs diff for %q: %w", childBuildID, err)
	}
	return nil
}

// ensureFile makes sure path exists as a file (creating an empty one + its parent dirs if absent), so it
// can be the target of the Restore-over-NBD bind mount (Stage 21c). An existing file -- a placeholder or
// a real rootfs left from a non-NBD run -- is left as-is; the bind shadows whatever is there.
func ensureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// prepareRestore makes a restore's artifacts available and returns the memfile page source for it:
//   - local-fs mode (no provider): artifacts are already at their local paths; the memfile is a local
//     mmap when --uffd, else nil (firecracker's File backend mmaps it).
//   - s3 mode: resolve the template's current build, materialize rootfs + snapfile (vmstate) to their
//     baked local paths if missing, and stream the memfile from object storage via UFFD (never
//     materialized). See docs/STAGE15_DESIGN.md.
func (s *server) prepareRestore(tmpl template.Template) (uffd.PageSource, error) {
	memfile := filepath.Join(tmpl.SnapshotDir, "memfile")
	if s.storage == nil {
		if s.useUffd {
			return uffd.MmapSource(memfile)
		}
		return nil, nil // File backend (local memfile)
	}
	ctx := context.Background()
	buildID, err := storage.ResolveAlias(ctx, s.storage, tmpl.Name)
	if err != nil {
		return nil, err
	}
	if s.useNBD {
		// NBD serves the rootfs lazily (buildRootfsBacking binds it to a device); we do NOT assemble it
		// whole. But the snapshot bakes tmpl.Rootfs as the drive path, and Restore bind-mounts the device
		// over it, so that path must exist as a file for `mount --bind` -- ensure a placeholder (the bind
		// shadows it; a real materialized rootfs left from a non-NBD run works as the target too).
		if err := ensureFile(tmpl.Rootfs); err != nil {
			return nil, err
		}
	} else if err := storage.MaterializeLayered(ctx, s.storage, buildID, tmpl.Rootfs); err != nil {
		// MaterializeLayered assembles a layered (COW) rootfs from each run's owning build (Stage 18) or,
		// when the build carries no rootfs header, downloads it whole -- a safe drop-in for the non-layered
		// default and old buckets too.
		return nil, err
	}
	vmstate := filepath.Join(tmpl.SnapshotDir, "vmstate")
	// A layered re-snapshot child (Stage 20) gets a FRESH buildID on every build, but the local vmstate
	// cache is keyed by template NAME, so Materialize's skip-if-exists would reuse a prior build's (or a
	// prior firecracker version's) vmstate -- a format mismatch that fails the load. Drop the stale local
	// vmstate for layered children (OpenRootfsBakedPath != "" marks one) so this build's vmstate is fetched
	// fresh. Layered children are never warm-pooled, so there is no concurrent-restore race on the file.
	if bakedPath, err := storage.OpenRootfsBakedPath(ctx, s.storage, buildID); err != nil {
		return nil, err
	} else if bakedPath != "" {
		_ = os.Remove(vmstate)
	}
	if err := storage.Materialize(ctx, s.storage, storage.ArtifactKey(buildID, storage.SnapfileName), vmstate); err != nil {
		return nil, err
	}
	// Resolve the memfile header first to pick the page source (opening the buildID object is deferred to
	// after, so a layered memfile -- which opens each owner lazily -- doesn't open a redundant reader):
	//   - no header   -> a pre-Stage-17 raw full memfile: stream it whole (chunkedSource).
	//   - v1 header    -> a Stage-17 compacted single-build memfile: remap logical->physical (mappedSource).
	//   - v2 header    -> a Stage-20 COW-layered memfile: pages resolve to different owning builds
	//                     ({owner}/memfile), served over the multi-owner layered source.
	hdr, err := storage.OpenMemfileHeader(ctx, s.storage, buildID)
	if err != nil {
		return nil, err
	}
	if hdr != nil && hdr.Metadata.Version >= header.VersionLayered {
		return s.layeredMemSource(hdr), nil
	}
	// Stream the memfile page-by-page from the bucket via UFFD -- the Stage-15 payoff. The reader is
	// owned by the uffd handler from here (closed on Destroy); 0 selects the default chunk size.
	rr, err := s.storage.OpenReaderAt(ctx, storage.ArtifactKey(buildID, storage.MemfileName))
	if err != nil {
		return nil, err
	}
	if hdr == nil {
		return uffd.NewChunkedSource(rr, rr.Close, 0), nil
	}
	extents := make([]uffd.Extent, len(hdr.Mapping))
	for i, m := range hdr.Mapping {
		extents[i] = uffd.Extent{Logical: int64(m.Offset), Length: int64(m.Length), Physical: int64(m.BuildStorageOffset)}
	}
	return uffd.NewMappedSource(rr, rr.Close, extents, int64(hdr.Metadata.Size), 0), nil
}

// layeredMemSource builds the multi-owner UFFD page source for a COW-layered (v2) memfile (Stage 20):
// each run names the build whose {owner}/memfile object holds those bytes, so a faulting page is read
// from THAT build's object (a zero-owner run or a gap -> zeros, no fetch) -- E2B's fault -> owning build
// -> that build's diff, over UFFD. The opener maps an owner to its object lazily (opened once per owner
// actually faulted into, then range-read through its own chunk cache), keeping pkg/uffd storage-free.
func (s *server) layeredMemSource(hdr *header.Header) uffd.PageSource {
	extents := make([]uffd.Extent, len(hdr.Mapping))
	for i, m := range hdr.Mapping {
		extents[i] = uffd.Extent{Logical: int64(m.Offset), Length: int64(m.Length), Physical: int64(m.BuildStorageOffset), Owner: m.Owner}
	}
	open := func(owner string) (io.ReaderAt, func() error, error) {
		rr, err := s.storage.OpenReaderAt(context.Background(), storage.ArtifactKey(owner, storage.MemfileName))
		if err != nil {
			return nil, nil, err
		}
		return rr, rr.Close, nil
	}
	return uffd.NewLayeredSource(extents, int64(hdr.Metadata.Size), 0, open)
}

// prepareSpawn ensures a cold start's rootfs is at its baked local path, materializing it from object
// storage on a cache miss (s3 mode). No snapshot/memfile is involved in a cold start.
func (s *server) prepareSpawn(tmpl template.Template) error {
	if s.storage == nil {
		return nil // local-fs: the rootfs is already at its local path
	}
	if s.useNBD {
		// NBD serves the rootfs lazily (buildRootfsBacking binds it to a device). Since Stage 22 E1 Spawn
		// binds that device over tmpl.Rootfs (a stable path, so the cold-start snapshot bakes a stable
		// rootfs path), that path must exist as a file for `mount --bind`; the bind shadows this placeholder.
		return ensureFile(tmpl.Rootfs)
	}
	if _, err := os.Stat(tmpl.Rootfs); err == nil {
		return nil // cache hit: no need to resolve the alias or download
	}
	ctx := context.Background()
	buildID, err := storage.ResolveAlias(ctx, s.storage, tmpl.Name)
	if err != nil {
		return err
	}
	return storage.MaterializeLayered(ctx, s.storage, buildID, tmpl.Rootfs) // layered (COW) or whole, per the build's header
}

// destroyAll terminates every running VM -- the warm pools' idle VMs first, then the
// active ones in the registry. Called on shutdown so nothing leaks.
func (s *server) destroyAll() {
	for _, p := range s.pools {
		p.Drain()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ls := range s.sandboxes {
		ls.Destroy()
		delete(s.sandboxes, id)
	}
}
