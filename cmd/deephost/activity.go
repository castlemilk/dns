package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	activityv1 "github.com/castlemilk/dns/gen/go/activity/v1"
)

func cmdActivity(ctx context.Context, e *env, o *options, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "show", "get":
			return cmdActivityShow(ctx, e, o, args[1:])
		case "help", "-h", "--help":
			return &helpError{usage: activityUsage}
		case "list":
			// `deephost activity list` reads naturally even though the bare
			// group already lists.
			args = args[1:]
		}
	}
	return cmdActivityList(ctx, e, o, args)
}

func cmdActivityList(ctx context.Context, e *env, o *options, args []string) error {
	var domain, since string
	var kinds stringList
	var limit uint
	var cursor string
	fs, err := o.parse("deephost activity", activityUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&domain, "domain", "", "only this zone's events")
		fs.StringVar(&domain, "zone", "", "alias for --domain")
		fs.Var(&kinds, "kind", "an exact kind, or a prefix ending in a dot; repeatable")
		fs.StringVar(&since, "since", "", "an RFC 3339 time, or a duration back from now")
		fs.UintVar(&limit, "limit", 0, "how many events (default 50, max 200)")
		fs.StringVar(&cursor, "cursor", "", "continue before this event id")
	})
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 0, activityUsage); err != nil {
		return err
	}
	request := &activityv1.ListEventsRequest{
		Kinds:  kinds,
		Limit:  uint32(limit), //nolint:gosec // bounded server-side
		Cursor: cursor,
	}
	if strings.TrimSpace(since) != "" {
		instant, err := parseSince(since, e.clock())
		if err != nil {
			return err
		}
		request.Since = timestamppb.New(instant)
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	if strings.TrimSpace(domain) != "" {
		zone, err := lookupZoneWithTimeout(ctx, o, clients, domain)
		if err != nil {
			return err
		}
		request.ZoneId = zone.GetId()
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Activity.ListEvents(callCtx, connect.NewRequest(request))
	if err != nil {
		return fail(clients, err)
	}
	events := response.Msg
	return newPrinter(e, o).emit(events, func(w io.Writer) error {
		if len(events.GetEvents()) == 0 {
			return writeLine(w, "no events matched")
		}
		table := newTable(w, "TIME", "SEVERITY", "ACTOR", "KIND", "DOMAIN", "SUMMARY")
		for _, event := range events.GetEvents() {
			table.row(
				formatTime(event.GetTime()),
				label(event.GetSeverity().String(), "SEVERITY_"),
				orDash(event.GetActor()),
				event.GetKind(),
				orDash(event.GetZoneName()),
				event.GetSummary(),
			)
		}
		if err := table.flush(); err != nil {
			return err
		}
		if next := events.GetNextCursor(); next != "" {
			return writeLine(w, "\nmore: --cursor "+next)
		}
		return nil
	})
}

func cmdActivityShow(ctx context.Context, e *env, o *options, args []string) error {
	fs, err := o.parse("deephost activity show", activityUsage, args, nil)
	if err != nil {
		return err
	}
	eventID, err := argument(fs, 0, "event_id", activityUsage)
	if err != nil {
		return err
	}
	if err := noExtraArgs(fs, 1, activityUsage); err != nil {
		return err
	}
	clients, err := o.connect(e)
	if err != nil {
		return err
	}
	callCtx, cancel := o.call(ctx)
	defer cancel()
	response, err := clients.Activity.GetEvent(callCtx,
		connect.NewRequest(&activityv1.GetEventRequest{Id: eventID}))
	if err != nil {
		return fail(clients, err)
	}
	event := response.Msg.GetEvent()
	return newPrinter(e, o).emit(event, func(w io.Writer) error {
		if err := fields(w,
			pair("event", event.GetId()),
			pair("time", formatTime(event.GetTime())),
			pair("severity", label(event.GetSeverity().String(), "SEVERITY_")),
			pair("actor", orDash(event.GetActor())),
			pair("kind", event.GetKind()),
			pair("domain", orDash(event.GetZoneName())),
			pair("correlation", orDash(event.GetCorrelationId())),
			pair("summary", event.GetSummary()),
		); err != nil {
			return err
		}
		details := event.GetDetails()
		if len(details) == 0 {
			return nil
		}
		if _, err := fmt.Fprintln(w, "\ndetails:"); err != nil {
			return err
		}
		names := make([]string, 0, len(details))
		for name := range details {
			names = append(names, name)
		}
		// Map order is random in Go; a stable rendering is what makes two runs
		// of the same command comparable.
		sort.Strings(names)
		table := newTable(w)
		for _, name := range names {
			table.row("  "+name+":", details[name])
		}
		return table.flush()
	})
}
