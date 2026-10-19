//go:build !windows

package system

import (
	"os/exec"
	"reagent/lifecycle"
)

func (sys *System) Reboot() error {
	_, err := exec.Command("reboot").Output()
	return err
}

func (sys *System) Poweroff() error {
	_, err := exec.Command("poweroff").Output()
	return err
}

// RestartAgent requests a restart from the service loop under
// ironflock-device-agent.service. A legacy install has no loop: restart its
// reagent.service directly.
func (sys *System) RestartAgent() error {
	if lifecycle.Supervised() {
		return lifecycle.RequestRestart("system_restart_agent")
	}
	_, err := exec.Command("systemctl", "restart", "reagent").Output()
	return err
}
