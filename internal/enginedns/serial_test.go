package enginedns_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dnsv1 "github.com/castlemilk/dns/gen/go/dns/v1"
	"github.com/castlemilk/dns/internal/enginedns"
	"github.com/castlemilk/dns/internal/zone"
)

func TestSerializerRunsOneCallerPerZoneAtATime(t *testing.T) {
	t.Parallel()

	serial := enginedns.NewSerializer()
	var inside, peak atomic.Int32
	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			err := serial.Run(context.Background(), "z1", func(context.Context) error {
				current := inside.Add(1)
				for {
					high := peak.Load()
					if current <= high || peak.CompareAndSwap(high, current) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				inside.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	group.Wait()
	if peak.Load() != 1 {
		t.Errorf("peak concurrency = %d, want 1", peak.Load())
	}
	if serial.Pending("z1") != 0 {
		t.Errorf("Pending after all callers finished = %d", serial.Pending("z1"))
	}
}

func TestSerializerDoesNotBlockOtherZones(t *testing.T) {
	t.Parallel()

	serial := enginedns.NewSerializer()
	held := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- serial.Run(context.Background(), "z1", func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	done := make(chan error, 1)
	go func() {
		done <- serial.Run(context.Background(), "z2", func(context.Context) error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run on the second zone: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("a busy zone blocked an unrelated zone")
	}
	close(release)
	if err := <-holder; err != nil {
		t.Errorf("Run on the held zone: %v", err)
	}
}

func TestSerializerHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	serial := enginedns.NewSerializer()
	held := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- serial.Run(context.Background(), "z1", func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := serial.Run(ctx, "z1", func(context.Context) error {
		t.Error("the queued work ran even though its context expired")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v, want a deadline error", err)
	}
	close(release)
	if err := <-holder; err != nil {
		t.Errorf("Run on the held zone: %v", err)
	}
}

func TestSerializerReturnsTheCallbackError(t *testing.T) {
	t.Parallel()

	serial := enginedns.NewSerializer()
	sentinel := errors.New("boom")
	if err := serial.Run(context.Background(), "z1", func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("Run = %v, want the callback error", err)
	}
	if err := serial.Run(context.Background(), "z1", nil); err == nil {
		t.Error("Run with no work = nil error")
	}
}

func TestSourceMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		source string
		want   dnsv1.RecordSource
	}{
		{source: enginedns.SourceUser, want: dnsv1.RecordSource_RECORD_SOURCE_USER},
		{source: enginedns.SourceHosting, want: dnsv1.RecordSource_RECORD_SOURCE_HOSTING},
		{source: enginedns.SourceMail, want: dnsv1.RecordSource_RECORD_SOURCE_MAIL},
		{source: "nonsense", want: dnsv1.RecordSource_RECORD_SOURCE_USER},
	}
	for _, test := range tests {
		if got := enginedns.SourceToProto(test.source); got != test.want {
			t.Errorf("SourceToProto(%q) = %v, want %v", test.source, got, test.want)
		}
	}

	for _, value := range []dnsv1.RecordSource{
		dnsv1.RecordSource_RECORD_SOURCE_USER,
		dnsv1.RecordSource_RECORD_SOURCE_HOSTING,
		dnsv1.RecordSource_RECORD_SOURCE_MAIL,
	} {
		source, ok := enginedns.SourceFromProto(value)
		if !ok {
			t.Errorf("SourceFromProto(%v) reported an unknown value", value)
		}
		if enginedns.SourceToProto(source) != value {
			t.Errorf("SourceFromProto/SourceToProto round trip failed for %v", value)
		}
		if !enginedns.ValidSource(source) {
			t.Errorf("ValidSource(%q) = false", source)
		}
	}
	if _, ok := enginedns.SourceFromProto(dnsv1.RecordSource_RECORD_SOURCE_UNSPECIFIED); ok {
		t.Error("SourceFromProto accepted UNSPECIFIED")
	}
	if enginedns.SourceLabel(enginedns.SourceHosting) != "Website" || enginedns.SourceLabel(enginedns.SourceMail) != "Email" {
		t.Error("SourceLabel does not match the console vocabulary")
	}
}

func TestNopObserverSatisfiesTheInterface(t *testing.T) {
	t.Parallel()

	var observer enginedns.ZoneObserver = enginedns.NopObserver{}
	observer.ZoneReplaced(context.Background(), "z1")
	observer.RecordDeleted(context.Background(), "z1", zone.Record{})
	observer.ZoneDeleted(context.Background(), "z1")
}
