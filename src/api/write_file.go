package api

import (
	"context"
	"errors"
	"fmt"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/messenger"
	"reagent/safe"
)

func (ex *External) writeToFileHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("DEVELOP", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges write data to device"))
	}

	args := response.Arguments

	// Matches file_transfer.ts payload
	if len(args) < 5 {
		return nil, fmt.Errorf("failed to parse args, expected 5 but got %d", len(args))
	}

	chunkArg := args[0]
	fileNameArg := args[1]
	containerNameArg := args[2]
	totalArg := args[3]
	idArg := args[4]

	fileName, ok := fileNameArg.(string)
	if !ok {
		return nil, errors.New("failed to parse name argument")
	}

	chunk, ok := chunkArg.(string)
	if !ok {
		return nil, errors.New("failed to parse chunk argument")
	}

	containerName, ok := containerNameArg.(string)
	if !ok {
		return nil, errors.New("failed to parse containerName argument")
	}

	total, ok := totalArg.(uint64)
	if !ok {
		return nil, errors.New("failed to parse total argument")
	}

	id, ok := idArg.(string)
	if !ok {
		return nil, errors.New("failed to parse id argument")
	}

	fileDir := ex.Messenger.GetConfig().CommandLineArguments.AppsBuildDir

	fileChunk := filesystem.FileChunk{
		ID:            id,
		FileName:      fileName,
		FilePath:      fileDir,
		Data:          chunk,
		ContainerName: containerName,
		Total:         total,
	}

	err = ex.Filesystem.Write(fileChunk)
	if err != nil {
		// Clean up the failed transfer and notify the app manager to reset state
		ex.Filesystem.CleanupFailedTransfer(containerName)
		safe.Go(func() {
			ex.AppManager.HandleTransferFailure(containerName, err)
		})
		return nil, err
	}

	var message string
	if fileChunk.Data == "BEGIN" {
		safe.Go(func() {
			_ = ex.LogManager.ClearLogHistory(containerName)
		})

		message = "Received first package on device!"
	} else if fileChunk.Data == "END" {
		message = "File transfer has finished, starting build..."
	}

	if fileChunk.Data == "BEGIN" || fileChunk.Data == "END" {
		safe.Go(func() {
			_ = ex.LogManager.Write(containerName, message)
		})

		return &messenger.InvokeResult{}, nil
	}

	activeTransfer := ex.Filesystem.GetActiveTransfer(containerName)
	if activeTransfer == nil {
		// received a chunk but no active stream was found
		return &messenger.InvokeResult{}, nil
	}

	safe.Go(func() {
		percentage := float64(activeTransfer.Current) / float64(activeTransfer.Total) * 100
		percentageString := fmt.Sprintf("%.3f%%", percentage)
		_ = ex.LogManager.PublishProgress(containerName, fileName, "Transfer Progress:", percentageString)
	})

	return &messenger.InvokeResult{}, nil
}
