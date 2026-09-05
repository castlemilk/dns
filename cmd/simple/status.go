package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"connectrpc.com/connect"

	platformv1 "github.com/castlemilk/dns/gen/go/platform/v1"
)

// cmdStatus is the view Settings shows: what each engine is, whether it is
// configured, whether it answered, and — when it is not configured — the names
// of the environment variables it is waiting for. Never a value, only a name.
func cmdStatus(ctx context.Context, e *env, o *options, args []string) error {
	var probe bool
	fs, err := o.parse("simple status", statusUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&probe, "probe", false, "re-probe every engine now instead of reading the last result")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, statusUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Platform.GetPlatformStatus(callCtx,
		connect.NewRequest(&platformv1.GetPlatformStatusRequest{Probe: probe}))
	if err != nil {
		return fail(clients, err)
	}
	status := response.Msg
	return newPrinter(e, o).emit(status, func(w io.Writer) error {
		if err := fields(w,
			pair("control plane", clients.BaseURL),
			pair("version", orDash(status.GetVersion())),
			pair("production", yesNo(status.GetProduction())),
			pair("started", formatTime(status.GetStartedAt())),
			pair("nameservers", orDash(strings.Join(status.GetNameservers(), " "))),
			pair("platform store", orDash(status.GetPlatformStoreName())),
			pair("mailboxes per domain", formatUint(status.GetMailboxesPerDomain())),
		); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		table := newTable(w, "ENGINE", "CONFIGURED", "REACHABLE", "PROVIDER", "ENDPOINT", "DETAIL")
		for _, engine := range status.GetEngines() {
			table.row(
				label(engine.GetKind().String(), "ENGINE_KIND_"),
				yesNo(engine.GetConfigured()),
				yesNo(engine.GetReachable()),
				orDash(engine.GetProvider()),
				orDash(engine.GetEndpointHost()),
				engineDetail(engine),
			)
		}
		return table.flush()
	})
}

// engineDetail says the honest thing about an engine that is not working: the
// probe's reason when it was tried, or the environment variable names that
// would configure it. An engine that is fine says so with a dash rather than a
// claim.
func engineDetail(engine *platformv1.EngineStatus) string {
	if !engine.GetConfigured() {
		if missing := engine.GetMissingEnv(); len(missing) > 0 {
			return "not configured; set " + strings.Join(missing, ", ")
		}
		return "not configured"
	}
	if reason := strings.TrimSpace(engine.GetReason()); reason != "" {
		return reason
	}
	if version := strings.TrimSpace(engine.GetVersion()); version != "" {
		return version
	}
	return dash
}

// cmdPlatform holds the operator recovery actions that belong to the platform
// itself rather than to one zone.
func cmdPlatform(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) == 0 {
		return &usageError{message: "platform: choose a command", usage: platformUsage}
	}
	switch args[0] {
	case "rebuild":
		return cmdPlatformRebuild(ctx, e, o, args[1:])
	case "help", "-h", "--help":
		return &helpError{usage: platformUsage}
	default:
		return &usageError{message: fmt.Sprintf("unknown platform command %q", args[0]), usage: platformUsage}
	}
}

func cmdPlatformRebuild(ctx context.Context, e *env, o *options, args []string) error {
	var dryRun bool
	fs, err := o.parse("simple platform rebuild", platformUsage, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&dryRun, "dry-run", false, "report what would be recreated and write nothing")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, platformUsage); err != nil {
		return err
	}
	if !dryRun {
		if err := confirm(e, o,
			"Rebuild sites, mail domains and subscriptions from the engines?"); err != nil {
			return err
		}
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Platform.RebuildPlatformStore(callCtx,
		connect.NewRequest(&platformv1.RebuildPlatformStoreRequest{DryRun: dryRun}))
	if err != nil {
		return fail(clients, err)
	}
	result := response.Msg
	return newPrinter(e, o).emit(result, func(w io.Writer) error {
		if err := fields(w,
			pair("dry run", yesNo(result.GetDryRun())),
			pair("sites", formatUint(result.GetSites())),
			pair("mail domains", formatUint(result.GetMailDomains())),
			pair("subscriptions", formatUint(result.GetSubscriptions())),
		); err != nil {
			return err
		}
		return writeList(w, "warnings", result.GetWarnings())
	})
}

// cmdVersion prints the banner and what this binary is. It never reaches the
// control plane, so it works before there is a credential.
func cmdVersion(e *env, o *options, args []string) error {
	fs, err := o.parse("simple version", versionUsage, args, nil)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, versionUsage); err != nil {
		return err
	}
	document := struct {
		Version   string `json:"version"`
		GoVersion string `json:"go_version"`
		Platform  string `json:"platform"`
	}{
		Version:   buildVersion(e),
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if o.json {
		return newPrinter(e, o).emit(document, nil)
	}
	if err := writeBanner(e); err != nil {
		return err
	}
	return fields(e.stdout,
		pair("version", document.Version),
		pair("go", document.GoVersion),
		pair("platform", document.Platform),
	)
}

// buildVersion reports the version the toolchain stamped in, falling back to
// "dev" for a local build rather than inventing a number.
func buildVersion(e *env) string {
	if version := strings.TrimSpace(e.version); version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if version := strings.TrimSpace(info.Main.Version); version != "" && version != "(devel)" {
		return version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 7 {
			return setting.Value[:7]
		}
	}
	return "dev"
}

// writeList prints a labelled list, or nothing at all when it is empty: an
// empty heading reads as a claim that something was checked.
func writeList(w io.Writer, heading string, values []string) error {
	if len(values) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s:\n", heading); err != nil {
		return err
	}
	for _, value := range values {
		if _, err := fmt.Fprintf(w, "  %s\n", value); err != nil {
			return err
		}
	}
	return nil
}
