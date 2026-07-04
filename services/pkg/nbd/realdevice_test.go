//go:build linux

package nbd

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"
)

// TestBindRealDeviceRoundTrip exercises the one part of pkg/nbd that the KVM-free unit tests cannot:
// the netlink bind (export.go) driving a real /dev/nbdX. It binds an in-memory Provider to a pooled
// device, then reads and writes the block device through the kernel and asserts the bytes round-trip
// through Dispatch to the Provider. It is gated like the S3/Postgres/Redis integration tests (skips
// unless MSB_TEST_NBD is set) and additionally needs the nbd kernel module + root, so a bare
// `go test ./...` stays hermetic. Run it as root:
//
//	go test -c -o /tmp/nbd.test ./pkg/nbd/     # compile as your user (deps already cached)
//	sudo MSB_TEST_NBD=1 /tmp/nbd.test -test.run TestBindRealDeviceRoundTrip -test.v
func TestBindRealDeviceRoundTrip(t *testing.T) {
	if os.Getenv("MSB_TEST_NBD") == "" {
		t.Skip("set MSB_TEST_NBD=1 and run as root (needs the nbd kernel module) to run the real-device NBD bind test")
	}

	const size = int64(1 << 20) // 1 MiB export
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*7 + 3) & 0xff) // a recognizable, offset-dependent pattern
	}
	provider := &bytesProvider{data: append([]byte(nil), data...)}

	pool, err := NewPool(8)
	if err != nil {
		t.Fatalf("NewPool (is the nbd module loadable, are we root?): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idx, err := pool.Get(ctx)
	if err != nil {
		t.Fatalf("pool.Get: %v", err)
	}

	exp, err := Bind(idx, provider)
	if err != nil {
		t.Fatalf("Bind nbd%d: %v", idx, err)
	}
	// Close the binding (netlink Disconnect + wait for Dispatch goroutines) before returning the slot.
	defer func() {
		if err := exp.Close(); err != nil {
			t.Errorf("Export.Close: %v", err)
		}
		pool.Put(idx)
	}()

	devPath := DevicePath(idx)
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", devPath, err)
	}
	defer func() { _ = f.Close() }()

	// Read one block back through kernel -> Dispatch -> provider and compare to the source pattern.
	// The reads counter proves the read reached the Provider (a fresh device has a cold page cache, so
	// this first read must fault through to Dispatch rather than being served from cache).
	got := make([]byte, 4096)
	if _, err := f.ReadAt(got, 8192); err != nil {
		t.Fatalf("read %s at 8192: %v", devPath, err)
	}
	if !bytes.Equal(got, data[8192:8192+4096]) {
		t.Fatalf("read mismatch at 8192: device bytes != source pattern")
	}
	if provider.reads.Load() == 0 {
		t.Fatalf("Provider.ReadAt was never called -- the read did not go through Dispatch")
	}

	// Write one block through the device; fsync forces the kernel to flush the dirty page as an
	// NBD_CMD_WRITE, which Dispatch applies to the Provider before replying -- so once Sync returns,
	// the bytes must be visible in provider.data.
	want := bytes.Repeat([]byte{0xAB}, 4096)
	if _, err := f.WriteAt(want, 4096); err != nil {
		t.Fatalf("write %s at 4096: %v", devPath, err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync %s: %v", devPath, err)
	}
	provider.mu.Lock()
	landed := append([]byte(nil), provider.data[4096:4096+4096]...)
	provider.mu.Unlock()
	if !bytes.Equal(landed, want) {
		t.Fatalf("write did not land in the Provider after fsync")
	}
	if provider.writes.Load() == 0 {
		t.Fatalf("Provider.WriteAt was never called -- the write did not go through Dispatch")
	}
}
