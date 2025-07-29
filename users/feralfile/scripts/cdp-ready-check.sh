#!/bin/bash

CDP_PORT=9222
MAX_RETRY=90

for i in $(seq 1 "$MAX_RETRY"); do
    if curl -sf "http://127.0.0.1:$CDP_PORT/json" > /dev/null; then
        echo "✅ CDP is available, Chromium ready"
        systemctl start chromium-ready.target
        exit 0
    fi

    echo "⏳ Waiting for CDP... ($i/$MAX_RETRY)"
    sleep 1
done

echo "❌ Timeout: CDP not responding, restarting Chromium kiosk service"
systemctl restart chromium-kiosk.service
exit 1