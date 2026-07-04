//go:build linux

package nbd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// sysfsBlockRoot is where the kernel exposes each block device's attributes. Overridable in tests so
// the free-slot scan can run over a fake tree with no nbd module loaded (the house KVM-free discipline).
const sysfsBlockRoot = "/sys/block"

// DevicePath is the /dev node for nbd device index idx. The constant baked into every snapshot (Stage
// 21c) is symlinked at one of these per VM, so the snapshot never records which device it got.
func DevicePath(idx int) string { return fmt.Sprintf("/dev/nbd%d", idx) }

// Pool hands out free /dev/nbdX device indices, mirroring E2B's nbd device pool. It is sized to the
// warm pool (each restored/cold VM claims one device for its rootfs and returns it on Destroy), so the
// pool bounds how many VMs can be live at once, like the network slot pool. Get/Put move indices over
// a buffered channel; NewPool primes it with the devices the kernel reports as unbound.
type Pool struct {
	root  string   // sysfs block root (injectable for tests; real path is sysfsBlockRoot)
	ready chan int // free device indices, ready to bind
}

// NewPool ensures the nbd kernel module is loaded with at least `size` devices (`modprobe nbd
// nbds_max=size`) and returns a pool primed with the devices currently unbound. It needs root and the
// nbd module, so it runs only on a real box (the orchestrator is already root); the pure scan it builds
// on (scanFree) is unit-tested over a fake sysfs tree without either.
//
// Caveat (documented, not worked around in this learning impl): nbds_max is a module *load* parameter,
// so if nbd is already loaded with a smaller max, modprobe will not raise it; the pool then only sees
// the devices that exist. A fresh box, or one where we control the load, gets the requested size.
func NewPool(size int) (*Pool, error) {
	if size <= 0 {
		return nil, fmt.Errorf("nbd: pool size must be positive, got %d", size)
	}
	if err := ensureModule(size); err != nil {
		return nil, err
	}
	p := &Pool{root: sysfsBlockRoot, ready: make(chan int, size)}
	free, err := scanFree(p.root, size)
	if err != nil {
		return nil, err
	}
	if len(free) == 0 {
		return nil, fmt.Errorf("nbd: no free devices among nbd0..nbd%d (is the module loaded?)", size-1)
	}
	for _, idx := range free {
		p.ready <- idx
	}
	return p, nil
}

// Get blocks until a free device index is available or ctx is done. The caller binds a Provider to
// DevicePath(idx) via Export and must Put(idx) back after Disconnect on teardown.
func (p *Pool) Get(ctx context.Context) (int, error) {
	select {
	case idx := <-p.ready:
		return idx, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Put returns a device index to the pool after it has been disconnected. It never blocks: the channel
// is sized to the device count, so a Put can only ever match a prior Get.
func (p *Pool) Put(idx int) { p.ready <- idx }

// ensureModule loads nbd with nbds_max=size. `modprobe nbd` is idempotent -- already-loaded is success,
// the nbds_max is only honored on the first load (see NewPool's caveat).
func ensureModule(size int) error {
	cmd := exec.Command("modprobe", "nbd", fmt.Sprintf("nbds_max=%d", size))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nbd: modprobe nbd nbds_max=%d: %w (%s)", size, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// scanFree returns the indices in [0,size) whose device is currently unbound, in ascending order. It
// is a pure function of the sysfs tree at root, so it is unit-tested over a fabricated tree. A device
// directory that does not exist is skipped (fewer devices than requested -- see NewPool's caveat).
func scanFree(root string, size int) ([]int, error) {
	var free []int
	for idx := 0; idx < size; idx++ {
		dir := filepath.Join(root, fmt.Sprintf("nbd%d", idx))
		if _, err := os.Stat(dir); err != nil {
			if os.IsNotExist(err) {
				continue // this device number was not created (nbds_max lower than size)
			}
			return nil, fmt.Errorf("nbd: stat %s: %w", dir, err)
		}
		bound, err := deviceBound(root, idx)
		if err != nil {
			return nil, err
		}
		if !bound {
			free = append(free, idx)
		}
	}
	return free, nil
}

// deviceBound reports whether nbd{idx} has an export attached, read from /sys/block/nbdN/size (the
// device size in 512-byte sectors). An unbound device reads 0; once Export binds a Provider, the kernel
// publishes the export's sector count. This single always-present file is a more robust free signal
// than the pid file (which only appears while a client is connected).
func deviceBound(root string, idx int) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("nbd%d", idx), "size"))
	if err != nil {
		return false, fmt.Errorf("nbd: read size for nbd%d: %w", idx, err)
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return false, fmt.Errorf("nbd: parse size %q for nbd%d: %w", strings.TrimSpace(string(raw)), idx, err)
	}
	return sectors != 0, nil
}
