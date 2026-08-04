package server

import (
	"strings"
	"testing"

	imagesv1 "github.com/agynio/image-catalog/gen/agynio/api/images/v1"
	"github.com/agynio/image-catalog/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func validCreate() *imagesv1.CreateImageRequest {
	return &imagesv1.CreateImageRequest{
		OrganizationId: "11111111-1111-1111-1111-111111111111",
		Name:           "devcontainer-go",
		Type:           imagesv1.ImageType_IMAGE_TYPE_WORKSPACE,
		Repository:     "ghcr.io/agynio/devcontainer-go",
		Visibility:     imagesv1.ImageVisibility_IMAGE_VISIBILITY_INTERNAL,
	}
}

func TestValidateCreateAcceptsAMinimalRecord(t *testing.T) {
	input, err := validateCreate(validCreate())
	if err != nil {
		t.Fatalf("validateCreate: %v", err)
	}
	if input.Name != "devcontainer-go" {
		t.Fatalf("name = %q", input.Name)
	}
	if input.Type != store.ImageTypeWorkspace {
		t.Fatalf("type = %q", input.Type)
	}
	if input.Visibility != store.VisibilityInternal {
		t.Fatalf("visibility = %q", input.Visibility)
	}
}

func TestValidateCreateRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*imagesv1.CreateImageRequest)
		message string
	}{
		{"empty name", func(r *imagesv1.CreateImageRequest) { r.Name = "" }, "name"},
		{"uppercase name", func(r *imagesv1.CreateImageRequest) { r.Name = "DevContainer" }, "name"},
		{"underscore name", func(r *imagesv1.CreateImageRequest) { r.Name = "dev_container" }, "name"},
		{"long name", func(r *imagesv1.CreateImageRequest) { r.Name = strings.Repeat("a", 65) }, "name"},
		{"missing type", func(r *imagesv1.CreateImageRequest) { r.Type = imagesv1.ImageType_IMAGE_TYPE_UNSPECIFIED }, "type"},
		{"missing visibility", func(r *imagesv1.CreateImageRequest) {
			r.Visibility = imagesv1.ImageVisibility_IMAGE_VISIBILITY_UNSPECIFIED
		}, "visibility"},
		{"empty repository", func(r *imagesv1.CreateImageRequest) { r.Repository = "" }, "repository"},
		{"repository with tag", func(r *imagesv1.CreateImageRequest) {
			r.Repository = "ghcr.io/agynio/devcontainer-go:1"
		}, "repository"},
		{"repository with digest", func(r *imagesv1.CreateImageRequest) {
			r.Repository = "ghcr.io/agynio/devcontainer-go@sha256:abc"
		}, "repository"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := validCreate()
			test.mutate(req)
			_, err := validateCreate(req)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
			}
			if !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error %q does not mention %q", err, test.message)
			}
		})
	}
}

// A registry with a port is a legitimate repository, and the colon in it must
// not read as a tag.
func TestValidateRepositoryAcceptsARegistryPort(t *testing.T) {
	if _, err := validateRepository("registry.internal:5000/team/image"); err != nil {
		t.Fatalf("validateRepository: %v", err)
	}
}

func TestValidateUpdateCarriesOnlyWhatWasSet(t *testing.T) {
	name := "renamed"
	input, err := validateUpdate(&imagesv1.UpdateImageRequest{
		Id:   "11111111-1111-1111-1111-111111111111",
		Name: &name,
	})
	if err != nil {
		t.Fatalf("validateUpdate: %v", err)
	}
	if input.Name == nil || *input.Name != "renamed" {
		t.Fatal("expected the name to be carried")
	}
	if input.Description != nil || input.Username != nil || input.Visibility != nil || input.TagFilter != nil {
		t.Fatal("expected untouched fields to stay absent")
	}
}

func TestValidateTagFilterRejectsAMalformedPattern(t *testing.T) {
	if err := validateTagFilter("v[1-"); err == nil {
		t.Fatal("expected a malformed glob to be rejected")
	}
	if err := validateTagFilter(""); err != nil {
		t.Fatalf("an empty filter is valid: %v", err)
	}
}
