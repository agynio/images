package discovery

import (
	"context"
	"errors"
	"log"
	"path"
	"time"

	"github.com/agynio/images/internal/registry"
	"github.com/agynio/images/internal/store"
	"github.com/google/uuid"
)

// metadataPerPass bounds how many manifests one pass reads. Listing tags is one
// request; reading a tag's push time and description is one request each, and a
// repository with hundreds of tags would make a full pass expensive. The rest
// are resolved on later passes and on first display.
const metadataPerPass = 20

// Catalog is the slice of the store discovery uses. It is an interface so the
// pass logic - what marks an image stale, what publishes - is testable without
// a database.
type Catalog interface {
	GetImage(ctx context.Context, id uuid.UUID) (store.Image, error)
	ClaimImagesForDiscovery(ctx context.Context, limit int, interval time.Duration) ([]store.Image, error)
	ReconcileVersions(ctx context.Context, imageID uuid.UUID, observed []store.ObservedTag) (bool, error)
	ListVersions(ctx context.Context, params store.ListVersionsParams) ([]store.Version, string, error)
	SetVersionMetadata(ctx context.Context, imageID uuid.UUID, tag string, pushedAt *time.Time, description string) error
	MarkDiscovered(ctx context.Context, imageID uuid.UUID, at time.Time) error
	MarkStale(ctx context.Context, imageID uuid.UUID, at time.Time) (bool, error)
	TouchDiscoveryDue(ctx context.Context, imageID uuid.UUID, interval time.Duration) error
}

// CredentialResolver reads an image's registry credential. Discovery holds it
// as an interface so it does not depend on the Secrets client directly.
type CredentialResolver interface {
	Resolve(ctx context.Context, image store.Image) (registry.Credential, error)
}

// Publisher receives the image.updated signal. Console lists and pickers react
// to it, so it fires when a version set changes or staleness flips - not on
// every pass.
type Publisher interface {
	ImageUpdated(ctx context.Context, image store.Image)
}

type Registry interface {
	ListTags(ctx context.Context, repository string, cred registry.Credential) ([]string, error)
	TagMetadata(ctx context.Context, repository, tag string, cred registry.Credential) (registry.Metadata, error)
}

type Discoverer struct {
	store       Catalog
	registry    Registry
	credentials CredentialResolver
	publisher   Publisher
	interval    time.Duration
	timeout     time.Duration
	batchSize   int
}

type Options struct {
	Interval  time.Duration
	Timeout   time.Duration
	BatchSize int
}

func New(s Catalog, reg Registry, credentials CredentialResolver, publisher Publisher, opts Options) *Discoverer {
	return &Discoverer{
		store:       s,
		registry:    reg,
		credentials: credentials,
		publisher:   publisher,
		interval:    opts.Interval,
		timeout:     opts.Timeout,
		batchSize:   opts.BatchSize,
	}
}

// Run polls due images until the context is cancelled.
func (d *Discoverer) Run(ctx context.Context) {
	ticker := time.NewTicker(d.tickInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runBatch(ctx)
		}
	}
}

func (d *Discoverer) tickInterval() time.Duration {
	// Tick more often than the poll interval so a batch-sized backlog drains
	// without waiting a full interval per batch.
	tick := d.interval / 4
	if tick < 10*time.Second {
		tick = 10 * time.Second
	}
	return tick
}

func (d *Discoverer) runBatch(ctx context.Context) {
	images, err := d.store.ClaimImagesForDiscovery(ctx, d.batchSize, d.interval)
	if err != nil {
		log.Printf("images: claim images for discovery: %v", err)
		return
	}
	for _, image := range images {
		passCtx, cancel := context.WithTimeout(ctx, d.timeout)
		if _, err := d.Pass(passCtx, image); err != nil {
			log.Printf("images: discovery pass for %s: %v", image.ID, err)
		}
		cancel()
	}
}

// metadataMode decides whether a pass waits for tag metadata.
type metadataMode int

const (
	// metadataInline resolves metadata before returning. Used by the poll
	// loop, which has nobody waiting on it.
	metadataInline metadataMode = iota
	// metadataDetached returns as soon as the tag list is reconciled and
	// resolves metadata afterwards.
	metadataDetached
)

// Pass runs one discovery pass: list the repository's tags, reconcile stored
// versions against them, and resolve metadata for tags that lack it. An
// unreachable repository marks the image stale and leaves stored versions
// intact.
func (d *Discoverer) Pass(ctx context.Context, image store.Image) (bool, error) {
	return d.run(ctx, image, metadataInline)
}

// Refresh reconciles the tag list synchronously and resolves metadata
// afterwards. It is what a picker or an image page opening calls: the tag list
// is what it needs to render, while reading a manifest per tag is seconds of
// upstream traffic for a push time beside a row.
func (d *Discoverer) Refresh(ctx context.Context, image store.Image) (bool, error) {
	return d.run(ctx, image, metadataDetached)
}

func (d *Discoverer) run(ctx context.Context, image store.Image, mode metadataMode) (bool, error) {
	credential, err := d.credentials.Resolve(ctx, image)
	if err != nil {
		return false, err
	}

	tags, err := d.registry.ListTags(ctx, image.Repository, credential)
	if err != nil {
		flipped, markErr := d.store.MarkStale(ctx, image.ID, time.Now().UTC())
		if markErr != nil {
			return false, markErr
		}
		if flipped {
			d.publish(ctx, image.ID)
		}
		return false, err
	}

	observed := make([]store.ObservedTag, 0, len(tags))
	for _, tag := range tags {
		observed = append(observed, store.ObservedTag{Tag: tag})
	}
	changed, err := d.store.ReconcileVersions(ctx, image.ID, observed)
	if err != nil {
		return false, err
	}

	switch mode {
	case metadataInline:
		if d.resolveMetadata(ctx, image, credential) {
			changed = true
		}
	case metadataDetached:
		d.resolveMetadataDetached(ctx, image, credential)
	}

	wasStale := image.StaleSince != nil
	if err := d.store.MarkDiscovered(ctx, image.ID, time.Now().UTC()); err != nil {
		return false, err
	}
	if changed || wasStale {
		d.publish(ctx, image.ID)
	}
	return changed, nil
}

// resolveMetadata reads manifests for stored tags that pass the image's filter
// and have no push time yet, up to metadataPerPass. A tag whose manifest cannot
// be read stays without metadata rather than failing the pass.
func (d *Discoverer) resolveMetadata(ctx context.Context, image store.Image, credential registry.Credential) bool {
	versions, _, err := d.store.ListVersions(ctx, store.ListVersionsParams{
		ImageID:  image.ID,
		PageSize: 100,
	})
	if err != nil {
		log.Printf("images: list versions for metadata on %s: %v", image.ID, err)
		return false
	}

	resolved := 0
	changed := false
	for _, version := range versions {
		if resolved >= metadataPerPass {
			break
		}
		if version.PushedAt != nil || !matchesFilter(image.TagFilter, version.Tag) {
			continue
		}
		metadata, err := d.registry.TagMetadata(ctx, image.Repository, version.Tag, credential)
		if err != nil {
			if errors.Is(err, registry.ErrUnauthorized) {
				continue
			}
			log.Printf("images: read metadata for %s:%s: %v", image.Repository, version.Tag, err)
			continue
		}
		resolved++
		if err := d.store.SetVersionMetadata(ctx, image.ID, version.Tag, metadata.PushedAt, metadata.Description); err != nil {
			log.Printf("images: store metadata for %s:%s: %v", image.Repository, version.Tag, err)
			continue
		}
		changed = true
	}
	return changed
}

// resolveMetadataDetached fills in push times and descriptions after the caller
// has been answered, publishing only if it found anything.
func (d *Discoverer) resolveMetadataDetached(ctx context.Context, image store.Image, credential registry.Credential) {
	// The request context is detached deliberately: this outlives the call that
	// triggered it, and it talks to a registry rather than the platform, so
	// nothing on the original context applies.
	metadataCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.timeout)
	go func() {
		defer cancel()
		if d.resolveMetadata(metadataCtx, image, credential) {
			d.publish(metadataCtx, image.ID)
		}
	}()
}

func (d *Discoverer) publish(ctx context.Context, imageID uuid.UUID) {
	if d.publisher == nil {
		return
	}
	image, err := d.store.GetImage(ctx, imageID)
	if err != nil {
		log.Printf("images: reload image %s for notification: %v", imageID, err)
		return
	}
	d.publisher.ImageUpdated(ctx, image)
}

// Interval reports the configured poll interval, so an on-demand refresh can
// push the next scheduled pass forward by the same amount.
func (d *Discoverer) Interval() time.Duration { return d.interval }

// Timeout reports the budget for one pass, so a caller running a pass outside
// the poll loop bounds it the same way.
func (d *Discoverer) Timeout() time.Duration { return d.timeout }

// matchesFilter reports whether a tag survives the image's tag_filter. An empty
// filter matches everything; otherwise the filter is a shell-style glob, which
// is the smallest thing that expresses "only the tags that look like releases"
// without asking a registrar to write a regular expression.
func matchesFilter(filter, tag string) bool {
	if filter == "" {
		return true
	}
	matched, err := path.Match(filter, tag)
	if err != nil {
		return true
	}
	return matched
}
