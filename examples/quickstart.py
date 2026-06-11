"""阶段 0 快速上手示例。

直接运行：
    python examples/quickstart.py

它会自动在本机拉起一个沙箱守护进程，跑几段代码，然后清理。
"""

from microsandbox import Sandbox


def main() -> None:
    # spawn_local=True（默认）会自动起一个本机守护进程并在退出时关闭。
    with Sandbox() as sandbox:
        print("=== 1. 基本输出 ===")
        ex = sandbox.run_code("print('hello from the sandbox')")
        print("stdout:", ex.stdout.strip())
        print("exit_code:", ex.exit_code, "success:", ex.success)
        print()

        print("=== 2. 多行计算 ===")
        ex = sandbox.run_code(
            "total = sum(range(101))\n"
            "print(f'0..100 求和 = {total}')"
        )
        print("stdout:", ex.stdout.strip())
        print()

        print("=== 3. 流式输出（边跑边收）===")
        sandbox.run_code(
            "import time\n"
            "for i in range(3):\n"
            "    print(f'tick {i}')\n"
            "    time.sleep(0.3)\n",
            on_stdout=lambda chunk: print("  [实时]", chunk.strip()),
        )
        print()

        print("=== 4. 捕获错误 ===")
        ex = sandbox.run_code("raise ValueError('故意出错')")
        print("success:", ex.success)
        print("stderr 末尾:", ex.stderr.strip().splitlines()[-1] if ex.stderr else "")
        print("exit_code:", ex.exit_code)


if __name__ == "__main__":
    main()
