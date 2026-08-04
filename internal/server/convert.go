package server

import (
	imagesv1 "github.com/agynio/image-catalog/gen/agynio/api/images/v1"
	"github.com/agynio/image-catalog/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func toProtoImage(image store.Image) *imagesv1.Image {
	proto := &imagesv1.Image{
		Meta: &imagesv1.EntityMeta{
			Id:        image.ID.String(),
			CreatedAt: timestamppb.New(image.CreatedAt),
			UpdatedAt: timestamppb.New(image.UpdatedAt),
		},
		OrganizationId: image.OrganizationID.String(),
		Name:           image.Name,
		Description:    image.Description,
		Type:           toProtoType(image.Type),
		Repository:     image.Repository,
		Username:       image.Username,
		Visibility:     toProtoVisibility(image.Visibility),
		TagFilter:      image.TagFilter,
	}
	// The password is never returned; the Secret holding it is named so a
	// caller can tell a credentialed image from an anonymous one.
	if image.SecretID != nil {
		proto.SecretId = image.SecretID.String()
	}
	if image.StaleSince != nil {
		proto.StaleSince = timestamppb.New(*image.StaleSince)
	}
	if image.LastDiscoveryAt != nil {
		proto.LastDiscoveryAt = timestamppb.New(*image.LastDiscoveryAt)
	}
	return proto
}

func toProtoVersion(version store.Version) *imagesv1.ImageVersion {
	proto := &imagesv1.ImageVersion{
		Id:           version.ID.String(),
		ImageId:      version.ImageID.String(),
		Tag:          version.Tag,
		Description:  version.Description,
		State:        toProtoState(version.State),
		DiscoveredAt: timestamppb.New(version.DiscoveredAt),
	}
	if version.PushedAt != nil {
		proto.PushedAt = timestamppb.New(*version.PushedAt)
	}
	return proto
}

func toProtoType(imageType store.ImageType) imagesv1.ImageType {
	switch imageType {
	case store.ImageTypeWorkspace:
		return imagesv1.ImageType_IMAGE_TYPE_WORKSPACE
	case store.ImageTypeAgentRuntime:
		return imagesv1.ImageType_IMAGE_TYPE_AGENT_RUNTIME
	case store.ImageTypeMCP:
		return imagesv1.ImageType_IMAGE_TYPE_MCP
	default:
		return imagesv1.ImageType_IMAGE_TYPE_UNSPECIFIED
	}
}

func fromProtoType(imageType imagesv1.ImageType) (store.ImageType, error) {
	switch imageType {
	case imagesv1.ImageType_IMAGE_TYPE_WORKSPACE:
		return store.ImageTypeWorkspace, nil
	case imagesv1.ImageType_IMAGE_TYPE_AGENT_RUNTIME:
		return store.ImageTypeAgentRuntime, nil
	case imagesv1.ImageType_IMAGE_TYPE_MCP:
		return store.ImageTypeMCP, nil
	default:
		return "", status.Error(codes.InvalidArgument, "type: must be workspace, agent_runtime, or mcp")
	}
}

func toProtoVisibility(visibility store.Visibility) imagesv1.ImageVisibility {
	switch visibility {
	case store.VisibilityPublic:
		return imagesv1.ImageVisibility_IMAGE_VISIBILITY_PUBLIC
	case store.VisibilityInternal:
		return imagesv1.ImageVisibility_IMAGE_VISIBILITY_INTERNAL
	default:
		return imagesv1.ImageVisibility_IMAGE_VISIBILITY_UNSPECIFIED
	}
}

func fromProtoVisibility(visibility imagesv1.ImageVisibility) (store.Visibility, error) {
	switch visibility {
	case imagesv1.ImageVisibility_IMAGE_VISIBILITY_PUBLIC:
		return store.VisibilityPublic, nil
	case imagesv1.ImageVisibility_IMAGE_VISIBILITY_INTERNAL:
		return store.VisibilityInternal, nil
	default:
		return "", status.Error(codes.InvalidArgument, "visibility: must be public or internal")
	}
}

func toProtoState(state store.VersionState) imagesv1.ImageVersionState {
	switch state {
	case store.VersionStatePresent:
		return imagesv1.ImageVersionState_IMAGE_VERSION_STATE_PRESENT
	case store.VersionStateGone:
		return imagesv1.ImageVersionState_IMAGE_VERSION_STATE_GONE
	default:
		return imagesv1.ImageVersionState_IMAGE_VERSION_STATE_UNSPECIFIED
	}
}
