package playerlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Client orders operations from this controld instance. Each operation has a
// persistent connection and bounded handshake. Keeping the connection alive
// preserves held input across commands; disconnect releases it in the host.
// A transport timeout discards the stream and is never an automatic replay.
type ArtworkSources struct {
	BundleRoot   string   `json:"bundleRoot"`
	GatewayHosts []string `json:"gatewayHosts,omitempty"`
	UserAgent    string   `json:"userAgent,omitempty"`
	Gateway      string   `json:"gateway,omitempty"`
}

type Client struct {
	// Sources is configured before starting the connectivity reconciler.
	Sources      *ArtworkSources
	Path         string
	Name         string
	lane         chan struct{}
	connection   net.Conn // protected by lane
	instance     string
	generation   uint64
	capabilities []string
}

func New(path string) *Client {
	return &Client{Path: path, Name: "controld-" + uuid.NewString(), lane: make(chan struct{}, 1)}
}

type Hello struct {
	Version      int      `json:"version"`
	Instance     string   `json:"instance"`
	Snapshot     Snapshot `json:"snapshot"`
	Capabilities []string `json:"capabilities"`
}

// Preconditions binds a mutation to the state on which the caller made its
// decision. Without it, mutations use the last generation observed by this
// command connection. A watch delivery does not silently update that fence.
type Preconditions struct {
	Instance   string
	Generation uint64
	Revision   *uint64
	Key        string
}

func (c *Client) Call(ctx context.Context, method string, params, result any, expected *Preconditions) (callErr error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case c.lane <- struct{}{}:
		defer func() { <-c.lane }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if c.connection == nil {
		dialer := net.Dialer{}
		connection, err := dialer.DialContext(ctx, "unix", c.Path)
		if err != nil {
			return fmt.Errorf("native player dial: %w", err)
		}
		if err := validatePeer(connection); err != nil {
			_ = connection.Close()
			return err
		}
		c.connection = connection
	}
	connection := c.connection
	defer func() {
		if callErr != nil {
			_ = connection.Close()
			c.connection = nil
			c.instance = ""
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = connection.Close(); close(closed) })
	defer func() {
		// Wait for a cancellation callback already running before releasing the
		// lane; it must not close a connection after a later call has begun.
		if !stop() {
			<-closed
			c.connection = nil
			c.instance = ""
		}
	}()
	if c.instance == "" {
		var hello Hello
		if err := exchange(connection, Request{Version: Version, ID: uuid.NewString(), Method: "hello", Params: map[string]any{"client": c.Name, "versions": []int{Version}}}, &hello); err != nil {
			return fmt.Errorf("native player negotiation: %w", err)
		}
		if hello.Version != Version || hello.Instance == "" || hello.Instance != hello.Snapshot.Instance || hello.Snapshot.Generation == 0 {
			return errors.New("native player returned inconsistent negotiation")
		}
		c.instance, c.generation = hello.Instance, hello.Snapshot.Generation
		c.capabilities = hello.Capabilities
	}
	if method == "overlay.set" {
		if p, ok := params.(map[string]any); ok && p["owner"] == "setup" && !slices.Contains(c.capabilities, "overlay.setup.v1") {
			return &RemoteError{Code: "unsupported", Message: "native player lacks overlay.setup.v1"}
		}
	}
	if params == nil {
		params = map[string]any{}
	}
	request := Request{Version: Version, ID: uuid.NewString(), Method: method, Params: params, DeadlineMS: deadline.UnixMilli()}
	if !readOnly(method) {
		request.Instance = c.instance
		generation := c.generation
		request.Generation = &generation
		request.Key = uuid.NewString()
		if expected != nil {
			request.Instance, request.Generation, request.Revision = expected.Instance, &expected.Generation, expected.Revision
			if expected.Key != "" {
				request.Key = expected.Key
			}
		}
	}
	var raw json.RawMessage
	if err := exchange(connection, request, &raw); err != nil {
		return err
	}
	// A successful snapshot or mutation receipt advances the observed fence.
	// Other clients can still replace playback: stale_generation is surfaced to
	// the caller, never hidden by an automatic re-read and replay.
	var stamp struct {
		Instance   string  `json:"instance"`
		Generation *uint64 `json:"generation"`
	}
	if err := json.Unmarshal(raw, &stamp); err != nil {
		return err
	}
	if stamp.Instance != "" && stamp.Instance != c.instance {
		return errors.New("native player identity changed on a live connection")
	}
	if stamp.Generation != nil {
		if *stamp.Generation == 0 {
			return errors.New("native player returned an invalid generation")
		}
		c.generation = *stamp.Generation
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(raw, result)
}

// Close releases any held input and closes the connection after in-flight work.
func (c *Client) Close() error {
	c.lane <- struct{}{}
	defer func() { <-c.lane }()
	if c.connection == nil {
		return nil
	}
	err := c.connection.Close()
	c.connection = nil
	c.instance = ""
	return err
}

func readOnly(method string) bool {
	switch method {
	case "state.get", "health.get", "artworkPermissions.get", "policy.get", "history.list", "history.resolve":
		return true
	default:
		return false
	}
}

func exchange(connection net.Conn, request Request, result any) error {
	if err := WriteFrame(connection, request); err != nil {
		return fmt.Errorf("native player write: %w", err)
	}
	var response Response
	if err := ReadFrame(connection, &response); err != nil {
		return fmt.Errorf("native player read (outcome may be unknown): %w", err)
	}
	if response.Version != Version || response.ID != request.ID || (response.Error == nil) == (len(response.Result) == 0) {
		return errors.New("native player response envelope mismatch")
	}
	if response.Error != nil {
		return response.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}
