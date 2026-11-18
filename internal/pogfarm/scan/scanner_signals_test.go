package scan

import (
	"context"
	"strings"
	"testing"

	"github.com/salehjafarli/pogfarm/internal/config"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/store"
)

// scanViewerClipCalls counts the viewer-clip requests the fake source got.
func scanViewerClipCalls(f *scanFixture) int {
	n := 0
	for _, c := range f.src.Calls() {
		if c.Method == "ViewerClips" {
			n++
		}
	}
	return n
}

// The fixture's VOD has one chat spike at 10:00 and one viewer clip at
// 20:00, so each signal on its own yields exactly one candidate.

func TestScannerWithChatSpikesOff(t *testing.T) {
	f := newScanFixture(t)
	f.scanner.ChatSpikes = false
	f.addVOD("v1", store.ScanNew, 0)

	res, scanned := f.next()
	if !scanned || res.Err != nil || res.VOD.ScanStatus != store.ScanScanned {
		t.Fatalf("scanned %v, result %+v", scanned, res)
	}
	if f.src.chatCalls() != 0 {
		t.Errorf("%d chat requests, want none with chat spikes off", f.src.chatCalls())
	}
	if res.Spikes != 0 || res.Queued != (QueueResult{Candidates: 1, Jobs: 1}) {
		t.Errorf("spikes %d, queued %+v; want the viewer clip alone", res.Spikes, res.Queued)
	}
	if n := scanViewerClipCalls(f); n != 1 {
		t.Errorf("%d viewer-clip requests, want 1", n)
	}
}

func TestScannerWithViewerClipsOff(t *testing.T) {
	f := newScanFixture(t)
	f.scanner.ViewerClips = false
	f.addVOD("v1", store.ScanNew, 0)

	res, scanned := f.next()
	if !scanned || res.Err != nil || res.VOD.ScanStatus != store.ScanScanned {
		t.Fatalf("scanned %v, result %+v", scanned, res)
	}
	if n := scanViewerClipCalls(f); n != 0 {
		t.Errorf("%d viewer-clip requests, want none with viewer clips off", n)
	}
	if res.Spikes != 1 || res.Queued != (QueueResult{Candidates: 1, Jobs: 1}) {
		t.Errorf("spikes %d, queued %+v; want the chat spike alone", res.Spikes, res.Queued)
	}
	if f.src.chatCalls() != 1 {
		t.Errorf("%d chat requests, want 1", f.src.chatCalls())
	}
}

func TestScannerWithBothSignalsOffIsRejected(t *testing.T) {
	f := newScanFixture(t)
	f.scanner.ChatSpikes = false
	f.scanner.ViewerClips = false
	v := f.addVOD("v1", store.ScanNew, 0)
	ctx := context.Background()

	if _, _, err := f.scanner.ScanNext(ctx); err == nil || !strings.Contains(err.Error(), "both off") {
		t.Errorf("ScanNext: err = %v, want a both-off error", err)
	}
	if err := f.scanner.Run(ctx); err == nil || !strings.Contains(err.Error(), "both off") {
		t.Errorf("Run: err = %v, want a both-off error", err)
	}
	if got := f.vod(v.ID); got.ScanStatus != store.ScanNew || got.ScanAttempts != 0 {
		t.Errorf("the VOD was touched: %+v", got)
	}
}

// NewScanner carries the config switches over.
func TestNewScannerTakesTheSignalSwitches(t *testing.T) {
	for _, c := range []config.Scan{{ChatSpikes: false, ViewerClips: true}, {ChatSpikes: true, ViewerClips: false}} {
		s := NewScanner(nil, nil, nil, c, nil)
		if s.ChatSpikes != c.ChatSpikes || s.ViewerClips != c.ViewerClips {
			t.Errorf("config %+v gave ChatSpikes %v, ViewerClips %v", c, s.ChatSpikes, s.ViewerClips)
		}
	}
}
