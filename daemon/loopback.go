package main

import "golang.org/x/sys/unix"

// bringLoopbackUp raises the loopback interface (lo), which defaults to *down* in a
// microVM. The kernel backend's Jupyter kernel talks ZMQ over 127.0.0.1, so lo must
// be up or the kernel never becomes ready. This is the Go port of server.py's
// _ensure_loopback_up (the SIOCSIFFLAGS ioctl): the minimal rootfs has no
// `ip`/`ifconfig`, so we set IFF_UP directly via ioctl on an AF_INET socket.
func bringLoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	ifreq, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	// Read the current flags, OR in IFF_UP, write them back.
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifreq); err != nil {
		return err
	}
	ifreq.SetUint16(ifreq.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifreq)
}
