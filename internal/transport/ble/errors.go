package ble

import "errors"

// ErrUnsupported means this build has no Bluetooth transport for the platform
// it is running on. Callers fall back to a local-only mode and say so rather
// than failing to start.
var ErrUnsupported = errors.New("ble: bluetooth is not implemented on this platform")
