package api

import (
	"context"
	"errors"
	"reagent/errdefs"
	"reagent/messenger"
	"time"
)

func (ex *External) getImagesHandler(_ context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("READ", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to read docker images"))
	}

	ctx, cancelFunc := context.WithTimeout(context.Background(), time.Millisecond*2000)
	result, err := ex.Container.ListImages(ctx, nil)
	if err != nil {
		cancelFunc()
		return nil, err
	}

	cancelFunc()

	// See https://github.com/golang/go/wiki/InterfaceSlice
	images := make([]interface{}, 0)
	for _, image := range result {
		images = append(images, image)
	}

	return &messenger.InvokeResult{
		Arguments: images,
	}, nil
}
