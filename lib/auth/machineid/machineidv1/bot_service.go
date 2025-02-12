/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package machineidv1

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/emptypb"

	headerv1 "github.com/gravitational/teleport/api/gen/proto/go/teleport/header/v1"
	pb "github.com/gravitational/teleport/api/gen/proto/go/teleport/machineid/v1"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/events"
	usagereporter "github.com/gravitational/teleport/lib/usagereporter/teleport"
)

// SupportedJoinMethods should match SupportedJoinMethods declared in
// lib/tbot/config
var SupportedJoinMethods = []types.JoinMethod{
	types.JoinMethodAzure,
	types.JoinMethodCircleCI,
	types.JoinMethodGCP,
	types.JoinMethodGitHub,
	types.JoinMethodGitLab,
	types.JoinMethodIAM,
	types.JoinMethodKubernetes,
	types.JoinMethodSpacelift,
	types.JoinMethodToken,
}

// BotResourceName returns the default name for resources associated with the
// given named bot.
func BotResourceName(botName string) string {
	return "bot-" + strings.ReplaceAll(botName, " ", "-")
}

// Cache is the subset of the cached resources that the Service queries.
type Cache interface {
	// GetUser returns a user by name.
	GetUser(ctx context.Context, user string, withSecrets bool) (types.User, error)
	// ListUsers lists users
	ListUsers(ctx context.Context, pageSize int, pageToken string, withSecrets bool) ([]types.User, string, error)
	// GetRole returns a role by name.
	GetRole(ctx context.Context, name string) (types.Role, error)
}

// Backend is the subset of the backend resources that the Service modifies.
type Backend interface {
	// CreateUser creates user, only if the user entry does not exist
	CreateUser(ctx context.Context, user types.User) (types.User, error)
	// CreateRole creates role, only if the role entry does not exist
	CreateRole(ctx context.Context, role types.Role) (types.Role, error)
	// UpdateUser updates an existing user if revisions match.
	UpdateUser(ctx context.Context, user types.User) (types.User, error)
	// UpdateRole updates an existing role if revisions match.
	UpdateRole(ctx context.Context, role types.Role) (types.Role, error)
	// UpsertUser creates a new user or forcefully updates an existing user.
	UpsertUser(ctx context.Context, user types.User) (types.User, error)
	// UpsertRole creates a new role or forcefully updates an existing role.
	UpsertRole(ctx context.Context, role types.Role) (types.Role, error)
	// UpsertToken creates a new token or forcefully updates an existing token.
	UpsertToken(ctx context.Context, token types.ProvisionToken) error
	// DeleteRole deletes a role by name.
	DeleteRole(ctx context.Context, name string) error
	// DeleteUser deletes a user and all associated objects.
	DeleteUser(ctx context.Context, user string) error
	// GetUser returns a user by name.
	GetUser(ctx context.Context, name string, withSecrets bool) (types.User, error)
	// GetRole returns a role by name.
	GetRole(ctx context.Context, name string) (types.Role, error)
	// GetToken returns a token by name.
	GetToken(ctx context.Context, name string) (types.ProvisionToken, error)
}

// BotServiceConfig holds configuration options for
// the bots gRPC service.
type BotServiceConfig struct {
	Authorizer authz.Authorizer
	Cache      Cache
	Backend    Backend
	Logger     logrus.FieldLogger
	Emitter    apievents.Emitter
	Reporter   usagereporter.UsageReporter
	Clock      clockwork.Clock
}

// NewBotService returns a new instance of the BotService.
func NewBotService(cfg BotServiceConfig) (*BotService, error) {
	switch {
	case cfg.Cache == nil:
		return nil, trace.BadParameter("cache service is required")
	case cfg.Backend == nil:
		return nil, trace.BadParameter("backend service is required")
	case cfg.Authorizer == nil:
		return nil, trace.BadParameter("authorizer is required")
	case cfg.Emitter == nil:
		return nil, trace.BadParameter("emitter is required")
	case cfg.Reporter == nil:
		return nil, trace.BadParameter("reporter is required")
	}

	if cfg.Logger == nil {
		cfg.Logger = logrus.WithField(trace.Component, "bot.service")
	}
	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}

	return &BotService{
		logger:     cfg.Logger,
		authorizer: cfg.Authorizer,
		cache:      cfg.Cache,
		backend:    cfg.Backend,
		emitter:    cfg.Emitter,
		reporter:   cfg.Reporter,
		clock:      cfg.Clock,
	}, nil
}

// BotService implements the teleport.machineid.v1.BotService RPC service.
type BotService struct {
	pb.UnimplementedBotServiceServer

	cache      Cache
	backend    Backend
	authorizer authz.Authorizer
	logger     logrus.FieldLogger
	emitter    apievents.Emitter
	reporter   usagereporter.UsageReporter
	clock      clockwork.Clock
}

// GetBot gets a bot by name. It will throw an error if the bot does not exist.
func (bs *BotService) GetBot(ctx context.Context, req *pb.GetBotRequest) (*pb.Bot, error) {
	_, err := authz.AuthorizeWithVerbs(
		ctx, bs.logger, bs.authorizer, false, types.KindBot, types.VerbRead,
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if req.BotName == "" {
		return nil, trace.BadParameter("bot_name: must be non-empty")
	}

	user, err := bs.cache.GetUser(ctx, BotResourceName(req.BotName), false)
	if err != nil {
		return nil, trace.Wrap(err, "fetching bot user")
	}
	role, err := bs.cache.GetRole(ctx, BotResourceName(req.BotName))
	if err != nil {
		return nil, trace.Wrap(err, "fetching bot role")
	}

	bot, err := botFromUserAndRole(user, role)
	if err != nil {
		return nil, trace.Wrap(err, "converting from resources")
	}

	return bot, nil
}

// ListBots lists all bots.
func (bs *BotService) ListBots(
	ctx context.Context, req *pb.ListBotsRequest,
) (*pb.ListBotsResponse, error) {
	_, err := authz.AuthorizeWithVerbs(
		ctx, bs.logger, bs.authorizer, false, types.KindBot, types.VerbList,
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// TODO(noah): Rewrite this to be less janky/better performing.
	// - Concurrency for fetching roles
	bots := []*pb.Bot{}
	users, token, err := bs.cache.ListUsers(
		ctx, int(req.PageSize), req.PageToken, false,
	)
	if err != nil {
		return nil, trace.Wrap(err, "listing users")
	}
	for _, u := range users {
		botName, isBot := u.GetLabel(types.BotLabel)
		if !isBot {
			continue
		}

		role, err := bs.cache.GetRole(ctx, BotResourceName(botName))
		if err != nil {
			bs.logger.WithError(err).WithFields(logrus.Fields{
				"bot.name": botName,
			}).Warn("Failed to fetch role for bot during ListBots. Bot will be omitted from results.")
			continue
		}

		bot, err := botFromUserAndRole(u, role)
		if err != nil {
			bs.logger.WithError(err).WithFields(logrus.Fields{
				"bot.name": botName,
			}).Warn("Failed to convert bot during ListBots. Bot will be omitted from results.")
			continue
		}
		bots = append(bots, bot)
	}

	return &pb.ListBotsResponse{
		Bots:          bots,
		NextPageToken: token,
	}, nil
}

// createBotAuthz allows the legacy rbac noun/verbs to continue being used until
// v16.0.0.
func (bs *BotService) createBotAuthz(ctx context.Context) (*authz.Context, error) {
	var authCtx *authz.Context
	var originalErr error
	authCtx, originalErr = authz.AuthorizeWithVerbs(
		ctx, bs.logger, bs.authorizer, false, types.KindBot, types.VerbCreate,
	)
	if originalErr != nil {
		// TODO(noah): DELETE IN 16.0.0
		if _, err := authz.AuthorizeWithVerbs(
			ctx, bs.logger, bs.authorizer, false, types.KindUser, types.VerbCreate,
		); err != nil {
			return nil, originalErr
		}
		if _, err := authz.AuthorizeWithVerbs(
			ctx, bs.logger, bs.authorizer, false, types.KindRole, types.VerbCreate,
		); err != nil {
			return nil, originalErr
		}
		var err error
		authCtx, err = authz.AuthorizeWithVerbs(
			ctx, bs.logger, bs.authorizer, false, types.KindToken, types.VerbCreate,
		)
		if err != nil {
			return nil, originalErr
		}
		bs.logger.Warn("CreateBot authz fell back to legacy resource/verbs. Explicitly grant access to the Bot resource. From V16.0.0, this will fail!")
	}
	// Support reused MFA for bulk tctl create requests.
	if err := authz.AuthorizeAdminActionAllowReusedMFA(ctx, authCtx); err != nil {
		return nil, trace.Wrap(err)
	}
	return authCtx, nil
}

// CreateBot creates a new bot. It will throw an error if the bot already
// exists.
func (bs *BotService) CreateBot(
	ctx context.Context, req *pb.CreateBotRequest,
) (*pb.Bot, error) {
	authCtx, err := bs.createBotAuthz(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err := validateBot(req.Bot); err != nil {
		return nil, trace.Wrap(err, "validating bot")
	}

	user, role, err := botToUserAndRole(
		req.Bot, bs.clock.Now(), authCtx.User.GetName(),
	)
	if err != nil {
		return nil, trace.Wrap(err, "converting to resources")
	}

	user, err = bs.backend.CreateUser(ctx, user)
	if err != nil {
		return nil, trace.Wrap(err, "creating bot user")
	}
	role, err = bs.backend.CreateRole(ctx, role)
	if err != nil {
		return nil, trace.Wrap(err, "creating bot role")
	}

	bot, err := botFromUserAndRole(user, role)
	if err != nil {
		return nil, trace.Wrap(err, "converting from resources")
	}

	bs.reporter.AnonymizeAndSubmit(&usagereporter.BotCreateEvent{
		UserName:    authz.ClientUsername(ctx),
		BotUserName: BotResourceName(bot.Metadata.Name),
		RoleName:    BotResourceName(bot.Metadata.Name),
		BotName:     bot.Metadata.Name,
		RoleCount:   int64(len(bot.Spec.Roles)),
	})
	if err := bs.emitter.EmitAuditEvent(ctx, &apievents.BotCreate{
		Metadata: apievents.Metadata{
			Type: events.BotCreateEvent,
			Code: events.BotCreateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: bot.Metadata.Name,
		},
	}); err != nil {
		bs.logger.WithError(err).Warn("Failed to emit BotCreate audit event.")
	}

	return bot, nil
}

// UpsertBot creates a new bot or forcefully updates an existing bot. This is
// a function rather than a method so that it can be used by both the gRPC
// service and the auth server init code when dealing with resources to be
// applied at startup.
func UpsertBot(
	ctx context.Context,
	backend Backend,
	bot *pb.Bot,
	now time.Time,
	createdBy string,
) (*pb.Bot, error) {
	if err := validateBot(bot); err != nil {
		return nil, trace.Wrap(err, "validating bot")
	}
	user, role, err := botToUserAndRole(bot, now, createdBy)
	if err != nil {
		return nil, trace.Wrap(err, "converting to resources")
	}

	// Copy in generation from existing user if exists
	existingUser, err := backend.GetUser(ctx, BotResourceName(bot.Metadata.Name), false)
	if err == nil {
		if existingGeneration, ok := existingUser.GetLabel(types.BotGenerationLabel); ok {
			meta := user.GetMetadata()
			meta.Labels[types.BotGenerationLabel] = existingGeneration
			user.SetMetadata(meta)
		} else {
			return nil, trace.BadParameter("unable to determine existing generation for bot due to missing label")
		}
	}

	user, err = backend.UpsertUser(ctx, user)
	if err != nil {
		return nil, trace.Wrap(err, "upserting bot user")
	}
	role, err = backend.UpsertRole(ctx, role)
	if err != nil {
		return nil, trace.Wrap(err, "upserting bot role")
	}

	bot, err = botFromUserAndRole(user, role)
	if err != nil {
		return nil, trace.Wrap(err, "converting from resources")
	}
	return bot, nil
}

// UpsertBot creates a new bot or forcefully updates an existing bot.
func (bs *BotService) UpsertBot(ctx context.Context, req *pb.UpsertBotRequest) (*pb.Bot, error) {
	authCtx, err := authz.AuthorizeWithVerbs(
		ctx, bs.logger, bs.authorizer, false, types.KindBot, types.VerbCreate, types.VerbUpdate,
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Support reused MFA for bulk tctl create requests.
	if err := authz.AuthorizeAdminActionAllowReusedMFA(ctx, authCtx); err != nil {
		return nil, trace.Wrap(err)
	}

	bot, err := UpsertBot(
		ctx, bs.backend, req.Bot, bs.clock.Now(), authCtx.User.GetName(),
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	bs.reporter.AnonymizeAndSubmit(&usagereporter.BotCreateEvent{
		UserName:    authz.ClientUsername(ctx),
		BotUserName: BotResourceName(bot.Metadata.Name),
		RoleName:    BotResourceName(bot.Metadata.Name),
		BotName:     bot.Metadata.Name,
		RoleCount:   int64(len(bot.Spec.Roles)),
	})
	if err := bs.emitter.EmitAuditEvent(ctx, &apievents.BotCreate{
		Metadata: apievents.Metadata{
			Type: events.BotCreateEvent,
			Code: events.BotCreateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: bot.Metadata.Name,
		},
	}); err != nil {
		bs.logger.WithError(err).Warn("Failed to emit BotCreate audit event.")
	}

	return bot, nil
}

// UpdateBot updates an existing bot. It will throw an error if the bot does
// not exist.
func (bs *BotService) UpdateBot(
	ctx context.Context, req *pb.UpdateBotRequest,
) (*pb.Bot, error) {
	authCtx, err := authz.AuthorizeWithVerbs(
		ctx, bs.logger, bs.authorizer, false, types.KindBot, types.VerbUpdate,
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := authz.AuthorizeAdminAction(ctx, authCtx); err != nil {
		return nil, trace.Wrap(err)
	}

	switch {
	case req.Bot == nil:
		return nil, trace.BadParameter("bot: must be non-nil")
	case req.Bot.Metadata == nil:
		return nil, trace.BadParameter("bot.metadata: must be non-nil")
	case req.Bot.Metadata.Name == "":
		return nil, trace.BadParameter("bot.metadata.name: must be non-empty")
	case req.Bot.Spec == nil:
		return nil, trace.BadParameter("bot.spec: must be non-nil")
	case req.UpdateMask == nil:
		return nil, trace.BadParameter("update_mask: must be non-nil")
	case len(req.UpdateMask.Paths) == 0:
		return nil, trace.BadParameter("update_mask.paths: must be non-empty")
	}

	user, err := bs.backend.GetUser(ctx, BotResourceName(req.Bot.Metadata.Name), false)
	if err != nil {
		return nil, trace.Wrap(err, "getting bot user")
	}
	role, err := bs.backend.GetRole(ctx, BotResourceName(req.Bot.Metadata.Name))
	if err != nil {
		return nil, trace.Wrap(err, "getting bot role")
	}

	for _, path := range req.UpdateMask.Paths {
		switch {
		case path == "spec.roles":
			role.SetImpersonateConditions(types.Allow, types.ImpersonateConditions{
				Roles: req.Bot.Spec.Roles,
			})
		case path == "spec.traits":
			traits := map[string][]string{}
			for _, t := range req.Bot.Spec.Traits {
				if len(t.Values) == 0 {
					continue
				}
				if traits[t.Name] == nil {
					traits[t.Name] = []string{}
				}
				traits[t.Name] = append(traits[t.Name], t.Values...)
			}
			user.SetTraits(traits)
		default:
			return nil, trace.BadParameter("update_mask: unsupported path %q", path)
		}
	}

	user, err = bs.backend.UpdateUser(ctx, user)
	if err != nil {
		return nil, trace.Wrap(err, "updating bot user")
	}
	role, err = bs.backend.UpdateRole(ctx, role)
	if err != nil {
		return nil, trace.Wrap(err, "updating bot role")
	}

	if err := bs.emitter.EmitAuditEvent(ctx, &apievents.BotUpdate{
		Metadata: apievents.Metadata{
			Type: events.BotUpdateEvent,
			Code: events.BotUpdateCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: req.Bot.Metadata.Name,
		},
	}); err != nil {
		bs.logger.WithError(err).Warn("Failed to emit BotUpdate audit event.")
	}

	bot, err := botFromUserAndRole(user, role)
	if err != nil {
		return nil, trace.Wrap(err, "converting from resources")
	}

	return bot, nil
}

func (bs *BotService) deleteBotUser(ctx context.Context, botName string) error {
	// Check the user that's being deleted is linked to the bot.
	user, err := bs.backend.GetUser(ctx, BotResourceName(botName), false)
	if err != nil {
		return trace.Wrap(err, "fetching bot user")
	}
	if v := user.GetMetadata().Labels[types.BotLabel]; v != botName {
		return trace.BadParameter(
			"user missing bot label matching bot name; consider manually deleting user",
		)
	}
	return bs.backend.DeleteUser(ctx, user.GetName())
}

func (bs *BotService) deleteBotRole(ctx context.Context, botName string) error {
	// Check the role that's being deleted is linked to the bot.
	role, err := bs.backend.GetRole(ctx, BotResourceName(botName))
	if err != nil {
		return trace.Wrap(err, "fetching bot role")
	}
	if v := role.GetMetadata().Labels[types.BotLabel]; v != botName {
		return trace.BadParameter(
			"role missing bot label matching bot name; consider manually deleting role",
		)
	}
	return bs.backend.DeleteRole(ctx, role.GetName())
}

// deleteBotAuthz allows the legacy rbac noun/verbs to continue being used until
// v16.0.0.
func (bs *BotService) deleteBotAuthz(ctx context.Context) error {
	var authCtx *authz.Context
	var originalErr error
	authCtx, originalErr = authz.AuthorizeWithVerbs(
		ctx, bs.logger, bs.authorizer, false, types.KindBot, types.VerbDelete,
	)
	if originalErr != nil {
		// TODO(noah): DELETE IN 16.0.0
		var err error
		authCtx, err = authz.AuthorizeWithVerbs(
			ctx, bs.logger, bs.authorizer, false, types.KindUser, types.VerbDelete,
		)
		if err != nil {
			return originalErr
		}
		if _, err := authz.AuthorizeWithVerbs(
			ctx, bs.logger, bs.authorizer, false, types.KindRole, types.VerbDelete,
		); err != nil {
			return originalErr
		}
		if _, err := authz.AuthorizeWithVerbs(
			ctx, bs.logger, bs.authorizer, false, types.KindToken, types.VerbDelete,
		); err != nil {
			return originalErr
		}
		bs.logger.Warn("DeleteBot authz fell back to legacy resource/verbs. Explicitly grant access to the Bot resource. From V16.0.0, this will fail!")
	}
	if err := authz.AuthorizeAdminAction(ctx, authCtx); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// DeleteBot deletes an existing bot. It will throw an error if the bot does
// not exist.
func (bs *BotService) DeleteBot(
	ctx context.Context, req *pb.DeleteBotRequest,
) (*emptypb.Empty, error) {
	// Note: this does not remove any locks for the bot's user / role. That
	// might be convenient in case of accidental bot locking but there doesn't
	// seem to be any automatic deletion of locks in teleport today (other
	// than expiration). Consistency around security controls seems important
	// but we can revisit this if desired.
	if err := bs.deleteBotAuthz(ctx); err != nil {
		return nil, trace.Wrap(err)
	}

	if req.BotName == "" {
		return nil, trace.BadParameter("bot_name: must be non-empty")
	}

	err := trace.NewAggregate(
		trace.Wrap(bs.deleteBotUser(ctx, req.BotName), "deleting bot user"),
		trace.Wrap(bs.deleteBotRole(ctx, req.BotName), "deleting bot role"),
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err := bs.emitter.EmitAuditEvent(ctx, &apievents.BotDelete{
		Metadata: apievents.Metadata{
			Type: events.BotDeleteEvent,
			Code: events.BotDeleteCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: req.BotName,
		},
	}); err != nil {
		bs.logger.WithError(err).Warn("Failed to emit BotDelete audit event.")
	}

	return &emptypb.Empty{}, nil
}

func validateBot(b *pb.Bot) error {
	if b == nil {
		return trace.BadParameter("must be non-nil")
	}
	if b.Metadata == nil {
		return trace.BadParameter("metadata: must be non-nil")
	}
	if b.Metadata.Name == "" {
		return trace.BadParameter("metadata.name: must be non-empty")
	}
	if b.Spec == nil {
		return trace.BadParameter("spec: must be non-nil")
	}
	return nil
}

// botFromUserAndRole
//
// Typically, we treat the bot user as the "canonical" source of information
// where possible. The bot role should be used for information which cannot
// come from the bot user.
func botFromUserAndRole(user types.User, role types.Role) (*pb.Bot, error) {
	// User label is canonical source of bot name
	botName, ok := user.GetLabel(types.BotLabel)
	if !ok {
		return nil, trace.BadParameter("user missing bot label")
	}

	b := &pb.Bot{
		Kind:    types.KindBot,
		Version: types.V1,
		Metadata: &headerv1.Metadata{
			Name: botName,
		},
		Status: &pb.BotStatus{
			UserName: user.GetName(),
			RoleName: role.GetName(),
		},
		Spec: &pb.BotSpec{
			Roles: role.GetImpersonateConditions(types.Allow).Roles,
		},
	}

	for k, v := range user.GetTraits() {
		if len(v) == 0 {
			continue
		}
		b.Spec.Traits = append(b.Spec.Traits, &pb.Trait{
			Name:   k,
			Values: v,
		})
	}

	return b, nil
}

func botToUserAndRole(bot *pb.Bot, now time.Time, createdBy string) (types.User, types.Role, error) {
	// Setup role
	resourceName := BotResourceName(bot.Metadata.Name)
	role, err := types.NewRole(resourceName, types.RoleSpecV6{
		Options: types.RoleOptions{
			MaxSessionTTL: types.Duration(12 * time.Hour),
		},
		Allow: types.RoleConditions{
			Rules: []types.Rule{
				// Bots read certificate authorities to watch for CA rotations
				types.NewRule(types.KindCertAuthority, []string{types.VerbReadNoSecrets}),
			},
			Impersonate: &types.ImpersonateConditions{
				Roles: bot.Spec.Roles,
			},
		},
	})
	if err != nil {
		return nil, nil, trace.Wrap(err, "new role")
	}
	roleMeta := role.GetMetadata()
	roleMeta.Description = fmt.Sprintf(
		"Automatically generated role for bot %s", bot.Metadata.Name,
	)
	roleMeta.Labels = map[string]string{
		types.BotLabel: bot.Metadata.Name,
	}
	role.SetMetadata(roleMeta)

	// Setup user
	user, err := types.NewUser(resourceName)
	if err != nil {
		return nil, nil, trace.Wrap(err, "new user")
	}
	user.SetRoles([]string{resourceName})
	userMeta := user.GetMetadata()
	userMeta.Labels = map[string]string{
		types.BotLabel: bot.Metadata.Name,
		// We always set this to zero here - but in Upsert, we copy from the
		// previous user before writing if necessary.
		types.BotGenerationLabel: "0",
	}
	user.SetMetadata(userMeta)

	traits := map[string][]string{}
	for _, t := range bot.Spec.Traits {
		if len(t.Values) == 0 {
			continue
		}
		if traits[t.Name] == nil {
			traits[t.Name] = []string{}
		}
		traits[t.Name] = append(traits[t.Name], t.Values...)
	}
	user.SetTraits(traits)
	user.SetCreatedBy(types.CreatedBy{
		User: types.UserRef{Name: createdBy},
		Time: now,
	})

	return user, role, nil
}
