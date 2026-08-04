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
)

var imagesAddress = envOrDefault("IMAGES_ADDRESS", "images:50051")

// A repository that is anonymously readable and publishes tags the platform
// itself uses, so the tests exercise real discovery rather than a stub.
var publicRepository = envOrDefault("TEST_PUBLIC_REPOSITORY", "ghcr.io/agynio/devcontainer-go")

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

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}
