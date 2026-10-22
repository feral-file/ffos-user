# Generic v2 carrier integration

Release target: develop, never main. Security dependency: #412 at
`51164c8c83afd091120de00e3b0270edcfb8e09e`. This integration selectively adds
transport around that authority; it does not replace its registration, grants,
replay windows, key notification rules or OTA work.

## Configuration and dialing

Optional controld configuration (do not add to shipped images before release gates):

```json
{
	"relayV2": {"endpoint": "wss://ff-relayer-device-test.bitmark-development.workers.dev"}
}
```

`relayer.endpoint` and `relayer.apiKey` retain their legacy meanings. No relayV2
key or empty endpoint means legacy-only, including package-only upgrades on old
images. V2 requires an actual control authority and valid stable `FF1-XXXXXXXX`
ID. Persisted control state can force the authority on with config disabled;
`control.enabled` alone never decides dialing. Fallback `FF1`/`FF1-UNKNOWN` do
not dial v2. Legacy topic acquisition, claim QR and lifecycle remain unchanged.

## Carrier boundaries

One independent socket dials `/v2/channels/{deviceId}/connections` using
`ff-relay.v2`. There are ordinary peers, no roles/tokens/origin-join/mailbox.
Every request enters a dedicated gate as RouteRelayV2 and must be ff1Envelope;
there is no plaintext profile/command/notification fallback in any state.
Existing application wrappers and relayChunk format stay inside opaque frames.
ACK well-formed transport bytes before endpoint acceptance; bounded assembly,
receipt credit and pacing are transport responsibilities.

Only a successful command admission after the authority persists its replay
window can bind controller to source peer. No presence, public error, or replay
binds. Replies target that source and socket generation. Private notifications
are separately sealed by Authority.Seal for each live bound peer, so current
expiry/revocation remains enforced. Peer-disconnect and socket-close callbacks
remove bindings; a new socket needs a fresh sealed getDeviceStatus.
Notification producers remain active when either legacy or v2 is connected.
V2 retry failures stay local and never exit controld or corrupt legacy metrics.

## Missing dependency and release gates

The [latest agreement](https://github.com/feral-file/feral-file/issues/3551#issuecomment-6332777506)
is accepted design, not implemented at the security head above. Brandon still
owns registerController, exact fixtures, invitation refusal on v2, LAN-only
requestAccess, grant lifecycle, notification-key rules and the remaining policy
changes. In particular, this carrier calls his gate without implementing those
missing rules itself. **Do not enable/release v2 until that dependency lands.**
The supported first-owner LAN/legacy migration route is preserved.

FFOS #174 OTA/control-state/build protections, force-update floors, recovery
images, shelf-stock dual-stack migration, telemetry and no channel rollback below
dual-stack are prerequisites. Legacy controller retirement precedes retirement
of device connections/topic issuance. No image/package is built or published,
no device is flashed/reset/rebooted and no OTA implementation is changed here.
The FF1-SKYZ2E3A secured manual matrix is blocked on the installed secure revision
and updated app/CLI/browser fixtures. Historical plaintext results are not proof.

Automated checks cover generic framing/chunks, targeted source replies, private
notification sealing after existing fixture admission, replay from another peer,
plaintext ingress refusal, disconnect retirement, v2-only producer readiness,
nonfatal reconnect, and endpoint/authority/ID eligibility. Run `go test ./...`,
`go test -race ./relayer ./relaywire ./controlgate`, `go vet ./...` and changed-diff
golangci-lint from components/feral-controld.
