package main

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAcquireFrpcWithinReturnsTheResult(t *testing.T) {
	require.NoError(t, acquireFrpcWithin(func() error { return nil }, time.Second))

	failed := errors.New("signature does not verify")
	require.ErrorIs(t, acquireFrpcWithin(func() error { return failed }, time.Second), failed)
}

// A stalled frpc download must not keep the tunnel client from starting.
func TestAcquireFrpcWithinGivesUpOnAStalledAcquisition(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	err := acquireFrpcWithin(func() error { <-release; return nil }, 50*time.Millisecond)

	require.ErrorContains(t, err, "not acquired within")
	require.Less(t, time.Since(start), 2*time.Second)
}
