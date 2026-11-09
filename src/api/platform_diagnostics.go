package api

import (
	"context"
	"errors"
	"fmt"
	"reagent/common"
	"reagent/errdefs"
	"reagent/logging"
	"reagent/messenger"
	"time"
)

// checkMaintain is the gate of the appliance diagnostics procedures. MAINTAIN
// and not READ: these logs cover every project on the appliance, and MAINTAIN
// is what already opens a shell on this host.
func (ex *External) checkMaintain(details common.Dict) error {
	privileged, err := ex.Privilege.Check("MAINTAIN", details)
	if err != nil {
		// Not a denial — see queryAppLogsHandler.
		return fmt.Errorf("could not verify privileges: %w", err)
	}

	if !privileged {
		return errdefs.InsufficientPrivileges(errors.New("insufficient privileges to read the appliance's diagnostics"))
	}

	return nil
}

// listPlatformServicesHandler reports the state of the appliance's platform
// services and of its host.
func (ex *External) listPlatformServicesHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	if err := ex.checkMaintain(response.Details); err != nil {
		return nil, err
	}

	services, err := ex.LogManager.PlatformServices(ctx)
	if err != nil {
		return nil, err
	}

	return &messenger.InvokeResult{
		Arguments: []interface{}{common.Dict{
			"services":    services,
			"host":        ex.storageData(),
			"device_time": time.Now().UTC().Format(time.RFC3339),
		}},
	}, nil
}

// queryPlatformLogsHandler serves a bounded slice of one source of the
// appliance's stack: a platform service, the host's units or the kernel.
func (ex *External) queryPlatformLogsHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	if err := ex.checkMaintain(response.Details); err != nil {
		return nil, err
	}

	argsDict, err := firstArgDict(response.Arguments)
	if err != nil {
		return nil, err
	}

	source, ok := argsDict["source"].(string)
	if !ok || source == "" {
		return nil, fmt.Errorf("invalid value for source")
	}

	now := time.Now().UTC()

	request := logging.PlatformLogRequest{Source: source}

	if request.Tail, err = optionalUint64(argsDict, "tail"); err != nil {
		return nil, err
	}
	if request.Since, err = optionalLogTime(argsDict, "since", now); err != nil {
		return nil, err
	}
	if request.Until, err = optionalLogTime(argsDict, "until", now); err != nil {
		return nil, err
	}

	if raw := argsDict["search"]; raw != nil {
		search, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("the search param should be a string")
		}
		request.Search = search
	}

	result, err := ex.LogManager.QueryPlatformLogs(ctx, request)
	if err != nil {
		return nil, err
	}

	payload := common.Dict{
		"lines":       result.Lines,
		"returned":    len(result.Lines),
		"truncated":   result.Truncated,
		"source":      result.Source,
		"device_time": now.Format(time.RFC3339),
	}

	if !result.OldestAvailable.IsZero() {
		payload["oldest_available"] = result.OldestAvailable.UTC().Format(time.RFC3339)
	}

	return &messenger.InvokeResult{
		Arguments: []interface{}{payload},
	}, nil
}
