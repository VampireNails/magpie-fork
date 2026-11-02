//go:build !linux

package gui

import (
	"errors"
	"net"
)

func listenProtectedUnix(string) (net.Listener, error) {
	return nil, errors.New("the protected remote management listener requires Linux")
}
