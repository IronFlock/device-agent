package logging

import (
	"bufio"
	"context"
	"errors"
	"io"
	"reagent/container"
	"reagent/errdefs"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// Windowed log queries answer "what did this app say around the time it broke?"
//
// They deliberately do NOT go through GetLogHistory. That path prefers the
// agent's own stores, and neither can answer a windowed question: the in-memory
// ring holds 100 bare strings with no timestamps (LogEntry has none), and the
// SQLite LogHistory table is a single JSON blob per app/stage with no timestamp
// column, written only when a log stream ends. Docker's json-file driver is the
// one store on the device that stamps every record, so a query goes there
// first, falls back to the compose CLI for multi-container apps, and only drops
// to the agent's history when the container is gone — flagging that the
// requested window was NOT applied rather than pretending it was.

const (
	// MaxQueryLines bounds the lines a single query may return, before the byte
	// cap trims further. Offset counts against it: paging back 4000 lines with a
	// tail of 200 reads 4200 and is refused.
	MaxQueryLines = 5000

	// MaxQueryBytes bounds the rendered response. The WAMP router drops a node
	// that emits a frame over 16 MiB, and a result travels with envelope and
	// serialization overhead on top of the payload, so stay well beneath it.
	MaxQueryBytes = 4 << 20

	// DefaultQueryLines applies when a caller names no tail.
	DefaultQueryLines = 200
)

// LogQueryRequest is a windowed read of one app's logs.
//
// Since and Until are already-resolved absolute times: a caller taking relative
// input ("30m") resolves it with common.ParseLogTime against the device clock
// before getting here, so the window is computed exactly once.
type LogQueryRequest struct {
	ContainerName string
	Tail          uint64
	Offset        uint64
	Since         time.Time
	Until         time.Time

	// NoTimestamps drops the per-line time prefix. A caller correlating lines
	// with a failure wants it; a live console does not, because the lines it
	// streams afterwards carry no timestamps and the seam between the two would
	// show. It does not affect OldestAvailable, which comes from its own probe.
	NoTimestamps bool
}

// LogQueryResult is one answer to a windowed read.
type LogQueryResult struct {
	Lines []string

	// Source is "docker", "compose", or "history". A "history" answer means the
	// container is gone and the window could not be applied — the caller must
	// say so rather than presenting stale lines as the requested range.
	Source string

	// Truncated reports that lines were dropped to fit the caps, oldest first.
	Truncated bool

	// OldestAvailable is the timestamp of the oldest line the device can still
	// produce for this container, or the zero time when unknown. It is what
	// separates "nothing was written in your window" from "your window is older
	// than what survived rotation" — two very different diagnoses.
	OldestAvailable time.Time
}

// QueryLogs reads a bounded, optionally time-windowed slice of a container's logs.
func (lm *LogManager) QueryLogs(ctx context.Context, request LogQueryRequest) (LogQueryResult, error) {
	if strings.TrimSpace(request.ContainerName) == "" {
		return LogQueryResult{}, errors.New("container name is empty")
	}

	tail := request.Tail
	if tail == 0 {
		tail = DefaultQueryLines
	}
	if tail > MaxQueryLines {
		tail = MaxQueryLines
	}

	// Docker has no offset, so paging back means reading past the window and
	// discarding the newest lines here. Bound the read, not just the answer.
	fetch := tail + request.Offset
	if fetch > MaxQueryLines {
		fetch = MaxQueryLines
	}

	query := container.LogQuery{
		Tail:       fetch,
		Timestamps: !request.NoTimestamps,
		Since:      formatBound(request.Since),
		Until:      formatBound(request.Until),
	}

	lines, source, err := lm.readWindow(ctx, request.ContainerName, query)
	if err != nil {
		return LogQueryResult{}, err
	}

	// Drop the newest `Offset` lines, then keep the newest `tail` of what is left.
	if request.Offset > 0 {
		if uint64(len(lines)) <= request.Offset {
			lines = nil
		} else {
			lines = lines[:uint64(len(lines))-request.Offset]
		}
	}

	truncated := false
	if uint64(len(lines)) > tail {
		lines = lines[uint64(len(lines))-tail:]
		truncated = true
	}

	lines, trimmedBytes := trimToBytes(lines, MaxQueryBytes)

	result := LogQueryResult{
		Lines:     lines,
		Source:    source,
		Truncated: truncated || trimmedBytes,
	}

	if source == sourceDocker {
		result.OldestAvailable = lm.oldestAvailable(ctx, request.ContainerName)
	}

	return result, nil
}

const (
	sourceDocker  = "docker"
	sourceCompose = "compose"
	sourceHistory = "history"
)

// readWindow tries Docker, then compose, then the agent's own history.
//
// The probe order is self-detecting rather than asking the app store what kind
// of app this is: a compose project has no container under the plain name, so
// Docker answers ContainerNotFound and the compose path takes over. That also
// covers a plain container that was removed.
func (lm *LogManager) readWindow(
	ctx context.Context,
	containerName string,
	query container.LogQuery,
) ([]string, string, error) {
	reader, err := lm.Container.Logs(ctx, containerName, query.DockerOptions())
	if err == nil {
		return scanLines(reader), sourceDocker, nil
	}

	if !errdefs.IsContainerNotFound(err) {
		return nil, "", err
	}

	composeReader, composeErr := lm.composeLogs(ctx, containerName, query)
	if composeErr == nil {
		return scanLines(composeReader), sourceCompose, nil
	}

	log.Debug().Err(composeErr).Msgf("no compose project for %s, falling back to stored history", containerName)

	history, historyErr := lm.GetLogHistory(containerName)
	if historyErr != nil {
		// Report the reason the *live* read failed: that the container is gone is
		// the useful fact, not that a fallback store was also empty.
		return nil, "", err
	}

	return history, sourceHistory, nil
}

// composeLogs reads a compose app's logs, finding its compose file through the
// label Docker keeps on every container of the project. That lookup is one
// Docker API call; the `docker compose ls` it replaces was a CLI spawn of its
// own, paid before the `compose logs` spawn that does the actual read.
func (lm *LogManager) composeLogs(ctx context.Context, containerName string, query container.LogQuery) (io.ReadCloser, error) {
	containers, err := container.ListComposeProjectContainers(ctx, lm.Container, containerName+"_compose")
	if err != nil {
		return nil, err
	}

	for _, c := range containers {
		if configFiles := c.Labels[container.ComposeConfigFilesLabel]; configFiles != "" {
			return lm.Container.Compose().Logs(ctx, configFiles, query)
		}
	}

	return nil, errors.New("compose project not found")
}

// oldestAvailable reads the first line the daemon will still serve.
//
// Docker offers no "head", so this opens an unbounded stream and closes it
// after one line. Because the daemon streams, that costs a single line rather
// than the whole log. The compose path has no equivalent — its CLI buffers the
// entire output before returning — so compose answers leave this unset and the
// caller reports the retention floor as unknown.
func (lm *LogManager) oldestAvailable(ctx context.Context, containerName string) time.Time {
	reader, err := lm.Container.Logs(ctx, containerName, container.LogQuery{Timestamps: true}.DockerOptions())
	if err != nil {
		return time.Time{}
	}
	defer func() { _ = reader.Close() }()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	if !scanner.Scan() {
		return time.Time{}
	}

	return parseLeadingTimestamp(scanner.Text())
}

// parseLeadingTimestamp reads the RFC3339Nano stamp Docker prefixes to a line
// when Timestamps is set. A line without one yields the zero time.
func parseLeadingTimestamp(line string) time.Time {
	field, _, found := strings.Cut(stripStreamHeader(line), " ")
	if !found {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339Nano, field)
	if err != nil {
		return time.Time{}
	}

	return parsed
}

// stripStreamHeader removes Docker's 8-byte multiplexing frame header.
//
// Containers the agent starts get Tty: true (apps/run_app.go), which turns
// multiplexing off, so in practice this is a no-op. It is here for containers
// created some other way, where the header would otherwise arrive as binary
// inside a log line and be handed to whoever asked for the logs.
//
// The header is [stream, 0, 0, 0, size0..size3]. Matching on the three NUL
// bytes rather than trimming a cutset of control characters matters: the four
// size bytes are arbitrary and are frequently printable, so a cutset trim stops
// in the wrong place and corrupts the line.
func stripStreamHeader(line string) string {
	if len(line) < 8 {
		return line
	}
	if line[0] > 2 || line[1] != 0 || line[2] != 0 || line[3] != 0 {
		return line
	}
	return line[8:]
}

// formatBound renders a window bound the way the Docker daemon and the compose
// CLI both accept. The zero time means "no bound".
func formatBound(bound time.Time) string {
	if bound.IsZero() {
		return ""
	}
	return bound.UTC().Format(time.RFC3339)
}

func scanLines(reader io.ReadCloser) []string {
	defer func() {
		if err := reader.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log reader")
		}
	}()

	var lines []string
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	for scanner.Scan() {
		lines = append(lines, stripStreamHeader(scanner.Text()))
	}

	if err := scanner.Err(); err != nil {
		// A truncated read still carries the lines already scanned, and for a
		// diagnosis those are worth more than an error.
		log.Debug().Err(err).Msg("log scan ended early")
	}

	return lines
}

// trimToBytes drops whole lines from the front until the rendered size fits.
// Oldest first: for a diagnosis the newest lines are the ones that matter.
func trimToBytes(lines []string, limit int) ([]string, bool) {
	total := 0
	for _, line := range lines {
		total += len(line) + 1
	}

	if total <= limit {
		return lines, false
	}

	index := 0
	for index < len(lines) && total > limit {
		total -= len(lines[index]) + 1
		index++
	}

	return lines[index:], true
}
