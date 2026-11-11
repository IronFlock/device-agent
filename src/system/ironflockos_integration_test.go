//go:build integration

package system

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The real RAUC on the system bus (an IronFlockOS device, or a container running
// rauc service): its slots decode, and on IronFlockOS the booted slot is marked
// good.
func TestIronFlockOSSlotsAgainstRAUC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	booted, primary, slots, err := IronFlockOSSlots(ctx)
	if err != nil {
		t.Skipf("no RAUC to ask: %v", err)
	}
	t.Logf("booted %s, primary %s, slots %+v", booted, primary, slots)
	require.NotEmpty(t, slots)
	assert.NotEmpty(t, booted)
	assert.NotEmpty(t, primary)
	if RunsIronFlockOS() {
		require.NoError(t, MarkIronFlockOSSlotGood(ctx))
		_, _, after, err := IronFlockOSSlots(ctx)
		require.NoError(t, err)
		for _, s := range after {
			if s.Name == booted {
				assert.Equal(t, "good", s.BootStatus)
			}
		}
	}
}
