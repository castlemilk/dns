package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultAPIBaseURL = "https://api.vultr.com"

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type runDependencies struct {
	apiBaseURL string
	httpClient *http.Client
	sleep      func(context.Context, time.Duration) error
}

func run(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stdout io.Writer,
	stderr io.Writer,
	dependencies runDependencies,
) error {
	flags := flag.NewFlagSet("vultr-dns-lb", flag.ContinueOnError)
	flags.SetOutput(stderr)
	planMode := flags.Bool("plan", false, "show the deterministic read-only plan (default)")
	applyMode := flags.Bool("apply", false, "apply the plan and wait for the load balancer")
	label := flags.String("label", defaultLabel, "exact Vultr load-balancer label")
	nodes := flags.Int("nodes", defaultNodes, "billable Vultr load-balancer node count; must be odd (1-99)")
	waitTimeout := flags.Duration("wait-timeout", 5*time.Minute, "maximum apply readiness wait")
	pollInterval := flags.Duration("poll-interval", 5*time.Second, "apply readiness poll interval")
	var instances stringListFlag
	flags.Var(&instances, "instance-id", "core VKE instance ID; repeat for each backend")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *planMode && *applyMode {
		return errors.New("--plan and --apply are mutually exclusive")
	}
	if *waitTimeout <= 0 {
		return errors.New("--wait-timeout must be greater than zero")
	}
	if *pollInterval <= 0 {
		return errors.New("--poll-interval must be greater than zero")
	}
	desired, err := newDesiredState(*label, instances, *nodes)
	if err != nil {
		return fmt.Errorf("invalid desired load balancer: %w", err)
	}

	apiKey := getenv("VULTR_API_KEY")
	baseURL := dependencies.apiBaseURL
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	client, err := newAPIClient(baseURL, apiKey, dependencies.httpClient)
	if err != nil {
		return err
	}
	sleep := dependencies.sleep
	if sleep == nil {
		sleep = sleepContext
	}
	reconciler := &reconciler{
		client:       client,
		waitTimeout:  *waitTimeout,
		pollInterval: *pollInterval,
		sleep:        sleep,
	}

	reconciliationPlan, err := reconciler.plan(ctx, desired)
	if err != nil {
		return err
	}
	mode := "plan"
	if *applyMode {
		mode = "apply"
	}
	if err := renderPlan(stdout, mode, reconciliationPlan); err != nil {
		return err
	}
	if !*applyMode {
		return nil
	}

	current, err := reconciler.apply(ctx, reconciliationPlan)
	if err != nil {
		return err
	}
	return renderApplyResult(stdout, reconciliationPlan, current)
}
