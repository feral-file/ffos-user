#!/bin/bash

# Base file URL
base_url="file:///opt/feral/ui/launcher/index.html"
query=""

# Parse key=value arguments
for arg in "$@"; do
  if [[ "$arg" == *"="* ]]; then
    key="${arg%%=*}"
    value="${arg#*=}"
    # URL encode the value
    encoded_value=$(printf '%s' "$value" | jq -s -R -r @uri)
    # Append to query string
    if [[ -n "$query" ]]; then
      query="${query}&${key}=${encoded_value}"
    else
      query="${key}=${encoded_value}"
    fi
  fi
done

# Construct the full URL
if [[ -n "$query" ]]; then
  full_url="${base_url}?${query}"
else
  full_url="${base_url}"
fi

echo "Launching URL: $full_url"

# Launch Chromium inside cage
exec cage -- /usr/bin/chromium --ozone-platform=wayland \
  --app="$full_url" \
  --disable-features=TranslateUI \
  --noerrdialogs \
  --start-fullscreen
