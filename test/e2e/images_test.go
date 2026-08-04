//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	imagesv1 "github.com/agynio/image-catalog/gen/agynio/api/images/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func registerImage(t *testing.T, ctx context.Context, client imagesv1.ImagesServiceClient, req *imagesv1.CreateImageRequest) *imagesv1.Image {
	t.Helper()
	resp, err := client.CreateImage(ctx, req)
	if err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	image := resp.GetImage()
	t.Cleanup(func() {
		_, _ = client.DeleteImage(context.Background(), &imagesv1.DeleteImageRequest{Id: image.GetMeta().GetId()})
	})
	return image
}

func newImageRequest(organizationID string, name string) *imagesv1.CreateImageRequest {
	return &imagesv1.CreateImageRequest{
		OrganizationId: organizationID,
		Name:           name,
		Type:           imagesv1.ImageType_IMAGE_TYPE_WORKSPACE,
		Repository:     publicRepository,
		Visibility:     imagesv1.ImageVisibility_IMAGE_VISIBILITY_INTERNAL,
	}
}

// Registering an image is one step, and its tags appear without anyone entering
// a version. Discovery runs in the background, so the tags arrive shortly after
// the record rather than with it.
func TestVersionsAppearWithoutBeingRegistered(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	organizationID := uuid.NewString()

	image := registerImage(t, ctx, client, newImageRequest(organizationID, "e2e-"+shortID()))
	if versions := awaitVersions(t, ctx, client, image.GetMeta().GetId()); len(versions) == 0 {
		t.Fatal("expected discovery to find tags without anyone registering them")
	}
}

// awaitVersions waits for the background pass triggered by registration.
func awaitVersions(t *testing.T, ctx context.Context, client imagesv1.ImagesServiceClient, imageID string) []*imagesv1.ImageVersion {
	t.Helper()
	for attempt := 0; attempt < 60; attempt++ {
		versions, err := client.ListVersions(ctx, &imagesv1.ListVersionsRequest{ImageId: imageID})
		if err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
		if len(versions.GetVersions()) > 0 {
			return versions.GetVersions()
		}
		time.Sleep(time.Second)
	}
	return nil
}

// A typo or a wrong credential fails at registration rather than at workload
// start.
func TestRegisteringAnUnreadableRepositoryFails(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)

	req := newImageRequest(uuid.NewString(), "e2e-"+shortID())
	req.Repository = "ghcr.io/agynio/does-not-exist-" + shortID()

	_, err := client.CreateImage(ctx, req)
	if err == nil {
		t.Fatal("expected registration against an unreadable repository to fail")
	}
	code := status.Code(err)
	if code != codes.InvalidArgument && code != codes.FailedPrecondition {
		t.Fatalf("code = %v, want InvalidArgument or FailedPrecondition", code)
	}
}

// repository and type are statements about what the record is; there is no way
// to change either.
func TestRepositoryAndTypeAreImmutable(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	image := registerImage(t, ctx, client, newImageRequest(uuid.NewString(), "e2e-"+shortID()))

	updated, err := client.UpdateImage(ctx, &imagesv1.UpdateImageRequest{
		Id:          image.GetMeta().GetId(),
		Description: strPtr("edited"),
	})
	if err != nil {
		t.Fatalf("UpdateImage: %v", err)
	}
	if updated.GetImage().GetRepository() != image.GetRepository() {
		t.Fatal("repository changed")
	}
	if updated.GetImage().GetType() != image.GetType() {
		t.Fatal("type changed")
	}
}

func TestNameIsUniqueWithinTheOrganization(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	organizationID := uuid.NewString()
	name := "e2e-" + shortID()

	registerImage(t, ctx, client, newImageRequest(organizationID, name))

	_, err := client.CreateImage(ctx, newImageRequest(organizationID, name))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists", status.Code(err))
	}
}

// A public image registered in one organization is selectable from another; an
// internal one is not.
func TestVisibilityDecidesCrossOrganizationListing(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	owner := uuid.NewString()
	consumer := uuid.NewString()

	internal := registerImage(t, ctx, client, newImageRequest(owner, "e2e-internal-"+shortID()))
	publicReq := newImageRequest(owner, "e2e-public-"+shortID())
	publicReq.Visibility = imagesv1.ImageVisibility_IMAGE_VISIBILITY_PUBLIC
	shared := registerImage(t, ctx, client, publicReq)

	listed, err := client.ListImages(ctx, &imagesv1.ListImagesRequest{OrganizationId: consumer, PageSize: 100})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	names := map[string]bool{}
	for _, image := range listed.GetImages() {
		names[image.GetMeta().GetId()] = true
	}
	if !names[shared.GetMeta().GetId()] {
		t.Fatal("expected the public image to be listed for another organization")
	}
	if names[internal.GetMeta().GetId()] {
		t.Fatal("expected the internal image to be hidden from another organization")
	}
}

func TestListImagesFiltersByType(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	organizationID := uuid.NewString()

	workspace := registerImage(t, ctx, client, newImageRequest(organizationID, "e2e-ws-"+shortID()))
	runtimeReq := newImageRequest(organizationID, "e2e-rt-"+shortID())
	runtimeReq.Type = imagesv1.ImageType_IMAGE_TYPE_AGENT_RUNTIME
	agentRuntime := registerImage(t, ctx, client, runtimeReq)

	listed, err := client.ListImages(ctx, &imagesv1.ListImagesRequest{
		OrganizationId: organizationID,
		Type:           imagesv1.ImageType_IMAGE_TYPE_AGENT_RUNTIME,
		PageSize:       100,
	})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	found := map[string]bool{}
	for _, image := range listed.GetImages() {
		found[image.GetMeta().GetId()] = true
	}
	if !found[agentRuntime.GetMeta().GetId()] {
		t.Fatal("expected the agent_runtime image in a type-filtered list")
	}
	if found[workspace.GetMeta().GetId()] {
		t.Fatal("expected the workspace image to be filtered out")
	}
}

// RefreshImage is what a picker calls on open, so a freshly pushed tag appears
// without waiting for the poll.
func TestRefreshReturnsVersions(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	image := registerImage(t, ctx, client, newImageRequest(uuid.NewString(), "e2e-"+shortID()))

	refreshed, err := client.RefreshImage(ctx, &imagesv1.RefreshImageRequest{ImageId: image.GetMeta().GetId()})
	if err != nil {
		t.Fatalf("RefreshImage: %v", err)
	}
	if len(refreshed.GetVersions()) == 0 {
		t.Fatal("expected a refresh to return versions")
	}
	if refreshed.GetImage().GetStaleSince() != nil {
		t.Fatal("expected a reachable repository not to be flagged stale")
	}
}

// The catalog resolves a reference to an upstream repository; a tag it never
// discovered is not resolvable.
func TestResolveVersion(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	image := registerImage(t, ctx, client, newImageRequest(uuid.NewString(), "e2e-"+shortID()))

	versions := awaitVersions(t, ctx, client, image.GetMeta().GetId())
	if len(versions) == 0 {
		t.Skip("no versions discovered; nothing to resolve")
	}
	tag := versions[0].GetTag()

	resolved, err := client.ResolveVersion(ctx, &imagesv1.ResolveVersionRequest{
		Reference: &imagesv1.ResolveVersionRequest_ImageId{ImageId: image.GetMeta().GetId()},
		Tag:       tag,
	})
	if err != nil {
		t.Fatalf("ResolveVersion: %v", err)
	}
	if resolved.GetRepository() != publicRepository {
		t.Fatalf("repository = %q, want %q", resolved.GetRepository(), publicRepository)
	}

	_, err = client.ResolveVersion(ctx, &imagesv1.ResolveVersionRequest{
		Reference: &imagesv1.ResolveVersionRequest_ImageId{ImageId: image.GetMeta().GetId()},
		Tag:       "tag-that-was-never-discovered",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

// An internal image is not resolvable by another organization, so a reference
// cannot outlive the visibility that allowed it.
func TestResolveVersionEnforcesVisibility(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	owner := uuid.NewString()
	image := registerImage(t, ctx, client, newImageRequest(owner, "e2e-"+shortID()))

	versions := awaitVersions(t, ctx, client, image.GetMeta().GetId())
	if len(versions) == 0 {
		t.Skip("no versions discovered; nothing to resolve")
	}

	_, err := client.ResolveVersion(ctx, &imagesv1.ResolveVersionRequest{
		Reference:              &imagesv1.ResolveVersionRequest_ImageId{ImageId: image.GetMeta().GetId()},
		Tag:                    versions[0].GetTag(),
		ConsumerOrganizationId: uuid.NewString(),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

// Re-running provisioning must change nothing, and must not overwrite an edit
// an operator made by hand.
func TestRegisterPlatformImageIsCreateIfAbsent(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	organizationID := uuid.NewString()
	name := "e2e-platform-" + shortID()

	first, err := client.RegisterPlatformImage(ctx, &imagesv1.RegisterPlatformImageRequest{
		OrganizationId: organizationID,
		Name:           name,
		Description:    "as shipped",
		Type:           imagesv1.ImageType_IMAGE_TYPE_WORKSPACE,
		Repository:     publicRepository,
		Visibility:     imagesv1.ImageVisibility_IMAGE_VISIBILITY_PUBLIC,
	})
	if err != nil {
		t.Fatalf("RegisterPlatformImage: %v", err)
	}
	t.Cleanup(func() {
		_, _ = client.DeleteImage(context.Background(), &imagesv1.DeleteImageRequest{Id: first.GetImage().GetMeta().GetId()})
	})
	if !first.GetCreated() {
		t.Fatal("expected the first registration to create")
	}

	if _, err := client.UpdateImage(ctx, &imagesv1.UpdateImageRequest{
		Id:          first.GetImage().GetMeta().GetId(),
		Description: strPtr("edited by an operator"),
	}); err != nil {
		t.Fatalf("UpdateImage: %v", err)
	}

	second, err := client.RegisterPlatformImage(ctx, &imagesv1.RegisterPlatformImageRequest{
		OrganizationId: organizationID,
		Name:           name,
		Description:    "as shipped",
		Type:           imagesv1.ImageType_IMAGE_TYPE_WORKSPACE,
		Repository:     publicRepository,
		Visibility:     imagesv1.ImageVisibility_IMAGE_VISIBILITY_PUBLIC,
	})
	if err != nil {
		t.Fatalf("RegisterPlatformImage (second run): %v", err)
	}
	if second.GetCreated() {
		t.Fatal("expected the second registration to be a no-op")
	}
	if second.GetImage().GetDescription() != "edited by an operator" {
		t.Fatalf("description = %q, want the operator's edit to survive", second.GetImage().GetDescription())
	}
}

// Deleting an image is permitted regardless of references: the platform flags a
// missing late-bound target rather than blocking the delete.
func TestDeleteIsNotBlocked(t *testing.T) {
	ctx := testContext(t)
	client := dial(t)
	image := registerImage(t, ctx, client, newImageRequest(uuid.NewString(), "e2e-"+shortID()))

	if _, err := client.DeleteImage(ctx, &imagesv1.DeleteImageRequest{Id: image.GetMeta().GetId()}); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	_, err := client.GetImage(ctx, &imagesv1.GetImageRequest{Id: image.GetMeta().GetId()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func strPtr(value string) *string { return &value }

func shortID() string { return uuid.NewString()[:8] }
