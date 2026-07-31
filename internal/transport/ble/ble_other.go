//go:build !darwin

// This build has no BLE transport.
//
// The mesh, crypto and storage layers are platform independent and their tests
// run anywhere, which is the point of keeping the radio behind an interface.
// Only the radio is missing here.
//
// Linux is the natural next port and the cheaper one: BlueZ supports both the
// central and peripheral roles, and tinygo.org/x/bluetooth exposes both there,
// so it needs none of the cbgo-level work macOS required.
package ble

import (
	"context"
	"log/slog"

	"github.com/notshekhar/yap/internal/transport"
)

// Transport satisfies the transport interface so that callers compile and
// link on every platform. Every method refuses, because there is no radio.
type Transport struct{}

var _ transport.Transport = (*Transport)(nil)

// New reports that this platform has no radio.
func New(name string, log *slog.Logger) (*Transport, error) {
	return nil, ErrUnsupported
}

func (t *Transport) Start(ctx context.Context) error     { return ErrUnsupported }
func (t *Transport) Send(transport.LinkID, []byte) error { return ErrUnsupported }
func (t *Transport) Broadcast([]byte) error              { return ErrUnsupported }
func (t *Transport) Links() []transport.Link             { return nil }
func (t *Transport) Events() <-chan transport.Event      { return nil }
func (t *Transport) Close() error                        { return nil }
