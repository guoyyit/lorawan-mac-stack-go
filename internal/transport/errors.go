package transport

import "errors"

// ErrClosed is returned by Send on a closed endpoint.
var ErrClosed = errors.New("transport: endpoint closed")
