/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"github.com/containerd/containerd"
	"github.com/containerd/containerd/log"
	cio "github.com/containerd/cri/pkg/server/io"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
	"io"
	"k8s.io/client-go/tools/remotecommand"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// Attach prepares a streaming endpoint to attach to a running container, and returns the address.
func (c *criService) Attach(ctx context.Context, r *runtime.AttachRequest) (*runtime.AttachResponse, error) {
	cntr, err := c.containerStore.Get(r.GetContainerId())
	if err != nil {
		return nil, errors.Wrap(err, "failed to find container in store")
	}
	state := cntr.Status.Get().State()
	if state != runtime.ContainerState_CONTAINER_RUNNING && state != runtime.ContainerState_CONTAINER_CREATED {
		return nil, errors.Errorf("container is in %s state", criContainerStateToString(state))
	}
	return c.streamServer.GetAttach(r)
}

func (c *criService) attachContainer(ctx context.Context, id string, stdin io.Reader, stdout, stderr io.WriteCloser,
	tty bool, resize <-chan remotecommand.TerminalSize) error {
	// Get container from our container store.
	cntr, err := c.containerStore.Get(id)
	if err != nil {
		return errors.Wrapf(err, "failed to find container %q in store", id)
	}
	id = cntr.ID

	state := cntr.Status.Get().State()
	if state != runtime.ContainerState_CONTAINER_RUNNING && state != runtime.ContainerState_CONTAINER_CREATED {
		return errors.Errorf("container is in %s state", criContainerStateToString(state))
	}

	opts := cio.AttachOptions{
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		Tty:       tty,
		StdinOnce: cntr.Config.StdinOnce,
		CloseStdin: func() error {
			task, err := cntr.Container.Task(ctx, nil)
			if err != nil {
				return errors.Wrap(err, "failed to load task")
			}
			return task.CloseIO(ctx, containerd.WithStdinCloser)
		},
	}

	eg := errgroup.Group{}
	eg.Go(func() error {
		cntr.IO.Attach(opts)
		return nil
	})

	if state == runtime.ContainerState_CONTAINER_CREATED {
		_, err := c.StartContainer(ctx, &runtime.StartContainerRequest{
			ContainerId: cntr.ID,
		})
		if err != nil {
			return err
		}
	}

	task, err := cntr.Container.Task(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "failed to load task")
	}
	handleResizing(resize, func(size remotecommand.TerminalSize) {
		if err := task.Resize(ctx, uint32(size.Width), uint32(size.Height)); err != nil {
			log.G(ctx).WithError(err).Errorf("Failed to resize task %q console", id)
		}
	})

	return eg.Wait()
}
