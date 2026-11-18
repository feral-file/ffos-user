Sean, I extended the trial on an FF1. We now have direct evidence for holz live,
holz offline, and a separate public Chromium process. The capsule test is deferred
until we have the exact engine image it names.

**holz and workers:** it loads as a main document from Leander's own HTTPS URL,
with the original origin and CSP. WebGPU geometry is visible in retained captures.
I also ran the artist's unchanged geometry worker module diagnostically: it
returned 49 bodies and 235 KB of geometry buffers. No player UI/control code was
added to the artwork.

**Offline:** I captured the actual responses and restarted with an empty browser
profile inside a network namespace with only a down loopback interface. Holz
still ran at the original URL, with its module worker and WebGPU. A second cold
process start also worked. The browser had no network capability or outbound
interface; this was not a warm-cache test.

There is one unavailable dependency: Cloudflare's analytics POST. It remains
recorded as a miss, so I am not calling the capture complete. As a negative test,
I removed geometry.mjs: the log named the missing module and the artwork showed
“Geometry worker failed.” A loaded page/empty canvas is not a pass.

**Public engine:** the same diagnostic runner used both unmodified system
Chromium 150 and public Chrome for Testing 156. I checked the latter's executable
against its downloaded archive and checked the system package for alterations.
Killing the separate experimental browser left the native host's playback
occurrence and signed device control intact. Graphics flags/AMD workarounds are
still needed; there is no local Chromium source patch.

There is an important implementation boundary: the installed C++ player still
embeds CEF's browser process. These experiments establish a separate public-engine
path for live/offline testing; they do not yet integrate that surface, native UI
and phone input into the product. So I cannot claim the complete one-path
architecture you describe is delivered.

Beyond holz, Transparent Grit ran live and then offline from its original URL,
including the original redirect, with a fresh profile and kernel networking
blocked. The earlier normal-control trial also exercised every work in the real
ten-work Social Codes playlist through FF-CLI, with retained visual captures,
and tested host/watchdog recovery. Those are compatibility smoke tests, not
artist-approved fidelity, physical-panel proof or preservation reproduction.
The existing matched-engine/seed Grit benchmark still shows the native offscreen
path about 15% slower than standalone Chrome; I am not claiming parity.

For Casey's There's No Distance #1, the repository supplies the capsule and
manifest, but not the engine archive pinned by SHA-256 f0d8ced3…390d400. I have not
substituted another browser and called it reproduced. Once that image is available,
we can test its exact harness, signature and reference-frame result on the FF1.

[Trial method, evidence and limits](https://github.com/feral-file/ffos-user/blob/codex/native-player/components/player/docs/SEAN-TRIAL-RESULTS.md)
