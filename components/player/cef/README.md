# FF1 CEF SDK

This recipe pins the CEF revision used by the native player and builds the Linux
x64 SDK with Wayland, VA-API, and H.264/AAC and HEVC support. CEF's compatibility
file pins Chromium. GitHub Actions fetches the source into the runner's temporary
directory. A player checkout contains only the recipe and build settings.

The custom CEF workflow requests the `self-hosted` label. On macOS it uses
Docker's `linux/amd64` guest. Docker must already be running. The container uses Linux
storage for the Chromium checkout and build. The preflight checks for 8 GiB
of guest RAM and the build recipe requires 150 GiB of free guest disk space.
On Apple silicon, Docker needs x64 translation enabled. A shared runner group
must grant `ffos-user` access to the runner.

To use a native Ubuntu x64 runner instead, set `CEF_BUILD_RUNNER` to its label.
The runner needs sudo to install Chromium's build dependencies. A hosted
runner with 16 or more CPUs and 300 GB of disk is a starting point.

The SDK artifact contains `cef.tar.xz`, its version and SHA-256 in `cef.json`,
and the generated `args.gn`. The player launches CEF with Wayland and ANGLE
Vulkan on the FF1 Radeon 680M. Its existing VA-API flags leave the FFOS kernel's
4096×4080 H.264 limit in effect.

The next job verifies the checksum and runs the existing native player build
and CTest suite with that SDK. Its player artifact has the same install layout
as the regular FF1 player bundle. Check `SHA256SUMS` before extracting it into
an isolated trial directory on FF1.

For the device check, load an HTML page containing the test video through the
normal CEF Views worker. Use the Media CDP domain to capture the decoder name
and platform status. The 3840×2160 H.264 control should report
`VaapiVideoDecoder` and advance the decoded frame count. The 4096×4096 H.264
control on the Radeon 680M FF1 should report `FFmpegVideoDecoder` and advance
the frame count. Use the same clips and launch settings as the
[homelab comparison](../docs/HOMELAB-PARITY.md).
