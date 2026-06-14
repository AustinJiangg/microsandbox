package main

// microVM is a handle to one running Firecracker process and its per-VM working
// directory. The next sub-step of Stage 4a ports the real lifecycle here from
// client.py: _spawn_microvm (cold) / _restore_microvm (snapshot) build one, and
// close() becomes destroy().
type microVM struct {
	id string
	// proc, workdir, udsPath, ... are added with the real lifecycle.
}

// destroy kills the firecracker process (which destroys the whole VM) and
// removes the working directory. A no-op until the lifecycle lands.
func (vm *microVM) destroy() {}
