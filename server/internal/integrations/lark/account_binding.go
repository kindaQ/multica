package lark

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrAccountBindingNeedsWorkspace = errors.New("account binding requires a workspace")

type AccountBindingToken struct {
	Raw       string
	ExpiresAt time.Time
}

type RedeemedAccountBinding struct {
	InstallationID     pgtype.UUID
	MulticaUserID      pgtype.UUID
	DefaultWorkspaceID pgtype.UUID
	LarkOpenID         OpenID
}

// AccountBindingService owns public-gateway identity binding. Unlike the
// legacy BindingTokenService, this mapping is account-scoped and carries no
// installation-workspace membership requirement.
type AccountBindingService struct {
	queries *db.Queries
	tx      TxStarter
	now     func() time.Time
}

func NewAccountBindingService(queries *db.Queries, tx TxStarter) *AccountBindingService {
	return newAccountBindingServiceWithClock(queries, tx, time.Now)
}

func newAccountBindingServiceWithClock(queries *db.Queries, tx TxStarter, now func() time.Time) *AccountBindingService {
	return &AccountBindingService{queries: queries, tx: tx, now: now}
}

func (s *AccountBindingService) Mint(ctx context.Context, installationID pgtype.UUID, openID OpenID, sourceMessageID string) (AccountBindingToken, error) {
	raw, err := randomToken(32)
	if err != nil {
		return AccountBindingToken{}, fmt.Errorf("generate account binding token: %w", err)
	}
	expiresAt := s.now().Add(BindingTokenTTL)
	row, err := s.queries.CreateChannelAccountBindingToken(ctx, db.CreateChannelAccountBindingTokenParams{
		TokenHash:       hashToken(raw),
		InstallationID:  installationID,
		ChannelType:     channelTypeFeishu,
		ChannelUserID:   string(openID),
		SourceMessageID: pgtype.Text{String: sourceMessageID, Valid: sourceMessageID != ""},
		ExpiresAt:       pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return AccountBindingToken{}, fmt.Errorf("persist account binding token: %w", err)
	}
	return AccountBindingToken{Raw: raw, ExpiresAt: row.ExpiresAt.Time}, nil
}

func (s *AccountBindingService) Redeem(ctx context.Context, raw string, multicaUserID pgtype.UUID) (RedeemedAccountBinding, error) {
	if s.tx == nil {
		return RedeemedAccountBinding{}, errors.New("lark: AccountBindingService missing TxStarter")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return RedeemedAccountBinding{}, fmt.Errorf("begin account binding transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.queries.WithTx(tx)

	token, err := qtx.ConsumeChannelAccountBindingToken(ctx, hashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RedeemedAccountBinding{}, ErrBindingTokenInvalid
		}
		return RedeemedAccountBinding{}, fmt.Errorf("consume account binding token: %w", err)
	}

	workspaces, err := qtx.ListWorkspaces(ctx, multicaUserID)
	if err != nil {
		return RedeemedAccountBinding{}, fmt.Errorf("list account workspaces: %w", err)
	}
	instance, err := qtx.GetInstanceState(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RedeemedAccountBinding{}, fmt.Errorf("load instance state: %w", err)
	}
	routableWorkspaces := workspaces[:0]
	for _, workspace := range workspaces {
		if instance.PublicWorkspaceID.Valid && workspace.ID == instance.PublicWorkspaceID {
			continue
		}
		routableWorkspaces = append(routableWorkspaces, workspace)
	}
	if len(routableWorkspaces) == 0 {
		return RedeemedAccountBinding{}, ErrAccountBindingNeedsWorkspace
	}
	user, err := qtx.GetUser(ctx, multicaUserID)
	if err != nil {
		return RedeemedAccountBinding{}, fmt.Errorf("load binding user: %w", err)
	}
	defaultWorkspaceID := user.DefaultWorkspaceID
	if !defaultWorkspaceID.Valid {
		updated, err := qtx.SetDefaultWorkspaceIfUnset(ctx, db.SetDefaultWorkspaceIfUnsetParams{
			WorkspaceID: routableWorkspaces[0].ID,
			UserID:      multicaUserID,
		})
		if err != nil {
			return RedeemedAccountBinding{}, fmt.Errorf("set binding default workspace: %w", err)
		}
		defaultWorkspaceID = updated.DefaultWorkspaceID
	}

	_, err = qtx.CreateChannelAccountBinding(ctx, db.CreateChannelAccountBindingParams{
		InstallationID: token.InstallationID,
		ChannelType:    channelTypeFeishu,
		ChannelUserID:  token.ChannelUserID,
		MulticaUserID:  multicaUserID,
		Config:         []byte("{}"),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.Is(err, pgx.ErrNoRows) || errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return RedeemedAccountBinding{}, ErrBindingAlreadyAssigned
		}
		return RedeemedAccountBinding{}, fmt.Errorf("create account binding: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RedeemedAccountBinding{}, fmt.Errorf("commit account binding: %w", err)
	}
	return RedeemedAccountBinding{
		InstallationID:     token.InstallationID,
		MulticaUserID:      multicaUserID,
		DefaultWorkspaceID: defaultWorkspaceID,
		LarkOpenID:         OpenID(token.ChannelUserID),
	}, nil
}
