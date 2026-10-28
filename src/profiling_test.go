package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The pprof server must never listen on a network interface.
func TestProfilingServerListensOnLoopbackOnly(t *testing.T) {
	server := newProfilingServer(6060)

	require.Equal(t, "127.0.0.1:6060", server.Addr)
	require.NotZero(t, server.ReadHeaderTimeout)
	require.NotZero(t, server.ReadTimeout)
	require.NotZero(t, server.IdleTimeout)
	// pprof refuses a profile that does not fit in the WriteTimeout; the
	// default CPU profile takes 30 s.
	require.Greater(t, server.WriteTimeout.Seconds(), 30.0)
}
