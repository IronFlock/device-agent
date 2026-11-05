//go:build integration

package firmware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The real fwupd on the system bus (just test-integration on a device or in
// a container with fwupd running): the inventory decodes, whatever it holds.
func TestReadSystemFwupd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	inv, err := Read(ctx)
	if errors.Is(err, ErrUnavailable) {
		t.Skip(err)
	}
	require.NoError(t, err)
	require.NotEmpty(t, inv.FwupdVersion)
	out, _ := json.MarshalIndent(inv, "", "  ")
	t.Logf("%s", out)
}
