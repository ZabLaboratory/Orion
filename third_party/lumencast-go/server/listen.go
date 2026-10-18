package server

import "net"

// netListen wraps net.Listen so the server package keeps a single
// import path for the standard library — convenient when swapping in
// a fake listener in tests via dependency injection.
func netListen(network, addr string) (net.Listener, error) {
	return net.Listen(network, addr)
}
