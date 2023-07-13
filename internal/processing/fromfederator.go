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

package processing

import (
	"context"
	"errors"
	"net/url"

	"codeberg.org/gruf/go-kv"
	"codeberg.org/gruf/go-logger/v2/level"
	"github.com/superseriousbusiness/gotosocial/internal/ap"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/id"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/messages"
)

// ProcessFromFederator reads the APActivityType and APObjectType of an incoming message from the federator,
// and directs the message into the appropriate side effect handler function, or simply does nothing if there's
// no handler function defined for the combination of Activity and Object.
func (p *Processor) ProcessFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	// Allocate new log fields slice
	fields := make([]kv.Field, 3, 5)
	fields[0] = kv.Field{"activityType", federatorMsg.APActivityType}
	fields[1] = kv.Field{"objectType", federatorMsg.APObjectType}
	fields[2] = kv.Field{"toAccount", federatorMsg.ReceivingAccount.Username}

	if federatorMsg.APIri != nil {
		// An IRI was supplied, append to log
		fields = append(fields, kv.Field{
			"iri", federatorMsg.APIri,
		})
	}

	if federatorMsg.GTSModel != nil &&
		log.Level() >= level.DEBUG {
		// Append converted model to log
		fields = append(fields, kv.Field{
			"model", federatorMsg.GTSModel,
		})
	}

	// Log this federated message
	l := log.WithContext(ctx).WithFields(fields...)
	l.Info("processing from federator")

	switch federatorMsg.APActivityType {
	case ap.ActivityCreate:
		// CREATE SOMETHING
		switch federatorMsg.APObjectType {
		case ap.ObjectNote:
			// CREATE A STATUS
			return p.processCreateStatusFromFederator(ctx, federatorMsg)
		case ap.ActivityLike:
			// CREATE A FAVE
			return p.processCreateFaveFromFederator(ctx, federatorMsg)
		case ap.ActivityFollow:
			// CREATE A FOLLOW REQUEST
			return p.processCreateFollowRequestFromFederator(ctx, federatorMsg)
		case ap.ActivityAnnounce:
			// CREATE AN ANNOUNCE
			return p.processCreateAnnounceFromFederator(ctx, federatorMsg)
		case ap.ActivityBlock:
			// CREATE A BLOCK
			return p.processCreateBlockFromFederator(ctx, federatorMsg)
		case ap.ActivityFlag:
			// CREATE A FLAG / REPORT
			return p.processCreateFlagFromFederator(ctx, federatorMsg)
		}
	case ap.ActivityUpdate:
		// UPDATE SOMETHING
		if federatorMsg.APObjectType == ap.ObjectProfile {
			// UPDATE AN ACCOUNT
			return p.processUpdateAccountFromFederator(ctx, federatorMsg)
		}
	case ap.ActivityDelete:
		// DELETE SOMETHING
		switch federatorMsg.APObjectType {
		case ap.ObjectNote:
			// DELETE A STATUS
			return p.processDeleteStatusFromFederator(ctx, federatorMsg)
		case ap.ObjectProfile:
			// DELETE A PROFILE/ACCOUNT
			return p.processDeleteAccountFromFederator(ctx, federatorMsg)
		}
	}

	// not a combination we can/need to process
	return nil
}

// processCreateStatusFromFederator handles Activity Create and Object Note.
func (p *Processor) processCreateStatusFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	// Check the federatorMsg for either an already dereferenced
	// and converted status pinned to the message, or a forwarded
	// AP IRI that we still need to deref.
	var (
		status    *gtsmodel.Status
		err       error
		forwarded = federatorMsg.GTSModel == nil
	)

	if forwarded {
		// Model was not set, deref with IRI.
		// This will also cause the status to be inserted into the db.
		status, err = p.statusFromAPIRI(ctx, federatorMsg)
	} else {
		// Model is set, use that.
		status, err = p.statusFromGTSModel(ctx, federatorMsg)
	}

	if err != nil {
		return gtserror.Newf("error extracting status from federatorMsg: %w", err)
	}

	// Before we go whacking this status in our db, ensure
	// it's actually relevant to the account who received it
	// in their Inbox, and it's not just a status randomly
	// blasted at us from somewhere we don't care about.

	if err := p.state.DB.PutStatus(ctx, status); err != nil {
		if errors.Is(err, db.ErrAlreadyExists) {
			// The status already exists in the database, which
			// means we've already processed it and some race
			// condition means we didn't catch it yet. We can
			// just return nil here and be done with it.
			return nil
		}

		// Real error.
		return gtserror.Newf("db error inserting status: %w", err)
	}

	if status.Account == nil || status.Account.IsRemote() {
		// Either no account attached yet, or a remote account.
		// Both situations we need to parse account URI to fetch it.
		accountURI, err := url.Parse(status.AccountURI)
		if err != nil {
			return err
		}

		// Ensure that account for this status has been deref'd.
		status.Account, _, err = p.federator.GetAccountByURI(ctx,
			federatorMsg.ReceivingAccount.Username,
			accountURI,
		)
		if err != nil {
			return err
		}
	}

	// Ensure status ancestors dereferenced. We need at least the
	// immediate parent (if present) to ascertain timelineability.
	if err := p.federator.DereferenceStatusAncestors(ctx,
		federatorMsg.ReceivingAccount.Username,
		status,
	); err != nil {
		return err
	}

	if status.InReplyToID != "" {
		// Interaction counts changed on the replied status;
		// uncache the prepared version from all timelines.
		p.invalidateStatusFromTimelines(ctx, status.InReplyToID)
	}

	if err := p.timelineAndNotifyStatus(ctx, status); err != nil {
		return gtserror.Newf("error timelining status: %w", err)
	}

	return nil
}

func (p *Processor) statusFromGTSModel(ctx context.Context, federatorMsg messages.FromFederator) (*gtsmodel.Status, error) {
	// There should be a status pinned to the federatorMsg
	// (we've already checked to ensure this is not nil).
	status, ok := federatorMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		err := gtserror.New("Note was not parseable as *gtsmodel.Status")
		return nil, err
	}

	// AP statusable representation may have also
	// been set on message (no problem if not).
	statusable, _ := federatorMsg.APObjectModel.(ap.Statusable)

	// Call refresh on status to update
	// it (deref remote) if necessary.
	var err error
	status, _, err = p.federator.RefreshStatus(
		ctx,
		federatorMsg.ReceivingAccount.Username,
		status,
		statusable,
		false,
	)
	if err != nil {
		return nil, gtserror.Newf("%w", err)
	}

	return status, nil
}

func (p *Processor) statusFromAPIRI(ctx context.Context, federatorMsg messages.FromFederator) (*gtsmodel.Status, error) {
	// There should be a status IRI pinned to
	// the federatorMsg for us to dereference.
	if federatorMsg.APIri == nil {
		err := gtserror.New("status was not pinned to federatorMsg, and neither was an IRI for us to dereference")
		return nil, err
	}

	// Get the status + ensure we have
	// the most up-to-date version.
	status, _, err := p.federator.GetStatusByURI(
		ctx,
		federatorMsg.ReceivingAccount.Username,
		federatorMsg.APIri,
	)
	if err != nil {
		return nil, gtserror.Newf("%w", err)
	}

	return status, nil
}

// processCreateFaveFromFederator handles Activity Create with Object Like.
func (p *Processor) processCreateFaveFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	statusFave, ok := federatorMsg.GTSModel.(*gtsmodel.StatusFave)
	if !ok {
		return gtserror.New("Like was not parseable as *gtsmodel.StatusFave")
	}

	if err := p.notifyFave(ctx, statusFave); err != nil {
		return gtserror.Newf("error notifying status fave: %w", err)
	}

	// Interaction counts changed on the faved status;
	// uncache the prepared version from all timelines.
	p.invalidateStatusFromTimelines(ctx, statusFave.StatusID)

	return nil
}

// processCreateFollowRequestFromFederator handles Activity Create and Object Follow
func (p *Processor) processCreateFollowRequestFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	followRequest, ok := federatorMsg.GTSModel.(*gtsmodel.FollowRequest)
	if !ok {
		return errors.New("incomingFollowRequest was not parseable as *gtsmodel.FollowRequest")
	}

	// make sure the account is pinned
	if followRequest.Account == nil {
		a, err := p.state.DB.GetAccountByID(ctx, followRequest.AccountID)
		if err != nil {
			return err
		}
		followRequest.Account = a
	}

	// Get the remote account to make sure the avi and header are cached.
	if followRequest.Account.Domain != "" {
		remoteAccountID, err := url.Parse(followRequest.Account.URI)
		if err != nil {
			return err
		}

		a, _, err := p.federator.GetAccountByURI(ctx,
			federatorMsg.ReceivingAccount.Username,
			remoteAccountID,
		)
		if err != nil {
			return err
		}

		followRequest.Account = a
	}

	if followRequest.TargetAccount == nil {
		a, err := p.state.DB.GetAccountByID(ctx, followRequest.TargetAccountID)
		if err != nil {
			return err
		}
		followRequest.TargetAccount = a
	}

	if *followRequest.TargetAccount.Locked {
		// if the account is locked just notify the follow request and nothing else
		return p.notifyFollowRequest(ctx, followRequest)
	}

	// if the target account isn't locked, we should already accept the follow and notify about the new follower instead
	follow, err := p.state.DB.AcceptFollowRequest(ctx, followRequest.AccountID, followRequest.TargetAccountID)
	if err != nil {
		return err
	}

	if err := p.federateAcceptFollowRequest(ctx, follow); err != nil {
		return err
	}

	return p.notifyFollow(ctx, follow, followRequest.TargetAccount)
}

// processCreateAnnounceFromFederator handles Activity Create with Object Announce.
func (p *Processor) processCreateAnnounceFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	status, ok := federatorMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.New("Announce was not parseable as *gtsmodel.Status")
	}

	// Dereference status that this status boosts.
	if err := p.federator.DereferenceAnnounce(ctx, status, federatorMsg.ReceivingAccount.Username); err != nil {
		return gtserror.Newf("error dereferencing announce: %w", err)
	}

	// Generate an ID for the boost wrapper status.
	statusID, err := id.NewULIDFromTime(status.CreatedAt)
	if err != nil {
		return gtserror.Newf("error generating id: %w", err)
	}
	status.ID = statusID

	// Store the boost wrapper status.
	if err := p.state.DB.PutStatus(ctx, status); err != nil {
		return gtserror.Newf("db error inserting status: %w", err)
	}

	// Ensure boosted status ancestors dereferenced. We need at least
	// the immediate parent (if present) to ascertain timelineability.
	if err := p.federator.DereferenceStatusAncestors(ctx,
		federatorMsg.ReceivingAccount.Username,
		status.BoostOf,
	); err != nil {
		return err
	}

	// Timeline and notify the announce.
	if err := p.timelineAndNotifyStatus(ctx, status); err != nil {
		return gtserror.Newf("error timelining status: %w", err)
	}

	if err := p.notifyAnnounce(ctx, status); err != nil {
		return gtserror.Newf("error notifying status: %w", err)
	}

	// Interaction counts changed on the boosted status;
	// uncache the prepared version from all timelines.
	p.invalidateStatusFromTimelines(ctx, status.ID)

	return nil
}

// processCreateBlockFromFederator handles Activity Create and Object Block
func (p *Processor) processCreateBlockFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	block, ok := federatorMsg.GTSModel.(*gtsmodel.Block)
	if !ok {
		return gtserror.New("block was not parseable as *gtsmodel.Block")
	}

	// Remove each account's posts from the other's timelines.
	//
	// First home timelines.
	if err := p.state.Timelines.Home.WipeItemsFromAccountID(ctx, block.AccountID, block.TargetAccountID); err != nil {
		return gtserror.Newf("%w", err)
	}

	if err := p.state.Timelines.Home.WipeItemsFromAccountID(ctx, block.TargetAccountID, block.AccountID); err != nil {
		return gtserror.Newf("%w", err)
	}

	// Now list timelines.
	if err := p.state.Timelines.List.WipeItemsFromAccountID(ctx, block.AccountID, block.TargetAccountID); err != nil {
		return gtserror.Newf("%w", err)
	}

	if err := p.state.Timelines.List.WipeItemsFromAccountID(ctx, block.TargetAccountID, block.AccountID); err != nil {
		return gtserror.Newf("%w", err)
	}

	// Remove any follows that existed between blocker + blockee.
	if err := p.state.DB.DeleteFollow(ctx, block.AccountID, block.TargetAccountID); err != nil {
		return gtserror.Newf(
			"db error deleting follow from %s targeting %s: %w",
			block.AccountID, block.TargetAccountID, err,
		)
	}

	if err := p.state.DB.DeleteFollow(ctx, block.TargetAccountID, block.AccountID); err != nil {
		return gtserror.Newf(
			"db error deleting follow from %s targeting %s: %w",
			block.TargetAccountID, block.AccountID, err,
		)
	}

	// Remove any follow requests that existed between blocker + blockee.
	if err := p.state.DB.DeleteFollowRequest(ctx, block.AccountID, block.TargetAccountID); err != nil {
		return gtserror.Newf(
			"db error deleting follow request from %s targeting %s: %w",
			block.AccountID, block.TargetAccountID, err,
		)
	}

	if err := p.state.DB.DeleteFollowRequest(ctx, block.TargetAccountID, block.AccountID); err != nil {
		return gtserror.Newf(
			"db error deleting follow request from %s targeting %s: %w",
			block.TargetAccountID, block.AccountID, err,
		)
	}

	return nil
}

func (p *Processor) processCreateFlagFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	incomingReport, ok := federatorMsg.GTSModel.(*gtsmodel.Report)
	if !ok {
		return errors.New("flag was not parseable as *gtsmodel.Report")
	}

	// TODO: handle additional side effects of flag creation:
	// - notify admins by dm / notification

	return p.emailReport(ctx, incomingReport)
}

// processUpdateAccountFromFederator handles Activity Update and Object Profile
func (p *Processor) processUpdateAccountFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	// Parse the old/existing account model.
	account, ok := federatorMsg.GTSModel.(*gtsmodel.Account)
	if !ok {
		return gtserror.New("account was not parseable as *gtsmodel.Account")
	}

	// Because this was an Update, the new Accountable should be set on the message.
	apubAcc, ok := federatorMsg.APObjectModel.(ap.Accountable)
	if !ok {
		return gtserror.New("Accountable was not parseable on update account message")
	}

	// Fetch up-to-date bio, avatar, header, etc.
	_, _, err := p.federator.RefreshAccount(
		ctx,
		federatorMsg.ReceivingAccount.Username,
		account,
		apubAcc,
		true, // Force refresh.
	)
	if err != nil {
		return gtserror.Newf("error refreshing updated account: %w", err)
	}

	return nil
}

// processDeleteStatusFromFederator handles Activity Delete and Object Note
func (p *Processor) processDeleteStatusFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	status, ok := federatorMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return errors.New("Note was not parseable as *gtsmodel.Status")
	}

	// Delete attachments from this status, since this request
	// comes from the federating API, and there's no way the
	// poster can do a delete + redraft for it on our instance.
	deleteAttachments := true
	if err := p.wipeStatus(ctx, status, deleteAttachments); err != nil {
		return gtserror.Newf("error wiping status: %w", err)
	}

	if status.InReplyToID != "" {
		// Interaction counts changed on the replied status;
		// uncache the prepared version from all timelines.
		p.invalidateStatusFromTimelines(ctx, status.InReplyToID)
	}

	return nil
}

// processDeleteAccountFromFederator handles Activity Delete and Object Profile
func (p *Processor) processDeleteAccountFromFederator(ctx context.Context, federatorMsg messages.FromFederator) error {
	account, ok := federatorMsg.GTSModel.(*gtsmodel.Account)
	if !ok {
		return errors.New("account delete was not parseable as *gtsmodel.Account")
	}

	return p.account.Delete(ctx, account, account.ID)
}
