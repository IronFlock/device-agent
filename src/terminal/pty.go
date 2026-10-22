package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"reagent/common"
	"reagent/config"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/safe"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

type TerminalSize struct {
	Rows uint16
	Cols uint16
}

// hostShell is the platform-specific half of a device terminal: a byte stream
// wired to a live interactive shell, plus resize, exit notification and
// teardown. Unix backs it with a Unix98 pty (creack/pty) running bash; Windows
// backs it with a ConPTY running PowerShell. Everything above this interface —
// the WAMP topics, the channel plumbing, the reconnect re-registration — is
// shared.
type hostShell interface {
	io.ReadWriteCloser

	// Resize resizes the shell's viewport. Callers have already clamped the
	// dimensions to something the platform accepts.
	Resize(rows, cols uint16) error

	// Wait blocks until the shell process exits. Read alone is not a reliable
	// exit signal — a ConPTY does not necessarily break its output pipe when
	// its client goes away — so the terminal is torn down on whichever of the
	// two fires first.
	Wait() error

	// Close terminates the shell and releases its handles. It is idempotent
	// and unblocks a Read parked in another goroutine.
	Close() error
}

type PseudoTerminal struct {
	Id          string
	shell       hostShell
	graces      terminalGraces
	cleanupOnce sync.Once
	readDone    chan struct{}
	Input       chan string
	Output      chan string
	Error       chan error
	Resize      chan TerminalSize
	Cleanup     chan struct{}
	WriteTopic  string
	DataTopic   string
	ResizeTopic string
	SessionID   string
}

var pseudoTerminals = make(map[string]*PseudoTerminal)

// pseudoTerminalsMu guards pseudoTerminals: terminals are created on init calls
// and nil'd on cleanup, while ReregisterControlTopics iterates the map from the
// agent's reconnect handler — concurrent access would otherwise race/panic.
var pseudoTerminalsMu sync.RWMutex

func GetPseudoTerminal(id string) *PseudoTerminal {
	pseudoTerminalsMu.RLock()
	defer pseudoTerminalsMu.RUnlock()
	return pseudoTerminals[id]
}

func (pT *PseudoTerminal) Setup(config *config.Config, session messenger.Messenger) common.Dict {
	sessionID := uuid.NewString()

	writeTopic := common.BuildExternalApiTopic(config.ReswarmConfig.SerialNumber, fmt.Sprintf("term_write.%s", sessionID))
	dataTopic := common.BuildExternalApiTopic(config.ReswarmConfig.SerialNumber, fmt.Sprintf("data.%s", sessionID))
	resizeTopic := common.BuildExternalApiTopic(config.ReswarmConfig.SerialNumber, fmt.Sprintf("term_resize.%s", sessionID))

	pT.SessionID = sessionID
	pT.WriteTopic = writeTopic
	pT.ResizeTopic = resizeTopic
	pT.DataTopic = dataTopic

	// Establish the write-subscription and resize-registration. Split into
	// registerControlTopics so the agent can re-run it on every reconnect
	// (see ReregisterControlTopics): the router drops these per-terminal
	// registrations on a disconnect and the WampSession has no dynamic-reg
	// replay, so an already-open terminal would otherwise silently lose
	// input/resize (output still flows on the live session) until reagent
	// restarts.
	pT.registerControlTopics(session)

	options, kwargs := outputAudience(pT.Id)

	safe.Go(func() {
		for output := range pT.Output {
			err := session.Publish(topics.Topic(dataTopic), []interface{}{output}, kwargs, options)
			if err != nil {
				fmt.Println(err.Error())
			}
		}

	})

	safe.Go(func() {
		<-pT.Cleanup

		// Close the shell from its own goroutine. Tearing a pty down can be
		// slow — a ConPTY has to wind conhost down, and on Windows builds
		// before 11 24H2 that call has no timeout — and none of the steps
		// below should be hostage to it. Nothing here needs Close to have
		// finished: it is what unblocks a read parked in the shell, and the
		// read loop's other exit is Cleanup, which is already closed. So the
		// read loop always returns, and waiting for it is safe.
		safe.Go(func() {
			if err := pT.shell.Close(); err != nil {
				log.Debug().Err(err).Msgf("terminal: closing the shell for %s returned an error", pT.SessionID)
			}
		})

		// Only the read loop sends on Output, so once it has returned this
		// goroutine owns the channel outright. Closing the shell is what
		// unblocks a read parked in it, so in practice this returns at once —
		// but the bound means a backend that fails to unblock its reader costs
		// two leaked goroutines rather than a terminal nobody can ever reopen.
		readFinished := true

		select {
		case <-pT.readDone:
		case <-time.After(pT.graces.readDrain):
			readFinished = false
			log.Warn().Msgf("terminal: the read loop for %s did not return; leaving its channel open", pT.SessionID)
		}

		// Publishes are acknowledged round-trips, so this can park for as long
		// as the router takes to answer. Bound it: the frontend missing its
		// end-of-session marker is a nuisance, a terminal that never leaves the
		// cache is a device that can never open one again.
		select {
		case pT.Output <- "TERMINAL_EOF":
		case <-time.After(pT.graces.eofPublish):
			log.Warn().Msgf("terminal: gave up publishing TERMINAL_EOF for %s", pT.SessionID)
		}

		_ = session.Unsubscribe(topics.Topic(writeTopic))
		_ = session.Unregister(topics.Topic(resizeTopic))

		if readFinished {
			close(pT.Output)
		}

		// Input and Resize are deliberately left open: their senders are WAMP
		// handlers that can still be in flight here, and closing a channel out
		// from under a blocked send panics in the router's goroutine. Both
		// senders and both consumers select on Cleanup instead, so they unwind
		// on their own and the channels are collected with the terminal.

		sessionID := pT.SessionID
		pseudoTerminalsMu.Lock()
		// Only evict ourselves. A slow teardown can outlive its own session,
		// and the caller may already have a fresh terminal registered under
		// this id — clearing that one would strand a live shell.
		if pseudoTerminals[pT.Id] == pT {
			pseudoTerminals[pT.Id] = nil
		}
		pseudoTerminalsMu.Unlock()

		log.Debug().Msgf("Cleaned up the pty with ID %s", sessionID)
	})

	return common.Dict{
		"sessionID":   sessionID,
		"writeTopic":  writeTopic,
		"dataTopic":   dataTopic,
		"resizeTopic": resizeTopic,
	}
}

// signalCleanup asks the cleanup goroutine to tear this terminal down. It is
// idempotent: the shell exiting, the read loop erroring out and an explicit
// teardown all race to get here, and Cleanup is a one-shot channel.
func (pT *PseudoTerminal) signalCleanup() {
	pT.cleanupOnce.Do(func() {
		close(pT.Cleanup)
	})
}

// registerControlTopics (re)establishes this terminal's write-subscription and
// resize-registration against the given session. Safe to call again after a
// reconnect: Register uses force_reregister and the prior subscription is gone
// router-side after a disconnect. Errors are logged rather than discarded so a
// failed (re)registration is visible in the agent logs.
func (pT *PseudoTerminal) registerControlTopics(session messenger.Messenger) {
	err := session.Subscribe(topics.Topic(pT.WriteTopic), func(r messenger.Result) error {
		if !fromOwner(pT.Id, r.Details) {
			return errors.New("dropped keystrokes from a publisher that does not own the terminal")
		}

		// Bounds-checked: the router invokes these handlers on its own receive
		// goroutine with no recover in the path, so an argument-less publish to
		// the write topic would take the whole agent down with it.
		if len(r.Arguments) == 0 {
			return errors.New("failed to parse args, payload is missing")
		}

		data, ok := r.Arguments[0].(string)
		if !ok {
			return errors.New("failed to parse args")
		}

		select {
		case pT.Input <- data:
		case <-pT.Cleanup:
			return errors.New("the terminal session has ended")
		}

		return nil
	}, nil)
	if err != nil {
		log.Error().Err(err).Msgf("terminal: failed to subscribe write topic %s", pT.WriteTopic)
	}

	err = session.Register(topics.Topic(pT.ResizeTopic), func(ctx context.Context, invocation messenger.Result) (*messenger.InvokeResult, error) {
		if !calledByOwner(pT.Id, invocation) {
			return nil, errNotOwner
		}

		if len(invocation.Arguments) == 0 {
			return nil, errors.New("failed to parse args, payload is missing")
		}

		payload, ok := invocation.Arguments[0].(map[string]interface{})
		if !ok {
			return nil, errors.New("failed to parse args")
		}

		height, err := parseDimension(payload["height"])
		if err != nil {
			return nil, fmt.Errorf("failed to parse height: %w", err)
		}

		width, err := parseDimension(payload["width"])
		if err != nil {
			return nil, fmt.Errorf("failed to parse width: %w", err)
		}

		select {
		case pT.Resize <- TerminalSize{Cols: width, Rows: height}:
		case <-pT.Cleanup:
			return nil, errors.New("the terminal session has ended")
		}

		return &messenger.InvokeResult{}, nil
	}, nil)
	if err != nil {
		log.Error().Err(err).Msgf("terminal: failed to register resize topic %s", pT.ResizeTopic)
	}
}

// ReregisterControlTopics re-establishes the write-subscription and
// resize-registration for every live cached PseudoTerminal against the given
// (reconnected) session. The agent calls this from OnConnect after a reconnect:
// the router lost these per-terminal registrations on the disconnect and the
// WampSession has no dynamic-registration replay, so an already-open device
// terminal would otherwise silently lose input/resize until reagent restarts.
func ReregisterControlTopics(session messenger.Messenger) {
	pseudoTerminalsMu.RLock()
	terms := make([]*PseudoTerminal, 0, len(pseudoTerminals))
	for _, pT := range pseudoTerminals {
		if pT != nil {
			terms = append(terms, pT)
		}
	}
	pseudoTerminalsMu.RUnlock()

	for _, pT := range terms {
		log.Info().Msgf("terminal: re-registering control topics for caller %s (terminal %s) after reconnect", pT.Id, pT.SessionID)
		pT.registerControlTopics(session)
	}
}

// GetOrCreatePseudoTerminal returns the caller's live terminal, starting a shell
// for it if there is none. created reports whether a shell was started, so the
// caller knows whether it still owes the terminal a Setup.
//
// The check and the create are one atomic step because the router runs every
// invocation on its own goroutine: a user with two browser tabs would otherwise
// have both calls miss the cache and start a shell, and the one that lost the
// race to the registry would be orphaned — nothing left holding it, and on
// Windows that is a SYSTEM PowerShell nobody will ever close.
func GetOrCreatePseudoTerminal(id string) (pT *PseudoTerminal, created bool, err error) {
	creationMu.Lock()
	defer creationMu.Unlock()

	if existing := GetPseudoTerminal(id); existing != nil {
		return existing, false, nil
	}

	shell, err := startHostShell()
	if err != nil {
		return nil, false, err
	}

	return newPseudoTerminal(id, shell, defaultGraces()), true, nil
}

// creationMu serializes GetOrCreatePseudoTerminal. It is held across the shell
// launch, which is fine: opening a terminal is a rare, human-paced operation.
var creationMu sync.Mutex

// newPseudoTerminal wires the shared plumbing around an already-started shell.
// Split out from NewPseudoTerminal so the channel choreography — split runes,
// resize clamping, who closes what during teardown — is testable without a real
// pty on the machine running the tests.
func newPseudoTerminal(id string, shell hostShell, graces terminalGraces) *PseudoTerminal {
	pT := &PseudoTerminal{
		Id:       id,
		shell:    shell,
		graces:   graces,
		readDone: make(chan struct{}),
		Input:    make(chan string),
		Output:   make(chan string),
		Error:    make(chan error),
		Resize:   make(chan TerminalSize),
		Cleanup:  make(chan struct{}),
	}

	safe.Go(func() {
		for {
			select {
			case <-pT.Cleanup:
				return
			case size := <-pT.Resize:
				rows, cols, ok := clampTerminalSize(size)
				if !ok {
					// A zero dimension is not an error on either platform, it
					// is a silent no-op that strands the shell at its default
					// size — so say so rather than letting it vanish.
					log.Debug().Msgf("terminal: ignoring out-of-range resize %dx%d for %s", size.Cols, size.Rows, id)
					continue
				}

				if err := shell.Resize(rows, cols); err != nil {
					log.Debug().Err(err).Msgf("terminal: failed to resize %s to %dx%d", id, cols, rows)
				}
			}
		}
	})

	safe.Go(func() {
		for {
			select {
			case <-pT.Cleanup:
				return
			case input := <-pT.Input:
				if _, err := shell.Write([]byte(input)); err != nil {
					log.Debug().Err(err).Msgf("terminal: failed to write to the shell for %s", id)
				}
			}
		}
	})

	// The shell exiting is the normal way a session ends (the user types
	// `exit`). On Unix that also breaks the pty master, but a ConPTY does not
	// reliably close its output pipe when its client goes away, so waiting on
	// the process is what actually guarantees the frontend gets its
	// TERMINAL_EOF instead of a terminal that looks alive forever.
	safe.Go(func() {
		if err := shell.Wait(); err != nil {
			log.Debug().Err(err).Msgf("terminal: shell for %s exited", id)
		}

		// Tearing down immediately would close the pty out from under whatever
		// the shell printed on its way out ("logout", a farewell banner, the
		// tail of a long command). Give the read loop a moment to drain it —
		// or skip the wait entirely if it has already finished.
		select {
		case <-pT.readDone:
		case <-time.After(graces.exitDrain):
		}

		pT.signalCleanup()
	})

	safe.Go(func() {
		defer close(pT.readDone)

		buffer := make([]byte, 4096)

		// carry holds the bytes of a rune the shell split across two reads.
		// A full slice expression bounds its capacity so the append below
		// always allocates rather than writing back into the previous chunk.
		var carry [utf8.UTFMax]byte
		carryLen := 0

		for {
			n, err := shell.Read(buffer)

			if n > 0 {
				chunk := append(carry[:carryLen:carryLen], buffer[:n]...)

				complete, partial := splitTrailingPartialRune(chunk)
				carryLen = copy(carry[:], partial)

				if len(complete) > 0 {
					select {
					case pT.Output <- string(complete):
					case <-pT.Cleanup:
						return
					}
				}
			}

			if err != nil {
				// io.EOF means the shell exited normally; anything else means
				// the pty went away. Both end the session, and both must reach
				// the frontend as TERMINAL_EOF — returning without signalling
				// would leave a dead terminal cached under this caller id that
				// every later init call would hand back as if it were alive.
				if !errors.Is(err, io.EOF) {
					log.Debug().Err(err).Msgf("terminal: read loop for %s ended", id)
				}

				// Flush whatever was held back for a rune that will now never
				// be completed, so the last line is not silently truncated.
				if carryLen > 0 {
					select {
					case pT.Output <- string(carry[:carryLen]):
					case <-pT.Cleanup:
					}
				}

				pT.signalCleanup()
				return
			}
		}
	})

	pseudoTerminalsMu.Lock()
	pseudoTerminals[id] = pT
	pseudoTerminalsMu.Unlock()

	return pT
}

// parseDimension reads a terminal dimension off a WAMP payload. The serializer
// is negotiated (json/cbor/msgpack), and they do not agree on which Go integer
// type a number decodes to, so accept the lot instead of asserting uint64 and
// failing every resize on a session that happened to negotiate JSON.
func parseDimension(value interface{}) (uint16, error) {
	var n int64

	switch v := value.(type) {
	case uint64:
		n = int64(v)
	case int64:
		n = v
	case int:
		n = int64(v)
	case uint32:
		n = int64(v)
	case int32:
		n = int64(v)
	case float64:
		n = int64(v)
	default:
		return 0, fmt.Errorf("unexpected type %T", value)
	}

	if n < 0 || n > int64(maxTerminalDimension) {
		return 0, fmt.Errorf("out of range: %d", n)
	}

	return uint16(n), nil
}

// maxTerminalDimension is the largest dimension a ConPTY accepts: its COORD
// fields are signed 16-bit, and conhost rejects anything above SHRT_MAX.
const maxTerminalDimension = 32767

// exitDrainGrace is how long the read loop gets to pick up the shell's last
// bytes after the process has exited, before the pty is torn down under it.
//
// Generous on purpose. In the ordinary case the pty breaks the moment the shell
// is gone, the read loop returns, and the wait ends immediately — the grace only
// runs out when the pty holds its output pipe open with nothing left to say.
// Every publish is an acknowledged round-trip to the router, so a read loop on a
// device with a slow uplink is routinely parked for a few hundred milliseconds
// mid-session; a short grace would expire on that alone and cut the tail off
// every terminal that had just printed something.
const exitDrainGrace = 5 * time.Second

// eofPublishGrace bounds the last publish of a session. Losing the end-of-session
// marker is a nuisance; a teardown wedged behind an unanswered publish leaves the
// terminal cached forever and the device unable to open another one.
const eofPublishGrace = 5 * time.Second

// readDrainGrace bounds how long teardown waits for the read loop to return
// after the shell has been closed. Closing the shell is what unblocks it, so
// this only runs out if a backend fails to hold up its end.
const readDrainGrace = 5 * time.Second

// terminalGraces carries the teardown timeouts per terminal rather than as
// package state, so a test that drives one to expiry can shorten it without
// reaching into a global that another terminal's goroutines are still reading.
type terminalGraces struct {
	exitDrain  time.Duration
	readDrain  time.Duration
	eofPublish time.Duration
}

func defaultGraces() terminalGraces {
	return terminalGraces{
		exitDrain:  exitDrainGrace,
		readDrain:  readDrainGrace,
		eofPublish: eofPublishGrace,
	}
}

// clampTerminalSize rejects sizes no pty will honour. Windows silently drops a
// resize with a zero dimension and leaves the console at 80x25 with no error
// anywhere, so this is the only place a bad size gets noticed.
func clampTerminalSize(size TerminalSize) (rows, cols uint16, ok bool) {
	if size.Rows == 0 || size.Cols == 0 {
		return 0, 0, false
	}

	if size.Rows > maxTerminalDimension || size.Cols > maxTerminalDimension {
		return 0, 0, false
	}

	return size.Rows, size.Cols, true
}

// splitTrailingPartialRune splits b at the last complete UTF-8 rune boundary,
// returning the emittable prefix plus any trailing bytes of a rune that the
// shell's output happened to straddle across two reads.
//
// This matters because the chunk is published as a WAMP string: nexus
// serializes with ugorji/go/codec, whose JSON encoder rewrites every byte of an
// invalid UTF-8 sequence to a literal U+FFFD. A three-byte rune cut at a buffer
// boundary therefore arrives as three replacement characters and is
// unrecoverable browser-side. Holding the tail back until the next read costs
// nothing: a shell with more to say always sends it, and the fragment is
// flushed when the session ends.
func splitTrailingPartialRune(b []byte) (complete []byte, partial []byte) {
	for i := 0; i < utf8.UTFMax && i < len(b); i++ {
		idx := len(b) - 1 - i

		if !utf8.RuneStart(b[idx]) {
			continue
		}

		// b[idx] opens the final rune. Hold it back only if the rune it
		// declares is longer than the bytes we actually received.
		if runeLen(b[idx]) > i+1 {
			return b[:idx], b[idx:]
		}

		return b, nil
	}

	// No start byte in the last four bytes, so the tail is not UTF-8 at all.
	// Emit it rather than stalling the stream waiting for a completion that is
	// never coming.
	return b, nil
}

// runeLen returns the encoded length of the UTF-8 rune opened by c, or 1 for a
// byte that cannot start one — so malformed output flushes instead of
// accumulating in the carry buffer.
func runeLen(c byte) int {
	switch {
	case c&0x80 == 0x00:
		return 1
	case c&0xE0 == 0xC0:
		return 2
	case c&0xF0 == 0xE0:
		return 3
	case c&0xF8 == 0xF0:
		return 4
	}

	return 1
}
