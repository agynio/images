package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultPageSize int32 = 50
	maxPageSize     int32 = 100
)

var (
	ErrImageNotFound    = errors.New("image not found")
	ErrNameTaken        = errors.New("image name already used in this organization")
	ErrInvalidPageToken = errors.New("invalid page token")
)

type ImageType string

const (
	ImageTypeWorkspace    ImageType = "workspace"
	ImageTypeAgentRuntime ImageType = "agent_runtime"
	ImageTypeMCP          ImageType = "mcp"
)

type Visibility string

const (
	VisibilityInternal Visibility = "internal"
	VisibilityPublic   Visibility = "public"
)

type VersionState string

const (
	VersionStatePresent VersionState = "present"
	VersionStateGone    VersionState = "gone"
)

type Image struct {
	ID              uuid.UUID
	OrganizationID  uuid.UUID
	Name            string
	Description     string
	Type            ImageType
	Repository      string
	Username        string
	SecretID        *uuid.UUID
	Visibility      Visibility
	TagFilter       string
	StaleSince      *time.Time
	LastDiscoveryAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type Version struct {
	ID           uuid.UUID
	ImageID      uuid.UUID
	Tag          string
	PushedAt     *time.Time
	Description  string
	State        VersionState
	DiscoveredAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const imageColumns = `id, organization_id, name, description, type, repository, username, secret_id,
	visibility, tag_filter, stale_since, last_discovery_at, created_at, updated_at`

type CreateImageInput struct {
	OrganizationID uuid.UUID
	Name           string
	Description    string
	Type           ImageType
	Repository     string
	Username       string
	SecretID       *uuid.UUID
	Visibility     Visibility
	TagFilter      string
}

func (s *Store) CreateImage(ctx context.Context, input CreateImageInput) (Image, error) {
	row := s.pool.QueryRow(ctx, `INSERT INTO images
		(organization_id, name, description, type, repository, username, secret_id, visibility, tag_filter)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+imageColumns,
		input.OrganizationID, input.Name, input.Description, string(input.Type), input.Repository,
		input.Username, input.SecretID, string(input.Visibility), input.TagFilter)
	image, err := scanImage(row)
	if err != nil {
		return Image{}, translateWriteError(err)
	}
	return image, nil
}

func (s *Store) GetImage(ctx context.Context, id uuid.UUID) (Image, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+imageColumns+` FROM images WHERE id = $1`, id)
	image, err := scanImage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Image{}, ErrImageNotFound
	}
	return image, err
}

func (s *Store) GetImageByName(ctx context.Context, organizationID uuid.UUID, name string) (Image, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+imageColumns+` FROM images WHERE organization_id = $1 AND name = $2`,
		organizationID, name)
	image, err := scanImage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Image{}, ErrImageNotFound
	}
	return image, err
}

// UpdateImageInput carries only the mutable fields. repository and type are
// absent by construction rather than by validation.
type UpdateImageInput struct {
	Name        *string
	Description *string
	Username    *string
	SecretID    **uuid.UUID
	Visibility  *Visibility
	TagFilter   *string
}

func (s *Store) UpdateImage(ctx context.Context, id uuid.UUID, input UpdateImageInput) (Image, error) {
	assignments := []string{"updated_at = NOW()"}
	args := []any{id}
	set := func(column string, value any) {
		args = append(args, value)
		assignments = append(assignments, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if input.Name != nil {
		set("name", *input.Name)
	}
	if input.Description != nil {
		set("description", *input.Description)
	}
	if input.Username != nil {
		set("username", *input.Username)
	}
	if input.SecretID != nil {
		set("secret_id", *input.SecretID)
	}
	if input.Visibility != nil {
		set("visibility", string(*input.Visibility))
	}
	if input.TagFilter != nil {
		set("tag_filter", *input.TagFilter)
	}

	stmt := `UPDATE images SET ` + strings.Join(assignments, ", ") + ` WHERE id = $1 RETURNING ` + imageColumns
	image, err := scanImage(s.pool.QueryRow(ctx, stmt, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Image{}, ErrImageNotFound
	}
	if err != nil {
		return Image{}, translateWriteError(err)
	}
	return image, nil
}

func (s *Store) DeleteImage(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM images WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrImageNotFound
	}
	return nil
}

// ImageIDsBySecret answers the Secrets service's reference check before a
// delete. Ordered so a caller naming the blockers reports them the same way
// twice.
func (s *Store) ImageIDsBySecret(ctx context.Context, secretID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM images WHERE secret_id = $1 ORDER BY id`, secretID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type ListImagesParams struct {
	// The organization reading. Its own images and every public image are
	// returned; visibility is a read rule, so it belongs in the query rather
	// than in a filter applied afterwards.
	OrganizationID *uuid.UUID
	Type           *ImageType
	PageSize       int32
	PageToken      string
}

func (s *Store) ListImages(ctx context.Context, params ListImagesParams) ([]Image, string, error) {
	page, err := newPageParams(params.PageSize, params.PageToken)
	if err != nil {
		return nil, "", err
	}

	conditions := []string{}
	args := []any{}
	if params.OrganizationID != nil {
		args = append(args, *params.OrganizationID)
		conditions = append(conditions, fmt.Sprintf("(organization_id = $%d OR visibility = 'public')", len(args)))
	}
	if params.Type != nil {
		args = append(args, string(*params.Type))
		conditions = append(conditions, fmt.Sprintf("type = $%d", len(args)))
	}

	stmt := `SELECT ` + imageColumns + ` FROM images`
	if len(conditions) > 0 {
		stmt += " WHERE " + strings.Join(conditions, " AND ")
	}
	args = append(args, page.Limit+1, page.Offset)
	stmt += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	images := make([]Image, 0, int(page.Limit))
	for rows.Next() {
		image, err := scanImage(rows)
		if err != nil {
			return nil, "", err
		}
		images = append(images, image)
	}
	if rows.Err() != nil {
		return nil, "", rows.Err()
	}
	return finalizePage(images, page)
}

type ListVersionsParams struct {
	ImageID     uuid.UUID
	IncludeGone bool
	PageSize    int32
	PageToken   string
}

// ListVersions serves stored rows, newest first. It never reaches upstream:
// a registry outage degrades freshness, not the ability to read the catalog.
func (s *Store) ListVersions(ctx context.Context, params ListVersionsParams) ([]Version, string, error) {
	page, err := newPageParams(params.PageSize, params.PageToken)
	if err != nil {
		return nil, "", err
	}

	stmt := `SELECT id, image_id, tag, pushed_at, description, state, discovered_at
		FROM image_versions WHERE image_id = $1`
	args := []any{params.ImageID}
	if !params.IncludeGone {
		stmt += ` AND state = 'present'`
	}
	args = append(args, page.Limit+1, page.Offset)
	// NULLS LAST keeps tags whose manifest has not been read yet from sorting
	// above tags with a known push time.
	stmt += fmt.Sprintf(` ORDER BY pushed_at DESC NULLS LAST, tag DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()

	versions := make([]Version, 0, int(page.Limit))
	for rows.Next() {
		version, err := scanVersion(rows)
		if err != nil {
			return nil, "", err
		}
		versions = append(versions, version)
	}
	if rows.Err() != nil {
		return nil, "", rows.Err()
	}
	return finalizePage(versions, page)
}

func (s *Store) GetVersion(ctx context.Context, imageID uuid.UUID, tag string) (Version, bool, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, image_id, tag, pushed_at, description, state, discovered_at
		FROM image_versions WHERE image_id = $1 AND tag = $2`, imageID, tag)
	version, err := scanVersion(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, false, nil
	}
	if err != nil {
		return Version{}, false, err
	}
	return version, true, nil
}

// ObservedTag is one tag as discovery read it upstream. Metadata is optional
// because listing tags is one request and reading each tag's manifest is
// another; a tag can be recorded before its metadata is known.
type ObservedTag struct {
	Tag         string
	PushedAt    *time.Time
	Description string
	HasMetadata bool
}

// ReconcileVersions makes the stored versions match what a discovery pass saw:
// unseen tags are inserted, absent tags are marked gone rather than deleted,
// and a tag that reappeared returns to present. It reports whether anything
// changed, which is what decides if image.updated is published.
func (s *Store) ReconcileVersions(ctx context.Context, imageID uuid.UUID, observed []ObservedTag) (bool, error) {
	changed := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		seen := make([]string, 0, len(observed))
		for _, tag := range observed {
			seen = append(seen, tag.Tag)

			var state VersionState
			var hadRow bool
			row := tx.QueryRow(ctx, `SELECT state FROM image_versions WHERE image_id = $1 AND tag = $2`, imageID, tag.Tag)
			switch err := row.Scan(&state); {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			default:
				hadRow = true
			}

			if !hadRow {
				if _, err := tx.Exec(ctx, `INSERT INTO image_versions (image_id, tag, pushed_at, description)
					VALUES ($1, $2, $3, $4)`, imageID, tag.Tag, tag.PushedAt, tag.Description); err != nil {
					return err
				}
				changed = true
				continue
			}

			if state == VersionStateGone {
				changed = true
			}
			if !tag.HasMetadata {
				if _, err := tx.Exec(ctx, `UPDATE image_versions SET state = 'present', updated_at = NOW()
					WHERE image_id = $1 AND tag = $2 AND state <> 'present'`, imageID, tag.Tag); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE image_versions
				SET state = 'present', pushed_at = $3, description = $4, updated_at = NOW()
				WHERE image_id = $1 AND tag = $2`, imageID, tag.Tag, tag.PushedAt, tag.Description); err != nil {
				return err
			}
		}

		tag, err := tx.Exec(ctx, `UPDATE image_versions SET state = 'gone', updated_at = NOW()
			WHERE image_id = $1 AND state <> 'gone' AND NOT (tag = ANY($2))`, imageID, seen)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			changed = true
		}
		return nil
	})
	return changed, err
}

// SetVersionMetadata records a tag's push time and description, read from its
// manifest after the tag itself was already stored.
func (s *Store) SetVersionMetadata(ctx context.Context, imageID uuid.UUID, tag string, pushedAt *time.Time, description string) error {
	_, err := s.pool.Exec(ctx, `UPDATE image_versions
		SET pushed_at = $3, description = $4, updated_at = NOW()
		WHERE image_id = $1 AND tag = $2`, imageID, tag, pushedAt, description)
	return err
}

// MarkDiscovered records a successful pass and clears staleness.
func (s *Store) MarkDiscovered(ctx context.Context, imageID uuid.UUID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE images
		SET last_discovery_at = $2, stale_since = NULL, updated_at = NOW() WHERE id = $1`, imageID, at)
	return err
}

// MarkStale records that the repository was unreachable. Stored versions are
// left alone: a registry outage degrades freshness, not the ability to start
// workloads from what is already known. Reports whether staleness flipped.
func (s *Store) MarkStale(ctx context.Context, imageID uuid.UUID, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE images SET stale_since = $2, updated_at = NOW()
		WHERE id = $1 AND stale_since IS NULL`, imageID, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClaimImagesForDiscovery takes the images whose next pass is due and pushes
// their due time forward, so a crashed pass is retried on the following tick
// instead of blocking the image.
func (s *Store) ClaimImagesForDiscovery(ctx context.Context, limit int, interval time.Duration) ([]Image, error) {
	rows, err := s.pool.Query(ctx, `UPDATE images SET discovery_due_at = NOW() + make_interval(secs => $2)
		WHERE id IN (
			SELECT id FROM images WHERE discovery_due_at <= NOW()
			ORDER BY discovery_due_at LIMIT $1 FOR UPDATE SKIP LOCKED
		)
		RETURNING `+imageColumns, limit, interval.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	images := []Image{}
	for rows.Next() {
		image, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

// TouchDiscoveryDue moves an image's next pass forward, used after an
// on-demand refresh so a poll does not immediately repeat the work.
func (s *Store) TouchDiscoveryDue(ctx context.Context, imageID uuid.UUID, interval time.Duration) error {
	_, err := s.pool.Exec(ctx, `UPDATE images SET discovery_due_at = NOW() + make_interval(secs => $2) WHERE id = $1`,
		imageID, interval.Seconds())
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanImage(row rowScanner) (Image, error) {
	var image Image
	var imageType, visibility string
	err := row.Scan(&image.ID, &image.OrganizationID, &image.Name, &image.Description, &imageType,
		&image.Repository, &image.Username, &image.SecretID, &visibility, &image.TagFilter,
		&image.StaleSince, &image.LastDiscoveryAt, &image.CreatedAt, &image.UpdatedAt)
	if err != nil {
		return Image{}, err
	}
	image.Type = ImageType(imageType)
	image.Visibility = Visibility(visibility)
	return image, nil
}

func scanVersion(row rowScanner) (Version, error) {
	var version Version
	var state string
	err := row.Scan(&version.ID, &version.ImageID, &version.Tag, &version.PushedAt,
		&version.Description, &state, &version.DiscoveredAt)
	if err != nil {
		return Version{}, err
	}
	version.State = VersionState(state)
	return version, nil
}

func translateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrNameTaken
	}
	return err
}

type pageParams struct {
	Limit  int32
	Offset int64
}

func newPageParams(pageSize int32, pageToken string) (pageParams, error) {
	limit := NormalizePageSize(pageSize)
	offset := int64(0)
	if pageToken != "" {
		var err error
		offset, err = DecodePageToken(pageToken)
		if err != nil {
			return pageParams{}, err
		}
	}
	return pageParams{Limit: limit, Offset: offset}, nil
}

func finalizePage[T any](items []T, params pageParams) ([]T, string, error) {
	if len(items) <= int(params.Limit) {
		return items, "", nil
	}
	nextToken, err := EncodePageToken(params.Offset + int64(params.Limit))
	if err != nil {
		return nil, "", err
	}
	return items[:params.Limit], nextToken, nil
}

type pageToken struct {
	Offset int64 `json:"offset"`
}

func EncodePageToken(offset int64) (string, error) {
	buf, err := json.Marshal(pageToken{Offset: offset})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func DecodePageToken(token string) (int64, error) {
	if token == "" {
		return 0, ErrInvalidPageToken
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, fmt.Errorf("%w: decode token: %v", ErrInvalidPageToken, err)
	}
	var payload pageToken
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("%w: unmarshal token: %v", ErrInvalidPageToken, err)
	}
	if payload.Offset < 0 {
		return 0, ErrInvalidPageToken
	}
	return payload.Offset, nil
}

func NormalizePageSize(size int32) int32 {
	if size <= 0 {
		return defaultPageSize
	}
	if size > maxPageSize {
		return maxPageSize
	}
	return size
}
