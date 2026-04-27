// Package push — TransportRouter dispatches by platform.
//
// The push subscriptions table carries a `device_type` column with
// one of "web" / "ios" / "android". TransportRouter holds a
// transport per platform and delegates Send to the matching one,
// falling back to the configured default (typically the logging
// transport in dev) when no platform-specific transport exists.
package push

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// TransportRouter is a Transport implementation that dispatches
// to a per-platform Transport.
type TransportRouter struct {
	IOS     Transport
	Android Transport
	Web     Transport
	// Default catches every subscription whose device_type does
	// not have a registered transport. Used by tests and dev.
	Default Transport
	Logger  *log.Logger
}

// NewTransportRouter is a small constructor for symmetry with the
// other transports. All fields are public so callers can
// initialise the struct directly when they prefer.
func NewTransportRouter(logger *log.Logger) *TransportRouter {
	if logger == nil {
		logger = log.Default()
	}
	return &TransportRouter{Logger: logger}
}

// Send picks the transport for sub.DeviceType and delegates. If no
// transport is configured for the platform and no Default is set,
// the call returns an error so callers can log / prune.
func (r *TransportRouter) Send(ctx context.Context, sub Subscription, n Notification) error {
	if r == nil {
		return errors.New("push: nil TransportRouter")
	}
	t := r.transportFor(sub.DeviceType)
	if t == nil {
		return fmt.Errorf("push: no transport configured for device_type=%q", sub.DeviceType)
	}
	return t.Send(ctx, sub, n)
}

// transportFor resolves the Transport for a device type, falling
// through to Default when no platform-specific transport is set.
func (r *TransportRouter) transportFor(deviceType string) Transport {
	switch deviceType {
	case "ios":
		if r.IOS != nil {
			return r.IOS
		}
	case "android":
		if r.Android != nil {
			return r.Android
		}
	case "web":
		if r.Web != nil {
			return r.Web
		}
	}
	return r.Default
}
