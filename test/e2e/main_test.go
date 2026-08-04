//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	imagesv1 "github.com/agynio/image-catalog/gen/agynio/api/images/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

var imagesAddress = envOrDefault("IMAGES_ADDRESS", "images:50051")

// A repository that is anonymously readable and publishes tags the platform
// itself uses, so the tests exercise real discovery rather than a stub.
var publicRepository = envOrDefault("TEST_PUBLIC_REPOSITORY", "ghcr.io/agynio/devcontainer-go")

// Authoring an image needs an identity that owns the organization, and the
// visibility tests need one that does not belong to it. Defaults are the local
// VM's provisioned values:
//
//	select organization_id, identity_id, role from memberships;   -- organizations db
var (
	organizationID   = envOrDefault("IMAGES_ORGANIZATION_ID", "d265cc4c-ffa6-4b7e-991c-d1d7b2221217")
	ownerIdentityID  = envOrDefault("IMAGES_OWNER_IDENTITY_ID", "a3c1e9d2-7f4b-5e1a-9c3d-2b8f6a4e7d10")
	outsiderIdentity = envOrDefault("IMAGES_OUTSIDER_IDENTITY_ID", "546cb74c-e4a8-4d3c-906e-22f09f5f3bed")
)

func envOrDefault(key, fallback string) string {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	return trimmed
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func dial(t *testing.T) imagesv1.ImagesServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(imagesAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial images: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return imagesv1.NewImagesServiceClient(conn)
}

// internalContext is a caller reaching the service over the mesh: no identity,
// as the Orchestrator and the Image Proxy do.
func internalContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// asIdentity is a user request, carrying the identity the Gateway would attach.
func asIdentity(t *testing.T, identityID string) context.Context {
	t.Helper()
	return metadata.AppendToOutgoingContext(internalContext(t), "x-identity-id", identityID)
}

func asOwner(t *testing.T) context.Context { return asIdentity(t, ownerIdentityID) }
