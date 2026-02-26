package mdns

import (
	"fmt"
	"sync"

	"github.com/grandcat/zeroconf"
	"go.uber.org/zap"
)

const (
	defaultPort   = 1111
	serviceType   = "_ff1._tcp"
	serviceDomain = "local."
)

// DeviceInfo contains the info to publish via mDNS.
type DeviceInfo struct {
	ID   string
	Name string
	Port int
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

	name := info.Name
	if name == "" {
		name = info.ID
	}
	if name == "" {
		name = "FF1"
	}

	txt := []string{}
	if info.ID != "" {
		txt = append(txt, "id="+info.ID)
	}
	if info.Name != "" {
		txt = append(txt, "name="+info.Name)
	}

	server, err := zeroconf.Register(name, serviceType, serviceDomain, port, txt, nil)
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
