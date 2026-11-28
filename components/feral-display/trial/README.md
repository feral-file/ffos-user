# Public engine trial (diagnostic, not product activation)

This runner launches an operator-selected, unmodified public Chromium executable
as a separate process, over inherited private DevTools pipes. Live capture and
offline replay use the same process/target setup. The installed C++ player is
still the CEF product path; this tool does not embed the public browser surface
or connect phone input/device UI to it. Do not describe it as that migration.
The exact-engine Casey capsule run is deferred by Anh pending its engine archive.

On FFOS, with an existing authorized Wayland session:

```sh
python3 browser_trial.py --engine /absolute/public/chrome --url https://artist.example/work/ \
  --output /absolute/new-live-run --capture /absolute/new-response-pack --headed
python3 browser_trial.py --engine /absolute/public/chrome --url https://artist.example/work/ \
  --output /absolute/new-offline-run --replay /absolute/new-response-pack --headed --isolate-network
```

Use a bounded systemd cgroup. Each output/profile is fresh and must not exist.
Offline uses bubblewrap: read-only host filesystem, writable output/profile,
private temporary directory, user/PID/network namespaces, no network interface
except down loopback. Chromium keeps its own sandbox; no TLS/security bypass.
Wayland and GPU access remain for the real accelerated window. The browser pipe
is private process plumbing, not a new external device control API.

Capture uses actual response-stage Fetch bodies/headers, including redirects;
worker requests are handled on the owning page target (dedicated-worker targets
in this engine do not expose Fetch). Runtime/network diagnostics also attach to
workers/frames. Decoded bodies omit stale transport encoding/length. Security
and repeated headers remain; original URLs are fulfilled without redirects to
local files. GET/HEAD public responses only; cookies, non-encoding Vary and
state-changing requests report unsupported capture. Replay checks recorded
variant headers and SHA-256 body identity. Resources are capped at 32 MiB,
4096 entries and a 128 MiB pipe response. This is bounded artwork-test tooling,
not the production streaming cache service or a general offline-ready guarantee.

A completed command records observations, not visual success. Inspect result.json,
network misses, errors and the captured frames. A navigation/empty canvas is
never a visual pass. Analytics POSTs may be unavailable while the artwork runs;
retain them as misses and never call capture complete in that case. Output
images are renderer captures, not photographs of physical panel output.

The tool leaves original artwork scripts unchanged. Diagnostic JS only reads
page state; the separate worker probe used for holz is labelled as an additional
instance of the artist's module. Engine/version hashes and process namespaces
are evidence; exact pixels are not expected for unseeded, real-time holz.
