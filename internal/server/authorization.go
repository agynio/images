package server

import (
	"context"

	authorizationv1 "github.com/agynio/image-catalog/gen/agynio/api/authorization/v1"
	"github.com/agynio/image-catalog/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	identityPrefix     = "identity:"
	organizationPrefix = "organization:"
)

// No OpenFGA type is introduced for images. Both visibility values resolve
// against existing organization relations, so images need no per-resource
// tuples and no share management.

// requireOrganizationOwner gates the authored writes. It demands an identity
// rather than tolerating its absence: the Gateway forwards a request with no
// identity when it carries no token, so treating absence as an internal caller
// would let anyone who can reach the Gateway author images. The internal write
// path is RegisterPlatformImage, which is a separate RPC the Gateway does not
// expose and Istio restricts.
func (s *Server) requireOrganizationOwner(ctx context.Context, organizationID uuid.UUID) error {
	identityID, err := identityFromContext(ctx)
	if err != nil {
		return err
	}
	return s.requireRelation(ctx, identityID, "owner", organizationID)
}

// requireImageRead enforces visibility: internal images are readable by members
// of the owning organization, public images by any authenticated identity.
func (s *Server) requireImageRead(ctx context.Context, image store.Image) error {
	identityID, hasIdentity, err := optionalIdentityFromContext(ctx)
	if err != nil {
		return err
	}
	if !hasIdentity {
		return nil
	}
	if image.Visibility == store.VisibilityPublic {
		return nil
	}
	if err := s.requireRelation(ctx, identityID, "member", image.OrganizationID); err != nil {
		// An image the caller may not see is reported as absent rather than
		// forbidden: the alternative confirms that a name exists in an
		// organization the caller cannot read.
		if status.Code(err) == codes.PermissionDenied {
			return status.Error(codes.NotFound, "image not found")
		}
		return err
	}
	return nil
}

// organizationListScope settles which organization a list reads. An identified
// caller names one and must be a member of it; an internal caller that names
// none reads everything, which is what lets the proxy and the provisioner work
// without an identity.
func (s *Server) organizationListScope(ctx context.Context, organizationID string) (*uuid.UUID, error) {
	identityID, hasIdentity, err := optionalIdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	if !hasIdentity && organizationID == "" {
		return nil, nil
	}
	parsed, err := parseUUID("organization_id", organizationID)
	if err != nil {
		return nil, err
	}
	if hasIdentity {
		if err := s.requireRelation(ctx, identityID, "member", parsed); err != nil {
			return nil, err
		}
	}
	return &parsed, nil
}

func (s *Server) requireRelation(ctx context.Context, identityID uuid.UUID, relation string, organizationID uuid.UUID) error {
	response, err := s.authz.Check(ctx, &authorizationv1.CheckRequest{
		TupleKey: &authorizationv1.TupleKey{
			User:     identityPrefix + identityID.String(),
			Relation: relation,
			Object:   organizationPrefix + organizationID.String(),
		},
	})
	if err != nil {
		return err
	}
	if !response.GetAllowed() {
		return status.Errorf(codes.PermissionDenied, "identity lacks %s on organization", relation)
	}
	return nil
}

func identityFromContext(ctx context.Context) (uuid.UUID, error) {
	identityID, hasIdentity, err := optionalIdentityFromContext(ctx)
	if err != nil {
		return uuid.UUID{}, err
	}
	if !hasIdentity {
		return uuid.UUID{}, status.Error(codes.Unauthenticated, "identity not available: x-identity-id not found in metadata")
	}
	return identityID, nil
}

// optionalIdentityFromContext reports the caller's identity when one is
// present. Absence means an internal caller reaching the service over the mesh
// rather than through the Gateway; a malformed identity is still an error.
func optionalIdentityFromContext(ctx context.Context) (uuid.UUID, bool, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return uuid.UUID{}, false, nil
	}
	values := md.Get("x-identity-id")
	if len(values) == 0 || values[0] == "" {
		return uuid.UUID{}, false, nil
	}
	id, err := uuid.Parse(values[0])
	if err != nil {
		return uuid.UUID{}, false, status.Errorf(codes.InvalidArgument, "identity_id: %v", err)
	}
	return id, true, nil
}
