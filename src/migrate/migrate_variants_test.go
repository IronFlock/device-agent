package migrate

import (
	"os"
	"reagent/layout"
	"reagent/linuxservice"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The kinds of legacy install in the field, as the OS images and the
// installers of the past left them (see plan.go).
var variants = map[string]func(t *testing.T, d *device){
	// ironflock-init since 2024-03: newDevice as it is.
	"ironflock-init": func(t *testing.T, d *device) {},

	// FlockOS: an A/B image (RAUC); its flock-init.sh links and enables the
	// units from /opt/reagent/ironflock/services at every boot; Docker's
	// data-root is the /apps partition.
	"flockos": func(t *testing.T, d *device) {
		require.NoError(t, os.RemoveAll(d.p("/opt/reagent/reswarmify")))
		for _, unit := range []string{legacyAgentUnit, legacyManagerUnit} {
			require.NoError(t, os.Remove(d.p("/etc/systemd/system/"+unit)))
		}
		d.file("/opt/reagent/ironflock/services/reagent.service", "[Service]\nExecStartPre=/opt/reagent/ironflock/scripts/reagent-setup.sh\nExecStart=/opt/reagent/Reagent-active -config /opt/reagent/device-config.flock -appsDir /apps -debug\n", 0o644)
		d.file("/opt/reagent/ironflock/services/reagent-manager.service", "[Service]\nExecStart=/opt/reagent/ironflock/scripts/reagent-manager.sh\n", 0o644)
		d.file("/opt/reagent/ironflock/scripts/flock-init.sh", "#!/bin/bash\n", 0o755)
		d.link("/etc/systemd/system/reagent.service", d.p("/opt/reagent/ironflock/services/reagent.service"))
		d.link("/etc/systemd/system/reagent-manager.service", d.p("/opt/reagent/ironflock/services/reagent-manager.service"))
		d.file("/opt/reagent/flock-mode", "", 0o644)
		d.file(raucConfig, "[system]\ncompatible=ReswarmOS-image-raspberrypi4-64\n", 0o644)
		d.file(daemonJSON, `{"data-root": "/apps"}`, 0o644)
		d.mounts = []string{"/", "/boot", "/apps", "/apps/overlay2/abc/merged"}
	},

	// ReswarmOS and the Cubic x86 image: A/B (RAUC), real unit files in
	// /etc/systemd/system, the config named .reswarm.
	"reswarmos": func(t *testing.T, d *device) {
		require.NoError(t, os.RemoveAll(d.p("/opt/reagent/reswarmify")))
		require.NoError(t, os.Remove(d.p("/opt/reagent/device-config.flock")))
		for _, unit := range []string{legacyAgentUnit, legacyManagerUnit} {
			require.NoError(t, os.Remove(d.p("/etc/systemd/system/"+unit)))
		}
		d.file("/etc/systemd/system/reagent.service", "[Service]\nExecStartPre=/usr/sbin/reagent-setup.sh\nExecStart=/opt/reagent/Reagent-active -config /opt/reagent/device-config.reswarm -appsDir /apps -debug\nExecStartPost=/usr/sbin/reagent-traffic.sh\n", 0o644)
		d.file("/etc/systemd/system/reagent-manager.service", "[Service]\nExecStart=/usr/sbin/reagent-manager.sh\n", 0o644)
		d.file("/boot/demo_swarm.reswarm", `{"secret":"s3cr3t"}`, 0o644)
		d.link("/opt/reagent/device-config.reswarm", d.p("/boot/demo_swarm.reswarm"))
		d.file(raucConfig, "[system]\ncompatible=ReswarmOS-image-raspberrypi4\n", 0o644)
		d.file(daemonJSON, `{"data-root": "/apps/docker"}`, 0o644)
		d.mounts = []string{"/", "/boot", "/apps", "/apps/docker/overlay2/abc/merged"}
	},

	// Jetson (stock L4T + reswarmify-nvidia.sh): a real unit file, no
	// manager, the config a real file, Docker's default data-root.
	"jetson": func(t *testing.T, d *device) {
		require.NoError(t, os.RemoveAll(d.p("/opt/reagent/reswarmify")))
		require.NoError(t, os.Remove(d.p("/opt/reagent/device-config.flock")))
		require.NoError(t, os.Remove(d.p("/boot/demo_swarm.flock")))
		require.NoError(t, os.Remove(d.p(bootDeviceINI)))
		for _, unit := range []string{legacyAgentUnit, legacyManagerUnit} {
			require.NoError(t, os.Remove(d.p("/etc/systemd/system/"+unit)))
		}
		d.notLoaded[legacyManagerUnit] = true
		d.file("/etc/systemd/system/reagent.service", "[Service]\nExecStart=/opt/reagent/reagent-latest -config /opt/reagent/device-config.reswarm -appsDir /apps -debug\nRestart=always\n", 0o644)
		d.file("/opt/reagent/device-config.reswarm", `{"secret":"s3cr3t"}`, 0o644)
		require.NoError(t, os.Remove(d.p(daemonJSON)))
		d.mounts = []string{"/", "/var/lib/docker/overlay2/abc/merged"}
	},

	// An appliance host: the appliance's files in /opt/ironflock already.
	"appliance host": func(t *testing.T, d *device) {
		d.file("/opt/ironflock/docker-compose.yml", "services: {}\n", 0o644)
		d.file("/opt/ironflock/.env", "X=1\n", 0o600)
	},

	// /opt/reagent with a mount below it (a data partition): cannot be
	// renamed as a whole.
	"a mount below /opt/reagent": func(t *testing.T, d *device) {
		d.mounts = append(d.mounts, "/opt/reagent/data")
	},

	// The installers of 2021–23: real unit files, no -appsDir (the agent's
	// default <agentDir>/apps), more flags.
	"reswarmify 2022": func(t *testing.T, d *device) {
		require.NoError(t, os.RemoveAll(d.p("/opt/reagent/reswarmify")))
		for _, unit := range []string{legacyAgentUnit, legacyManagerUnit} {
			require.NoError(t, os.Remove(d.p("/etc/systemd/system/"+unit)))
		}
		d.file("/etc/systemd/system/reagent.service", "[Service]\nExecStart=/opt/reagent/Reagent-active -config /opt/reagent/device-config.flock -env production -nmw=false\n", 0o644)
		d.file("/etc/systemd/system/reagent-manager.service", "[Service]\nExecStart=/usr/sbin/reagent-manager.sh\n", 0o644)
	},
}

func variantDevice(t *testing.T, name string) *device {
	d := newDevice(t)
	variants[name](t, d)
	return d
}

func TestEveryKindOfInstallMoves(t *testing.T) {
	for name := range variants {
		t.Run(name, func(t *testing.T) {
			d := variantDevice(t, name)
			e := d.env()
			d.stage(e)
			require.NoError(t, Run(e))
			j := d.journal()
			require.True(t, j.Done)
			assert.Equal(t, layout.Current, layout.Detect(d.root).Kind)

			// The new unit starts the agent from the new folder with the
			// legacy unit's flags and apps folder.
			unit, err := os.ReadFile(d.p(linuxservice.UnitPath))
			require.NoError(t, err)
			assert.Equal(t, newUnitPaths(j).Unit(), string(unit))
			// Its config resolves to the device's secret.
			data, err := os.ReadFile(d.p(layout.AgentDir + "/device-config.flock"))
			require.NoError(t, err)
			assert.Contains(t, string(data), "s3cr3t")
			// The database comes along.
			db, err := os.ReadFile(d.p(layout.AgentDir + "/device-agent.db"))
			require.NoError(t, err)
			assert.Equal(t, "database", string(db))
			// No legacy unit can run a second agent.
			for _, u := range j.Units {
				if target, err := os.Readlink(d.p("/etc/systemd/system/" + u.Name)); err == nil {
					assert.Equal(t, "/dev/null", target, "%s may only be masked", u.Name)
				} else {
					_, statErr := os.Lstat(d.p("/etc/systemd/system/" + u.Name))
					assert.True(t, os.IsNotExist(statErr), "%s is still there", u.Name)
				}
			}
			assert.NoFileExists(t, d.p("/opt/reagent/reagent-v1.3.0"))
		})
	}
}

func TestEveryKindOfInstallRollsBackAtEveryStep(t *testing.T) {
	for name := range variants {
		for _, step := range StepNames() {
			t.Run(name+"/"+step, func(t *testing.T) {
				d := variantDevice(t, name)
				before := d.snapshot()
				e := d.env()
				d.stage(e)
				e.Fault = step
				require.Error(t, Run(e))
				assert.Equal(t, before, d.snapshot())
				if !d.notLoaded[legacyManagerUnit] {
					assert.Equal(t, "start "+legacyManagerUnit, lastCall(d.calls, "start "+legacyManagerUnit))
				} else {
					assert.Equal(t, "start "+legacyAgentUnit, lastCall(d.calls, "start "+legacyAgentUnit))
				}
			})
		}
	}
}

func TestThePlanPerKind(t *testing.T) {
	for name, want := range map[string]struct {
		strategy string
		image    bool
		appsDir  string
		extra    string
	}{
		"ironflock-init":             {StrategyMove, false, "/apps", "-debug"},
		"flockos":                    {StrategyLink, true, "/apps", "-debug"},
		"reswarmos":                  {StrategyLink, true, "/apps", "-debug"},
		"jetson":                     {StrategyMove, false, "/apps", "-debug"},
		"appliance host":             {StrategyMove, false, "/apps", "-debug"},
		"a mount below /opt/reagent": {StrategyLink, false, "/apps", "-debug"},
		"reswarmify 2022":            {StrategyMove, false, layout.AgentDir + "/apps", "-env production -nmw=false"},
	} {
		t.Run(name, func(t *testing.T) {
			d := variantDevice(t, name)
			e := d.env()
			d.stage(e)
			j, err := LoadJournal(d.root)
			require.NoError(t, err)
			require.NoError(t, plan(e, j))
			assert.Equal(t, want.strategy, j.Strategy, j.StrategyReason)
			assert.Equal(t, want.image, j.Image)
			assert.Equal(t, want.appsDir, j.AppsDir)
			assert.Equal(t, want.extra, strings.Join(j.ExtraArgs, " "))
		})
	}
}

// On an OS image the boot scripts relink the config from /boot and enable
// the legacy units again: the /boot files stay, the units stay masked.
func TestOnAnImageTheBootFilesStay(t *testing.T) {
	for _, name := range []string{"flockos", "reswarmos"} {
		t.Run(name, func(t *testing.T) {
			d := variantDevice(t, name)
			e := d.env()
			d.stage(e)
			require.NoError(t, Run(e))
			assert.FileExists(t, d.p(bootDeviceINI))
			assert.FileExists(t, d.p("/boot/demo_swarm.flock"))
			assert.NotContains(t, d.calls, "unmask "+legacyManagerUnit+" "+legacyAgentUnit)
			target, err := os.Readlink(d.p(layout.AgentDir))
			require.NoError(t, err, "a link to the image's /opt/reagent")
			assert.Equal(t, d.p("/opt/reagent"), target)
			info, err := os.Lstat(d.p(layout.AgentDir + "/device-config.flock"))
			require.NoError(t, err)
			assert.NotZero(t, info.Mode()&os.ModeSymlink, "the config stays the image's link")
		})
	}
	t.Run("flockos keeps its boot scripts", func(t *testing.T) {
		d := variantDevice(t, "flockos")
		e := d.env()
		d.stage(e)
		require.NoError(t, Run(e))
		assert.FileExists(t, d.p("/opt/reagent/ironflock/scripts/flock-init.sh"))
		assert.FileExists(t, d.p("/opt/reagent/flock-mode"))
	})
}
