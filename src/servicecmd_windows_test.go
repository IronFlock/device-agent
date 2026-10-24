//go:build windows

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The ImagePath is parsed the way the SCM composed it (CommandLineToArgvW),
// quoted segments included: a custom -agentDir may carry a space.
func TestAgentDirFromImagePath(t *testing.T) {
	// Unquoted (default ProgramData path, no spaces).
	img := `C:\ProgramData\IronFlock\DeviceAgent\device-agent.exe -config C:\ProgramData\IronFlock\DeviceAgent\device.flock -agentDir C:\ProgramData\IronFlock\DeviceAgent -appsDir C:\ProgramData\IronFlock\DeviceAgent\apps`
	assert.Equal(t, `C:\ProgramData\IronFlock\DeviceAgent`, agentDirFromImagePath(img))

	// The service from before 1.4.0.
	legacy := `C:\ProgramData\IronFlock\Reagent\reagent.exe -config C:\ProgramData\IronFlock\Reagent\device.flock -agentDir C:\ProgramData\IronFlock\Reagent`
	assert.Equal(t, `C:\ProgramData\IronFlock\Reagent`, agentDirFromImagePath(legacy))

	// Quoted path with a space.
	imgQuoted := `"C:\Program Files\IronFlock\device-agent.exe" -agentDir "D:\my data\agent" -logFile "D:\my data\agent\device-agent.log"`
	assert.Equal(t, `D:\my data\agent`, agentDirFromImagePath(imgQuoted))

	// Missing -agentDir.
	assert.Equal(t, "", agentDirFromImagePath(`C:\x\device-agent.exe -config c.flock`))
}
