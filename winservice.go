/*
Copyright (c) 2022 Evan Wheeler

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package winservice

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

type Job interface {
	// Run should block until complete after cancelation.  The process may
	// terminate before Run completes if the job takes too long to return after
	// context is canceled. Non-nil error will be logged to service Event Log
	// unless the error is context.Canceled.
	Run(context.Context) error
}

// handler implements svc.Handler
type handler struct {
	ctx         context.Context
	job         Job
	elog        *eventlog.Log
	exitTimeout time.Duration
}

func (h *handler) Execute(args []string, req <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown

	// Update Service status to start-pending
	changes <- svc.Status{State: svc.StartPending}

	// Make cancellable context for Run
	ctx, cancel := context.WithCancel(h.ctx)

	// done is to allow Run to complete after external cancellation.
	done := make(chan struct{})

	// Run main service job in goroutine.
	go func(ctx context.Context) {
		defer cancel()    // cancel if we exit
		defer close(done) // mark done when we exit

		if err := h.job.Run(ctx); err != nil && err != context.Canceled {
			h.elog.Error(1, err.Error())
		}
	}(ctx)

	// Update Service status to running
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	// process service messages until service shutdown or cancellation
	h.serviceLoop(ctx, req, changes)

	// Signal Run to exit in case it is still running
	cancel()

	// Update Service status to stop-pending
	changes <- svc.Status{State: svc.StopPending}

	// wait up to ExitTimeout for Run to exit.
	select {
	case <-time.After(h.exitTimeout):
	case <-done:
	}

	return
}

// serviceLoop processes requests for state change from the service until stop/shutdown is received,
// the request channel is closed, or context is canceled externally.
func (d *handler) serviceLoop(ctx context.Context, req <-chan svc.ChangeRequest, changes chan<- svc.Status) {
	for {
		select {
		case c, ok := <-req:
			if !ok {
				// this is unexpected
				return
			}

			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// exit on stop/cancel
				return
			default:
				d.elog.Error(1, fmt.Sprintf("unexpected control request #%d", c))
			}
		case <-ctx.Done():
			// exit on external cancellation
			return
		}
	}
}

// Start launches the Windows service and executes job in a separate goroutine.
func Start(ctx context.Context, name string, job Job) error {
	elog, err := eventlog.Open(name)

	if err != nil {
		return err
	}

	defer elog.Close()

	elog.Info(1, fmt.Sprintf("starting %s service", name))

	s := &handler{
		ctx:         ctx,
		job:         job,
		elog:        elog,
		exitTimeout: 30 * time.Second,
	}

	err = svc.Run(name, s)

	if err != nil && err != context.Canceled {
		elog.Error(1, fmt.Sprintf("%s service failed: %v", name, err))
		return err
	}

	elog.Info(1, fmt.Sprintf("%s service stopped", name))

	return nil
}

// InstallService installs a windows service with the given name, display name, description and arguments
func InstallService(name, display, desc string, args ...string) error {
	exepath, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", name)
	}
	s, err = m.CreateService(name, exepath, mgr.Config{StartType: mgr.StartAutomatic, Description: desc, DisplayName: display}, args...)
	if err != nil {
		return err
	}
	defer s.Close()
	err = eventlog.InstallAsEventCreate(name, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil {
		s.Delete()
		return fmt.Errorf("setupEventLogSource failed: %s", err)
	}
	return nil
}

// RemoveService uninstalls a service with the given name
func RemoveService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service %s is not installed", name)
	}
	defer s.Close()
	err = s.Delete()
	if err != nil {
		return err
	}
	err = eventlog.Remove(name)
	if err != nil {
		return fmt.Errorf("removeEventLogSource failed: %s", err)
	}
	return nil
}

// IsWindowsService reports whether the process is running as a Windows service.
func IsWindowsService() (bool, error) {
	return svc.IsWindowsService()
}
