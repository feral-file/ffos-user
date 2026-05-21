#!/bin/bash

CDP_PORT=9222
MAX_RETRY=90

for i in $(seq 1 "$MAX_RETRY"); do
    resp=$(curl -sf "http://127.0.0.1:$CDP_PORT/json" 2>/dev/null)
    if [ -n "$resp" ]; then
        # Gate on feral-controld's exact CDP.Init() precondition: it counts
        # targets by type=="page" alone, requires exactly one, then uses that
        # page's webSocketDebuggerUrl. Matching it here keeps controld/setupd
        # from being pulled in before they can actually connect to CDP.
        page_count=$(printf '%s' "$resp" | jq '[.[] | select(.type=="page")] | length' 2>/dev/null)
        ws_url=$(printf '%s' "$resp" | jq -r 'first(.[] | select(.type=="page") | .webSocketDebuggerUrl) // ""' 2>/dev/null)
        if [ "$page_count" = "1" ] && [ -n "$ws_url" ]; then
            echo "✅ CDP is available, Chromium ready"
            systemctl --user start chromium-ready.target
            exit 0
        fi
    fi

    echo "⏳ Waiting for CDP... ($i/$MAX_RETRY)"
    sleep 1
done

echo "❌ Timeout: CDP not responding, restarting Chromium kiosk service"
systemctl --user restart chromium-kiosk.service
exit 1