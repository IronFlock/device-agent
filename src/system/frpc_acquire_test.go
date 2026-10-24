package system

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"reagent/embedded"
	"reagent/filesystem"
	"reagent/testutil/builders"

	"github.com/stretchr/testify/require"
)

// At agent start the update check and the connect handler ensure frpc at the
// same moment, each with its own System. The second must wait for the first
// and then find frpc current: acquiring it a second time (or giving up on a
// download in progress) let the connect handler start the old frpc, and
// Windows then refused to replace the running frpc.exe.
func TestDownloadFrpIfNotExistsSerializesAcquisition(t *testing.T) {
	cfg := builders.DefaultTestConfig()
	cfg.CommandLineArguments.AgentDir = t.TempDir()
	frpcPath := filesystem.GetTunnelBinaryPath(cfg, "frpc")
	require.NoError(t, os.WriteFile(frpcPath, []byte("an older frpc"), 0o755)) //nolint:gosec // G306: stands in for an executable

	var mu sync.Mutex
	onDisk := "0.70.0"
	var versionReads, acquisitions atomic.Int32

	origVersion, origAcquire := frpcVersionOnDisk, acquireFrpc
	t.Cleanup(func() { frpcVersionOnDisk, acquireFrpc = origVersion, origAcquire })
	frpcVersionOnDisk = func(*System) (string, error) {
		versionReads.Add(1)
		mu.Lock()
		defer mu.Unlock()
		return onDisk, nil
	}
	acquireFrpc = func(*System, string) error {
		acquisitions.Add(1)
		// The download takes a while; the other caller arrives meanwhile.
		// Unserialized it reads the old version here too; serialized it
		// cannot, and this waits out the deadline.
		deadline := time.Now().Add(300 * time.Millisecond)
		for versionReads.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		onDisk = embedded.FRP_VERSION
		mu.Unlock()
		return nil
	}

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sys := New(cfg, nil)
			errs[i] = sys.DownloadFrpIfNotExists()
		}()
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.EqualValues(t, 1, acquisitions.Load(), "the second caller must wait and find frpc current, not acquire it again")
	require.EqualValues(t, 2, versionReads.Load())
}

// A current frpc on disk is left alone.
func TestDownloadFrpIfNotExistsSkipsCurrentFrpc(t *testing.T) {
	cfg := builders.DefaultTestConfig()
	cfg.CommandLineArguments.AgentDir = t.TempDir()
	require.NoError(t, os.WriteFile(filesystem.GetTunnelBinaryPath(cfg, "frpc"), []byte("frpc"), 0o755)) //nolint:gosec // G306: stands in for an executable

	origVersion, origAcquire := frpcVersionOnDisk, acquireFrpc
	t.Cleanup(func() { frpcVersionOnDisk, acquireFrpc = origVersion, origAcquire })
	frpcVersionOnDisk = func(*System) (string, error) { return embedded.FRP_VERSION, nil }
	acquireFrpc = func(*System, string) error {
		t.Fatal("frpc is current; nothing may be acquired")
		return nil
	}

	sys := New(cfg, nil)
	require.NoError(t, sys.DownloadFrpIfNotExists())
}
