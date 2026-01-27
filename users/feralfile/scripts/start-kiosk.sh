#!/bin/bash

# Read saved rotation
ROTATION="normal"
if [ -f /home/feralfile/.state/screen-orientation ]; then
    ROTATION=$(cat /home/feralfile/.state/screen-orientation)
fi

export WLR_DRM_FORMATS="XR24/I915_FORMAT_MOD_Y_TILED;XR24/I915_FORMAT_MOD_Yf_TILED"

# Detect SOC vendor
VENDOR=$(cat /proc/cpuinfo | grep -m1 vendor_id | awk '{print $3}')
if [ "$VENDOR" = "GenuineIntel" ]; then
    FEATURES="UseOzonePlatform,AcceleratedVideoDecodeLinuxGL,AcceleratedVideoDecodeLinuxZeroCopyGL"
else
    # Default to AMD
    FEATURES="UseOzonePlatform,VaapiVideoDecoder,VaapiIgnoreDriverChecks,Vulkan,DefaultANGLEVulkan,VulkanFromANGLE"
fi

# Features to disable for kiosk mode
# TranslateUI: Disable translate prompt
# InterestFeedContentSuggestions: Disable NTP feed
# CalculateNativeWinOcclusion: Prevent occlusion throttling (mostly Windows but good practice)
# GlobalMediaControls: Hide media control UI
DISABLE_FEATURES="TranslateUI,InterestFeedContentSuggestions,CalculateNativeWinOcclusion,GlobalMediaControls"

/home/feralfile/scripts/cdp-ready-check.sh &

# Start cage with bash, which will wait, rotate the screen, and start Chromium
exec cage -- /bin/bash -c "wlr-randr --output HDMI-A-1 --transform $ROTATION && exec /usr/bin/chromium \
    --kiosk \
    --ozone-platform=wayland \
    --enable-features=$FEATURES \
    --ignore-gpu-blocklist \
    --enable-gpu-rasterization \
    --remote-debugging-port=9222 \
    --no-first-run \
    --disable-sync \
    --disable-translate \
    --disable-infobars \
    --disable-features=$DISABLE_FEATURES \
    --disable-background-networking \
    --noerrdialogs \
    --disable-extensions \
    --autoplay-policy=no-user-gesture-required \
    --disable-client-side-phishing-detection \
    --allow-file-access-from-files \
    --enable-logging=stderr \
    --v=0 \
    --disk-cache-size=85899345920 \
    --hide-scrollbars \
    --disable-search-engine-choice-screen \
    --ash-no-nudges \
    --no-default-browser-check \
    --propagate-iph-for-testing \
    --disable-background-timer-throttling \
    --disable-renderer-backgrounding \
    --disable-hang-monitor \
    --deny-permission-prompts \
    --disable-external-intent-requests \
    --disable-component-extensions-with-background-pages \
    file:///opt/feral/ui/launcher/index.html?step=logo"