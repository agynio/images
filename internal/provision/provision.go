// Package provision declares the platform resources that must exist for a
// freshly installed platform to be usable at all.
//
// The ordinary creation path requires a signed-in user with organization
// ownership, and at install time there is no user, no organization, and nothing
// for an operator to click. So provisioning calls internal, non-Gateway RPCs
// that are create-if-absent: a call creates a resource when nothing of that name
// exists and returns the existing one otherwise. It never overwrites, which is
// what makes re-running an upgrade safe - an operator who edited a provisioned
// resource keeps their edit.
//
// The consequence is accepted deliberately: a release cannot correct metadata on
// a resource it provisioned earlier. For images this costs nothing, since a
// release publishes new tags rather than editing records.
package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	imagesv1 "github.com/agynio/image-catalog/gen/agynio/api/images/v1"
	organizationsv1 "github.com/agynio/image-catalog/gen/agynio/api/organizations/v1"
	"google.golang.org/grpc"
)

// Config is what a release declares. It is rendered from Helm values, so the
// set of provisioned resources is a property of the chart rather than of this
// binary.
type Config struct {
	Organization Organization `json:"organization"`
	Images       []Image      `json:"images"`
}

type Organization struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type Image struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Repository  string `json:"repository"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Visibility  string `json:"visibility"`
	TagFilter   string `json:"tagFilter"`
}

func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if config.Organization.Slug == "" {
		return Config{}, fmt.Errorf("%s: organization.slug is required", path)
	}
	return config, nil
}

type Organizations interface {
	RegisterPlatformOrganization(ctx context.Context, req *organizationsv1.RegisterPlatformOrganizationRequest, opts ...grpc.CallOption) (*organizationsv1.RegisterPlatformOrganizationResponse, error)
}

type Images interface {
	RegisterPlatformImage(ctx context.Context, req *imagesv1.RegisterPlatformImageRequest, opts ...grpc.CallOption) (*imagesv1.RegisterPlatformImageResponse, error)
}

// Runner applies a Config. Per-resource failures are reported but do not stop
// the run: a component whose provisioning fails does not block the platform
// from starting, and the next upgrade attempts it again.
type Runner struct {
	Organizations Organizations
	Images        Images
	Timeout       time.Duration
}

// Result reports what a run did, so the Job's logs say which resources were
// created and which are simply absent until the next upgrade.
type Result struct {
	OrganizationID string
	Created        []string
	Existing       []string
	Failed         []string
}

func (r *Runner) Run(ctx context.Context, config Config) (Result, error) {
	result := Result{}

	orgCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	organization, err := r.Organizations.RegisterPlatformOrganization(orgCtx, &organizationsv1.RegisterPlatformOrganizationRequest{
		Slug: config.Organization.Slug,
		Name: config.Organization.Name,
	})
	cancel()
	if err != nil {
		// Without the organization there is nothing to register images into,
		// so this one failure ends the run.
		return result, fmt.Errorf("register platform organization %q: %w", config.Organization.Slug, err)
	}
	result.OrganizationID = organization.GetOrganization().GetId()
	if organization.GetCreated() {
		log.Printf("provision: created organization %s (%s)", config.Organization.Slug, result.OrganizationID)
	} else {
		log.Printf("provision: organization %s already present (%s)", config.Organization.Slug, result.OrganizationID)
	}

	for _, image := range config.Images {
		imageType, err := imageType(image.Type)
		if err != nil {
			log.Printf("provision: image %s: %v", image.Name, err)
			result.Failed = append(result.Failed, image.Name)
			continue
		}
		visibility, err := visibility(image.Visibility)
		if err != nil {
			log.Printf("provision: image %s: %v", image.Name, err)
			result.Failed = append(result.Failed, image.Name)
			continue
		}

		imageCtx, cancel := context.WithTimeout(ctx, r.Timeout)
		registered, err := r.Images.RegisterPlatformImage(imageCtx, &imagesv1.RegisterPlatformImageRequest{
			OrganizationId: result.OrganizationID,
			Name:           image.Name,
			Description:    image.Description,
			Type:           imageType,
			Repository:     image.Repository,
			Username:       image.Username,
			Password:       image.Password,
			Visibility:     visibility,
			TagFilter:      image.TagFilter,
		})
		cancel()
		if err != nil {
			log.Printf("provision: register image %s: %v", image.Name, err)
			result.Failed = append(result.Failed, image.Name)
			continue
		}
		if registered.GetCreated() {
			result.Created = append(result.Created, image.Name)
			log.Printf("provision: created image %s", image.Name)
			continue
		}
		result.Existing = append(result.Existing, image.Name)
		log.Printf("provision: image %s already present", image.Name)
	}

	return result, nil
}

// The values file names types and visibilities the way the product does, so an
// operator editing the chart writes "workspace" rather than an enum constant.
func imageType(value string) (imagesv1.ImageType, error) {
	switch value {
	case "workspace":
		return imagesv1.ImageType_IMAGE_TYPE_WORKSPACE, nil
	case "agent_runtime":
		return imagesv1.ImageType_IMAGE_TYPE_AGENT_RUNTIME, nil
	case "mcp":
		return imagesv1.ImageType_IMAGE_TYPE_MCP, nil
	default:
		return 0, fmt.Errorf("type %q: must be workspace, agent_runtime, or mcp", value)
	}
}

func visibility(value string) (imagesv1.ImageVisibility, error) {
	switch value {
	// Platform images default to public, which is what makes them usable from
	// every organization on the platform.
	case "", "public":
		return imagesv1.ImageVisibility_IMAGE_VISIBILITY_PUBLIC, nil
	case "internal":
		return imagesv1.ImageVisibility_IMAGE_VISIBILITY_INTERNAL, nil
	default:
		return 0, fmt.Errorf("visibility %q: must be public or internal", value)
	}
}
