package server

import (
	"context"
	"fmt"
	"log"

	secretsv1 "github.com/agynio/images/gen/agynio/api/secrets/v1"
	"github.com/agynio/images/internal/registry"
	"github.com/agynio/images/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Registry passwords are stored as ordinary Secrets, so they inherit the same
// encryption at rest and remote-provider handling as every other secret value.
// The Images service owns the record's Secret rather than asking the registrar
// to create one first: registration is a single step, and a Secret nothing
// points at is not left behind when it fails.

func (s *Server) storeCredential(ctx context.Context, organizationID uuid.UUID, imageName, password string) (*uuid.UUID, error) {
	if password == "" {
		return nil, nil
	}
	resp, err := s.secrets.CreateSecret(ctx, &secretsv1.CreateSecretRequest{
		OrganizationId: organizationID.String(),
		Title:          credentialTitle(imageName),
		Description:    fmt.Sprintf("Registry password for image %q", imageName),
		Value:          password,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store registry credential: %v", err)
	}
	id, err := uuid.Parse(resp.GetSecret().GetMeta().GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store registry credential: %v", err)
	}
	return &id, nil
}

// replaceCredential applies a password change: an empty password removes the
// credential, so an image can be moved to an anonymously readable repository.
func (s *Server) replaceCredential(ctx context.Context, image store.Image, password string) (*uuid.UUID, error) {
	if password == "" {
		s.discardCredential(ctx, image.SecretID)
		return nil, nil
	}
	if image.SecretID == nil {
		return s.storeCredential(ctx, image.OrganizationID, image.Name, password)
	}
	if _, err := s.secrets.UpdateSecret(ctx, &secretsv1.UpdateSecretRequest{
		Id:    image.SecretID.String(),
		Value: &password,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "update registry credential: %v", err)
	}
	return image.SecretID, nil
}

// discardCredential removes a Secret this service created. Failure is logged
// rather than surfaced: the image operation it accompanies has already
// succeeded, and leaving an orphan Secret is better than reporting a failure
// for work that was done.
func (s *Server) discardCredential(ctx context.Context, secretID *uuid.UUID) {
	if secretID == nil {
		return
	}
	if _, err := s.secrets.DeleteSecret(ctx, &secretsv1.DeleteSecretRequest{Id: secretID.String()}); err != nil {
		log.Printf("images: delete registry credential %s: %v", secretID, err)
	}
}

func (s *Server) resolveCredential(ctx context.Context, image store.Image) (registry.Credential, error) {
	credential := registry.Credential{Username: image.Username}
	if image.SecretID == nil {
		return credential, nil
	}
	resp, err := s.secrets.ResolveSecret(ctx, &secretsv1.ResolveSecretRequest{Id: image.SecretID.String()})
	if err != nil {
		return registry.Credential{}, status.Errorf(codes.Internal, "resolve registry credential: %v", err)
	}
	credential.Password = resp.GetValue()
	return credential, nil
}

// Resolve satisfies discovery.CredentialResolver.
func (s *Server) Resolve(ctx context.Context, image store.Image) (registry.Credential, error) {
	return s.resolveCredential(ctx, image)
}

func credentialTitle(imageName string) string {
	return fmt.Sprintf("image-%s-registry", imageName)
}

func logf(format string, args ...any) { log.Printf("images: "+format, args...) }
