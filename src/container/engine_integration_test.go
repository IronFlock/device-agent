//go:build integration

package container

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The engine on this host answers its platform and version.
func TestIntegrationEngineInfo(t *testing.T) {
	docker := newIntegrationDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := docker.EngineInfo(ctx)

	require.NoError(t, err)
	assert.Regexp(t, `^linux/(amd64|arm64|arm(/v[5-8])?)$`, info.Platform)
	assert.NotEmpty(t, info.Version)
	t.Logf("engine: %s, Docker %s", info.Platform, info.Version)
}
