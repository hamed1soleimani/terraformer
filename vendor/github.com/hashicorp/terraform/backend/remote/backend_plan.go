package remote

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tfe "github.com/hashicorp/go-tfe"
	"github.com/hashicorp/terraform/backend"
)

func (b *Remote) opPlan(stopCtx, cancelCtx context.Context, op *backend.Operation, w *tfe.Workspace) (*tfe.Run, error) {
	log.Printf("[INFO] backend/remote: starting Plan operation")

	if !w.Permissions.CanQueueRun {
		return nil, fmt.Errorf(strings.TrimSpace(fmt.Sprintf(planErrNoQueueRunRights)))
	}

	if op.ModuleDepth != defaultModuleDepth {
		return nil, fmt.Errorf(strings.TrimSpace(planErrModuleDepthNotSupported))
	}

	if op.Parallelism != defaultParallelism {
		return nil, fmt.Errorf(strings.TrimSpace(planErrParallelismNotSupported))
	}

	if op.Plan != nil {
		return nil, fmt.Errorf(strings.TrimSpace(planErrPlanNotSupported))
	}

	if op.PlanOutPath != "" {
		return nil, fmt.Errorf(strings.TrimSpace(planErrOutPathNotSupported))
	}

	if !op.PlanRefresh {
		return nil, fmt.Errorf(strings.TrimSpace(planErrNoRefreshNotSupported))
	}

	if op.Targets != nil {
		return nil, fmt.Errorf(strings.TrimSpace(planErrTargetsNotSupported))
	}

	if op.Variables != nil {
		return nil, fmt.Errorf(strings.TrimSpace(
			fmt.Sprintf(planErrVariablesNotSupported, b.hostname, b.organization, op.Workspace)))
	}

	if (op.Module == nil || op.Module.Config().Dir == "") && !op.Destroy {
		return nil, fmt.Errorf(strings.TrimSpace(planErrNoConfig))
	}

	return b.plan(stopCtx, cancelCtx, op, w)
}

func (b *Remote) plan(stopCtx, cancelCtx context.Context, op *backend.Operation, w *tfe.Workspace) (*tfe.Run, error) {
	if b.CLI != nil {
		header := planDefaultHeader
		if op.Type == backend.OperationTypeApply {
			header = applyDefaultHeader
		}
		b.CLI.Output(b.Colorize().Color(strings.TrimSpace(header) + "\n"))
	}

	configOptions := tfe.ConfigurationVersionCreateOptions{
		AutoQueueRuns: tfe.Bool(false),
		Speculative:   tfe.Bool(op.Type == backend.OperationTypePlan),
	}

	cv, err := b.client.ConfigurationVersions.Create(stopCtx, w.ID, configOptions)
	if err != nil {
		return nil, generalError("error creating configuration version", err)
	}

	var configDir string
	if op.Module != nil && op.Module.Config().Dir != "" {
		// Make sure to take the working directory into account by removing
		// the working directory from the current path. This will result in
		// a path that points to the expected root of the workspace.
		configDir = filepath.Clean(strings.TrimSuffix(
			filepath.Clean(op.Module.Config().Dir),
			filepath.Clean(w.WorkingDirectory),
		))
	} else {
		// We did a check earlier to make sure we either have a config dir,
		// or the plan is run with -destroy. So this else clause will only
		// be executed when we are destroying and doesn't need the config.
		configDir, err = ioutil.TempDir("", "tf")
		if err != nil {
			return nil, generalError("error creating temporary directory", err)
		}
		defer os.RemoveAll(configDir)

		// Make sure the configured working directory exists.
		err = os.MkdirAll(filepath.Join(configDir, w.WorkingDirectory), 0700)
		if err != nil {
			return nil, generalError(
				"error creating temporary working directory", err)
		}
	}

	err = b.client.ConfigurationVersions.Upload(stopCtx, cv.UploadURL, configDir)
	if err != nil {
		return nil, generalError("error uploading configuration files", err)
	}

	uploaded := false
	for i := 0; i < 60 && !uploaded; i++ {
		select {
		case <-stopCtx.Done():
			return nil, context.Canceled
		case <-cancelCtx.Done():
			return nil, context.Canceled
		case <-time.After(500 * time.Millisecond):
			cv, err = b.client.ConfigurationVersions.Read(stopCtx, cv.ID)
			if err != nil {
				return nil, generalError("error retrieving configuration version", err)
			}

			if cv.Status == tfe.ConfigurationUploaded {
				uploaded = true
			}
		}
	}

	if !uploaded {
		return nil, generalError(
			"error uploading configuration files", errors.New("operation timed out"))
	}

	runOptions := tfe.RunCreateOptions{
		IsDestroy:            tfe.Bool(op.Destroy),
		Message:              tfe.String("Queued manually using Terraform"),
		ConfigurationVersion: cv,
		Workspace:            w,
	}

	r, err := b.client.Runs.Create(stopCtx, runOptions)
	if err != nil {
		return r, generalError("error creating run", err)
	}

	// When the lock timeout is set,
	if op.StateLockTimeout > 0 {
		go func() {
			select {
			case <-stopCtx.Done():
				return
			case <-cancelCtx.Done():
				return
			case <-time.After(op.StateLockTimeout):
				// Retrieve the run to get its current status.
				r, err := b.client.Runs.Read(cancelCtx, r.ID)
				if err != nil {
					log.Printf("[ERROR] error reading run: %v", err)
					return
				}

				if r.Status == tfe.RunPending && r.Actions.IsCancelable {
					if b.CLI != nil {
						b.CLI.Output(b.Colorize().Color(strings.TrimSpace(lockTimeoutErr)))
					}

					// We abuse the auto aprove flag to indicate that we do not
					// want to ask if the remote operation should be canceled.
					op.AutoApprove = true

					p, err := os.FindProcess(os.Getpid())
					if err != nil {
						log.Printf("[ERROR] error searching process ID: %v", err)
						return
					}
					p.Signal(syscall.SIGINT)
				}
			}
		}()
	}

	if b.CLI != nil {
		b.CLI.Output(b.Colorize().Color(strings.TrimSpace(fmt.Sprintf(
			runHeader, b.hostname, b.organization, op.Workspace, r.ID)) + "\n"))
	}

	r, err = b.waitForRun(stopCtx, cancelCtx, op, "plan", r, w)
	if err != nil {
		return r, err
	}

	logs, err := b.client.Plans.Logs(stopCtx, r.Plan.ID)
	if err != nil {
		return r, generalError("error retrieving logs", err)
	}
	reader := bufio.NewReaderSize(logs, 64*1024)

	if b.CLI != nil {
		for next := true; next; {
			var l, line []byte

			for isPrefix := true; isPrefix; {
				l, isPrefix, err = reader.ReadLine()
				if err != nil {
					if err != io.EOF {
						return r, generalError("error reading logs", err)
					}
					next = false
				}
				line = append(line, l...)
			}

			if next || len(line) > 0 {
				b.CLI.Output(b.Colorize().Color(string(line)))
			}
		}
	}

	// Retrieve the run to get its current status.
	r, err = b.client.Runs.Read(stopCtx, r.ID)
	if err != nil {
		return r, generalError("error retrieving run", err)
	}

	// Return if the run is canceled or errored. We return without
	// an error, even if the run errored, as the error is already
	// displayed by the output of the remote run.
	if r.Status == tfe.RunCanceled || r.Status == tfe.RunErrored {
		return r, nil
	}

	// Check any configured sentinel policies.
	if len(r.PolicyChecks) > 0 {
		err = b.checkPolicy(stopCtx, cancelCtx, op, r)
		if err != nil {
			return r, err
		}
	}

	return r, nil
}

const planErrNoQueueRunRights = `
Insufficient rights to generate a plan!

[reset][yellow]The provided credentials have insufficient rights to generate a plan. In order
to generate plans, at least plan permissions on the workspace are required.[reset]
`

const planErrModuleDepthNotSupported = `
Custom module depths are currently not supported!

The "remote" backend does not support setting a custom module
depth at this time.
`

const planErrParallelismNotSupported = `
Custom parallelism values are currently not supported!

The "remote" backend does not support setting a custom parallelism
value at this time.
`

const planErrPlanNotSupported = `
Displaying a saved plan is currently not supported!

The "remote" backend currently requires configuration to be present and
does not accept an existing saved plan as an argument at this time.
`

const planErrOutPathNotSupported = `
Saving a generated plan is currently not supported!

The "remote" backend does not support saving the generated execution
plan locally at this time.
`

const planErrNoRefreshNotSupported = `
Planning without refresh is currently not supported!

Currently the "remote" backend will always do an in-memory refresh of
the Terraform state prior to generating the plan.
`

const planErrTargetsNotSupported = `
Resource targeting is currently not supported!

The "remote" backend does not support resource targeting at this time.
`

const planErrVariablesNotSupported = `
Run variables are currently not supported!

The "remote" backend does not support setting run variables at this time.
Currently the only to way to pass variables to the remote backend is by
creating a '*.auto.tfvars' variables file. This file will automatically
be loaded by the "remote" backend when the workspace is configured to use
Terraform v0.10.0 or later.

Additionally you can also set variables on the workspace in the web UI:
https://%s/app/%s/%s/variables
`

const planErrNoConfig = `
No configuration files found!

Plan requires configuration to be present. Planning without a configuration
would mark everything for destruction, which is normally not what is desired.
If you would like to destroy everything, please run plan with the "-destroy"
flag or create a single empty configuration file. Otherwise, please create
a Terraform configuration file in the path being executed and try again.
`

const planDefaultHeader = `
[reset][yellow]Running plan in the remote backend. Output will stream here. Pressing Ctrl-C
will stop streaming the logs, but will not stop the plan running remotely.[reset]

Preparing the remote plan...
`

const runHeader = `
[reset][yellow]To view this run in a browser, visit:
https://%s/app/%s/%s/runs/%s[reset]
`

// The newline in this error is to make it look good in the CLI!
const lockTimeoutErr = `
[reset][red]Lock timeout exceeded, sending interrupt to cancel the remote operation.
[reset]
`
