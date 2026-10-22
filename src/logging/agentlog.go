package logging

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Reading the agent's own log back out.
//
// reagent.log is zerolog JSON — one object per line carrying "time" (RFC3339),
// "level" and "message" — rotated by lumberjack at 100 MB with two backups.
// That format is what makes a windowed, severity-filtered read possible here
// without any of the plumbing the container path needs: the timestamps are
// already on every line.
//
// The file is streamed, never read whole. Its predecessor slurped the entire
// file into one WAMP result, which on a busy device meant handing the router a
// frame far past its 16 MiB limit and losing the device's session.

const (
	// DefaultAgentLogLines applies when a caller names no tail.
	DefaultAgentLogLines = 300

	// MaxAgentLogLines bounds a single agent-log read.
	MaxAgentLogLines = 5000
)

// levelRank orders zerolog's severities so a caller can ask for "warn and
// worse". Unknown levels rank alongside info: a line we cannot classify is
// worth keeping over silently dropping it.
var levelRank = map[string]int{
	"trace": 0,
	"debug": 1,
	"info":  2,
	"warn":  3,
	"error": 4,
	"fatal": 5,
	"panic": 6,
}

const defaultLevelRank = 2

// AgentLogQuery is a windowed, severity-filtered read of the agent's own log.
type AgentLogQuery struct {
	// Path is the active log file. Its rotated siblings are discovered from it.
	Path string

	Tail  uint64
	Since time.Time
	Until time.Time

	// MinLevel keeps this severity and worse. Empty means "info".
	MinLevel string
}

// AgentLogResult is one answer to an agent-log read.
type AgentLogResult struct {
	Lines     []string
	Truncated bool

	// OldestAvailable is the timestamp of the oldest line still on disk across
	// every rotation, or the zero time when nothing could be dated. It tells a
	// caller whether an empty answer means "quiet" or "already rotated away".
	OldestAvailable time.Time

	// FilesScanned counts the rotations actually read, so a caller can say how
	// far back the answer reaches.
	FilesScanned int
}

type agentLogLine struct {
	Level   string `json:"level"`
	Time    string `json:"time"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

// QueryAgentLog reads the agent log back, newest `Tail` matching lines last.
func QueryAgentLog(query AgentLogQuery) (AgentLogResult, error) {
	tail := query.Tail
	if tail == 0 {
		tail = DefaultAgentLogLines
	}
	if tail > MaxAgentLogLines {
		tail = MaxAgentLogLines
	}

	minRank, ok := levelRank[strings.ToLower(strings.TrimSpace(query.MinLevel))]
	if !ok {
		minRank = defaultLevelRank
	}

	files, err := agentLogFiles(query.Path)
	if err != nil {
		return AgentLogResult{}, err
	}

	result := AgentLogResult{}
	kept := make([]string, 0, tail)

	// Timestamp and level carry forward across lines: a panic stack or any other
	// raw write lands in the file without JSON around it, and those lines belong
	// with the entry they followed rather than being dropped for lacking a date.
	var lastStamp time.Time
	lastRank := defaultLevelRank

	for _, path := range files {
		file, openErr := os.Open(path)
		if openErr != nil {
			// A rotation can vanish between listing and opening. Keep going: a
			// partial answer beats no answer.
			continue
		}

		result.FilesScanned++

		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)

		for scanner.Scan() {
			raw := scanner.Text()
			if strings.TrimSpace(raw) == "" {
				continue
			}

			rendered, stamp, rank, parsed := renderAgentLogLine(raw)
			if parsed {
				lastStamp = stamp
				lastRank = rank
			} else {
				stamp = lastStamp
				rank = lastRank
			}

			if !stamp.IsZero() && result.OldestAvailable.IsZero() {
				result.OldestAvailable = stamp
			}

			if rank < minRank {
				continue
			}
			if !query.Since.IsZero() && !stamp.IsZero() && stamp.Before(query.Since) {
				continue
			}
			if !query.Until.IsZero() && !stamp.IsZero() && stamp.After(query.Until) {
				continue
			}

			if uint64(len(kept)) == tail {
				kept = kept[1:]
				result.Truncated = true
			}
			kept = append(kept, rendered)
		}

		_ = file.Close()
	}

	lines, trimmed := trimToBytes(kept, MaxQueryBytes)
	result.Lines = lines
	result.Truncated = result.Truncated || trimmed

	return result, nil
}

// renderAgentLogLine turns one zerolog record into a compact line.
//
// The raw JSON carries "caller", stack frames and arbitrary context fields.
// Those are noise for a reader working out what the device did, and on a
// token-budgeted consumer they are expensive noise, so only time, level,
// message and error survive. A line that is not JSON is returned verbatim —
// panics and raw writes are exactly the lines worth keeping.
func renderAgentLogLine(raw string) (string, time.Time, int, bool) {
	var entry agentLogLine
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		return raw, time.Time{}, defaultLevelRank, false
	}

	if entry.Time == "" && entry.Level == "" && entry.Message == "" {
		return raw, time.Time{}, defaultLevelRank, false
	}

	stamp, err := time.Parse(time.RFC3339, entry.Time)
	if err != nil {
		stamp = time.Time{}
	}

	level := strings.ToLower(entry.Level)
	rank, ok := levelRank[level]
	if !ok {
		rank = defaultLevelRank
	}

	var builder strings.Builder
	if !stamp.IsZero() {
		builder.WriteString(stamp.UTC().Format(time.RFC3339))
		builder.WriteByte(' ')
	}
	if level != "" {
		builder.WriteString(strings.ToUpper(level))
		builder.WriteByte(' ')
	}
	builder.WriteString(entry.Message)
	if entry.Error != "" {
		builder.WriteString(": ")
		builder.WriteString(entry.Error)
	}

	return builder.String(), stamp, rank, true
}

// agentLogFiles lists the active log file and its rotations, oldest first.
//
// lumberjack names a backup "<name>-<timestamp><ext>" in the same directory, so
// the rotations sort chronologically by name. Reading them before the active
// file means a `since` that predates the last rotation still finds its lines.
func agentLogFiles(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("no agent log path configured")
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)

	entries, err := os.ReadDir(dir)
	if err != nil {
		// The directory may not be readable, but the active file might still be.
		if _, statErr := os.Stat(path); statErr == nil {
			return []string{path}, nil
		}
		return nil, err
	}

	var backups []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == base {
			continue
		}
		if strings.HasPrefix(name, prefix+"-") && strings.HasSuffix(name, ext) {
			backups = append(backups, filepath.Join(dir, name))
		}
	}

	sort.Strings(backups)

	if _, statErr := os.Stat(path); statErr != nil {
		if len(backups) == 0 {
			return nil, statErr
		}
		return backups, nil
	}

	return append(backups, path), nil
}
