package api

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"reagent/common"
	"reagent/diskguard"
	"reagent/errdefs"
	"reagent/messenger"
)

func (ex *External) getStorageDataHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("READ", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to get storage data"))
	}

	stats := common.GetStats(filepath.Join(ex.Config.CommandLineArguments.AgentDir, "docker-apps"))

	result := common.Dict{
		"cpu_count":           stats.CPUCount,
		"cpu_usage":           stats.CPUUsagePercent,
		"memory_total":        stats.MemoryTotal,
		"memory_used":         stats.MemoryUsed,
		"memory_available":    stats.MemoryAvailable,
		"storage_total":       stats.StorageTotal,
		"storage_used":        stats.StorageUsed,
		"storage_free":        stats.StorageFree,
		"docker_apps_total":   stats.DockerAppsTotal,
		"docker_apps_used":    stats.DockerAppsUsed,
		"docker_apps_free":    stats.DockerAppsFree,
		"docker_apps_mounted": stats.DockerAppsMounted,
	}
	// filesystems and disk_level, as on the heartbeat (see diskguard.StatsFields).
	maps.Copy(result, diskguard.StatsFields())

	return &messenger.InvokeResult{
		Arguments: []interface{}{result},
	}, nil
}
