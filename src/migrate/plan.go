package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"reagent/layout"
	"reagent/linuxservice"
	"strings"
)

// Every legacy install moves, whatever put it there; the plan (in the
// journal) records how:
//
//   - the units: links into /opt/reagent (ironflock-init since 2024-03,
//     FlockOS), real files in /etc/systemd/system (ReswarmOS, the Cubic
//     installer, Jetson, the installers of 2021–23) or package files in
//     /lib/systemd/system;
//   - the data: moved with one rename (StrategyMove), or, where that cannot
//     work — a mount at or below /opt/reagent, another filesystem than /opt,
//     an OS image — left in place with /opt/ironflock/device-agent a link to
//     it (StrategyLink);
//   - OS images (RAUC slots: FlockOS, ReswarmOS, Cubic): their boot scripts
//     relink the config from /boot and re-enable the legacy units, so the
//     /boot files stay and the legacy units stay masked. An OS update brings
//     the image's legacy layout back with its own agent, which updates and
//     moves again.
//   - the legacy unit's flags: -config, -appsDir, -logFile and the rest
//     (-debug, -env, …), carried over to the new unit.
const (
	StrategyMove = "move"
	StrategyLink = "link"
)

const (
	systemUnitDir  = "/etc/systemd/system"
	packageUnitDir = "/lib/systemd/system"
	raucConfig     = "/etc/rauc/system.conf"
	// movedAsideSuffix: a real unit file is renamed to this before the unit
	// is masked (mask cannot replace a file) and renamed back on rollback.
	movedAsideSuffix = ".ironflock-legacy"
)

// legacyUnit is one legacy unit as the plan found it.
type legacyUnit struct {
	Name string `json:"name"`
	// Path is the unit file systemd loads it from (/etc/systemd/system/…, a
	// link or a file, or /lib/systemd/system/…).
	Path string `json:"path"`
	// LinkTarget: Path is a link to this file (enable it by this path).
	LinkTarget string `json:"link_target,omitempty"`
	// MovedAside: Path was a file and now sits at Path+movedAsideSuffix.
	MovedAside bool `json:"moved_aside,omitempty"`
}

// movable: a file in /etc/systemd/system, which mask cannot replace.
func (u legacyUnit) movable() bool {
	return u.LinkTarget == "" && strings.HasPrefix(u.Path, systemUnitDir+"/")
}

// findLegacyUnit returns the unit file systemd loads name from, if any.
func findLegacyUnit(e Env, name string) (legacyUnit, bool) {
	for _, dir := range []string{systemUnitDir, packageUnitDir} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(e.p(path))
		if err != nil {
			continue
		}
		u := legacyUnit{Name: name, Path: path}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(e.p(path))
			if err != nil {
				continue
			}
			target = e.unroot(target)
			if target == "/dev/null" {
				return legacyUnit{}, false // masked: not what runs the agent
			}
			u.LinkTarget = target
		}
		return u, true
	}
	return legacyUnit{}, false
}

// unitFile is the file the unit's text is in.
func (u legacyUnit) unitFile() string {
	if u.LinkTarget != "" {
		return u.LinkTarget
	}
	if u.MovedAside {
		return u.Path + movedAsideSuffix
	}
	return u.Path
}

// legacyCommand is what the legacy unit's ExecStart passes.
type legacyCommand struct {
	Config  string
	AppsDir string
	LogFile string
	Extra   []string
}

// flags the new unit sets itself, from the plan.
var pathFlags = map[string]bool{"config": true, "agentDir": true, "appsDir": true, "dbFileName": true, "logFile": true}

func parseExecStart(unitText string) legacyCommand {
	c := legacyCommand{
		Config:  layout.LegacyAgentDir + "/device-config.flock",
		AppsDir: layout.LegacyAgentDir + "/apps", // the legacy agent's own default
		LogFile: legacyLog,
	}
	for _, line := range strings.Split(unitText, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		for i := 1; i < len(fields); i++ {
			field := fields[i]
			if !strings.HasPrefix(field, "-") {
				continue
			}
			name, value, hasValue := strings.Cut(strings.TrimLeft(field, "-"), "=")
			if !pathFlags[name] {
				c.Extra = append(c.Extra, field)
				// "-env production": the value is the next field.
				if !hasValue && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
					c.Extra = append(c.Extra, fields[i+1])
					i++
				}
				continue
			}
			if !hasValue && i+1 < len(fields) {
				value = fields[i+1]
				i++
			}
			switch name {
			case "config":
				c.Config = value
			case "appsDir":
				c.AppsDir = value
			case "logFile":
				c.LogFile = value
			}
		}
	}
	return c
}

// isImage: an A/B OS image owns /opt/reagent and /etc (RAUC), or FlockOS's
// boot script re-enables the legacy units.
func isImage(e Env) bool {
	return exists(e.p(raucConfig)) || isDirectory(e.p(layout.LegacyAgentDir+"/ironflock/services"))
}

// plan fills the journal's description of this install. It is the first
// thing prepare does, and idempotent.
func plan(e Env, j *Journal) error {
	if j.Planned {
		return nil
	}
	agent, ok := findLegacyUnit(e, legacyAgentUnit)
	if !ok {
		return fmt.Errorf("no %s", legacyAgentUnit)
	}
	units := []legacyUnit{}
	if manager, ok := findLegacyUnit(e, legacyManagerUnit); ok {
		units = append(units, manager)
	}
	units = append(units, agent)
	text, err := os.ReadFile(e.p(agent.unitFile()))
	if err != nil {
		return fmt.Errorf("reading %s: %w", agent.unitFile(), err)
	}
	cmd := parseExecStart(string(text))

	j.Units = units
	j.Image = isImage(e)
	j.LegacyConfig = cmd.Config
	j.LegacyLog = cmd.LogFile
	j.ExtraArgs = cmd.Extra
	// An apps folder inside /opt/reagent moves with it.
	j.AppsDir = cmd.AppsDir
	if underLegacyDir(cmd.AppsDir) {
		j.AppsDir = layout.AgentDir + strings.TrimPrefix(cmd.AppsDir, layout.LegacyAgentDir)
	}
	j.Strategy, j.StrategyReason = chooseStrategy(e, j.Image)
	if j.PreferLink && j.Strategy == StrategyMove {
		j.Strategy, j.StrategyReason = StrategyLink, "an earlier attempt could not free "+layout.LegacyAgentDir+" of its mounts"
	}
	j.Planned = true
	return e.saveJournal(j)
}

// chooseStrategy: a move when /opt/reagent can be renamed as a whole, else a
// link.
func chooseStrategy(e Env, image bool) (string, string) {
	if image {
		return StrategyLink, "an OS image owns /opt/reagent"
	}
	dataRoot, _ := dockerDataRoot(e, daemonJSON)
	mount, err := mountBelowLegacyDir(e, dataRoot)
	if err != nil {
		return StrategyLink, "the mount points are unreadable"
	}
	if mount != "" {
		return StrategyLink, mount + " is a mount point"
	}
	same, err := e.SameFilesystem(e.p(layout.LegacyAgentDir), e.p("/opt"))
	if err != nil || !same {
		return StrategyLink, layout.LegacyAgentDir + " is on another filesystem than /opt"
	}
	return StrategyMove, ""
}

// newUnitPaths is what the new unit refers to.
func newUnitPaths(j *Journal) linuxservice.Paths {
	p := linuxservice.DefaultPaths()
	if j.AppsDir != "" {
		p.AppsDir = j.AppsDir
	}
	p.ExtraArgs = j.ExtraArgs
	return p
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
