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

package report

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	apimodel "github.com/superseriousbusiness/gotosocial/internal/api/model"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/gtserror"
	"github.com/superseriousbusiness/gotosocial/internal/gtsmodel"
	"github.com/superseriousbusiness/gotosocial/internal/util"
)

// Get returns the user view of a moderation report, with the given id.
func (p *Processor) Get(ctx context.Context, account *gtsmodel.Account, id string) (*apimodel.Report, gtserror.WithCode) {
	report, err := p.state.DB.GetReportByID(ctx, id)
	if err != nil {
		if err == db.ErrNoEntries {
			return nil, gtserror.NewErrorNotFound(err)
		}
		return nil, gtserror.NewErrorInternalError(err)
	}

	if report.AccountID != account.ID {
		err = fmt.Errorf("report with id %s does not belong to account %s", report.ID, account.ID)
		return nil, gtserror.NewErrorNotFound(err)
	}

	apiReport, err := p.tc.ReportToAPIReport(ctx, report)
	if err != nil {
		return nil, gtserror.NewErrorInternalError(fmt.Errorf("error converting report to api: %s", err))
	}

	return apiReport, nil
}

// GetMultiple returns multiple reports created by the given account, filtered according to the provided parameters.
func (p *Processor) GetMultiple(
	ctx context.Context,
	account *gtsmodel.Account,
	resolved *bool,
	targetAccountID string,
	maxID string,
	sinceID string,
	minID string,
	limit int,
) (*apimodel.PageableResponse, gtserror.WithCode) {
	reports, err := p.state.DB.GetReports(ctx, resolved, account.ID, targetAccountID, maxID, sinceID, minID, limit)
	if err != nil && !errors.Is(err, db.ErrNoEntries) {
		return nil, gtserror.NewErrorInternalError(err)
	}

	count := len(reports)
	if count == 0 {
		return util.EmptyPageableResponse(), nil
	}

	items := make([]interface{}, 0, count)
	nextMaxIDValue := reports[count-1].ID
	prevMinIDValue := reports[0].ID

	for _, r := range reports {
		item, err := p.tc.ReportToAPIReport(ctx, r)
		if err != nil {
			return nil, gtserror.NewErrorInternalError(fmt.Errorf("error converting report to api: %s", err))
		}
		items = append(items, item)
	}

	extraQueryParams := []string{}
	if resolved != nil {
		extraQueryParams = append(extraQueryParams, "resolved="+strconv.FormatBool(*resolved))
	}
	if targetAccountID != "" {
		extraQueryParams = append(extraQueryParams, "target_account_id="+targetAccountID)
	}

	return util.PackagePageableResponse(util.PageableResponseParams{
		Items:            items,
		Path:             "/api/v1/reports",
		NextMaxIDValue:   nextMaxIDValue,
		PrevMinIDValue:   prevMinIDValue,
		Limit:            limit,
		ExtraQueryParams: extraQueryParams,
	})
}
