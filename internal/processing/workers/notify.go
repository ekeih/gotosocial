// GoToSocial
// Copyright (C) GoToSocial Authors admin@gotosocial.org
// SPDX-License-Identifier: AGPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package workers

import (
	"context"
	"errors"

	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtscontext"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/id"
)

func (p *Processor) notifyStatusMentions(ctx context.Context, status *gtsmodel.Status) error {
	errs := gtserror.NewMultiError(len(status.Mentions))

	for _, m := range status.Mentions {
		if err := p.notify(
			ctx,
			gtsmodel.NotificationMention,
			m.TargetAccountID,
			m.OriginAccountID,
			m.StatusID,
		); err != nil {
			errs.Append(err)
		}
	}

	if err := errs.Combine(); err != nil {
		return gtserror.Newf("%w", err)
	}

	return nil
}

func (p *Processor) notifyFollowRequest(ctx context.Context, followRequest *gtsmodel.FollowRequest) error {
	return p.notify(
		ctx,
		gtsmodel.NotificationFollowRequest,
		followRequest.TargetAccountID,
		followRequest.AccountID,
		"",
	)
}

func (p *Processor) notifyFollow(ctx context.Context, follow *gtsmodel.Follow, targetAccount *gtsmodel.Account) error {
	// Remove previous follow request notification, if it exists.
	prevNotif, err := p.state.DB.GetNotification(
		gtscontext.SetBarebones(ctx),
		gtsmodel.NotificationFollowRequest,
		targetAccount.ID,
		follow.AccountID,
		"",
	)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		// Proper error while checking.
		return gtserror.Newf("db error checking for previous follow request notification: %w", err)
	}

	if prevNotif != nil {
		// Previous notification existed, delete.
		if err := p.state.DB.DeleteNotificationByID(ctx, prevNotif.ID); err != nil {
			return gtserror.Newf("db error removing previous follow request notification %s: %w", prevNotif.ID, err)
		}
	}

	// Now notify the follow itself.
	return p.notify(
		ctx,
		gtsmodel.NotificationFollow,
		targetAccount.ID,
		follow.AccountID,
		"",
	)
}

func (p *Processor) notifyFave(ctx context.Context, fave *gtsmodel.StatusFave) error {
	if fave.TargetAccountID == fave.AccountID {
		// Self-fave, nothing to do.
		return nil
	}

	return p.notify(
		ctx,
		gtsmodel.NotificationFave,
		fave.TargetAccountID,
		fave.AccountID,
		fave.StatusID,
	)
}

func (p *Processor) notifyAnnounce(ctx context.Context, status *gtsmodel.Status) error {
	if status.BoostOfID == "" {
		// Not a boost, nothing to do.
		return nil
	}

	if status.BoostOfAccountID == status.AccountID {
		// Self-boost, nothing to do.
		return nil
	}

	return p.notify(
		ctx,
		gtsmodel.NotificationReblog,
		status.BoostOfAccountID,
		status.AccountID,
		status.ID,
	)
}

func (p *Processor) notify(
	ctx context.Context,
	notificationType gtsmodel.NotificationType,
	targetAccountID string,
	originAccountID string,
	statusID string,
) error {
	targetAccount, err := p.state.DB.GetAccountByID(ctx, targetAccountID)
	if err != nil {
		return gtserror.Newf("error getting target account %s: %w", targetAccountID, err)
	}

	if !targetAccount.IsLocal() {
		// Nothing to do.
		return nil
	}

	// Make sure a notification doesn't
	// already exist with these params.
	if _, err := p.state.DB.GetNotification(
		ctx,
		notificationType,
		targetAccountID,
		originAccountID,
		statusID,
	); err == nil {
		// Notification exists, nothing to do.
		return nil
	} else if !errors.Is(err, db.ErrNoEntries) {
		// Real error.
		return gtserror.Newf("error checking existence of notification: %w", err)
	}

	// Notification doesn't yet exist, so
	// we need to create + store one.
	notif := &gtsmodel.Notification{
		ID:               id.NewULID(),
		NotificationType: notificationType,
		TargetAccountID:  targetAccountID,
		OriginAccountID:  originAccountID,
		StatusID:         statusID,
	}

	if err := p.state.DB.PutNotification(ctx, notif); err != nil {
		return gtserror.Newf("error putting notification in database: %w", err)
	}

	// Stream notification to the user.
	apiNotif, err := p.tc.NotificationToAPINotification(ctx, notif)
	if err != nil {
		return gtserror.Newf("error converting notification to api representation: %w", err)
	}

	if err := p.stream.Notify(apiNotif, targetAccount); err != nil {
		return gtserror.Newf("error streaming notification to account: %w", err)
	}

	return nil
}
