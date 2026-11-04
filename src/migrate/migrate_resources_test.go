package migrate

import (
	"os"
	"reagent/linuxservice"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A legacy-unit agent of this version has written its own resource drop-in
// beside reagent.service. The migration copies the legacy drop-ins, but this
// one must be the current rendering, never the copy.
func TestMigrateWritesItsOwnResourceDropIn(t *testing.T) {
	d := newDevice(t)
	d.file(legacyDropInDir+"/"+linuxservice.ResourcesDropInName, "[Service]\nNice=-5\n", 0o644)
	e := d.env()
	d.stage(e)
	require.NoError(t, Run(e))

	dropIn, err := os.ReadFile(d.p(linuxservice.ResourcesDropInPath(linuxservice.UnitName)))
	require.NoError(t, err)
	assert.Equal(t, linuxservice.ResourcesDropIn(!linuxservice.ReadMemoryRecursiveProt()), string(dropIn))

	slice, err := os.ReadFile(d.p(linuxservice.SlicePath))
	require.NoError(t, err)
	assert.Equal(t, linuxservice.SliceUnit(), string(slice))
}
