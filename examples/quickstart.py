"""Stage 0 quickstart example.

Run it directly:
    python examples/quickstart.py

It automatically spins up a sandbox daemon locally, runs a few snippets of code,
and then cleans up.
"""

from microsandbox import Sandbox


def main() -> None:
    # spawn_local=True (the default) automatically starts a local daemon and shuts it down on exit.
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
