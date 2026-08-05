package registry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

const descriptionLabel = "org.opencontainers.image.description"

// ErrUnauthorized separates "the credential is wrong" from "the registry is
// unreachable". The first is a create-time validation failure the registrar can
// fix; the second is staleness.
var ErrUnauthorized = errors.New("registry rejected the credential")

type Credential struct {
	Username string
	Password string
}

func (c Credential) authenticator() authn.Authenticator {
	if c.Username == "" && c.Password == "" {
		return authn.Anonymous
	}
	return &authn.Basic{Username: c.Username, Password: c.Password}
}

// Metadata is what a tag's manifest says about itself. Both fields are best
// effort: a registry may serve a manifest the platform cannot introspect, and a
// tag with no metadata is still a usable tag.
type Metadata struct {
	PushedAt    *time.Time
	Description string
}

type Client struct{}

func New() *Client { return &Client{} }

// ListTags reads the repository's tag list. It doubles as the readability
// check performed before an image record is stored, so a typo or a wrong
// credential fails at registration rather than at workload start.
func (c *Client) ListTags(ctx context.Context, repository string, cred Credential) ([]string, error) {
	repo, err := name.NewRepository(repository)
	if err != nil {
		return nil, fmt.Errorf("parse repository %q: %w", repository, err)
	}
	tags, err := remote.List(repo, remote.WithContext(ctx), remote.WithAuth(cred.authenticator()))
	if err != nil {
		return nil, classify(err)
	}
	return tags, nil
}

// TagMetadata reads one tag's manifest. Listing tags is one request and this is
// another per tag, which is why it is resolved lazily rather than for every tag
// on every pass.
func (c *Client) TagMetadata(ctx context.Context, repository, tag string, cred Credential) (Metadata, error) {
	ref, err := name.NewTag(fmt.Sprintf("%s:%s", repository, tag))
	if err != nil {
		return Metadata{}, fmt.Errorf("parse reference %q: %w", repository+":"+tag, err)
	}
	image, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(cred.authenticator()))
	if err != nil {
		return Metadata{}, classify(err)
	}
	return metadataFromConfig(image)
}

func metadataFromConfig(image v1.Image) (Metadata, error) {
	config, err := image.ConfigFile()
	if err != nil {
		return Metadata{}, classify(err)
	}
	metadata := Metadata{Description: config.Config.Labels[descriptionLabel]}
	if !config.Created.Time.IsZero() {
		pushedAt := config.Created.Time
		metadata.PushedAt = &pushedAt
	}
	return metadata, nil
}

func classify(err error) error {
	var transportErr *transport.Error
	if errors.As(err, &transportErr) {
		switch transportErr.StatusCode {
		case 401, 403:
			return fmt.Errorf("%w: %v", ErrUnauthorized, err)
		case 404:
			return fmt.Errorf("%w: repository not found: %v", ErrUnauthorized, err)
		}
	}
	return err
}
