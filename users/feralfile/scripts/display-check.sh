#!/usr/bin/env bash
# display-check.sh: the "Display check" boot entries of the factory ISO (ffos
# factory-iso/entries/archiso-x86_64-linux-display-check-*.conf; on the factory
# stick only, which has no install entry: the normal ISO boots Auto Install,
# which wipes the NVMe, unless a key is held). Boots an assembled unit from the
# factory USB stick straight into full-screen test slides, nothing installed,
# nothing on the NVMe touched, no network. The slides are one offline page,
# /home/feralfile/display-check/index.html (source: feral-file/toko-batch-2
# factory/assembly-check; change it there and copy it here).
#
# The boot entry names the set on the kernel command line:
#   display_check=assembly    nine captioned slides, about 100 s, looping: white,
#                             red, green, blue, ramps, geometry, motion, video,
#                             graphics. The last three show a counter that turns
#                             red on late or dropped frames.
#   display_check=brightness  white, gray, black, 60 s each, looping: brightness,
#                             evenness, light leak. With a keyboard, Right/Space
#                             and Left step and hold a slide; A resumes.
#   display_check=panel       black 15 s, gray 15 s, no caption: the Panel Check
#                             loop Feral File scores from a phone capture.
# Hands-off like factory-check.sh: the keyboard is needed for the boot menu
# only (hold a key at power-on; the factory ISO boots the Factory check
# otherwise). The entries mask kmsconvt@tty1, which would run the factory
# check on tty1 and hold the display, so the getty@tty1 autologin logs root in
# and runs this through /root/.automated_script.sh; Chromium therefore needs
# --no-sandbox; the page is a local file and nothing else is loaded. Ends when
# the power is pulled.
set -uo pipefail

PAGE=/home/feralfile/display-check/index.html
CMDLINE="${FF_CMDLINE:-/proc/cmdline}"

# The set named by display_check= on the kernel command line. Anything missing
# or unknown runs the assembly set: a typo in a boot entry still shows a test.
check_set() {
  local param
  for param in $(<"$CMDLINE"); do
    case "$param" in
      display_check=brightness|display_check=panel|display_check=assembly)
        echo "${param#display_check=}"; return 0 ;;
    esac
  done
  echo assembly
}

page_url() {
  case "$1" in
    assembly) echo "file://$PAGE" ;;
    *) echo "file://$PAGE?set=$1" ;;
  esac
}

# Sourced (scripts/test-display-check.sh), only the functions load.
[ "${BASH_SOURCE[0]}" = "$0" ] || return 0

if [ ! -r "$PAGE" ]; then
  clear
  printf '\nDisplay check: %s is not on this image.\n' "$PAGE"
  sleep infinity
fi

url=$(page_url "$(check_set)")

# The flags the kiosk uses for decode and graphics (start-kiosk.sh), so the
# video and graphics slides exercise the same path an installed unit does.
FEATURES="UseOzonePlatform,VaapiVideoDecoder,VaapiIgnoreDriverChecks,Vulkan,DefaultANGLEVulkan,VulkanFromANGLE"

# Chromium exits when the panel is unplugged or the GPU process dies; start it
# again so a re-seated cable brings the slides back without a reboot.
while true; do
  cage -s -- chromium "$url" \
    --no-sandbox \
    --kiosk \
    --no-first-run \
    --disable-sync \
    --disable-infobars \
    --disable-features=TranslateUI \
    --autoplay-policy=no-user-gesture-required \
    --allow-file-access-from-files \
    --ozone-platform=wayland \
    --enable-features="$FEATURES" \
    --user-data-dir=/tmp/display-check-profile >/tmp/display-check.log 2>&1
  clear
  printf '\nDisplay check stopped; starting again in 3 s. Log: /tmp/display-check.log\n'
  sleep 3
done
