//go:build linux

package nbd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeSysfs builds a sysfs-block-shaped tree: one nbd{idx} dir per entry with a `size` file holding the
// device's sector count. A size of 0 is an unbound (free) device; nonzero is bound. Devices absent from
// the map have no directory (simulating nbds_max lower than the requested pool size).
func fakeSysfs(t *testing.T, sizes map[int]int64) string {
	t.Helper()
	root := t.TempDir()
	for idx, sectors := range sizes {
		dir := filepath.Join(root, fmt.Sprintf("nbd%d", idx))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "size"), []byte(fmt.Sprintf("%d\n", sectors)), 0o644); err != nil {
			t.Fatalf("write size: %v", err)
		}
	}
	return root
}

func TestScanFreeReportsUnboundDevicesInOrder(t *testing.T) {
	// nbd0 free, nbd1 bound (1024 sectors), nbd2 free, nbd3 absent (fewer devices than requested).
	root := fakeSysfs(t, map[int]int64{0: 0, 1: 1024, 2: 0})

	free, err := scanFree(root, 4)
	if err != nil {
		t.Fatalf("scanFree: %v", err)
	}
	want := []int{0, 2}
	if len(free) != len(want) {
		t.Fatalf("scanFree = %v, want %v", free, want)
	}
	for i := range want {
		if free[i] != want[i] {
			t.Fatalf("scanFree = %v, want %v", free, want)
		}
	}
}

func TestDeviceBound(t *testing.T) {
	root := fakeSysfs(t, map[int]int64{0: 0, 1: 2048})
	if bound, err := deviceBound(root, 0); err != nil || bound {
		t.Fatalf("deviceBound(0) = %v,%v, want false,nil", bound, err)
	}
	if bound, err := deviceBound(root, 1); err != nil || !bound {
		t.Fatalf("deviceBound(1) = %v,%v, want true,nil", bound, err)
	}
}

func TestScanFreeAllBoundIsEmpty(t *testing.T) {
	root := fakeSysfs(t, map[int]int64{0: 512, 1: 512})
	free, err := scanFree(root, 2)
	if err != nil {
		t.Fatalf("scanFree: %v", err)
	}
	if len(free) != 0 {
		t.Fatalf("scanFree = %v, want empty (all bound)", free)
	}
}

func TestDevicePath(t *testing.T) {
	if got := DevicePath(3); got != "/dev/nbd3" {
		t.Fatalf("DevicePath(3) = %q, want /dev/nbd3", got)
	}
}
