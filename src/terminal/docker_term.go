package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reagent/common"
	"reagent/container"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/safe"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

type TerminalSession struct {
	Session       *container.HijackedResponse
	ContainerName string
	Owner         string // the caller that asked for the session, see owner.go
	SessionID     string
	inputChan     chan string
	errorChan     chan error
	DataTopic     string
	WriteTopic    string
	ResizeTopic   string
	once          sync.Once

	// unwatchedSince is when the reaper found no one subscribed to the
	// output, first since it last found someone; zero while someone watches.
	// Only the reaper touches it.
	unwatchedSince time.Time
}

func (termSess *TerminalSession) Close() {
	termSess.once.Do(func() {
		close(termSess.inputChan)
		close(termSess.errorChan)
	})
}

func NewSession(containerName string, serialNumber string, owner string, hijackedResponse *container.HijackedResponse) *TerminalSession {
	session := TerminalSession{
		ContainerName: containerName,
		Owner:         owner,
		Session:       hijackedResponse,
	}

	return session.init(serialNumber)
}

func (termSess *TerminalSession) init(serialNumber string) *TerminalSession {
	sessionID := uuid.NewString()
	inChan := make(chan string, 5)
	errorChan := make(chan error, 2)

	// topic that can be called to write data
	writeTopic := common.BuildExternalApiTopic(serialNumber, fmt.Sprintf("term_write.%s.%s", termSess.ContainerName, sessionID))

	// topic that the data will be publish to
	dataTopic := common.BuildExternalApiTopic(serialNumber, fmt.Sprintf("term_data.%s.%s", termSess.ContainerName, sessionID))

	resizeTopic := common.BuildExternalApiTopic(serialNumber, fmt.Sprintf("term_resize.%s.%s", termSess.ContainerName, sessionID))

	termSess.DataTopic = dataTopic
	termSess.WriteTopic = writeTopic
	termSess.ResizeTopic = resizeTopic

	termSess.SessionID = sessionID
	termSess.inputChan = inChan
	termSess.errorChan = errorChan

	return termSess
}

type TerminalManager struct {
	Container      container.Container
	Messenger      messenger.Messenger
	ActiveSessions map[string]*TerminalSession
	mapMutex       *sync.Mutex

	// reaping is whether the reaper runs (see startReaperLocked). Guarded by
	// mapMutex.
	reaping bool
	// reapInterval is the reaper's period; zero means defaultReapInterval. A
	// test shortens it.
	reapInterval time.Duration
	// now is the reaper's clock; nil means time.Now. A test moves it on.
	now func() time.Time
	// matchRefusalLogged is whether the reaper has warned that the router
	// refuses to say who watches a terminal (see watched). Guarded by
	// mapMutex.
	matchRefusalLogged bool
}

var supportedShells = [...]string{"/bin/zsh", "/bin/bash"}
var defaultShell = "/bin/sh"
var shellRegex = regexp.MustCompile("\r?\n")

func (tm *TerminalManager) getShell(containerName string) (string, error) {
	execCommandContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	hijackedResponse, err := tm.Container.ExecCommand(execCommandContext, containerName, []string{"cat", "/etc/shells"})
	if err != nil {
		return "", err
	}

	defer func() { _ = hijackedResponse.Conn.Close() }()

	result, err := io.ReadAll(hijackedResponse.Reader)
	if err != nil {
		return "", err
	}

	etcShells := shellRegex.Split(string(result), -1)

	for _, foundShell := range etcShells {
		for _, supportedShell := range supportedShells {
			if supportedShell == foundShell {
				return supportedShell, nil
			}
		}
	}

	return defaultShell, nil
}

func (tm *TerminalManager) registerResizeTopic(termSess *TerminalSession) error {
	return tm.Messenger.Register(topics.Topic(termSess.ResizeTopic), func(ctx context.Context, invocation messenger.Result) (*messenger.InvokeResult, error) {
		if !calledByOwner(termSess.Owner, invocation) {
			return nil, errNotOwner
		}

		// Bounds-checked like pty.go: the WAMP client runs this handler on a
		// goroutine of its own with no recover, so an argument-less call
		// would take the whole agent down.
		if len(invocation.Arguments) == 0 {
			return nil, errors.New("failed to parse args, payload is missing")
		}

		payload, ok := invocation.Arguments[0].(map[string]interface{})
		if !ok {
			return nil, errors.New("failed to parse args")
		}

		heightKw := payload["height"]
		widthKw := payload["width"]

		height, ok := heightKw.(uint64)
		if !ok {
			return nil, errors.New("failed to parse height")
		}

		width, ok := widthKw.(uint64)
		if !ok {
			return nil, errors.New("failed to parse width")
		}

		err := tm.ResizeTerminal(termSess.SessionID, container.TtyDimension{Height: uint(height), Width: uint(width)})
		if err != nil {
			return nil, err
		}

		return &messenger.InvokeResult{}, nil
	}, nil)
}

func (tm *TerminalManager) subscribeWriteTopic(termSess *TerminalSession) error {
	return tm.Messenger.Subscribe(topics.Topic(termSess.WriteTopic), func(r messenger.Result) error {
		if !fromOwner(termSess.Owner, r.Details) {
			return errors.New("dropped keystrokes from a publisher that does not own the terminal")
		}

		// Bounds-checked like pty.go: the router's receive goroutine runs this
		// handler with no recover in the path, so an argument-less publish
		// would take the whole agent down.
		if len(r.Arguments) == 0 {
			return errors.New("failed to parse args, payload is missing")
		}

		data, ok := r.Arguments[0].(string)
		if !ok {
			return errors.New("failed to parse args")
		}

		termSess.inputChan <- data

		return nil
	}, nil)
}

func (tm *TerminalManager) initTerminalMessagingChannels(termSess *TerminalSession) error {
	// Register in channel (receives data from WAMP sends it to channel)

	err := tm.subscribeWriteTopic(termSess)
	if err != nil {
		return err
	}

	err = tm.registerResizeTopic(termSess)
	if err != nil {
		return err
	}

	// read incoming (WAMP) data from the channel and write it to the attached terminal
	safe.Go(func() {
		defer log.Debug().Msgf("term writer goroutine for %s has exited", termSess.ContainerName)

	exit:
		for {
			select {
			case incomingData, ok := <-termSess.inputChan: // will break if channel is closed
				if !ok {
					// cleanupSession closed it, and closes the connection
					// once it has hung the shell up (see hangUp).
					break exit
				}

				_, err := termSess.Session.Conn.Write([]byte(incomingData))
				if err != nil {
					_ = termSess.Session.Conn.Close() // will close both read and write
					termSess.errorChan <- err
					break exit
				}
			}
		}
	})

	// read outgoing data from the channel and 'publish' it (WAMP) to a given topic
	options, kwargs := outputAudience(termSess.Owner)
	safe.Go(func() {
		defer log.Debug().Msgf("term reader goroutine for %s has exited", termSess.ContainerName)
		buf := make([]byte, 32*1024)

	exit:
		for {
			nr, er := termSess.Session.Reader.Read(buf)
			if er != nil {
				err = er
				break exit
			}

			if nr > 0 {
				bytesToPublish := buf[0:nr]
				er := tm.Messenger.Publish(topics.Topic(termSess.DataTopic), []interface{}{bytesToPublish}, kwargs, options)
				if er != nil {
					err = er
					break exit
				}
			}
		}

		if err.Error() == "EOF" {
			tm.cleanupSession(termSess)
			return
		}

		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				termSess.errorChan <- err
			}
			return
		}
	})

	return nil
}

func (tm *TerminalManager) getSession(sessionID string) (*TerminalSession, error) {
	tm.mapMutex.Lock()
	aTSession := tm.ActiveSessions[sessionID]
	tm.mapMutex.Unlock()

	if aTSession == nil {
		return nil, errors.New("session was not found")
	}
	return aTSession, nil
}

func (tm *TerminalManager) ResizeTerminal(sessionID string, dimension container.TtyDimension) error {
	termSession, err := tm.getSession(sessionID)
	if err != nil {
		return err
	}

	resizeExecContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	return tm.Container.ResizeExecContainer(resizeExecContainerContext, termSession.Session.ExecID, dimension)
}

// cleanupSession ends the session: its shell, its topics and its entry. It
// ends a session once, whoever asks (the owner's stop, the reaper, the shell's
// exit) and however the router answers: a failed unregister used to keep the
// session, and every later sweep ended it again, with another end marker for
// the UI.
func (tm *TerminalManager) cleanupSession(session *TerminalSession) {
	if session == nil {
		return
	}

	// Taken out under the lock, so of two calls at once only one goes on.
	tm.mapMutex.Lock()
	active := tm.ActiveSessions[session.SessionID] == session
	if active {
		delete(tm.ActiveSessions, session.SessionID)
	}
	tm.mapMutex.Unlock()

	// has already been cleaned up
	if !active {
		return
	}

	// closes both reader and writer goroutine
	session.Close()

	// Ends the shell, of a session never started too (see hangUp).
	if session.Session != nil && session.Session.Conn != nil {
		hangUp(session.Session.Conn)
	}

	// Read off the session before the goroutine starts: this function nils
	// session on its way out, which used to lose the marker to a nil deref.
	dataTopic := topics.Topic(session.DataTopic)
	options, kwargs := outputAudience(session.Owner)
	safe.Go(func() {
		// is ok if this errors
		payload := []interface{}{[]byte("TERMINAL_EOF")}
		_ = tm.Messenger.Publish(dataTopic, payload, kwargs, options)
	})

	// The session has ended either way: a registration the router kept only
	// answers that the session is gone (see getSession).
	var failed []error

	_, ok := tm.Messenger.RegistrationID(topics.Topic(session.ResizeTopic))
	if ok {
		if err := tm.Messenger.Unregister(topics.Topic(session.ResizeTopic)); err != nil {
			failed = append(failed, err)
		}
	}

	_, ok = tm.Messenger.SubscriptionID(topics.Topic(session.WriteTopic))
	if ok {
		if err := tm.Messenger.Unsubscribe(topics.Topic(session.WriteTopic)); err != nil {
			failed = append(failed, err)
		}
	}

	if len(failed) > 0 {
		log.Warn().Err(errors.Join(failed...)).Msgf("terminal: ended the terminal session %s of %s, but could not drop its topics", session.SessionID, session.ContainerName)
	} else {
		log.Debug().Msgf("cleaned up terminal session for %s", session.ContainerName)
	}

	session = nil
}

// hangUpGrace bounds how long hangUp types into a shell, and hangUpRetry is
// how long it waits between two ^D.
const (
	hangUpGrace = 2 * time.Second
	hangUpRetry = 200 * time.Millisecond
)

// hangUp ends the shell on conn, the exec's attached pty, and closes conn.
// Closing it alone does not end the shell: the daemon keeps the pty open, and
// the shell waits on it for input as long as the container runs. So the agent
// types what a user leaving would: ^C for whatever runs in the foreground,
// then ^D, again every hangUpRetry for hangUpGrace: the pty drops what was
// typed along with a ^C, and a shell still taking its prompt back can miss
// one. The daemon takes the keys after the exec has ended too, so they tell
// nothing about it; they are harmless then. A program that ignores both (an
// editor, a pager) keeps its shell until the container stops. It runs on a
// goroutine of its own: a daemon that stopped reading must not hold up the
// cleanup.
func hangUp(conn net.Conn) {
	safe.Go(func() {
		defer func() { _ = conn.Close() }()

		deadline := time.Now().Add(hangUpGrace)
		_ = conn.SetWriteDeadline(deadline)

		if _, err := conn.Write([]byte{0x03}); err != nil {
			return
		}

		for time.Now().Add(hangUpRetry).Before(deadline) {
			time.Sleep(hangUpRetry)

			if _, err := conn.Write([]byte{0x04}); err != nil {
				return
			}
		}
	})
}

// StopTerminalSession ends the session for caller, its owner or 'system'
// relaying for it.
func (tm *TerminalManager) StopTerminalSession(sessionID string, caller string) error {
	session, err := tm.getSession(sessionID)
	if err != nil {
		return err
	}

	if !actsForOwner(session.Owner, caller) {
		return errNotOwner
	}

	tm.cleanupSession(session)

	return nil
}

// StartTerminalSession wires the session up for caller, its owner or 'system'
// relaying for it.
func (tm *TerminalManager) StartTerminalSession(sessionID string, caller string) error {
	session, err := tm.getSession(sessionID)
	if err != nil {
		return err
	}

	if !actsForOwner(session.Owner, caller) {
		return errNotOwner
	}

	err = tm.initTerminalMessagingChannels(session)
	if err != nil {
		return err
	}

	return nil
}

func (tm *TerminalManager) createTerminalSession(containerName string, shell string, owner string) (*TerminalSession, error) {
	execAttachContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	hijackedResponse, err := tm.Container.ExecAttach(execAttachContext, containerName, shell)
	if err != nil {
		return nil, err
	}

	serialNumber := tm.Messenger.GetConfig().ReswarmConfig.SerialNumber
	termSession := NewSession(containerName, serialNumber, owner, &hijackedResponse)

	tm.mapMutex.Lock()
	tm.ActiveSessions[termSession.SessionID] = termSession
	tm.startReaperLocked()
	tm.mapMutex.Unlock()

	return termSession, nil
}

// The reaper ends the sessions nobody watches. A UI stops its session when it
// lets go of the terminal, but not when it crashes, and older UIs never do:
// the shell would run as long as its container. Every reapInterval the agent
// asks the router whether anyone still subscribes to a session's output, and
// ends a session nobody has watched for unwatchedGrace. The grace is long: a
// laptop asleep, a Wi-Fi handover or a reconnect backing off keeps a UI away
// for minutes, and ending its shell meanwhile would end the user's foreground
// job with it. It also covers a UI that has asked for a session and not yet
// subscribed to it.
const (
	unwatchedGrace      = 10 * time.Minute
	watchedQueryTimeout = 10 * time.Second
)

// defaultReapInterval is the reaper's period unless the manager sets one. A
// variable, so a test can shorten it.
var defaultReapInterval = 30 * time.Second

// startReaperLocked starts the reaper unless it runs. The caller holds
// mapMutex, which the reaper stops under once no session is left, so a new
// session is either swept by it or starts the next one.
func (tm *TerminalManager) startReaperLocked() {
	if tm.reaping {
		return
	}
	tm.reaping = true

	interval := tm.reapInterval
	if interval == 0 {
		interval = defaultReapInterval
	}

	safe.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for range ticker.C {
			if !tm.sweep() {
				return
			}
		}
	})
}

// clock is the reaper's time.
func (tm *TerminalManager) clock() time.Time {
	if tm.now != nil {
		return tm.now()
	}
	return time.Now()
}

// sweep ends the sessions nobody has watched for unwatchedGrace, and reports
// whether any session is left to sweep.
func (tm *TerminalManager) sweep() bool {
	tm.mapMutex.Lock()
	sessions := make([]*TerminalSession, 0, len(tm.ActiveSessions))
	for _, session := range tm.ActiveSessions {
		sessions = append(sessions, session)
	}
	tm.mapMutex.Unlock()

	for _, session := range sessions {
		// Any answer but a clear "no one" starts the grace over.
		if tm.watched(session) {
			session.unwatchedSince = time.Time{}
			continue
		}

		now := tm.clock()
		if session.unwatchedSince.IsZero() {
			session.unwatchedSince = now
		}
		unwatched := now.Sub(session.unwatchedSince)
		if unwatched < unwatchedGrace {
			continue
		}

		log.Info().Msgf("terminal: nobody has watched the terminal session %s of %s for %s, ending it", session.SessionID, session.ContainerName, unwatched.Round(time.Second))
		tm.cleanupSession(session)
	}

	tm.mapMutex.Lock()
	defer tm.mapMutex.Unlock()

	if len(tm.ActiveSessions) == 0 {
		tm.reaping = false
		return false
	}

	return true
}

// matchRefusals are the router's answers to wamp.subscription.match that the
// next sweep gets again: its policy does not let the agent ask, or it does not
// know the procedure.
var matchRefusals = []string{"wamp.error.not_authorized", "wamp.error.no_such_procedure"}

// watched reports whether anyone subscribes to the session's output. It asks
// wamp.subscription.match, not lookup: on an appliance the cloudbridge
// forwards the output to the cloud through a prefix subscription, which only
// match finds. Anything but a clear "no one" counts as watched: an error, a
// timeout or an answer it cannot read must never end a shell in use.
func (tm *TerminalManager) watched(session *TerminalSession) bool {
	ctx, cancel := context.WithTimeout(context.Background(), watchedQueryTimeout)
	defer cancel()

	result, err := tm.Messenger.Call(ctx, topics.MetaProcMatchSubscription, []interface{}{session.DataTopic}, nil, nil, nil)
	if err != nil {
		tm.logWatchedError(session, err)
		return true
	}

	if len(result.Arguments) == 0 {
		return true
	}

	// A list of subscription ids: null (one router) or empty (a cluster) when
	// there are none.
	switch ids := result.Arguments[0].(type) {
	case nil:
		return false
	case []interface{}:
		return len(ids) > 0
	}

	return true
}

// logWatchedError logs why the router could not say who watches a session. A
// refusal comes back every sweep and leaves every session to its owner's stop:
// worth a warning, once, not a line per session every sweep. Anything else (a
// timeout, a cluster member that did not answer, a lost connection) passes,
// and is logged at debug.
func (tm *TerminalManager) logWatchedError(session *TerminalSession, err error) {
	refused := false
	for _, refusal := range matchRefusals {
		if strings.Contains(err.Error(), refusal) {
			refused = true
		}
	}

	if refused {
		tm.mapMutex.Lock()
		warn := !tm.matchRefusalLogged
		tm.matchRefusalLogged = true
		tm.mapMutex.Unlock()

		if warn {
			log.Warn().Err(err).Msg("terminal: the router refuses to say who watches a terminal, so terminal sessions nobody watches are not ended")
			return
		}
	}

	log.Debug().Err(err).Msgf("terminal: could not ask who watches the terminal session %s", session.SessionID)
}

func NewTerminalManager(messenger messenger.Messenger, container container.Container) TerminalManager {
	sessionsMap := make(map[string]*TerminalSession)

	manager := TerminalManager{
		ActiveSessions: sessionsMap,
		Messenger:      messenger,
		Container:      container,
		mapMutex:       &sync.Mutex{},
	}

	return manager
}

func (tm *TerminalManager) SetMessenger(messenger messenger.Messenger) {
	tm.Messenger = messenger
}

// RequestTerminalSession opens a shell in the container for owner, the caller
// that asked for it.
func (tm *TerminalManager) RequestTerminalSession(containerName string, owner string) (*TerminalSession, error) {
	shell, err := tm.getShell(containerName)
	if err != nil {
		return nil, err
	}

	termSess, err := tm.createTerminalSession(containerName, shell, owner)
	if err != nil {
		return nil, err
	}

	return termSess, nil
}
