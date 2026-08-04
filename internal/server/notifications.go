package server

import (
	"context"
	"fmt"

	notificationsv1 "github.com/agynio/images/gen/agynio/api/notifications/v1"
	"github.com/agynio/images/internal/store"
	"google.golang.org/protobuf/types/known/structpb"
)

const imageUpdatedEvent = "image.updated"

// ImageUpdated publishes to the owning organization's room when an image's
// version set changes or its staleness flips, so Console lists and pickers
// reflect new tags without a reload.
//
// Organizations consuming a public image do not receive its owner's
// notifications - they are not in the room, and adding every consumer to it
// would mean tracking who consumes what. Their pickers refresh on open.
func (s *Server) ImageUpdated(ctx context.Context, image store.Image) {
	if s.notifications == nil {
		return
	}
	payload, err := structpb.NewStruct(map[string]any{
		"image_id":        image.ID.String(),
		"organization_id": image.OrganizationID.String(),
		"name":            image.Name,
		"stale":           image.StaleSince != nil,
	})
	if err != nil {
		logf("build image.updated payload: %v", err)
		return
	}
	if _, err := s.notifications.Publish(ctx, &notificationsv1.PublishRequest{
		Event:   imageUpdatedEvent,
		Rooms:   []string{fmt.Sprintf("organization:%s", image.OrganizationID)},
		Payload: payload,
		Source:  "images",
	}); err != nil {
		logf("publish image.updated: %v", err)
	}
}
