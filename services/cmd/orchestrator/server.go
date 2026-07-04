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

	mu        sync.Mutex             // guards sandboxes
	sandboxes map[string]*fc.MicroVM // sandbox id -> running VM
}

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
		sandboxes: map[string]*fc.MicroVM{},
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
			vm, err := s.restoreHealthy(tmpl)
			if err != nil {
				return nil, err
			}
			return vm, nil
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
func (s *server) create(fromSnapshot bool, tmpl template.Template) (*fc.MicroVM, error) {
	// Serve from a warm pool only if this template has one; otherwise restore/spawn
	// its own image inline. A pooled VM is always the right image -- each pool restores
	// from its template's own snapshot (newServer).
	p := s.poolFor(tmpl)
	var vm *fc.MicroVM
	var err error
	switch {
	case fromSnapshot && p != nil:
		var v pool.VM
		v, err = p.Get()
		if err == nil {
			vm = v.(*fc.MicroVM) // the pool only ever holds *fc.MicroVM (newServer's restore)
		}
	case fromSnapshot:
		vm, err = s.restoreHealthy(tmpl)
	default:
		vm, err = s.spawnHealthy(tmpl)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sandboxes[vm.ID] = vm
	s.mu.Unlock()
	return vm, nil
}

// destroy kills a sandbox by id and drops it from the registry; it reports whether the
// id existed (a false lets the gRPC layer answer NotFound).
func (s *server) destroy(id string) bool {
	s.mu.Lock()
	vm, ok := s.sandboxes[id]
	if ok {
		delete(s.sandboxes, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	vm.Destroy()
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

// lookup returns the VM for an id, if known (used by the data proxy to find the VM's network slot).
func (s *server) lookup(id string) (*fc.MicroVM, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.sandboxes[id]
	return vm, ok
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
func (s *server) restoreHealthy(tmpl template.Template) (*fc.MicroVM, error) {
	memSource, err := s.prepareRestore(tmpl)
	if err != nil {
		return nil, fmt.Errorf("prepare restore artifacts for %q: %w", tmpl.Name, err)
	}
	rootfs, err := s.buildRootfsBacking(tmpl) // NBD device (Stage 21c), or a zero backing (legacy file)
	if err != nil {
		if memSource != nil {
			_ = memSource.Close() // fc.Restore would have owned it; we bailed before calling it
		}
		return nil, fmt.Errorf("prepare rootfs backing for %q: %w", tmpl.Name, err)
	}
	return healthyOrDestroy(fc.Restore(fc.NewID(), s.vendorDir, tmpl, s.net, memSource, rootfs))
}

func (s *server) spawnHealthy(tmpl template.Template) (*fc.MicroVM, error) {
	if err := s.prepareSpawn(tmpl); err != nil {
		return nil, fmt.Errorf("prepare spawn artifacts for %q: %w", tmpl.Name, err)
	}
	rootfs, err := s.buildRootfsBacking(tmpl)
	if err != nil {
		return nil, fmt.Errorf("prepare rootfs backing for %q: %w", tmpl.Name, err)
	}
	return healthyOrDestroy(fc.Spawn(fc.NewID(), s.vendorDir, tmpl, s.net, rootfs))
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
		return nil // NBD serves the rootfs lazily (buildRootfsBacking); the drive points at the device
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
	for id, vm := range s.sandboxes {
		vm.Destroy()
		delete(s.sandboxes, id)
	}
}
