#!/bin/bash
# A synthetic Internet route and LAN inside disposable namespaces. Neither
# endpoint has an uplink. Exercise the real route lookup, proxy and TLS client.
set -eu
helper=$(realpath "$1")
if [ "${2:-}" != inside ]; then
  for tool in bwrap curl ip nsenter openssl python3 unshare; do
    command -v "$tool" >/dev/null || exit 77
  done
  unshare --map-root-user --net -- /bin/true || exit 77
  exec unshare --map-root-user --net --pid --fork --mount-proc bash "$0" "$helper" inside
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
unshare --net sleep 45 &
peer=$!
for ((i=0; i<100; i++)); do
  [ "$(readlink /proc/"$peer"/ns/net)" != "$(readlink /proc/self/ns/net)" ] && break
  sleep .02
done
ip link add art-client type veth peer name art-server netns "$peer"
ip addr add 192.168.250.1/30 dev art-client
ip link set art-client up
in_peer=(nsenter -t "$peer" -n)
"${in_peer[@]}" ip addr add 192.168.250.2/30 dev art-server
"${in_peer[@]}" ip link set art-server up
# This globally numbered IP exists only on the test server's isolated loopback.
public=93.184.216.34
"${in_peer[@]}" ip link set lo up
"${in_peer[@]}" ip addr add "$public/32" dev lo
ip route add default via 192.168.250.2
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=fixture \
  -addext "subjectAltName=IP:$public" -out cert.pem -keyout key.pem 2>/dev/null
echo artwork-response > canary
"${in_peer[@]}" python3 - <<'PY' &
import http.server, ssl, threading
from pathlib import Path
plain = http.server.ThreadingHTTPServer(("", 18080), http.server.SimpleHTTPRequestHandler)
https = http.server.ThreadingHTTPServer(("", 18443), http.server.SimpleHTTPRequestHandler)
tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
tls.load_cert_chain("cert.pem", "key.pem")
https.socket = tls.wrap_socket(https.socket, server_side=True)
threading.Thread(target=https.serve_forever, daemon=True).start()
Path("ready").touch()
plain.serve_forever()
PY
for ((i=0; i<100; i++)); do [ -f ready ] && break; sleep .02; done
test -f ready
request() {
  "$helper" "$1" /usr/bin/bwrap --die-with-parent --unshare-all --ro-bind / / \
    --dev /dev --proc /proc "$helper" inside /usr/bin/curl --max-time 5 --silent \
    --show-error --noproxy '' --proxy http://127.0.0.1:17873 --cacert "$work/cert.pem" "${@:2}"
}
for url in "http://$public:18080/canary" "https://$public:18443/canary"; do
  test "$(request host --fail "$url")" = artwork-response
done
lan=http://192.168.250.2:18080/canary
test "$(request host -o /dev/null -w '%{http_code}' "$lan")" = 502
test "$(request host-lan --fail "$lan")" = artwork-response
echo 'Verified public HTTP and HTTPS bodies, default LAN denial, and explicit LAN grant.'
