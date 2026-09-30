package server

import "errors"

var (
	errConfig = errors.New("server: store and air endpoint required")
)
