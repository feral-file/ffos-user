package contentpath

// A device-local source is an https URL under a reserved `.invalid` host that
// names content the device resolves itself rather than a host it dials: an
// onchfs tree read from chain, or a web3 contract call. Replay serves it out
// of the offline-cache store; nothing ever dials the host, which by RFC 6761
// cannot resolve.
//
// This is the seam every scheme-agnostic caller uses. Before web3 there was
// one such scheme and every caller named it (`IsOnchfsSourceURL` at eleven
// sites across offlinecache, mediahealth and commandrouter), so the second
// scheme had to re-thread all eleven and a third would have been missed at
// whichever one nobody remembered — the mechanism this seam retires
// (feral-file#3599). A caller that only needs to know "the device holds this,
// do not dial it, do not probe it, do not capture it" asks here.
//
// # Adding a third scheme: the complete list of places that must know
//
// An earlier version of this comment said only the resolver and the
// materializer need to know which scheme it is. That was wrong, and wrong in
// the way that costs: a scheme registered in only those two places falls
// through `offlinecache.Classify`'s switch into the source guard, which
// refuses the unresolvable host, so the work still plays (replay and the
// materializer are predicate-driven) while the app's download command answers
// a classification failure and the queue logs a failed item (round 3 review,
// F-D). The places are:
//
//  1. this file — a `DeviceLocalScheme` constant and its case in
//     `DeviceLocalSourceScheme`;
//  2. the scheme's own parser beside `onchfs.go` / `web3.go` here, with its
//     `SourceURL` / `Parse…SourceURL` inverse pair;
//  3. `offlinecache/classify.go` — a `MediaClass`, its case in `Classify`,
//     and its case in `IsDeviceHeldClass`;
//  4. `offlinecache.DeviceLocalMaterializer.resources` — the resolver call
//     and the resources it produces, plus the config field that wires it;
//  5. `dp1/devicelocal.go` — `deviceLocalItemSource`, and the three raw-URI
//     helpers `DeviceLocalSource`, `DeviceLocalSourceVariants` and
//     `SameDeviceLocalWork`.
//
// Everything else — mediahealth's five device-held passes, the offline cache's
// queue skips and warming, the commandrouter's open wait, the retention pool —
// is predicate-driven and needs no edit. That is what the seam bought; it did
// not buy a one-line registration, and saying it did is how the next scheme
// gets step 3 wrong.

// DeviceLocalScheme names the scheme behind a device-local source.
type DeviceLocalScheme string

// The schemes the device resolves itself.
const (
	// SchemeNone is the zero value: not a device-local source.
	SchemeNone DeviceLocalScheme = ""
	// SchemeOnchfs is fxhash's on-chain file system, content-addressed and
	// verified inode by inode (ffos-user#427).
	SchemeOnchfs DeviceLocalScheme = "onchfs"
	// SchemeWeb3 is ERC-4804, a work that is a contract call's return value.
	// There is no content address, so the trust is the node's.
	SchemeWeb3 DeviceLocalScheme = "web3"
)

// DeviceLocalSourceScheme reports which scheme raw is a device-local source
// for, or SchemeNone when it is an ordinary source the device dials.
func DeviceLocalSourceScheme(raw string) DeviceLocalScheme {
	switch {
	case IsOnchfsSourceURL(raw):
		return SchemeOnchfs
	case IsWeb3SourceURL(raw):
		return SchemeWeb3
	}
	return SchemeNone
}

// IsDeviceLocalSource reports whether raw is a source the device resolves
// itself. Callers use it to keep these URLs away from anything that would
// dial, probe or capture them.
func IsDeviceLocalSource(raw string) bool {
	return DeviceLocalSourceScheme(raw) != SchemeNone
}
