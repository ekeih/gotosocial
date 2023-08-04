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

	"github.com/superseriousbusiness/gotosocial/internal/config"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/email"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
)

func (p *Processor) emailReportOpened(ctx context.Context, report *gtsmodel.Report) error {
	instance, err := p.state.DB.GetInstance(ctx, config.GetHost())
	if err != nil {
		return gtserror.Newf("error getting instance: %w", err)
	}

	toAddresses, err := p.state.DB.GetInstanceModeratorAddresses(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNoEntries) {
			// No registered moderator addresses.
			return nil
		}
		return gtserror.Newf("error getting instance moderator addresses: %w", err)
	}

	if report.Account == nil {
		report.Account, err = p.state.DB.GetAccountByID(ctx, report.AccountID)
		if err != nil {
			return gtserror.Newf("error getting report account: %w", err)
		}
	}

	if report.TargetAccount == nil {
		report.TargetAccount, err = p.state.DB.GetAccountByID(ctx, report.TargetAccountID)
		if err != nil {
			return gtserror.Newf("error getting report target account: %w", err)
		}
	}

	reportData := email.NewReportData{
		InstanceURL:        instance.URI,
		InstanceName:       instance.Title,
		ReportURL:          instance.URI + "/settings/admin/reports/" + report.ID,
		ReportDomain:       report.Account.Domain,
		ReportTargetDomain: report.TargetAccount.Domain,
	}

	if err := p.emailSender.SendNewReportEmail(toAddresses, reportData); err != nil {
		return gtserror.Newf("error emailing instance moderators: %w", err)
	}

	return nil
}

func (p *Processor) emailReportClosed(ctx context.Context, report *gtsmodel.Report) error {
	user, err := p.state.DB.GetUserByAccountID(ctx, report.Account.ID)
	if err != nil {
		return gtserror.Newf("db error getting user: %w", err)
	}

	if user.ConfirmedAt.IsZero() || !*user.Approved || *user.Disabled || user.Email == "" {
		// Only email users who:
		// - are confirmed
		// - are approved
		// - are not disabled
		// - have an email address
		return nil
	}

	instance, err := p.state.DB.GetInstance(ctx, config.GetHost())
	if err != nil {
		return gtserror.Newf("db error getting instance: %w", err)
	}

	if report.Account == nil {
		report.Account, err = p.state.DB.GetAccountByID(ctx, report.AccountID)
		if err != nil {
			return gtserror.Newf("error getting report account: %w", err)
		}
	}

	if report.TargetAccount == nil {
		report.TargetAccount, err = p.state.DB.GetAccountByID(ctx, report.TargetAccountID)
		if err != nil {
			return gtserror.Newf("error getting report target account: %w", err)
		}
	}

	reportClosedData := email.ReportClosedData{
		Username:             report.Account.Username,
		InstanceURL:          instance.URI,
		InstanceName:         instance.Title,
		ReportTargetUsername: report.TargetAccount.Username,
		ReportTargetDomain:   report.TargetAccount.Domain,
		ActionTakenComment:   report.ActionTaken,
	}

	return p.emailSender.SendReportClosedEmail(user.Email, reportClosedData)
}
