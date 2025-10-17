#!/bin/bash

# Read saved rotation
ROTATION="normal"
if [ -f /home/feralfile/.config/screen-orientation ]; then
    ROTATION=$(cat /home/feralfile/.config/screen-orientation)
fi

# Set Wayland/wlroots environment variables
export WLR_DRM_FORMATS="XR24/I915_FORMAT_MOD_Y_TILED;XR24/I915_FORMAT_MOD_Yf_TILED"

/home/feralfile/scripts/cdp-ready-check.sh &

# Start cage with bash, which will wait, rotate the screen, and start Chromium
exec cage -- /bin/bash -c "wlr-randr --output HDMI-A-1 --transform $ROTATION && exec /usr/bin/chromium \
    --kiosk \
    --ozone-platform=wayland \
    --enable-features=UseOzonePlatform \
    --remote-debugging-port=9222 \
    --no-first-run \
    --disable-sync \
    --disable-translate \
    --disable-infobars \
    --disable-features=TranslateUI \
    --disable-background-networking \
    --noerrdialogs \
    --disable-extensions \
    --autoplay-policy=no-user-gesture-required \
    --allow-file-access-from-files \
    --enable-logging=stderr \
    --v=0 \
    --disk-cache-size=1073741824 \
    --hide-scrollbars \
    file:///opt/feral/ui/launcher/index.html?step=logo"