"""Stage 6: named templates -- a sandbox boots from a custom (rootfs, snapshot) image.

The 'example' template (templates/example/Dockerfile) is the stock agent image plus a
marker file. These assert the named template boots and carries that marker, while the
default image does not -- proving that selecting a template actually swaps the image,
not just the name. Real VMs, so they auto-skip without go/firecracker/kvm like the
other microVM tests (the control_plane fixture behind both fixtures does the skipping).
"""

MARKER = "/etc/microsandbox-template"


def test_named_template_carries_its_own_image(example_sandbox):
    assert "example template" in example_sandbox.files.read(MARKER)


def test_default_image_lacks_the_template_marker(sandbox):
    # The stock image has no such file, so `cat` exits non-zero.
    assert sandbox.commands.run(f"cat {MARKER}").exit_code != 0
