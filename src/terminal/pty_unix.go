//go:build !windows

package terminal

import (
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// unixShell is the Unix98 pty backing a device terminal: bash on the slave
// end, the master handed to the shared read/write loops.
type unixShell struct {
	ptmx *os.File
	cmd  *exec.Cmd

	waitOnce  sync.Once
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

func startHostShell() (hostShell, error) {
	c := hostShellCommand()
	// PATH is forwarded explicitly because c.Env is non-nil below, which
	// otherwise hands bash an empty environment and leaves it on the
	// compiled-in confstr default.
	c.Env = append(c.Env, "PATH="+os.Getenv("PATH"))
	c.Env = append(c.Env, "TERM=xterm")

	ptmx, err := pty.Start(c)
	if err != nil {
		return nil, err
	}
	resetPriority(c.Process.Pid)

	return &unixShell{ptmx: ptmx, cmd: c}, nil
}

func (s *unixShell) Read(p []byte) (int, error) {
	return s.ptmx.Read(p)
}

func (s *unixShell) Write(p []byte) (int, error) {
	return s.ptmx.Write(p)
}

func (s *unixShell) Resize(rows, cols uint16) error {
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Wait blocks until bash exits and reaps it. Guarded by a Once because Close
// waits too, and os/exec rejects a second Wait.
func (s *unixShell) Wait() error {
	s.waitOnce.Do(func() {
		s.waitErr = s.cmd.Wait()
	})

	return s.waitErr
}

// Close kills bash and reaps it. Closing the master alone is not enough: bash
// only notices the hangup the next time it touches the tty, so a device whose
// user closes the terminal tab would otherwise accumulate idle shells.
func (s *unixShell) Close() error {
	s.closeOnce.Do(func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
			_ = s.Wait()
		}

		s.closeErr = s.ptmx.Close()
	})

	return s.closeErr
}
