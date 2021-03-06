// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package boot

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"git.arvados.org/arvados.git/lib/cmd"
	"git.arvados.org/arvados.git/lib/config"
	"git.arvados.org/arvados.git/sdk/go/ctxlog"
)

var Command cmd.Handler = bootCommand{}

type supervisedTask interface {
	// Execute the task. Run should return nil when the task is
	// done enough to satisfy a dependency relationship (e.g., the
	// service is running and ready). If the task starts a
	// goroutine that fails after Run returns (e.g., the service
	// shuts down), it should call fail().
	Run(ctx context.Context, fail func(error), super *Supervisor) error
	String() string
}

type bootCommand struct{}

func (bootCommand) RunCommand(prog string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	super := &Supervisor{
		Stderr: stderr,
		logger: ctxlog.New(stderr, "json", "info"),
	}

	ctx := ctxlog.Context(context.Background(), super.logger)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var err error
	defer func() {
		if err != nil {
			super.logger.WithError(err).Info("exiting")
		}
	}()

	flags := flag.NewFlagSet(prog, flag.ContinueOnError)
	flags.SetOutput(stderr)
	loader := config.NewLoader(stdin, super.logger)
	loader.SetupFlags(flags)
	versionFlag := flags.Bool("version", false, "Write version information to stdout and exit 0")
	flags.StringVar(&super.SourcePath, "source", ".", "arvados source tree `directory`")
	flags.StringVar(&super.ClusterType, "type", "production", "cluster `type`: development, test, or production")
	flags.StringVar(&super.ListenHost, "listen-host", "localhost", "host name or interface address for service listeners")
	flags.StringVar(&super.ControllerAddr, "controller-address", ":0", "desired controller address, `host:port` or `:port`")
	flags.BoolVar(&super.OwnTemporaryDatabase, "own-temporary-database", false, "bring up a postgres server and create a temporary database")
	timeout := flags.Duration("timeout", 0, "maximum time to wait for cluster to be ready")
	shutdown := flags.Bool("shutdown", false, "shut down when the cluster becomes ready")
	err = flags.Parse(args)
	if err == flag.ErrHelp {
		err = nil
		return 0
	} else if err != nil {
		return 2
	} else if *versionFlag {
		return cmd.Version.RunCommand(prog, args, stdin, stdout, stderr)
	} else if super.ClusterType != "development" && super.ClusterType != "test" && super.ClusterType != "production" {
		err = fmt.Errorf("cluster type must be 'development', 'test', or 'production'")
		return 2
	}

	loader.SkipAPICalls = true
	cfg, err := loader.Load()
	if err != nil {
		return 1
	}

	super.Start(ctx, cfg)
	defer super.Stop()

	var timer *time.Timer
	if *timeout > 0 {
		timer = time.AfterFunc(*timeout, super.Stop)
	}

	url, ok := super.WaitReady()
	if timer != nil && !timer.Stop() {
		err = errors.New("boot timed out")
		return 1
	} else if !ok {
		err = errors.New("boot failed")
		return 1
	}
	// Write controller URL to stdout. Nothing else goes to
	// stdout, so this provides an easy way for a calling script
	// to discover the controller URL when everything is ready.
	fmt.Fprintln(stdout, url)
	if *shutdown {
		super.Stop()
	}
	// Wait for signal/crash + orderly shutdown
	<-super.done
	return 0
}
