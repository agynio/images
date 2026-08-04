package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agynio/image-catalog/internal/registry"
	"github.com/agynio/image-catalog/internal/store"
	"github.com/google/uuid"
)

type fakeCatalog struct {
	image        store.Image
	versions     []store.Version
	reconciled   []store.ObservedTag
	changed      bool
	markedStale  bool
	staleFlipped bool
	discovered   bool
	metadataSet  map[string]registry.Metadata
}

func newFakeCatalog(image store.Image) *fakeCatalog {
	return &fakeCatalog{image: image, metadataSet: map[string]registry.Metadata{}}
}

func (f *fakeCatalog) GetImage(context.Context, uuid.UUID) (store.Image, error) { return f.image, nil }

func (f *fakeCatalog) ClaimImagesForDiscovery(context.Context, int, time.Duration) ([]store.Image, error) {
	return []store.Image{f.image}, nil
}

func (f *fakeCatalog) ReconcileVersions(_ context.Context, _ uuid.UUID, observed []store.ObservedTag) (bool, error) {
	f.reconciled = observed
	return f.changed, nil
}

func (f *fakeCatalog) ListVersions(context.Context, store.ListVersionsParams) ([]store.Version, string, error) {
	return f.versions, "", nil
}

func (f *fakeCatalog) SetVersionMetadata(_ context.Context, _ uuid.UUID, tag string, pushedAt *time.Time, description string) error {
	f.metadataSet[tag] = registry.Metadata{PushedAt: pushedAt, Description: description}
	return nil
}

func (f *fakeCatalog) MarkDiscovered(context.Context, uuid.UUID, time.Time) error {
	f.discovered = true
	f.image.StaleSince = nil
	return nil
}

func (f *fakeCatalog) MarkStale(context.Context, uuid.UUID, time.Time) (bool, error) {
	f.markedStale = true
	return f.staleFlipped, nil
}

func (f *fakeCatalog) TouchDiscoveryDue(context.Context, uuid.UUID, time.Duration) error { return nil }

type fakeRegistry struct {
	tags     []string
	listErr  error
	metadata map[string]registry.Metadata
	metaErr  error
}

func (f *fakeRegistry) ListTags(context.Context, string, registry.Credential) ([]string, error) {
	return f.tags, f.listErr
}

func (f *fakeRegistry) TagMetadata(_ context.Context, _, tag string, _ registry.Credential) (registry.Metadata, error) {
	if f.metaErr != nil {
		return registry.Metadata{}, f.metaErr
	}
	return f.metadata[tag], nil
}

type fakeCredentials struct{}

func (fakeCredentials) Resolve(context.Context, store.Image) (registry.Credential, error) {
	return registry.Credential{}, nil
}

type recordingPublisher struct{ calls int }

func (r *recordingPublisher) ImageUpdated(context.Context, store.Image) { r.calls++ }

func newDiscoverer(catalog Catalog, reg Registry, publisher Publisher) *Discoverer {
	return New(catalog, reg, fakeCredentials{}, publisher, Options{
		Interval:  time.Minute,
		Timeout:   time.Second,
		BatchSize: 1,
	})
}

func testImage() store.Image {
	return store.Image{
		ID:         uuid.New(),
		Repository: "ghcr.io/agynio/example",
		Type:       store.ImageTypeWorkspace,
		Visibility: store.VisibilityInternal,
	}
}

func TestPassPublishesWhenVersionsChange(t *testing.T) {
	catalog := newFakeCatalog(testImage())
	catalog.changed = true
	publisher := &recordingPublisher{}

	changed, err := newDiscoverer(catalog, &fakeRegistry{tags: []string{"1.0.0", "latest"}}, publisher).
		Pass(context.Background(), catalog.image)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if !changed {
		t.Fatal("expected the pass to report a change")
	}
	if len(catalog.reconciled) != 2 {
		t.Fatalf("reconciled %d tags, want 2", len(catalog.reconciled))
	}
	if publisher.calls != 1 {
		t.Fatalf("published %d times, want 1", publisher.calls)
	}
}

func TestPassIsQuietWhenNothingChanges(t *testing.T) {
	catalog := newFakeCatalog(testImage())
	publisher := &recordingPublisher{}

	if _, err := newDiscoverer(catalog, &fakeRegistry{tags: []string{"1.0.0"}}, publisher).
		Pass(context.Background(), catalog.image); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if publisher.calls != 0 {
		t.Fatalf("published %d times, want 0", publisher.calls)
	}
	if !catalog.discovered {
		t.Fatal("expected a successful pass to be recorded")
	}
}

// An unreachable repository degrades freshness, not the ability to read the
// catalog: the image is flagged and stored versions are left alone.
func TestPassMarksStaleWithoutTouchingVersions(t *testing.T) {
	catalog := newFakeCatalog(testImage())
	catalog.staleFlipped = true
	publisher := &recordingPublisher{}

	_, err := newDiscoverer(catalog, &fakeRegistry{listErr: errors.New("dial tcp: no route to host")}, publisher).
		Pass(context.Background(), catalog.image)
	if err == nil {
		t.Fatal("expected the upstream failure to be reported")
	}
	if !catalog.markedStale {
		t.Fatal("expected the image to be marked stale")
	}
	if catalog.reconciled != nil {
		t.Fatal("expected stored versions to be left alone")
	}
	if publisher.calls != 1 {
		t.Fatalf("published %d times, want 1 for the staleness flip", publisher.calls)
	}
}

// Staleness that does not flip is not news, so it does not publish.
func TestPassDoesNotRepublishPersistentStaleness(t *testing.T) {
	catalog := newFakeCatalog(testImage())
	catalog.staleFlipped = false
	publisher := &recordingPublisher{}

	_, _ = newDiscoverer(catalog, &fakeRegistry{listErr: errors.New("timeout")}, publisher).
		Pass(context.Background(), catalog.image)
	if publisher.calls != 0 {
		t.Fatalf("published %d times, want 0", publisher.calls)
	}
}

// Recovery is news even when the version set is unchanged, because the Console
// shows staleness.
func TestPassPublishesOnRecovery(t *testing.T) {
	image := testImage()
	staleSince := time.Now().Add(-time.Hour)
	image.StaleSince = &staleSince
	catalog := newFakeCatalog(image)
	publisher := &recordingPublisher{}

	if _, err := newDiscoverer(catalog, &fakeRegistry{tags: []string{"1.0.0"}}, publisher).
		Pass(context.Background(), image); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if publisher.calls != 1 {
		t.Fatalf("published %d times, want 1", publisher.calls)
	}
}

func TestResolveMetadataSkipsTagsOutsideTheFilter(t *testing.T) {
	image := testImage()
	image.TagFilter = "v*"
	catalog := newFakeCatalog(image)
	catalog.versions = []store.Version{
		{ImageID: image.ID, Tag: "v1.2.3"},
		{ImageID: image.ID, Tag: "sha-abcdef"},
	}
	pushed := time.Now()
	reg := &fakeRegistry{
		tags:     []string{"v1.2.3", "sha-abcdef"},
		metadata: map[string]registry.Metadata{"v1.2.3": {PushedAt: &pushed, Description: "release"}},
	}

	if _, err := newDiscoverer(catalog, reg, &recordingPublisher{}).Pass(context.Background(), image); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if _, ok := catalog.metadataSet["v1.2.3"]; !ok {
		t.Fatal("expected metadata for the filtered-in tag")
	}
	if _, ok := catalog.metadataSet["sha-abcdef"]; ok {
		t.Fatal("expected no metadata request for the filtered-out tag")
	}
}

// A tag already carrying a push time is not re-read: metadata is one request
// per tag, and re-reading every tag on every pass is what lazy resolution
// avoids.
func TestResolveMetadataSkipsTagsThatAlreadyHaveIt(t *testing.T) {
	image := testImage()
	catalog := newFakeCatalog(image)
	pushed := time.Now()
	catalog.versions = []store.Version{{ImageID: image.ID, Tag: "1.0.0", PushedAt: &pushed}}

	if _, err := newDiscoverer(catalog, &fakeRegistry{tags: []string{"1.0.0"}}, &recordingPublisher{}).
		Pass(context.Background(), image); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if len(catalog.metadataSet) != 0 {
		t.Fatalf("resolved metadata for %d tags, want 0", len(catalog.metadataSet))
	}
}

func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		filter string
		tag    string
		want   bool
	}{
		{"", "anything", true},
		{"v*", "v1.2.3", true},
		{"v*", "1.2.3", false},
		{"release-*", "release-2026", true},
		{"[", "anything", true}, // a malformed filter must not hide every tag
	}
	for _, test := range tests {
		if got := matchesFilter(test.filter, test.tag); got != test.want {
			t.Errorf("matchesFilter(%q, %q) = %v, want %v", test.filter, test.tag, got, test.want)
		}
	}
}

// A picker waits for the tag list, not for a manifest read per tag.
func TestRefreshDoesNotWaitForMetadata(t *testing.T) {
	image := testImage()
	catalog := newFakeCatalog(image)
	catalog.versions = []store.Version{{ImageID: image.ID, Tag: "1.0.0"}}
	blocked := make(chan struct{})
	reg := &blockingRegistry{tags: []string{"1.0.0"}, release: blocked}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := newDiscoverer(catalog, reg, &recordingPublisher{}).Refresh(context.Background(), image); err != nil {
			t.Errorf("Refresh: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Refresh waited for tag metadata")
	}
	close(blocked)
}

// blockingRegistry answers ListTags immediately and holds TagMetadata until
// released, so a caller that waits for metadata is visible as a hang.
type blockingRegistry struct {
	tags    []string
	release chan struct{}
}

func (b *blockingRegistry) ListTags(context.Context, string, registry.Credential) ([]string, error) {
	return b.tags, nil
}

func (b *blockingRegistry) TagMetadata(ctx context.Context, _, _ string, _ registry.Credential) (registry.Metadata, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return registry.Metadata{}, nil
}
