package server

import (
	"context"
	"log"

	secretsv1 "github.com/agynio/images/gen/agynio/api/secrets/v1"
	"github.com/agynio/images/internal/registry"
	"github.com/agynio/images/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A registry password is an ordinary Secret, named here by id. The registrar
// creates it, so it can be rotated, reused, or backed by a remote provider like
// every other credential on the platform; this service holds the reference and
// reads the value, and never writes one.

// requireOwnedSecret rejects a reference the organization may not use. The
// ownership check is what stops an owner from naming another organization's
// secret and pointing the image at a registry they control, which would send
// them its value on the next discovery pass.
func (s *Server) requireOwnedSecret(ctx context.Context, organizationID, secretID uuid.UUID) error {
	if s.secrets == nil {
		return status.Error(codes.FailedPrecondition, "cannot verify the secret exists")
	}
	resp, err := s.secrets.ResolveSecretExists(ctx, &secretsv1.ResolveSecretExistsRequest{Id: secretID.String()})
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "cannot verify the secret exists: %v", err)
	}
	if !resp.GetExists() {
		return status.Errorf(codes.InvalidArgument, "secret_id: secret %s does not exist", secretID)
	}
	if resp.GetOrganizationId() != organizationID.String() {
		return status.Error(codes.InvalidArgument, "secret_id: secret belongs to another organization")
	}
	return nil
}

func (s *Server) resolveCredential(ctx context.Context, username string, secretID *uuid.UUID) (registry.Credential, error) {
	credential := registry.Credential{Username: username}
	if secretID == nil {
		return credential, nil
	}
	if s.secrets == nil {
		return registry.Credential{}, status.Error(codes.FailedPrecondition, "secrets service is not configured")
	}
	resp, err := s.secrets.ResolveSecret(ctx, &secretsv1.ResolveSecretRequest{Id: secretID.String()})
	if err != nil {
		return registry.Credential{}, status.Errorf(codes.Internal, "resolve registry credential: %v", err)
	}
	credential.Password = resp.GetValue()
	return credential, nil
}

// Resolve satisfies discovery.CredentialResolver.
func (s *Server) Resolve(ctx context.Context, image store.Image) (registry.Credential, error) {
	return s.resolveCredential(ctx, image.Username, image.SecretID)
}

func logf(format string, args ...any) { log.Printf("images: "+format, args...) }
