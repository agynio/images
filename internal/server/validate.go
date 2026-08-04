package server

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	imagesv1 "github.com/agynio/images/gen/agynio/api/images/v1"
	"github.com/agynio/images/internal/store"
	"github.com/google/go-containerregistry/pkg/name"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxNameLength = 64

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

func validateCreate(req *imagesv1.CreateImageRequest) (store.CreateImageInput, error) {
	imageName, err := validateName(req.GetName())
	if err != nil {
		return store.CreateImageInput{}, err
	}
	imageType, err := fromProtoType(req.GetType())
	if err != nil {
		return store.CreateImageInput{}, err
	}
	visibility, err := fromProtoVisibility(req.GetVisibility())
	if err != nil {
		return store.CreateImageInput{}, err
	}
	repository, err := validateRepository(req.GetRepository())
	if err != nil {
		return store.CreateImageInput{}, err
	}
	if err := validateTagFilter(req.GetTagFilter()); err != nil {
		return store.CreateImageInput{}, err
	}
	return store.CreateImageInput{
		Name:        imageName,
		Description: req.GetDescription(),
		Type:        imageType,
		Repository:  repository,
		Username:    req.GetUsername(),
		Visibility:  visibility,
		TagFilter:   req.GetTagFilter(),
	}, nil
}

// validateUpdate covers only the mutable fields. repository and type are absent
// from the request by construction, so there is nothing to reject.
func validateUpdate(req *imagesv1.UpdateImageRequest) (store.UpdateImageInput, error) {
	input := store.UpdateImageInput{}
	if req.Name != nil {
		imageName, err := validateName(req.GetName())
		if err != nil {
			return store.UpdateImageInput{}, err
		}
		input.Name = &imageName
	}
	if req.Description != nil {
		description := req.GetDescription()
		input.Description = &description
	}
	if req.Username != nil {
		username := req.GetUsername()
		input.Username = &username
	}
	if req.Visibility != nil {
		visibility, err := fromProtoVisibility(req.GetVisibility())
		if err != nil {
			return store.UpdateImageInput{}, err
		}
		input.Visibility = &visibility
	}
	if req.TagFilter != nil {
		if err := validateTagFilter(req.GetTagFilter()); err != nil {
			return store.UpdateImageInput{}, err
		}
		filter := req.GetTagFilter()
		input.TagFilter = &filter
	}
	return input, nil
}

func validateName(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", status.Error(codes.InvalidArgument, "name: value is empty")
	}
	if len(trimmed) > maxNameLength {
		return "", status.Errorf(codes.InvalidArgument, "name: must be at most %d characters", maxNameLength)
	}
	if !namePattern.MatchString(trimmed) {
		return "", status.Error(codes.InvalidArgument, "name: must match ^[a-z0-9-]+$")
	}
	return trimmed, nil
}

// validateRepository rejects a reference carrying a tag or a digest. The record
// names a repository; which tag runs is a separate decision made per
// environment, and accepting one here would give a record two answers.
func validateRepository(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", status.Error(codes.InvalidArgument, "repository: value is empty")
	}
	if strings.Contains(trimmed, "@") || strings.Contains(path.Base(trimmed), ":") {
		return "", status.Error(codes.InvalidArgument, "repository: must not carry a tag or digest")
	}
	if _, err := name.NewRepository(trimmed); err != nil {
		return "", status.Errorf(codes.InvalidArgument, "repository: %v", err)
	}
	return trimmed, nil
}

func validateTagFilter(filter string) error {
	if filter == "" {
		return nil
	}
	if _, err := path.Match(filter, "probe"); err != nil {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("tag_filter: %v", err))
	}
	return nil
}
