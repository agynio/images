// Command provisioner registers the platform's own resources on install and
// upgrade. See internal/provision for why it exists and what create-if-absent
// buys.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	imagesv1 "github.com/agynio/images/gen/agynio/api/images/v1"
	organizationsv1 "github.com/agynio/images/gen/agynio/api/organizations/v1"
	"github.com/agynio/images/internal/provision"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := run(); err != nil {
		// A configuration error is the operator's to fix and is worth failing
		// on. Everything else is reported and left for the next upgrade; see
		// provisionFailureIsNotFatal below.
		log.Fatalf("provisioner: %v", err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	configPath := envOrDefault("PROVISION_CONFIG", "/etc/agyn/provision/provision.json")
	config, err := provision.LoadConfig(configPath)
	if err != nil {
		return err
	}

	organizationsTarget := os.Getenv("ORGANIZATIONS_GRPC_TARGET")
	if organizationsTarget == "" {
		return fmt.Errorf("ORGANIZATIONS_GRPC_TARGET must be set")
	}
	imagesTarget := os.Getenv("IMAGES_GRPC_TARGET")
	if imagesTarget == "" {
		return fmt.Errorf("IMAGES_GRPC_TARGET must be set")
	}

	organizationsConn, err := grpc.NewClient(organizationsTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial organizations: %w", err)
	}
	defer organizationsConn.Close()

	imagesConn, err := grpc.NewClient(imagesTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial images: %w", err)
	}
	defer imagesConn.Close()

	runner := &provision.Runner{
		Organizations: organizationsv1.NewOrganizationsServiceClient(organizationsConn),
		Images:        imagesv1.NewImagesServiceClient(imagesConn),
		Timeout:       durationOrDefault("PROVISION_TIMEOUT", 60*time.Second),
	}

	// Provisioning runs as part of install and upgrade, and hook ordering does
	// not guarantee the services it calls are serving yet. Retry the whole run
	// rather than probing: it is create-if-absent, so a partial run followed by
	// a full one is the same as one full run.
	attempts := 5
	var result provision.Result
	for attempt := 1; ; attempt++ {
		result, err = runner.Run(ctx, config)
		if err == nil {
			break
		}
		if attempt >= attempts || ctx.Err() != nil {
			return provisionFailureIsNotFatal(err)
		}
		wait := time.Duration(attempt) * 5 * time.Second
		log.Printf("provisioner: attempt %d/%d failed (%v); retrying in %s", attempt, attempts, err, wait)
		select {
		case <-ctx.Done():
			return provisionFailureIsNotFatal(err)
		case <-time.After(wait):
		}
	}

	log.Printf("provisioner: organization %s, %d created, %d already present, %d failed",
		result.OrganizationID, len(result.Created), len(result.Existing), len(result.Failed))
	if len(result.Failed) > 0 {
		log.Printf("provisioner: not provisioned, will be retried on the next upgrade: %v", result.Failed)
	}
	return nil
}

// provisionFailureIsNotFatal reports a failed run without failing the process.
// A component whose provisioning fails does not block the platform from
// starting; the resource is simply absent, and the next upgrade attempts it
// again. Exiting non-zero here would fail the Helm release instead.
func provisionFailureIsNotFatal(err error) error {
	log.Printf("provisioner: provisioning did not complete: %v", err)
	log.Printf("provisioner: the platform starts without it; the next upgrade attempts it again")
	return nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationOrDefault(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		log.Printf("provisioner: %s=%q is not a positive duration; using %s", key, raw, fallback)
		return fallback
	}
	return value
}
