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
	"fmt"

	"codeberg.org/gruf/go-kv"
	"codeberg.org/gruf/go-logger/v2/level"
	"github.com/superseriousbusiness/gotosocial/internal/ap"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/log"
	"github.com/superseriousbusiness/gotosocial/internal/messages"
)

func (p *Processor) EnqueueClientAPI(ctx context.Context, msgs ...messages.FromClientAPI) {
	log.Trace(ctx, "enqueuing")
	_ = p.state.Workers.ClientAPI.MustEnqueueCtx(ctx, func(ctx context.Context) {
		for _, msg := range msgs {
			log.Trace(ctx, "processing: %+v", msg)
			if err := p.ProcessFromClientAPI(ctx, msg); err != nil {
				log.Errorf(ctx, "error processing client API message: %v", err)
			}
		}
	})
}

func (p *Processor) ProcessFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	// Allocate new log fields slice
	fields := make([]kv.Field, 3, 4)
	fields[0] = kv.Field{"activityType", clientMsg.APActivityType}
	fields[1] = kv.Field{"objectType", clientMsg.APObjectType}
	fields[2] = kv.Field{"fromAccount", clientMsg.OriginAccount.Username}

	if clientMsg.GTSModel != nil &&
		log.Level() >= level.DEBUG {
		// Append converted model to log
		fields = append(fields, kv.Field{
			"model", clientMsg.GTSModel,
		})
	}

	// Log this federated message
	l := log.WithContext(ctx).WithFields(fields...)
	l.Info("processing from client")

	switch clientMsg.APActivityType {
	case ap.ActivityCreate:
		// CREATE
		switch clientMsg.APObjectType {
		case ap.ObjectProfile, ap.ActorPerson:
			// CREATE ACCOUNT/PROFILE
			return p.createAccountFromClientAPI(ctx, clientMsg)
		case ap.ObjectNote:
			// CREATE NOTE
			return p.createStatusFromClientAPI(ctx, clientMsg)
		case ap.ActivityFollow:
			// CREATE FOLLOW REQUEST
			return p.createFollowRequestFromClientAPI(ctx, clientMsg)
		case ap.ActivityLike:
			// CREATE LIKE/FAVE
			return p.createFaveFromClientAPI(ctx, clientMsg)
		case ap.ActivityAnnounce:
			// CREATE BOOST/ANNOUNCE
			return p.createAnnounceFromClientAPI(ctx, clientMsg)
		case ap.ActivityBlock:
			// CREATE BLOCK
			return p.createBlockFromClientAPI(ctx, clientMsg)
		}
	case ap.ActivityUpdate:
		// UPDATE
		switch clientMsg.APObjectType {
		case ap.ObjectProfile, ap.ActorPerson:
			// UPDATE ACCOUNT/PROFILE
			return p.updateAccountFromClientAPI(ctx, clientMsg)
		case ap.ActivityFlag:
			// UPDATE A FLAG/REPORT (mark as resolved/closed)
			return p.updateReportFromClientAPI(ctx, clientMsg)
		}
	case ap.ActivityAccept:
		// ACCEPT
		if clientMsg.APObjectType == ap.ActivityFollow {
			// ACCEPT FOLLOW
			return p.acceptFollowFromClientAPI(ctx, clientMsg)
		}
	case ap.ActivityReject:
		// REJECT
		if clientMsg.APObjectType == ap.ActivityFollow {
			// REJECT FOLLOW (request)
			return p.rejectFollowFromClientAPI(ctx, clientMsg)
		}
	case ap.ActivityUndo:
		// UNDO
		switch clientMsg.APObjectType {
		case ap.ActivityFollow:
			// UNDO FOLLOW
			return p.undoFollowFromClientAPI(ctx, clientMsg)
		case ap.ActivityBlock:
			// UNDO BLOCK
			return p.undoBlockFromClientAPI(ctx, clientMsg)
		case ap.ActivityLike:
			// UNDO LIKE/FAVE
			return p.undoFaveFromClientAPI(ctx, clientMsg)
		case ap.ActivityAnnounce:
			// UNDO ANNOUNCE/BOOST
			return p.undoAnnounceFromClientAPI(ctx, clientMsg)
		}
	case ap.ActivityDelete:
		// DELETE
		switch clientMsg.APObjectType {
		case ap.ObjectNote:
			// DELETE STATUS/NOTE
			return p.deleteStatusFromClientAPI(ctx, clientMsg)
		case ap.ObjectProfile, ap.ActorPerson:
			// DELETE ACCOUNT/PROFILE
			return p.deleteAccountFromClientAPI(ctx, clientMsg)
		}
	case ap.ActivityFlag:
		// FLAG
		if clientMsg.APObjectType == ap.ObjectProfile {
			// FLAG/REPORT A PROFILE
			return p.reportAccountFromClientAPI(ctx, clientMsg)
		}
	}
	return nil
}

func (p *Processor) createAccountFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	account, ok := clientMsg.GTSModel.(*gtsmodel.Account)
	if !ok {
		return errors.New("account was not parseable as *gtsmodel.Account")
	}

	// Do nothing if this isn't our activity.
	if !account.IsLocal() {
		return nil
	}

	// get the user this account belongs to
	user, err := p.state.DB.GetUserByAccountID(ctx, account.ID)
	if err != nil {
		return err
	}

	// email a confirmation to this user
	return p.user.EmailSendConfirmation(ctx, user, account.Username)
}

func (p *Processor) createStatusFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	status, ok := clientMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.New("status was not parseable as *gtsmodel.Status")
	}

	if err := p.timelineAndNotifyStatus(ctx, status); err != nil {
		return gtserror.Newf("error timelining status: %w", err)
	}

	if status.InReplyToID != "" {
		// Interaction counts changed on the replied status;
		// uncache the prepared version from all timelines.
		p.invalidateStatusFromTimelines(ctx, status.InReplyToID)
	}

	if err := p.federateStatus(ctx, status); err != nil {
		return gtserror.Newf("error federating status: %w", err)
	}

	return nil
}

func (p *Processor) createFollowRequestFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	followRequest, ok := clientMsg.GTSModel.(*gtsmodel.FollowRequest)
	if !ok {
		return errors.New("followrequest was not parseable as *gtsmodel.FollowRequest")
	}

	if err := p.notifyFollowRequest(ctx, followRequest); err != nil {
		return err
	}

	return p.federateFollow(ctx, followRequest, clientMsg.OriginAccount, clientMsg.TargetAccount)
}

func (p *Processor) createFaveFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	statusFave, ok := clientMsg.GTSModel.(*gtsmodel.StatusFave)
	if !ok {
		return gtserror.New("statusFave was not parseable as *gtsmodel.StatusFave")
	}

	if err := p.notifyFave(ctx, statusFave); err != nil {
		return gtserror.Newf("error notifying status fave: %w", err)
	}

	// Interaction counts changed on the faved status;
	// uncache the prepared version from all timelines.
	p.invalidateStatusFromTimelines(ctx, statusFave.StatusID)

	if err := p.federateFave(ctx, statusFave, clientMsg.OriginAccount, clientMsg.TargetAccount); err != nil {
		return gtserror.Newf("error federating status fave: %w", err)
	}

	return nil
}

func (p *Processor) createAnnounceFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	status, ok := clientMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return errors.New("boost was not parseable as *gtsmodel.Status")
	}

	// Timeline and notify.
	if err := p.timelineAndNotifyStatus(ctx, status); err != nil {
		return gtserror.Newf("error timelining boost: %w", err)
	}

	if err := p.notifyAnnounce(ctx, status); err != nil {
		return gtserror.Newf("error notifying boost: %w", err)
	}

	// Interaction counts changed on the boosted status;
	// uncache the prepared version from all timelines.
	p.invalidateStatusFromTimelines(ctx, status.BoostOfID)

	if err := p.federateAnnounce(ctx, status, clientMsg.OriginAccount, clientMsg.TargetAccount); err != nil {
		return gtserror.Newf("error federating boost: %w", err)
	}

	return nil
}

func (p *Processor) createBlockFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	block, ok := clientMsg.GTSModel.(*gtsmodel.Block)
	if !ok {
		return errors.New("block was not parseable as *gtsmodel.Block")
	}

	// remove any of the blocking account's statuses from the blocked account's timeline, and vice versa
	if err := p.state.Timelines.Home.WipeItemsFromAccountID(ctx, block.AccountID, block.TargetAccountID); err != nil {
		return err
	}
	if err := p.state.Timelines.Home.WipeItemsFromAccountID(ctx, block.TargetAccountID, block.AccountID); err != nil {
		return err
	}

	// TODO: same with notifications
	// TODO: same with bookmarks

	return p.federateBlock(ctx, block)
}

func (p *Processor) updateAccountFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	account, ok := clientMsg.GTSModel.(*gtsmodel.Account)
	if !ok {
		return errors.New("account was not parseable as *gtsmodel.Account")
	}

	return p.federateAccountUpdate(ctx, account, clientMsg.OriginAccount)
}

func (p *Processor) updateReportFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	report, ok := clientMsg.GTSModel.(*gtsmodel.Report)
	if !ok {
		return errors.New("report was not parseable as *gtsmodel.Report")
	}

	if report.Account.IsRemote() {
		// Report creator is a remote account,
		// we shouldn't email or notify them.
		return nil
	}

	return p.emailReportClosed(ctx, report)
}

func (p *Processor) acceptFollowFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	follow, ok := clientMsg.GTSModel.(*gtsmodel.Follow)
	if !ok {
		return errors.New("accept was not parseable as *gtsmodel.Follow")
	}

	if err := p.notifyFollow(ctx, follow, clientMsg.TargetAccount); err != nil {
		return err
	}

	return p.federateAcceptFollowRequest(ctx, follow)
}

func (p *Processor) rejectFollowFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	followRequest, ok := clientMsg.GTSModel.(*gtsmodel.FollowRequest)
	if !ok {
		return errors.New("reject was not parseable as *gtsmodel.FollowRequest")
	}

	return p.federateRejectFollowRequest(ctx, followRequest)
}

func (p *Processor) undoFollowFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	follow, ok := clientMsg.GTSModel.(*gtsmodel.Follow)
	if !ok {
		return errors.New("undo was not parseable as *gtsmodel.Follow")
	}
	return p.federateUnfollow(ctx, follow, clientMsg.OriginAccount, clientMsg.TargetAccount)
}

func (p *Processor) undoBlockFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	block, ok := clientMsg.GTSModel.(*gtsmodel.Block)
	if !ok {
		return errors.New("undo was not parseable as *gtsmodel.Block")
	}
	return p.federateUnblock(ctx, block)
}

func (p *Processor) undoFaveFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	statusFave, ok := clientMsg.GTSModel.(*gtsmodel.StatusFave)
	if !ok {
		return gtserror.New("statusFave was not parseable as *gtsmodel.StatusFave")
	}

	// Interaction counts changed on the faved status;
	// uncache the prepared version from all timelines.
	p.invalidateStatusFromTimelines(ctx, statusFave.StatusID)

	if err := p.federateUnfave(ctx, statusFave, clientMsg.OriginAccount, clientMsg.TargetAccount); err != nil {
		return gtserror.Newf("error federating status unfave: %w", err)
	}

	return nil
}

func (p *Processor) undoAnnounceFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	status, ok := clientMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return errors.New("boost was not parseable as *gtsmodel.Status")
	}

	if err := p.state.DB.DeleteStatusByID(ctx, status.ID); err != nil {
		return gtserror.Newf("db error deleting boost: %w", err)
	}

	if err := p.deleteStatusFromTimelines(ctx, status.ID); err != nil {
		return gtserror.Newf("error removing boost from timelines: %w", err)
	}

	// Interaction counts changed on the boosted status;
	// uncache the prepared version from all timelines.
	p.invalidateStatusFromTimelines(ctx, status.BoostOfID)

	if err := p.federateUnannounce(ctx, status, clientMsg.OriginAccount, clientMsg.TargetAccount); err != nil {
		return gtserror.Newf("error federating status unboost: %w", err)
	}

	return nil
}

func (p *Processor) deleteStatusFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	status, ok := clientMsg.GTSModel.(*gtsmodel.Status)
	if !ok {
		return gtserror.New("status was not parseable as *gtsmodel.Status")
	}

	if err := p.state.DB.PopulateStatus(ctx, status); err != nil {
		return gtserror.Newf("db error populating status: %w", err)
	}

	// Don't delete attachments, just unattach them: this
	// request comes from the client API and the poster
	// may want to use attachments again in a new post.
	deleteAttachments := false
	if err := p.wipeStatus(ctx, status, deleteAttachments); err != nil {
		return gtserror.Newf("error wiping status: %w", err)
	}

	if status.InReplyToID != "" {
		// Interaction counts changed on the replied status;
		// uncache the prepared version from all timelines.
		p.invalidateStatusFromTimelines(ctx, status.InReplyToID)
	}

	if err := p.federateStatusDelete(ctx, status); err != nil {
		return gtserror.Newf("error federating status delete: %w", err)
	}

	return nil
}

func (p *Processor) deleteAccountFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	// the origin of the delete could be either a domain block, or an action by another (or this) account
	var origin string
	if domainBlock, ok := clientMsg.GTSModel.(*gtsmodel.DomainBlock); ok {
		// origin is a domain block
		origin = domainBlock.ID
	} else {
		// origin is whichever account caused this message
		origin = clientMsg.OriginAccount.ID
	}

	if err := p.federateAccountDelete(ctx, clientMsg.TargetAccount); err != nil {
		return err
	}

	return p.account.Delete(ctx, clientMsg.TargetAccount, origin)
}

func (p *Processor) reportAccountFromClientAPI(ctx context.Context, clientMsg messages.FromClientAPI) error {
	report, ok := clientMsg.GTSModel.(*gtsmodel.Report)
	if !ok {
		return errors.New("report was not parseable as *gtsmodel.Report")
	}

	if *report.Forwarded {
		if err := p.federateReport(ctx, report); err != nil {
			return fmt.Errorf("processReportAccountFromClientAPI: error federating report: %w", err)
		}
	}

	if err := p.emailReportOpened(ctx, report); err != nil {
		return fmt.Errorf("processReportAccountFromClientAPI: error notifying report: %w", err)
	}

	return nil
}
