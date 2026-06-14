"""Quickstart example.

Run it directly:
    python examples/quickstart.py

It boots a Firecracker microVM, runs a few snippets of code inside it, and cleans
up on exit. Requires the one-time microVM setup (firecracker + kernel under
vendor/, /dev/kvm access, and scripts/build-rootfs.sh) -- see README / docs.
"""

from microsandbox import Sandbox


def main() -> None:
    # Constructing the Sandbox cold-starts a microVM and connects in over vsock;
    # leaving the `with` block kills the VM and cleans up.
    with Sandbox() as sandbox:
        print("=== 1. Basic output ===")
        ex = sandbox.run_code("print('hello from the sandbox')")
        print("stdout:", ex.stdout.strip())
        print("exit_code:", ex.exit_code, "success:", ex.success)
        print()

        print("=== 2. Multi-line computation ===")
        ex = sandbox.run_code(
            "total = sum(range(101))\n"
            "print(f'sum of 0..100 = {total}')"
        )
        print("stdout:", ex.stdout.strip())
        print()

        print("=== 3. Streaming output (received as it runs) ===")
        sandbox.run_code(
            "import time\n"
            "for i in range(3):\n"
            "    print(f'tick {i}')\n"
            "    time.sleep(0.3)\n",
            on_stdout=lambda chunk: print("  [live]", chunk.strip()),
        )
        print()

        print("=== 4. Capturing errors ===")
        ex = sandbox.run_code("raise ValueError('intentional error')")
        print("success:", ex.success)
        print("stderr tail:", ex.stderr.strip().splitlines()[-1] if ex.stderr else "")
        print("exit_code:", ex.exit_code)


if __name__ == "__main__":
    main()
