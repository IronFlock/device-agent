//go:build windows

package terminal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// The pseudoconsole is created at the classic console default and resized as
// soon as the frontend reports its real geometry, which it does immediately
// after the init call returns.
const (
	defaultCols = 80
	defaultRows = 25
)

// powershellPreamble puts the pseudoconsole on UTF-8 before the first
// interactive prompt is drawn.
//
// A ConPTY carries UTF-8 on the wire but does not set the child's console code
// page: PowerShell inherits the machine's OEM page (437/850/1252/…) and every
// non-ASCII character reaches the browser as the wrong glyph. There is no
// ConPTY-level knob for this — the code page can only be set from inside the
// child — and it has to happen before PSReadLine's first ReadLine, because
// PSReadLine latches the code page it starts with and restores it around every
// external command, so a late fix leaves interactive text and command output
// on different encodings.
//
// [Console]::OutputEncoding is SetConsoleOutputCP by another name, but without
// chcp's screen-clearing side effect. $OutputEncoding is a separate setting
// that only governs what PowerShell pipes *into* native executables; the
// BOM-less UTF8Encoding matters there, since that path really does emit the
// preamble. The console input code page is deliberately left alone —
// PowerShell reads through ReadConsoleW and is unaffected by it, while setting
// it to 65001 trips a long-standing conhost bug that turns non-ASCII input
// into NUL bytes for native console programs.
const powershellPreamble = `[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); $OutputEncoding = [Console]::OutputEncoding`

// The generated wrappers in x/sys reach the pseudoconsole entry points through
// LazyProc.Addr(), which panics rather than erroring when the symbol is
// missing. Resolve them ourselves first so a pre-1809 device gets a sentence it
// can act on instead of a stack trace.
var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole     = kernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole     = kernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole      = kernel32.NewProc("ClosePseudoConsole")
	errPseudoConsoleUnsupported = errors.New("the device terminal requires the Windows pseudoconsole API, which needs Windows 10 version 1809 (build 17763) or newer")
)

func pseudoConsoleAvailable() bool {
	return procCreatePseudoConsole.Find() == nil &&
		procResizePseudoConsole.Find() == nil &&
		procClosePseudoConsole.Find() == nil
}

// windowsShell is the ConPTY backing a device terminal: PowerShell attached to
// a pseudoconsole, with the two anonymous pipes that carry its input and output
// handed to the shared read/write loops.
type windowsShell struct {
	console windows.Handle
	in      *os.File
	out     *os.File

	// mu guards console against a resize racing teardown — resizing a closed
	// pseudoconsole is a use-after-free, not an error return.
	mu     sync.RWMutex
	closed bool

	// procMu guards process, which Wait zeroes once it has reaped the shell.
	// Close terminates through the same lock so it can never hand
	// TerminateProcess a handle Wait has just closed.
	procMu  sync.Mutex
	process windows.Handle

	waitOnce  sync.Once
	waitErr   error
	closeOnce sync.Once
	closeErr  error
}

func startHostShell() (hostShell, error) {
	if !pseudoConsoleAvailable() {
		return nil, errPseudoConsoleUnsupported
	}

	commandLine, err := shellCommandLine()
	if err != nil {
		return nil, err
	}

	// Two anonymous pipes, crossed over: the pseudoconsole reads the input
	// pipe and writes the output pipe, we hold the other end of each.
	ptyIn, shellIn, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("terminal: failed to create the input pipe: %w", err)
	}

	shellOut, ptyOut, err := os.Pipe()
	if err != nil {
		_ = ptyIn.Close()
		_ = shellIn.Close()
		return nil, fmt.Errorf("terminal: failed to create the output pipe: %w", err)
	}

	// os.Pipe hands back inheritable handles on Windows, because os/exec needs
	// them that way. The ends we keep must not be: a child that inherited the
	// output pipe's write end would hold it open after the shell died, and the
	// read loop would park on a pipe that can never break. Go's own
	// StartProcess restricts inheritance to an explicit handle list, but
	// nothing guarantees every future spawn path does.
	windows.CloseOnExec(windows.Handle(shellIn.Fd()))
	windows.CloseOnExec(windows.Handle(shellOut.Fd()))

	var console windows.Handle
	size := windows.Coord{X: defaultCols, Y: defaultRows}
	createErr := windows.CreatePseudoConsole(size, windows.Handle(ptyIn.Fd()), windows.Handle(ptyOut.Fd()), 0, &console)

	// The pseudoconsole keeps its own duplicates, and our copies have to go
	// either way. Dropping the output pipe's write end is what makes the shell
	// exiting observable at all: while this process still holds it, the pipe
	// can never break and the read loop would park on it forever.
	_ = ptyIn.Close()
	_ = ptyOut.Close()

	if createErr != nil {
		_ = shellIn.Close()
		_ = shellOut.Close()
		return nil, fmt.Errorf("terminal: failed to create the pseudoconsole: %w", createErr)
	}

	process, err := startAttachedProcess(commandLine, console)
	if err != nil {
		// Same ordering as Close: our end of the output pipe goes first, so
		// ClosePseudoConsole can never be left waiting on a pipe nobody reads.
		_ = shellIn.Close()
		_ = shellOut.Close()
		windows.ClosePseudoConsole(console)
		return nil, err
	}

	return &windowsShell{console: console, in: shellIn, out: shellOut, process: process}, nil
}

// startAttachedProcess launches the shell attached to the pseudoconsole and
// returns its process handle. Ownership of that handle passes to Wait.
func startAttachedProcess(commandLine string, console windows.Handle) (windows.Handle, error) {
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, fmt.Errorf("terminal: failed to allocate the process attribute list: %w", err)
	}

	// The list only has to outlive CreateProcess.
	defer attributes.Delete()

	// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE takes the HPCON by value, not a
	// pointer to it, which is why the handle itself is the attribute value.
	// `go vet` flags the conversion as a possible misuse of unsafe.Pointer;
	// it is what the Win32 API asks for, and both reference implementations
	// (creack-adjacent go-pty and UserExistsError/conpty) do the same.
	err = attributes.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(console), unsafe.Sizeof(console)) //nolint:gosec,govet // G103/unsafeptr: the Win32 pseudoconsole API takes the handle by value (see comment above)
	if err != nil {
		return 0, fmt.Errorf("terminal: failed to attach the pseudoconsole to the shell: %w", err)
	}

	commandLinePtr, err := windows.UTF16PtrFromString(commandLine)
	if err != nil {
		return 0, fmt.Errorf("terminal: failed to encode the shell command line: %w", err)
	}

	var startupInfo windows.StartupInfoEx
	startupInfo.ProcThreadAttributeList = attributes.List()
	startupInfo.Cb = uint32(unsafe.Sizeof(startupInfo))

	// STARTF_USESTDHANDLES with all three handles left NULL. This looks
	// pointless and is not in Microsoft's pseudoconsole sample, but without it
	// the shell is silent and deaf when the agent runs as a service.
	//
	// Windows has an old compatibility hack: when the target is a console
	// application, handle inheritance is off, STARTF_USESTDHANDLES is absent,
	// and the creation flags name no console disposition, the kernel
	// *duplicates* the parent's standard handles into the child — not through
	// inheritance, so bInheritHandles=false does not prevent it. The OS undoes
	// this for a pseudoconsole child only when the duplicated handles are
	// console handles (ConsoleCloseIfConsoleHandle). A service has no console:
	// its handles are NULL or point at a log file, they survive the cleanup,
	// and the shell then reads and writes those instead of the pseudoconsole.
	// The terminal comes up showing nothing and swallowing every keystroke.
	//
	// Setting the flag with NULL handles is the documented remedy from the
	// Windows Terminal maintainers (microsoft/terminal#11276): it stops the
	// duplication, and the pseudoconsole attribute above supplies the child's
	// real handles.
	startupInfo.Flags |= windows.STARTF_USESTDHANDLES

	var processInfo windows.ProcessInformation

	// The environment is inherited (env == nil): a terminal that does not see
	// the device's own PATH is close to useless. Handle inheritance stays off —
	// the shell reaches its console through the attribute above, not through
	// inherited handles, and the ends of the pipes this process kept must not
	// leak into it.
	err = windows.CreateProcess(
		nil,
		commandLinePtr,
		nil,
		nil,
		false,
		windows.EXTENDED_STARTUPINFO_PRESENT,
		nil,
		nil,
		&startupInfo.StartupInfo,
		&processInfo,
	)
	if err != nil {
		return 0, fmt.Errorf("terminal: failed to start the shell: %w", err)
	}

	_ = windows.CloseHandle(processInfo.Thread)

	// The shell being alive is not the same as the shell being reachable, and
	// the two failure modes look identical from the browser: a cursor and
	// nothing else. Record what was launched so the next report can be
	// diagnosed from the agent log instead of from first principles.
	log.Info().Msgf("terminal: started the device shell (pid %d): %s", processInfo.ProcessId, commandLine)

	return processInfo.Process, nil
}

// shellCommandLine picks the best shell present on the device and returns a
// fully quoted command line for it. PowerShell 7 wins when it is installed,
// Windows PowerShell is the universal fallback, and cmd.exe is the last resort
// on an install where neither resolves.
func shellCommandLine() (string, error) {
	if pwsh, err := exec.LookPath("pwsh.exe"); err == nil {
		return powershellCommandLine(pwsh), nil
	}

	if powershell, err := exec.LookPath("powershell.exe"); err == nil {
		return powershellCommandLine(powershell), nil
	}

	// A service's PATH is whatever it was when the service control manager
	// started it, which is not always the machine PATH, so fall back to the
	// fixed location Windows PowerShell has always shipped in.
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}

	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(powershell); err == nil { //nolint:gosec // G703: path of the system PowerShell
		return powershellCommandLine(powershell), nil
	}

	if cmd, err := exec.LookPath("cmd.exe"); err == nil {
		return windows.ComposeCommandLine([]string{cmd}), nil
	}

	return "", errors.New("no usable shell was found on this device (looked for pwsh.exe, powershell.exe and cmd.exe)")
}

func powershellCommandLine(path string) string {
	return windows.ComposeCommandLine([]string{path, "-NoLogo", "-NoExit", "-Command", powershellPreamble})
}

func (s *windowsShell) Read(p []byte) (int, error) {
	return s.out.Read(p)
}

func (s *windowsShell) Write(p []byte) (int, error) {
	return s.in.Write(p)
}

func (s *windowsShell) Resize(rows, cols uint16) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return errors.New("the terminal has already been closed")
	}

	// The caller has clamped both dimensions to 1..32767, so neither cast can
	// wrap into a negative COORD.
	return windows.ResizePseudoConsole(s.console, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Wait blocks until the shell exits, then releases the process handle. Closing
// that handle here rather than in Close keeps a concurrent teardown from
// pulling it out from under WaitForSingleObject.
func (s *windowsShell) Wait() error {
	s.waitOnce.Do(func() {
		s.procMu.Lock()
		process := s.process
		s.procMu.Unlock()

		defer s.releaseProcess()

		if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
			s.waitErr = err
			return
		}

		var exitCode uint32
		if err := windows.GetExitCodeProcess(process, &exitCode); err != nil {
			s.waitErr = err
			return
		}

		if exitCode != 0 {
			s.waitErr = fmt.Errorf("the shell exited with code %d", exitCode)
		}
	})

	return s.waitErr
}

// terminate kills the shell if it is still running. Safe against a concurrent
// Wait: the handle is only closed by releaseProcess, under the same lock.
func (s *windowsShell) terminate() {
	s.procMu.Lock()
	defer s.procMu.Unlock()

	if s.process != 0 {
		_ = windows.TerminateProcess(s.process, 1)
	}
}

func (s *windowsShell) releaseProcess() {
	s.procMu.Lock()
	defer s.procMu.Unlock()

	if s.process != 0 {
		_ = windows.CloseHandle(s.process)
		s.process = 0
	}
}

// Close terminates the shell and tears the pseudoconsole down.
//
// The order is load-bearing. Microsoft's contract for ClosePseudoConsole is
// that the caller must "either close the output pipe before calling
// ClosePseudoConsole or continue reading from the pipe until after
// ClosePseudoConsole has returned" — "failure to either close or drain the
// output pipe may cause ClosePseudoConsole to wait indefinitely" on every build
// before Windows 11 24H2, which is to say on every device this feature targets.
// On teardown conhost flushes its remaining VT into a 4 KiB anonymous pipe; if
// nothing is reading it, conhost parks in WriteFile, and the pre-24H2
// ClosePseudoConsole waits on conhost forever. Since we cannot promise a reader
// is still there — the shared read loop may already have unwound — we take the
// other option and close our end first, which breaks that write instead of
// waiting on it. Go's pipe Close also issues a CancelIoEx, so this is what
// unblocks a Read parked in the read loop.
func (s *windowsShell) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		// ClosePseudoConsole is documented to take the attached client down
		// with it, but an orphaned SYSTEM PowerShell is not a good thing to
		// bet on documentation for.
		s.terminate()

		s.closeErr = errors.Join(s.in.Close(), s.out.Close())

		windows.ClosePseudoConsole(s.console)
	})

	return s.closeErr
}
