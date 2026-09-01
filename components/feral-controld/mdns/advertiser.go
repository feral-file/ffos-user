package mdns

import (
	"fmt"
	"strings"
	"sync"

	"github.com/grandcat/zeroconf"
	"go.uber.org/zap"
)

const (
	defaultPort   = 1111
	serviceType   = "_ff1._tcp"
	serviceDomain = "local."

	// APITXTVersion is the LAN API version advertised as the `api=<v>` TXT
	// key. It mirrors hub.StatusContractV2 (kept in sync by convention, not a
	// production import — mdns stays hub-free; a hub-side test asserts the two
	// constants are equal): the pairing app filters discovery on this
	// key, so old firmware — which advertises no `api` key and serves no
	// /api/v2/status — never pops a pairing notification and is never offered
	// as pairable, without the app needing an HTTP probe per discovered device.
	APITXTVersion = "2"
)

// DeviceInfo contains the info to publish via mDNS.
type DeviceInfo struct {
	ID   string
	Name string
	Port int
	// Claimed reflects whether the device is currently paired/claimed. It is
	// published as a mDNS TXT flag so LAN discovery can tell claimed devices
	// from unclaimed ones without a round-trip. Because zeroconf only publishes
	// the TXT once at Register time, a claim-state change requires a Stop+Start
	// re-registration (see mediator.SetClaimed).
	Claimed bool
}

// Advertiser publishes FF1 discovery records over mDNS.
type Advertiser interface {
	Start(info DeviceInfo) error
	Stop()
}

type advertiser struct {
	logger *zap.Logger
	mu     sync.Mutex
	server *zeroconf.Server
}

// New creates a new Advertiser instance.
func New(logger *zap.Logger) Advertiser {
	return &advertiser{logger: logger}
}

// txtRecords builds the advertised TXT set. Split from Start so the record
// contract is unit-testable without zeroconf binding sockets.
func txtRecords(info DeviceInfo) []string {
	txt := []string{}
	if info.ID != "" {
		txt = append(txt, "id="+info.ID)
	}
	// The advertised name always resolves to something: an owner who clears
	// their custom name gets the serial back, not a missing key. Resolvers —
	// the pairing app, ff-cli's device lookup — treat TXT `name` as the label
	// to show, and dropping the key on a cleared name would silently change
	// what a controller displays for a frame that used to announce one.
	if name := displayName(info); name != "" {
		txt = append(txt, "name="+name)
	}
	// claimed is always published (even when false) so a resolver can rely on
	// its presence rather than having to infer "unclaimed" from an absent key.
	if info.Claimed {
		txt = append(txt, "claimed=true")
	} else {
		txt = append(txt, "claimed=false")
	}
	// api is always published: it is the discovery-time firmware gate (see
	// APITXTVersion). Old firmware's records lack the key entirely.
	txt = append(txt, "api="+APITXTVersion)
	return txt
}

// maxInstanceLabelOctets is the DNS label ceiling. A DNS-SD service-instance
// name is one label, so it cannot exceed 63 octets — and the limit is octets,
// not characters, so a name well inside its own display limit can still be too
// long here (32 accented or CJK runes are 64+ octets).
const maxInstanceLabelOctets = 63

// displayName is the label to publish for a device: the owner's name when
// there is one, otherwise the serial.
func displayName(info DeviceInfo) string {
	if info.Name != "" {
		return info.Name
	}
	return info.ID
}

// instanceLabel is the DNS-SD service-instance name, bounded separately from
// the display name.
//
// A name too long for a DNS label is not truncated: a chopped label is neither
// what the owner set nor a stable identifier, and two long names could collide
// on the same prefix. The serial is used instead — always in range, always
// unique — and the full name still travels in the TXT record, which has no
// such limit. The alternative, letting the registration fail, is far worse
// than a plain instance name: the rename path stops the existing advertisement
// before re-registering, so a rejected label would leave the frame
// undiscoverable while the command reported success.
//
// A name containing a dot or a backslash takes the same serial fallback, and
// this branch is load-bearing, not cosmetic. An RFC 6763 §4.3 service-instance
// name is exactly ONE label, and zeroconf performs no DNS escaping: it splices
// the instance string verbatim into "<instance>.<service>.<domain>", which
// miekg/dns then parses as DNS PRESENTATION format. Both metacharacters of
// that format are therefore live:
//
//   - A single dot produces a multi-label instance name that
//     Avahi/Bonjour/NsdManager parse differently from each other, and
//     consecutive dots ("Hi.. there", an ellipsis) produce an EMPTY label,
//     which makes every outgoing response fail dns.Msg.Pack ("bad rdata") —
//     zeroconf.Register still returns nil because probe/announce run in
//     goroutines, so the daemon believes it is advertising while answering
//     no queries at all.
//   - A backslash escapes whatever follows it: a trailing "\" swallows the
//     label separator, publishing the record under "_tcp.local." instead of
//     the browsed "_ff1._tcp.local.", and a "\DDD" sequence is consumed as a
//     decimal escape, silently advertising a label that differs from the
//     stored name.
//
// The name persists on disk, so without this guard one such rename would
// silently and permanently remove the frame from mDNS discovery — the
// BLE-replacement LAN recovery channel.
func instanceLabel(info DeviceInfo) string {
	name := displayName(info)
	if name == "" {
		return "FF1"
	}
	if len(name) > maxInstanceLabelOctets || strings.ContainsAny(name, `.\`) {
		if info.ID != "" && len(info.ID) <= maxInstanceLabelOctets && !strings.ContainsAny(info.ID, `.\`) {
			return info.ID
		}
		return "FF1"
	}
	return name
}

// Start registers an mDNS service.
func (a *advertiser) Start(info DeviceInfo) error {
	a.mu.Lock()
	if a.server != nil {
		a.mu.Unlock()
		return fmt.Errorf("mdns advertiser already started")
	}

	port := info.Port
	if port == 0 {
		port = defaultPort
	}

	name := instanceLabel(info)

	server, err := zeroconf.Register(name, serviceType, serviceDomain, port, txtRecords(info), nil)
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("failed to register mdns service: %w", err)
	}

	a.server = server
	a.mu.Unlock()

	a.logger.Info("mDNS advertiser started",
		zap.String("service", serviceType),
		zap.String("name", name),
		zap.Int("port", port))

	return nil
}

// Stop shuts down the mDNS service.
func (a *advertiser) Stop() {
	a.mu.Lock()
	server := a.server
	a.server = nil
	a.mu.Unlock()

	if server == nil {
		return
	}

	server.Shutdown()
	a.logger.Info("mDNS advertiser stopped")
}
