package logging

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"reagent/container"
	"reagent/errdefs"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
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
// first — to the app's container, or to every container of a multi-container
// app — and only drops to the agent's history when the containers are gone,
// flagging that the requested window was NOT applied rather than pretending it
// was.

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

	read, err := lm.readWindow(ctx, request.ContainerName, query)
	if err != nil {
		return LogQueryResult{}, err
	}

	lines := read.Lines

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

	return LogQueryResult{
		Lines:           lines,
		Source:          read.Source,
		Truncated:       truncated || trimmedBytes,
		OldestAvailable: read.OldestAvailable,
	}, nil
}

const (
	sourceDocker  = "docker"
	sourceCompose = "compose"
	sourceHistory = "history"
)

// readWindow tries Docker, then the containers of a compose project, then the
// agent's own history. Its answer is not yet trimmed to the caps.
//
// The probe order is self-detecting rather than asking the app store what kind
// of app this is: a compose project has no container under the plain name, so
// Docker answers ContainerNotFound and the compose path takes over. That also
// covers a plain container that was removed.
func (lm *LogManager) readWindow(
	ctx context.Context,
	containerName string,
	query container.LogQuery,
) (LogQueryResult, error) {
	reader, err := lm.Container.Logs(ctx, containerName, query.DockerOptions())
	if err == nil {
		return LogQueryResult{
			Lines:           readLogLines(reader),
			Source:          sourceDocker,
			OldestAvailable: lm.oldestAvailable(ctx, containerName),
		}, nil
	}

	if !errdefs.IsContainerNotFound(err) {
		return LogQueryResult{}, err
	}

	lines, oldest, composeErr := lm.composeLogs(ctx, containerName, query)
	if composeErr == nil {
		return LogQueryResult{Lines: lines, Source: sourceCompose, OldestAvailable: oldest}, nil
	}

	if !errors.Is(composeErr, errNoComposeProject) {
		// The project's containers exist but could not be read. Serving history
		// here would present an empty or stale log as the app's answer and hide
		// the failure that matters.
		return LogQueryResult{}, composeErr
	}

	history, historyErr := lm.GetLogHistory(containerName)
	if historyErr != nil {
		// Report the reason the *live* read failed: that the container is gone is
		// the useful fact, not that a fallback store was also empty.
		return LogQueryResult{}, err
	}

	return LogQueryResult{Lines: history, Source: sourceHistory}, nil
}

// errNoComposeProject means no container carries the app's compose project
// label: the app is not a compose app, or its containers are gone.
var errNoComposeProject = errors.New("compose project not found")

// composeLogs reads every container of a compose app through the Docker API and
// merges them into one timeline, newest Tail lines across the whole app. Each
// line carries the prefix `docker compose logs` gives it ("web-1  | …"): for a
// multi-container app that prefix is the only thing saying which service spoke,
// and the console's live tail, which follows `compose logs -f`, prints the same.
//
// This used to shell out to `docker compose logs`. That re-parses the compose
// file — without the --env-file the app was started with — and costs two
// process spawns that run to seconds on a loaded device; any failure fell
// through to the stored history, which for a compose app is usually empty, so
// the logs looked absent while the containers were right there. The daemon
// needs no compose file, and can also say how far back each container's log
// goes, which the CLI could not.
//
// Every container is read with timestamps because the merge needs them; they
// are dropped again when the caller did not ask for them.
func (lm *LogManager) composeLogs(ctx context.Context, containerName string, query container.LogQuery) ([]string, time.Time, error) {
	containers, err := container.ListComposeProjectContainers(ctx, lm.Container, containerName+"_compose")
	if err != nil {
		return nil, time.Time{}, err
	}

	if len(containers) == 0 {
		return nil, time.Time{}, errNoComposeProject
	}

	stamped := query
	stamped.Timestamps = true

	var merged []composeLine
	var oldest time.Time

	for _, c := range containers {
		reader, err := lm.Container.Logs(ctx, c.ID, stamped.DockerOptions())
		if err != nil {
			// Removed between the listing and the read: nothing left to say.
			if errdefs.IsContainerNotFound(err) {
				continue
			}
			return nil, time.Time{}, err
		}

		prefix := composeLogPrefix(c)

		// A record Docker split for length carries its own stamp, but should one
		// arrive without, it stays next to the line it continues.
		var last time.Time
		for _, line := range readLogLines(reader) {
			if at := parseLeadingTimestamp(line); !at.IsZero() {
				last = at
			}
			merged = append(merged, composeLine{at: last, prefix: prefix, text: line})
		}

		// The app's log reaches back as far as its longest-lived container's.
		first := lm.oldestAvailable(ctx, c.ID)
		if !first.IsZero() && (oldest.IsZero() || first.Before(oldest)) {
			oldest = first
		}
	}

	sort.SliceStable(merged, func(i, j int) bool { return merged[i].at.Before(merged[j].at) })

	if query.Tail > 0 && uint64(len(merged)) > query.Tail {
		merged = merged[uint64(len(merged))-query.Tail:]
	}

	lines := make([]string, 0, len(merged))
	for _, entry := range merged {
		text := entry.text
		if !query.Timestamps {
			text = dropLeadingTimestamp(text)
		}
		lines = append(lines, entry.prefix+text)
	}

	return lines, oldest, nil
}

type composeLine struct {
	at     time.Time
	prefix string
	text   string
}

// composeLogPrefix is what `docker compose logs` puts in front of a container's
// lines: the container name without its project ("web-1"), or the whole name
// when the service sets its own container_name.
func composeLogPrefix(c container.ContainerResult) string {
	name := c.Labels[container.ComposeServiceLabel]
	if len(c.Names) > 0 {
		full := strings.TrimPrefix(c.Names[0], "/")
		name = strings.TrimPrefix(full, c.Labels[container.ComposeProjectLabel]+"-")
	}

	return name + "  | "
}

// oldestAvailable reads the first line the daemon will still serve.
//
// Docker offers no "head", so this opens an unbounded stream and closes it
// after one line. Because the daemon streams, that costs a single line rather
// than the whole log.
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

// dropLeadingTimestamp removes the stamp parseLeadingTimestamp reads. A line
// without one is returned unchanged.
func dropLeadingTimestamp(line string) string {
	field, rest, _ := strings.Cut(line, " ")
	if _, err := time.Parse(time.RFC3339Nano, field); err != nil {
		return line
	}

	return rest
}

// stripStreamHeader removes Docker's 8-byte multiplexing frame header from the
// start of a line. readLogLines demultiplexes whole streams; this covers the
// single line oldestAvailable reads without doing so.
func stripStreamHeader(line string) string {
	if len(line) < 8 || !isStreamHeader([]byte(line[:8])) {
		return line
	}
	return line[8:]
}

// isStreamHeader reports whether head is a multiplexing frame header:
// [stream, 0, 0, 0, size0..size3].
//
// Matching on the three NUL bytes rather than on a cutset of control characters
// matters: the four size bytes are arbitrary and frequently printable. A TTY
// stream cannot be mistaken for one when Docker stamps it — every line then
// starts with a digit.
func isStreamHeader(head []byte) bool {
	return len(head) >= 8 && head[0] <= 2 && head[1] == 0 && head[2] == 0 && head[3] == 0
}

// readLogLines reads a log stream to the end, demultiplexing it first when
// Docker framed it.
//
// A container without a TTY gets a multiplexed stream: every write is framed
// behind an 8-byte header, and one frame can hold several lines or part of one,
// so stripping a header off the front of each line is not enough. Containers
// the agent starts itself run with Tty: true (apps/run_app.go) and stream raw;
// compose services only get a TTY when their author asked for one.
func readLogLines(reader io.ReadCloser) []string {
	defer func() {
		if err := reader.Close(); err != nil {
			log.Debug().Err(err).Msg("failed to close log reader")
		}
	}()

	buffered := bufio.NewReader(reader)
	head, _ := buffered.Peek(8)
	if !isStreamHeader(head) {
		return scanLines(buffered)
	}

	var plain bytes.Buffer
	if _, err := stdcopy.StdCopy(&plain, &plain, buffered); err != nil {
		// What was demultiplexed before the failure is still worth returning.
		log.Debug().Err(err).Msg("log demultiplexing ended early")
	}

	return scanLines(&plain)
}

// formatBound renders a window bound the way the Docker daemon and the compose
// CLI both accept. The zero time means "no bound".
func formatBound(bound time.Time) string {
	if bound.IsZero() {
		return ""
	}
	return bound.UTC().Format(time.RFC3339)
}

func scanLines(reader io.Reader) []string {
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
