/*
	GoToSocial
	Copyright (C) GoToSocial Authors admin@gotosocial.org
	SPDX-License-Identifier: AGPL-3.0-or-later

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU Affero General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU Affero General Public License for more details.

	You should have received a copy of the GNU Affero General Public License
	along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

"use strict";

const React = require("react");
const { Link } = require("wouter");

module.exports = function Username({ user, link = true }) {
	let className = "user";
	let isLocal = user.domain == null;

	if (user.suspended) {
		className += " suspended";
	}

	if (isLocal) {
		className += " local";
	}

	let icon = isLocal
		? { fa: "fa-home", info: "Local user" }
		: { fa: "fa-external-link-square", info: "Remote user" };

	let Element = "div";
	let href = null;

	if (link) {
		Element = Link;
		href = `/settings/admin/accounts/${user.id}`;
	}

	return (
		<Element className={className} to={href}>
			<span className="acct">@{user.account.acct}</span>
			<i className={`fa fa-fw ${icon.fa}`} aria-hidden="true" title={icon.info} />
			<span className="sr-only">{icon.info}</span>
		</Element>
	);
};