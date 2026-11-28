#!/bin/bash
set -euo pipefail
arm=$1
systemctl --user stop ff-native-runtime-test.service ff-native-chrome-test.service || true
for unit in ff-native-runtime-test.service ff-native-chrome-test.service; do
 for attempt in {1..50}; do
  [[ $(systemctl --user show "$unit" -p LoadState --value) == not-found ]] && break
  sleep 0.1
 done
done
if ! systemctl --user is-active --quiet ff-native-test-media.service; then
 systemd-run --user --unit=ff-native-test-media --collect -p MemoryMax=128M -p RuntimeMaxSec=14400 /usr/bin/python3 /home/feralfile/ff-native-debug/tools/test-media-server.py /home/feralfile/ff-native-debug/tests
fi
common=(--ozone-platform=wayland --use-angle=gl-egl --autoplay-policy=no-user-gesture-required --test-type=gpu)
if [[ $arm == chrome ]]; then
 systemd-run --user --unit=ff-native-chrome-test --collect -p MemoryMax=6G -p TasksMax=512 -p RuntimeMaxSec=900 -p TimeoutStopSec=8 --setenv=XDG_RUNTIME_DIR=/run/user/1000 --setenv=WAYLAND_DISPLAY=wayland-0 --setenv=AMD_DEBUG=nodcc /home/feralfile/ff-native-pacing/chrome-linux64/chrome "${common[@]}" --user-data-dir=/home/feralfile/ff-native-pacing/chrome-profile --no-first-run --no-default-browser-check --kiosk --remote-debugging-port=9225 http://127.0.0.1:18990/grit-fixed.html
else
 systemd-run --user --unit=ff-native-runtime-test --collect -p MemoryMax=6G -p TasksMax=512 -p KillMode=mixed -p RuntimeMaxSec=900 -p TimeoutStopSec=8 --setenv=XDG_RUNTIME_DIR=/run/user/1000 --setenv=WAYLAND_DISPLAY=wayland-0 --setenv=SDL_VIDEODRIVER=wayland --setenv=AMD_DEBUG=nodcc /usr/bin/env -u DBUS_SESSION_BUS_ADDRESS /home/feralfile/ff-native-pacing/stage/usr/lib/ff-player-native/ff-player-native "${common[@]}" --state-dir=/home/feralfile/ff-native-pacing/state --socket=/run/user/1000/ff-native-test/player.sock --renderer-dir=/home/feralfile/ff-native-pacing/stage/usr/share/ff-player-native/renderers --diagnostic-presentation-timing=/home/feralfile/ff-native-pacing/timing.json --remote-debugging-port=9224
fi
