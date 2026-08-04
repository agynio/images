package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	authorizationv1 "github.com/agynio/image-catalog/gen/agynio/api/authorization/v1"
	imagesv1 "github.com/agynio/image-catalog/gen/agynio/api/images/v1"
	notificationsv1 "github.com/agynio/image-catalog/gen/agynio/api/notifications/v1"
	secretsv1 "github.com/agynio/image-catalog/gen/agynio/api/secrets/v1"
	"github.com/agynio/image-catalog/internal/discovery"
	"github.com/agynio/image-catalog/internal/registry"
	"github.com/agynio/image-catalog/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const rpcTimeout = 30 * time.Second

type AuthorizationClient interface {
	Check(ctx context.Context, req *authorizationv1.CheckRequest, opts ...grpc.CallOption) (*authorizationv1.CheckResponse, error)
}

type SecretsClient interface {
	CreateSecret(ctx context.Context, req *secretsv1.CreateSecretRequest, opts ...grpc.CallOption) (*secretsv1.CreateSecretResponse, error)
	UpdateSecret(ctx context.Context, req *secretsv1.UpdateSecretRequest, opts ...grpc.CallOption) (*secretsv1.UpdateSecretResponse, error)
	DeleteSecret(ctx context.Context, req *secretsv1.DeleteSecretRequest, opts ...grpc.CallOption) (*secretsv1.DeleteSecretResponse, error)
	ResolveSecret(ctx context.Context, req *secretsv1.ResolveSecretRequest, opts ...grpc.CallOption) (*secretsv1.ResolveSecretResponse, error)
}

type NotificationsClient interface {
	Publish(ctx context.Context, req *notificationsv1.PublishRequest, opts ...grpc.CallOption) (*notificationsv1.PublishResponse, error)
}

type Registry interface {
	ListTags(ctx context.Context, repository string, cred registry.Credential) ([]string, error)
	TagMetadata(ctx context.Context, repository, tag string, cred registry.Credential) (registry.Metadata, error)
}

type Server struct {
	imagesv1.UnimplementedImagesServiceServer

	store         *store.Store
	authz         AuthorizationClient
	secrets       SecretsClient
	notifications NotificationsClient
	registry      Registry
	discoverer    *discovery.Discoverer
}

func New(s *store.Store, authz AuthorizationClient, secrets SecretsClient, reg Registry) *Server {
	return &Server{store: s, authz: authz, secrets: secrets, registry: reg}
}

func (s *Server) WithNotifications(client NotificationsClient) *Server {
	s.notifications = client
	return s
}

func (s *Server) WithDiscoverer(d *discovery.Discoverer) *Server {
	s.discoverer = d
	return s
}

func (s *Server) CreateImage(ctx context.Context, req *imagesv1.CreateImageRequest) (*imagesv1.CreateImageResponse, error) {
	organizationID, err := parseUUID("organization_id", req.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if err := s.requireOrganizationOwner(ctx, organizationID); err != nil {
		return nil, err
	}
	input, err := validateCreate(req)
	if err != nil {
		return nil, err
	}
	input.OrganizationID = organizationID

	// Validating readability before storing is what makes a typo or a wrong
	// credential fail at registration rather than at workload start.
	credential := registry.Credential{Username: req.GetUsername(), Password: req.GetPassword()}
	if err := s.checkReadable(ctx, input.Repository, credential); err != nil {
		return nil, err
	}

	secretID, err := s.storeCredential(ctx, organizationID, input.Name, req.GetPassword())
	if err != nil {
		return nil, err
	}
	input.SecretID = secretID

	image, err := s.store.CreateImage(ctx, input)
	if err != nil {
		s.discardCredential(ctx, secretID)
		return nil, translateStoreError(err)
	}

	s.discoverNow(ctx, image)
	return &imagesv1.CreateImageResponse{Image: toProtoImage(image)}, nil
}

func (s *Server) GetImage(ctx context.Context, req *imagesv1.GetImageRequest) (*imagesv1.GetImageResponse, error) {
	image, err := s.loadReadableImage(ctx, req.GetId())
	if err != nil {
		return nil, err
	}
	return &imagesv1.GetImageResponse{Image: toProtoImage(image)}, nil
}

func (s *Server) UpdateImage(ctx context.Context, req *imagesv1.UpdateImageRequest) (*imagesv1.UpdateImageResponse, error) {
	id, err := parseUUID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	existing, err := s.store.GetImage(ctx, id)
	if err != nil {
		return nil, translateStoreError(err)
	}
	if err := s.requireOrganizationOwner(ctx, existing.OrganizationID); err != nil {
		return nil, err
	}
	input, err := validateUpdate(req)
	if err != nil {
		return nil, err
	}

	if req.Password != nil {
		secretID, err := s.replaceCredential(ctx, existing, req.GetPassword())
		if err != nil {
			return nil, err
		}
		input.SecretID = &secretID
	}

	image, err := s.store.UpdateImage(ctx, id, input)
	if err != nil {
		return nil, translateStoreError(err)
	}

	// A changed credential applies to the next discovery pass, and a narrowed
	// visibility changes who may see the record - both are worth a pass now.
	s.discoverNow(ctx, image)
	return &imagesv1.UpdateImageResponse{Image: toProtoImage(image)}, nil
}

// DeleteImage is not blocked by references. Blocking would require asking the
// Agents service which environments name the record, and would surface
// cross-organization usage to an owner who cannot see the organizations
// involved. References are late-bound and flagged instead.
func (s *Server) DeleteImage(ctx context.Context, req *imagesv1.DeleteImageRequest) (*imagesv1.DeleteImageResponse, error) {
	id, err := parseUUID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	existing, err := s.store.GetImage(ctx, id)
	if err != nil {
		return nil, translateStoreError(err)
	}
	if err := s.requireOrganizationOwner(ctx, existing.OrganizationID); err != nil {
		return nil, err
	}
	if err := s.store.DeleteImage(ctx, id); err != nil {
		return nil, translateStoreError(err)
	}
	s.discardCredential(ctx, existing.SecretID)
	return &imagesv1.DeleteImageResponse{}, nil
}

func (s *Server) ListImages(ctx context.Context, req *imagesv1.ListImagesRequest) (*imagesv1.ListImagesResponse, error) {
	scope, err := s.organizationListScope(ctx, req.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	params := store.ListImagesParams{
		OrganizationID: scope,
		PageSize:       req.GetPageSize(),
		PageToken:      req.GetPageToken(),
	}
	if req.GetType() != imagesv1.ImageType_IMAGE_TYPE_UNSPECIFIED {
		imageType, err := fromProtoType(req.GetType())
		if err != nil {
			return nil, err
		}
		params.Type = &imageType
	}

	images, nextToken, err := s.store.ListImages(ctx, params)
	if err != nil {
		return nil, translateStoreError(err)
	}
	response := &imagesv1.ListImagesResponse{NextPageToken: nextToken}
	for _, image := range images {
		response.Images = append(response.Images, toProtoImage(image))
	}
	return response, nil
}

func (s *Server) ListVersions(ctx context.Context, req *imagesv1.ListVersionsRequest) (*imagesv1.ListVersionsResponse, error) {
	image, err := s.loadReadableImage(ctx, req.GetImageId())
	if err != nil {
		return nil, err
	}
	versions, nextToken, err := s.store.ListVersions(ctx, store.ListVersionsParams{
		ImageID:     image.ID,
		IncludeGone: req.GetIncludeGone(),
		PageSize:    req.GetPageSize(),
		PageToken:   req.GetPageToken(),
	})
	if err != nil {
		return nil, translateStoreError(err)
	}
	response := &imagesv1.ListVersionsResponse{NextPageToken: nextToken}
	for _, version := range versions {
		response.Versions = append(response.Versions, toProtoVersion(version))
	}
	return response, nil
}

// RefreshImage runs a pass inline, so a picker or an image page opening shows a
// freshly pushed tag without waiting for the poll interval. An upstream failure
// is not an error to the caller: the image is flagged stale and its stored
// versions are served.
func (s *Server) RefreshImage(ctx context.Context, req *imagesv1.RefreshImageRequest) (*imagesv1.RefreshImageResponse, error) {
	image, err := s.loadReadableImage(ctx, req.GetImageId())
	if err != nil {
		return nil, err
	}
	if s.discoverer != nil {
		if _, err := s.discoverer.Refresh(ctx, image); err != nil {
			logf("refresh %s: %v", image.ID, err)
		}
		if err := s.store.TouchDiscoveryDue(ctx, image.ID, s.discoverer.Interval()); err != nil {
			logf("touch discovery due for %s: %v", image.ID, err)
		}
		if refreshed, err := s.store.GetImage(ctx, image.ID); err == nil {
			image = refreshed
		}
	}

	versions, _, err := s.store.ListVersions(ctx, store.ListVersionsParams{ImageID: image.ID, PageSize: 100})
	if err != nil {
		return nil, translateStoreError(err)
	}
	response := &imagesv1.RefreshImageResponse{Image: toProtoImage(image)}
	for _, version := range versions {
		response.Versions = append(response.Versions, toProtoVersion(version))
	}
	return response, nil
}

// ResolveVersion is internal: reachability is restricted by Istio rather than
// by an identity check, because its callers - the Agents service validating a
// reference and the Image Proxy serving a pull - hold no OpenFGA tuples.
func (s *Server) ResolveVersion(ctx context.Context, req *imagesv1.ResolveVersionRequest) (*imagesv1.ResolveVersionResponse, error) {
	image, err := s.resolveReference(ctx, req)
	if err != nil {
		return nil, err
	}

	if consumer := req.GetConsumerOrganizationId(); consumer != "" {
		consumerID, err := parseUUID("consumer_organization_id", consumer)
		if err != nil {
			return nil, err
		}
		if image.Visibility != store.VisibilityPublic && image.OrganizationID != consumerID {
			return nil, status.Error(codes.NotFound, "image not found")
		}
	}
	if req.GetRequireType() != imagesv1.ImageType_IMAGE_TYPE_UNSPECIFIED {
		required, err := fromProtoType(req.GetRequireType())
		if err != nil {
			return nil, err
		}
		if image.Type != required {
			return nil, status.Errorf(codes.FailedPrecondition,
				"image %s is of type %s, not %s", image.Name, image.Type, required)
		}
	}

	tag := req.GetTag()
	if tag == "" {
		return nil, status.Error(codes.InvalidArgument, "tag: value is empty")
	}
	version, found, err := s.store.GetVersion(ctx, image.ID, tag)
	if err != nil {
		return nil, translateStoreError(err)
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "tag %q is not a discovered version of image %s", tag, image.Name)
	}

	credential, err := s.resolveCredential(ctx, image)
	if err != nil {
		return nil, err
	}
	return &imagesv1.ResolveVersionResponse{
		Image:      toProtoImage(image),
		Repository: image.Repository,
		Tag:        tag,
		State:      toProtoState(version.State),
		Username:   credential.Username,
		Password:   credential.Password,
	}, nil
}

// RegisterPlatformImage creates an image when nothing of that name exists in
// the organization and returns the existing one otherwise. It never updates, so
// re-running an upgrade cannot overwrite a change an operator made by hand.
func (s *Server) RegisterPlatformImage(ctx context.Context, req *imagesv1.RegisterPlatformImageRequest) (*imagesv1.RegisterPlatformImageResponse, error) {
	organizationID, err := parseUUID("organization_id", req.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	existing, err := s.store.GetImageByName(ctx, organizationID, req.GetName())
	switch {
	case err == nil:
		return &imagesv1.RegisterPlatformImageResponse{Image: toProtoImage(existing), Created: false}, nil
	case !errors.Is(err, store.ErrImageNotFound):
		return nil, translateStoreError(err)
	}

	create := &imagesv1.CreateImageRequest{
		OrganizationId: req.GetOrganizationId(),
		Name:           req.GetName(),
		Description:    req.GetDescription(),
		Type:           req.GetType(),
		Repository:     req.GetRepository(),
		Username:       req.GetUsername(),
		Password:       req.GetPassword(),
		Visibility:     req.GetVisibility(),
		TagFilter:      req.GetTagFilter(),
	}
	input, err := validateCreate(create)
	if err != nil {
		return nil, err
	}
	input.OrganizationID = organizationID

	secretID, err := s.storeCredential(ctx, organizationID, input.Name, req.GetPassword())
	if err != nil {
		return nil, err
	}
	input.SecretID = secretID

	image, err := s.store.CreateImage(ctx, input)
	if err != nil {
		s.discardCredential(ctx, secretID)
		// A concurrent provisioning run won the insert; returning its record
		// keeps the call create-if-absent rather than failing the upgrade.
		if errors.Is(err, store.ErrNameTaken) {
			if raced, getErr := s.store.GetImageByName(ctx, organizationID, input.Name); getErr == nil {
				return &imagesv1.RegisterPlatformImageResponse{Image: toProtoImage(raced), Created: false}, nil
			}
		}
		return nil, translateStoreError(err)
	}

	s.discoverNow(ctx, image)
	return &imagesv1.RegisterPlatformImageResponse{Image: toProtoImage(image), Created: true}, nil
}

// resolveReference settles which image a request names - by id, or by the
// (organization, name) pair the proxy's reference path encodes.
func (s *Server) resolveReference(ctx context.Context, req *imagesv1.ResolveVersionRequest) (store.Image, error) {
	switch reference := req.GetReference().(type) {
	case *imagesv1.ResolveVersionRequest_ImageId:
		id, err := parseUUID("image_id", reference.ImageId)
		if err != nil {
			return store.Image{}, err
		}
		image, err := s.store.GetImage(ctx, id)
		if err != nil {
			return store.Image{}, translateStoreError(err)
		}
		return image, nil
	case *imagesv1.ResolveVersionRequest_Ref:
		organizationID, err := parseUUID("ref.organization_id", reference.Ref.GetOrganizationId())
		if err != nil {
			return store.Image{}, err
		}
		image, err := s.store.GetImageByName(ctx, organizationID, reference.Ref.GetName())
		if err != nil {
			return store.Image{}, translateStoreError(err)
		}
		return image, nil
	default:
		return store.Image{}, status.Error(codes.InvalidArgument, "reference: one of image_id or ref is required")
	}
}

func (s *Server) checkReadable(ctx context.Context, repository string, credential registry.Credential) error {
	checkCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := s.registry.ListTags(checkCtx, repository, credential); err != nil {
		if errors.Is(err, registry.ErrUnauthorized) {
			return status.Errorf(codes.InvalidArgument, "repository %s is not readable with the supplied credential: %v", repository, err)
		}
		return status.Errorf(codes.FailedPrecondition, "repository %s could not be read: %v", repository, err)
	}
	return nil
}

// discoverNow runs a pass so a newly registered or re-credentialed image shows
// its versions without waiting for the poll.
//
// It runs in the background because a pass reads a manifest per tag, which is
// seconds of upstream traffic that a form submit should not wait on. The record
// exists either way, and image.updated tells the Console when versions arrive.
func (s *Server) discoverNow(ctx context.Context, image store.Image) {
	if s.discoverer == nil {
		return
	}
	// The request context is detached deliberately: the pass outlives the call
	// that triggered it. Values carried on it - the caller's identity - are
	// irrelevant, since discovery talks to a registry rather than the platform.
	passCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.discoverer.Timeout())
	go func() {
		defer cancel()
		if _, err := s.discoverer.Pass(passCtx, image); err != nil {
			logf("initial discovery pass for %s: %v", image.ID, err)
		}
	}()
}

func (s *Server) loadReadableImage(ctx context.Context, rawID string) (store.Image, error) {
	id, err := parseUUID("id", rawID)
	if err != nil {
		return store.Image{}, err
	}
	image, err := s.store.GetImage(ctx, id)
	if err != nil {
		return store.Image{}, translateStoreError(err)
	}
	if err := s.requireImageRead(ctx, image); err != nil {
		return store.Image{}, err
	}
	return image, nil
}

func parseUUID(field, raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.UUID{}, status.Errorf(codes.InvalidArgument, "%s: value is empty", field)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.UUID{}, status.Errorf(codes.InvalidArgument, "%s: %v", field, err)
	}
	return id, nil
}

func translateStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrImageNotFound):
		return status.Error(codes.NotFound, "image not found")
	case errors.Is(err, store.ErrNameTaken):
		return status.Error(codes.AlreadyExists, "an image with that name already exists in this organization")
	case errors.Is(err, store.ErrInvalidPageToken):
		return status.Error(codes.InvalidArgument, "page_token: invalid")
	case err == nil:
		return nil
	default:
		return status.Error(codes.Internal, fmt.Sprintf("images: %v", err))
	}
}
