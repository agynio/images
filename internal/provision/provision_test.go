package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	imagesv1 "github.com/agynio/image-catalog/gen/agynio/api/images/v1"
	organizationsv1 "github.com/agynio/image-catalog/gen/agynio/api/organizations/v1"
	"google.golang.org/grpc"
)

type fakeOrganizations struct {
	id      string
	created bool
	err     error
	calls   int
}

func (f *fakeOrganizations) RegisterPlatformOrganization(context.Context, *organizationsv1.RegisterPlatformOrganizationRequest, ...grpc.CallOption) (*organizationsv1.RegisterPlatformOrganizationResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &organizationsv1.RegisterPlatformOrganizationResponse{
		Organization: &organizationsv1.Organization{Id: f.id},
		Created:      f.created,
	}, nil
}

type fakeImages struct {
	requests []*imagesv1.RegisterPlatformImageRequest
	created  map[string]bool
	failures map[string]error
}

func (f *fakeImages) RegisterPlatformImage(_ context.Context, req *imagesv1.RegisterPlatformImageRequest, _ ...grpc.CallOption) (*imagesv1.RegisterPlatformImageResponse, error) {
	f.requests = append(f.requests, req)
	if err, ok := f.failures[req.GetName()]; ok {
		return nil, err
	}
	return &imagesv1.RegisterPlatformImageResponse{
		Image:   &imagesv1.Image{Meta: &imagesv1.EntityMeta{Id: req.GetName()}},
		Created: f.created[req.GetName()],
	}, nil
}

func newRunner(orgs *fakeOrganizations, images *fakeImages) *Runner {
	return &Runner{Organizations: orgs, Images: images, Timeout: time.Second}
}

func testConfig() Config {
	return Config{
		Organization: Organization{Slug: "agyn-platform", Name: "Agyn Platform"},
		Images: []Image{
			{Name: "devcontainer-go", Type: "workspace", Repository: "ghcr.io/agynio/devcontainer-go"},
			{Name: "runtime-codex", Type: "agent_runtime", Repository: "ghcr.io/agynio/agyn-runtime-codex"},
		},
	}
}

func TestRunRegistersTheOrganizationAndItsImages(t *testing.T) {
	orgs := &fakeOrganizations{id: "org-1", created: true}
	images := &fakeImages{created: map[string]bool{"devcontainer-go": true, "runtime-codex": true}}

	result, err := newRunner(orgs, images).Run(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OrganizationID != "org-1" {
		t.Fatalf("organization = %q", result.OrganizationID)
	}
	if len(result.Created) != 2 {
		t.Fatalf("created %v, want both images", result.Created)
	}
	for _, req := range images.requests {
		if req.GetOrganizationId() != "org-1" {
			t.Fatalf("image %s registered into %q", req.GetName(), req.GetOrganizationId())
		}
		if req.GetVisibility() != imagesv1.ImageVisibility_IMAGE_VISIBILITY_PUBLIC {
			t.Fatalf("image %s is not public; platform images must be usable from every organization", req.GetName())
		}
	}
}

// Running an upgrade twice changes nothing the second time.
func TestRunReportsExistingResourcesAsUnchanged(t *testing.T) {
	orgs := &fakeOrganizations{id: "org-1", created: false}
	images := &fakeImages{created: map[string]bool{}}

	result, err := newRunner(orgs, images).Run(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Created) != 0 {
		t.Fatalf("created %v, want nothing", result.Created)
	}
	if len(result.Existing) != 2 {
		t.Fatalf("existing %v, want both images", result.Existing)
	}
}

// One image failing must not stop the rest: the resource is simply absent, and
// the next upgrade attempts it again.
func TestRunContinuesPastAFailedImage(t *testing.T) {
	orgs := &fakeOrganizations{id: "org-1"}
	images := &fakeImages{
		created:  map[string]bool{"runtime-codex": true},
		failures: map[string]error{"devcontainer-go": errors.New("upstream unreachable")},
	}

	result, err := newRunner(orgs, images).Run(context.Background(), testConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Failed) != 1 || result.Failed[0] != "devcontainer-go" {
		t.Fatalf("failed = %v", result.Failed)
	}
	if len(result.Created) != 1 || result.Created[0] != "runtime-codex" {
		t.Fatalf("created = %v", result.Created)
	}
}

// Without the organization there is nothing to register images into.
func TestRunStopsWhenTheOrganizationCannotBeRegistered(t *testing.T) {
	orgs := &fakeOrganizations{err: errors.New("connection refused")}
	images := &fakeImages{}

	if _, err := newRunner(orgs, images).Run(context.Background(), testConfig()); err == nil {
		t.Fatal("expected the run to report the failure")
	}
	if len(images.requests) != 0 {
		t.Fatal("expected no image registrations without an organization")
	}
}

func TestRunRejectsAnUnknownType(t *testing.T) {
	orgs := &fakeOrganizations{id: "org-1"}
	images := &fakeImages{created: map[string]bool{}}
	config := testConfig()
	config.Images = []Image{{Name: "odd", Type: "vm", Repository: "ghcr.io/agynio/odd"}}

	result, err := newRunner(orgs, images).Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Failed) != 1 {
		t.Fatalf("failed = %v, want the unknown type rejected", result.Failed)
	}
	if len(images.requests) != 0 {
		t.Fatal("expected an unknown type not to reach the service")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provision.json")
	if err := os.WriteFile(path, []byte(`{"organization":{"slug":"agyn-platform"},"images":[{"name":"a","type":"workspace","repository":"r"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.Organization.Slug != "agyn-platform" || len(config.Images) != 1 {
		t.Fatalf("config = %+v", config)
	}

	if err := os.WriteFile(path, []byte(`{"images":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected a config with no organization slug to be rejected")
	}
}
