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

package cache

import (
	"database/sql"
	"errors"
	"time"

	errorsv2 "codeberg.org/gruf/go-errors/v2"
	"github.com/superseriousbusiness/gotosocial/internal/db"
	"github.com/superseriousbusiness/gotosocial/internal/log"
)

// SentinelError is an error that can be returned and checked against to indicate a non-permanent
// error return from a cache loader callback, e.g. a temporary situation that will soon be fixed.
var SentinelError = errors.New("BUG: error should not be returned") //nolint:revive

// ignoreErrors is an error matching function used to signal which errors
// the result caches should NOT hold onto. these amount to anything non-permanent.
func ignoreErrors(err error) bool {
	return !errorsv2.Comparable(
		err,

		// the only cacheable errs,
		// i.e anything permanent
		// (until invalidation).
		db.ErrNoEntries,
		db.ErrAlreadyExists,
		sql.ErrNoRows,
	)
}

// nocopy when embedded will signal linter to
// error on pass-by-value of parent struct.
type nocopy struct{}

func (*nocopy) Lock() {}

func (*nocopy) Unlock() {}

// tryUntil will attempt to call 'do' for 'count' attempts, before panicking with 'msg'.
func tryUntil(msg string, count int, do func() bool) {
	for i := 0; i < count; i++ {
		if do() {
			// success.
			return
		}

		// Sleep for a little before retry (a bcakoff).
		time.Sleep(time.Millisecond * 1 << (i + 1))
	}

	// panic on total failure as this shouldn't happen.
	log.Panicf(nil, "failed %s after %d tries", msg, count)
}
