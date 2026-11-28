# Homelab evidence

The [device report](../../HOMELAB-PARITY.md) describes the hardware, measurements and deployment. These filenames identify benchmark inputs to `tools/summarize-matched.py`. The sibling `.timing.json.gz` files contain the native host's changed-frame Wayland presentation timestamps. Standalone and shipping runs carry Chromium's compositor events inside the main benchmark JSON.

| File stem | Engine and configuration |
| --- | --- |
| `shipping`, `shipping-repeat` | Shipping ff-player 3.0.1, installed Chromium 150 |
| `chrome`, `standalone-repeat` | Standalone Chrome for Testing 158 |
| `before` | Public Chromium, 3b8dfaf5 |
| `after` | Public Chromium, 25828c29, fenced GPU copy |
| `cef-final` | Off-screen CEF, 25828c29, adaptive frame clock |
| `public-final`, `views` | Public Chromium and windowed CEF, e0e915e9, event-driven host wake-up |
| `e98-views` | Windowed CEF, e98c06e5, fenced GPU copy |
| `import-public`, `import-views`, `import-views-repeat` | Direct RGB import, e21063b4 |
| `pipe-public`, `pipe-views` | Two outstanding browser submissions, afb23112 |
| `chosen-public`, `chosen-views` | One outstanding browser submission, direct RGB import, 18639600 |
| `delivered-public`, `delivered-views` | Delivered build e9d751f3, fenced GPU copy on public Chromium and direct RGB import on CEF |

`output-summary.json` contains the calculated intervals, tails and kiosk cgroup readings. In `embedded-video.json`, arm names use the same stems plus `-media`, except that `public-media` names the 25828c29 public run. H.264 rows include decoder observations, cumulative counters before and after playback, and their deltas. The final minute-long runs also record each ten-second window. HEVC rows record decoded frames, dimensions, playback time and media errors for the Main-profile control and each of the 19 Range Extensions cases.

`shipping-sustained-h264.json` contains the minute-long shipping baseline. `public-fenced-h264.json` contains the separate 25828c29 minute-long 4K hardware-decoder check. The GPU JSON files record Chromium SystemInfo reports. The video contact sheet pairs compositor captures 650 ms apart.
