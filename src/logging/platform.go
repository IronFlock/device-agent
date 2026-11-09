package logging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reagent/common"
	"reagent/container"
	"reagent/errdefs"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The appliance's own stack, read from the host.
//
// An appliance runs its platform as one compose project on the host this agent
// runs on. No service container holds the Docker socket, on purpose, so the
// agent is the only part of the platform that can see the others: their state
// and their logs. Everything here is limited to that one project and to a fixed
// list of host units; the source a caller names is only ever compared against
// them.
const (
	// PlatformComposeProject is the compose project of the appliance's stack
	// (`name:` in its docker-compose.yml).
	PlatformComposeProject = "ironflock-appliance"

	// PlatformSourceHost reads the appliance's own host units from the journal.
	PlatformSourceHost = "host"

	// PlatformSourceKernel reads the kernel's messages from the journal, which
	// is where an out-of-memory kill is recorded.
	PlatformSourceKernel = "kernel"

	// sourceJournal is the Source of an answer read from the journal.
	sourceJournal = "journal"

	// journalTimeout bounds one journalctl run. A journal on a slow or nearly
	// full disk can take long, and the caller allows about 20 seconds in total.
	journalTimeout = 15 * time.Second
)

// hostJournalUnits are the units PlatformSourceHost reads: the appliance's own
// (start, watchdog, updates, renewals) and the container runtime under them.
var hostJournalUnits = []string{"ironflock*.service", "docker.service", "containerd.service"}

// ErrNotAppliance means this host does not run the appliance's stack.
var ErrNotAppliance = errors.New("this device does not run the appliance stack")

// runJournalctl runs journalctl and returns its stdout. Replaced in tests.
var runJournalctl = func(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "journalctl", args...) //nolint:gosec // G204: fixed binary; the units are fixed and the numbers and times are formatted here, a caller's text never reaches the arguments
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// journalctl exits non-zero on some versions when nothing matched.
		if stdout.Len() == 0 && stderr.Len() == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("could not read the journal: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}

// PlatformLogRequest is a windowed read of one source of the appliance's stack.
type PlatformLogRequest struct {
	// Source is a compose service, PlatformSourceHost or PlatformSourceKernel.
	Source string

	Tail  uint64
	Since time.Time
	Until time.Time

	// Search keeps only the lines containing this text, ignoring case.
	Search string
}

// platformContainers lists the stack's containers, stopped ones included. An
// empty list means this host is no appliance, which is what keeps every read
// here off an ordinary device.
func (lm *LogManager) platformContainers(ctx context.Context) ([]container.ContainerResult, error) {
	containers, err := container.ListComposeProjectContainers(ctx, lm.Container, PlatformComposeProject)
	if err != nil {
		return nil, err
	}

	if len(containers) == 0 {
		return nil, ErrNotAppliance
	}

	return containers, nil
}

// PlatformServices reports the state of every container of the stack, sorted by
// service. A container that cannot be inspected keeps what the listing said.
func (lm *LogManager) PlatformServices(ctx context.Context) ([]common.Dict, error) {
	containers, err := lm.platformContainers(ctx)
	if err != nil {
		return nil, err
	}

	services := make([]common.Dict, 0, len(containers))
	for _, c := range containers {
		entry := common.Dict{
			"service": c.Labels[container.ComposeServiceLabel],
			"image":   c.Image,
			"state":   c.State,
			"status":  c.Status,
		}

		if state, err := lm.Container.GetContainerState(ctx, c.ID); err == nil {
			entry["health"] = state.Health
			entry["restart_count"] = state.RestartCount
			entry["oom_killed"] = state.OOMKilled
			entry["exit_code"] = state.ExitCode
			entry["started_at"] = state.StartedAt
			entry["finished_at"] = state.FinishedAt
			if state.Error != "" {
				entry["error"] = state.Error
			}
		}

		services = append(services, entry)
	}

	sort.SliceStable(services, func(i, j int) bool {
		return fmt.Sprint(services[i]["service"]) < fmt.Sprint(services[j]["service"])
	})

	return services, nil
}

// QueryPlatformLogs reads a bounded, optionally time-windowed slice of one
// source of the stack. A search is applied before the tail, so it can find a
// line older than the newest Tail lines, within what a single read may scan.
func (lm *LogManager) QueryPlatformLogs(ctx context.Context, request PlatformLogRequest) (LogQueryResult, error) {
	containers, err := lm.platformContainers(ctx)
	if err != nil {
		return LogQueryResult{}, err
	}

	tail := request.Tail
	if tail == 0 {
		tail = DefaultQueryLines
	}
	if tail > MaxQueryLines {
		tail = MaxQueryLines
	}

	fetch := tail
	if request.Search != "" {
		fetch = MaxQueryLines
	}

	var lines []string
	var oldest time.Time
	var source string

	switch request.Source {
	case PlatformSourceHost, PlatformSourceKernel:
		source = sourceJournal
		lines, err = readJournal(ctx, request, fetch)
	default:
		source = sourceCompose
		var selected []container.ContainerResult
		for _, c := range containers {
			if c.Labels[container.ComposeServiceLabel] == request.Source {
				selected = append(selected, c)
			}
		}

		if len(selected) == 0 {
			return LogQueryResult{}, errdefs.ContainerNotFound(
				fmt.Errorf("no platform service %q on this appliance", request.Source),
			)
		}

		lines, oldest, err = lm.mergeContainerLogs(ctx, selected, container.LogQuery{
			Tail:       fetch,
			Timestamps: true,
			Since:      formatBound(request.Since),
			Until:      formatBound(request.Until),
		})
	}
	if err != nil {
		return LogQueryResult{}, err
	}

	lines = filterLines(lines, request.Search)

	truncated := false
	if uint64(len(lines)) > tail {
		lines = lines[uint64(len(lines))-tail:]
		truncated = true
	}

	lines, trimmedBytes := trimToBytes(lines, MaxQueryBytes)

	return LogQueryResult{
		Lines:           lines,
		Source:          source,
		Truncated:       truncated || trimmedBytes,
		OldestAvailable: oldest,
	}, nil
}

// readJournal reads the host's journal for PlatformSourceHost or
// PlatformSourceKernel. Kernel messages are matched by transport rather than
// with -k, which covers the current boot only: an out-of-memory kill that was
// followed by a reboot is exactly the one worth finding.
func readJournal(ctx context.Context, request PlatformLogRequest, fetch uint64) ([]string, error) {
	args := []string{"--no-pager", "-q", "-o", "short-iso", "-n", strconv.FormatUint(fetch, 10)}

	if request.Source == PlatformSourceKernel {
		args = append(args, "_TRANSPORT=kernel")
	} else {
		for _, unit := range hostJournalUnits {
			args = append(args, "-u", unit)
		}
	}

	if !request.Since.IsZero() {
		args = append(args, "--since", journalTime(request.Since))
	}
	if !request.Until.IsZero() {
		args = append(args, "--until", journalTime(request.Until))
	}

	runCtx, cancel := context.WithTimeout(ctx, journalTimeout)
	defer cancel()

	output, err := runJournalctl(runCtx, args...)
	if err != nil {
		return nil, err
	}

	return splitLines(output), nil
}

// journalTime renders a bound in the one form every journalctl version parses,
// which it reads as local time.
func journalTime(bound time.Time) string {
	return bound.Local().Format("2006-01-02 15:04:05")
}

func splitLines(output []byte) []string {
	text := strings.TrimRight(string(output), "\n")
	if text == "" {
		return nil
	}

	return strings.Split(text, "\n")
}

func filterLines(lines []string, search string) []string {
	if search == "" {
		return lines
	}

	needle := strings.ToLower(search)
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), needle) {
			kept = append(kept, line)
		}
	}

	return kept
}
