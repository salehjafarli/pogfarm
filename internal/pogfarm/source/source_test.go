package source_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salehjafarli/pogfarm/internal/api"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source"
	"github.com/salehjafarli/pogfarm/internal/pogfarm/source/sourcetest"
)

// The fakes must satisfy the interfaces they stand in for, and the plain
// fake must not have the optional capability.
var (
	_ source.Source           = (*sourcetest.Fake)(nil)
	_ source.Source           = (*sourcetest.FakeWithClips)(nil)
	_ source.ViewerClipLister = (*sourcetest.FakeWithClips)(nil)
)

func TestRegistryGet(t *testing.T) {
	a := sourcetest.New("alpha")
	b := sourcetest.New("beta")
	r, err := source.NewRegistry(b, a)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []source.Source{a, b} {
		got, err := r.Get(want.Platform())
		if err != nil {
			t.Fatalf("Get(%q): %v", want.Platform(), err)
		}
		if got != want {
			t.Errorf("Get(%q) returned a different source", want.Platform())
		}
	}

	if got, want := r.Platforms(), []api.Platform{"alpha", "beta"}; !slices.Equal(got, want) {
		t.Errorf("Platforms() = %v, want %v", got, want)
	}
}

func TestRegistryUnknownPlatform(t *testing.T) {
	r, err := source.NewRegistry(sourcetest.New("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Get("gamma")
	if !errors.Is(err, source.ErrUnknownPlatform) {
		t.Fatalf("err = %v, want ErrUnknownPlatform", err)
	}
	if s != nil {
		t.Errorf("got a source for an unknown platform")
	}
	if !strings.Contains(err.Error(), "gamma") {
		t.Errorf("error %q does not name the platform", err)
	}
}

func TestRegistryZeroValue(t *testing.T) {
	var r source.Registry
	if _, err := r.Get("alpha"); !errors.Is(err, source.ErrUnknownPlatform) {
		t.Errorf("Get on empty registry: err = %v, want ErrUnknownPlatform", err)
	}
	if got := r.Platforms(); len(got) != 0 {
		t.Errorf("Platforms() = %v, want none", got)
	}
	if err := r.Register(sourcetest.New("alpha")); err != nil {
		t.Fatalf("Register on zero value: %v", err)
	}
	if _, err := r.Get("alpha"); err != nil {
		t.Errorf("Get after Register: %v", err)
	}
}

func TestRegistryRejects(t *testing.T) {
	first := sourcetest.New("alpha")
	r, err := source.NewRegistry(first)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Register(sourcetest.New("alpha")); err == nil {
		t.Error("registering a platform twice succeeded")
	}
	if got, _ := r.Get("alpha"); got != first {
		t.Error("a rejected duplicate replaced the registered source")
	}
	if err := r.Register(nil); err == nil {
		t.Error("registering nil succeeded")
	}
	if err := r.Register(sourcetest.New("")); err == nil {
		t.Error("registering an empty platform name succeeded")
	}

	if _, err := source.NewRegistry(sourcetest.New("alpha"), sourcetest.New("alpha")); err == nil {
		t.Error("NewRegistry with a duplicate platform succeeded")
	}
}

func TestRegistryConcurrentUse(t *testing.T) {
	var r source.Registry
	names := []api.Platform{"a", "b", "c", "d", "e", "f", "g", "h"}

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Go(func() {
			if err := r.Register(sourcetest.New(name)); err != nil {
				t.Errorf("Register(%q): %v", name, err)
			}
		})
		wg.Go(func() {
			r.Get(name) // may or may not be registered yet
			r.Platforms()
		})
	}
	wg.Wait()

	if got := r.Platforms(); !slices.Equal(got, names) {
		t.Errorf("Platforms() = %v, want %v", got, names)
	}
}

func TestViewerClipsOptionalCapability(t *testing.T) {
	ctx := context.Background()
	vod := source.VOD{ID: "v1", Streamer: api.Streamer{Login: "streamer"}}
	clips := []source.ViewerClip{{Offset: time.Minute, Duration: 30 * time.Second, Views: 12, Title: "wow"}}

	without := sourcetest.New("alpha")
	without.AddVOD(vod)
	without.SetViewerClips(vod.ID, clips)
	got, err := source.ViewerClips(ctx, without, vod)
	if !errors.Is(err, source.ErrViewerClipsUnsupported) {
		t.Errorf("source without the capability: err = %v, want ErrViewerClipsUnsupported", err)
	}
	if got != nil {
		t.Errorf("source without the capability returned clips: %v", got)
	}

	with := sourcetest.NewWithClips("alpha")
	with.AddVOD(vod)
	with.SetViewerClips(vod.ID, clips)
	got, err = source.ViewerClips(ctx, with, vod)
	if err != nil {
		t.Fatalf("source with the capability: %v", err)
	}
	if !reflect.DeepEqual(got, clips) {
		t.Errorf("got %+v, want %+v", got, clips)
	}

	// Errors of the implementation pass through unchanged.
	if _, err := source.ViewerClips(ctx, with, source.VOD{ID: "missing"}); !errors.Is(err, source.ErrVODNotFound) {
		t.Errorf("unknown VOD: err = %v, want ErrVODNotFound", err)
	}
}

// The common types carry exactly what the rest of pogfarm needs and nothing
// that belongs to one platform.
func TestCommonTypesArePlatformNeutral(t *testing.T) {
	want := map[reflect.Type][]string{
		reflect.TypeFor[source.VOD]():         {"Platform", "ID", "Streamer", "Title", "StartedAt", "Duration", "Ended"},
		reflect.TypeFor[source.ChatMessage](): {"Offset", "User", "Text", "Emotes"},
		reflect.TypeFor[source.ViewerClip]():  {"Offset", "Duration", "Views", "Title"},
	}
	for typ, fields := range want {
		var got []string
		for i := range typ.NumField() {
			got = append(got, typ.Field(i).Name)
		}
		if !slices.Equal(got, fields) {
			t.Errorf("%s fields = %v, want %v", typ.Name(), got, fields)
		}
	}
}

// No platform is named anywhere in the package or its fake: not in a type,
// a field, a constant or a comment. Test files are exempt.
func TestPackageNamesNoPlatform(t *testing.T) {
	// Assembled so this file does not contain the word either.
	banned := []string{"twi" + "tch", "he" + "lix"}

	var checked int
	for _, dir := range []string{".", "sourcetest"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			lower := strings.ToLower(string(data))
			for _, word := range banned {
				if strings.Contains(lower, word) {
					t.Errorf("%s mentions %q", file, word)
				}
			}
		}
	}
	if checked < 3 {
		t.Fatalf("checked only %d files; the test is not looking at the package", checked)
	}
}
