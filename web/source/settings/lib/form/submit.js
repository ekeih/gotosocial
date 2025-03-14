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

const Promise = require("bluebird");
const React = require("react");
const getFormMutations = require("./get-form-mutations");

module.exports = function useFormSubmit(form, mutationQuery, { changedOnly = true, onFinish } = {}) {
	if (!Array.isArray(mutationQuery)) {
		throw new ("useFormSubmit: mutationQuery was not an Array. Is a valid useMutation RTK Query provided?");
	}
	const [runMutation, result] = mutationQuery;
	const usedAction = React.useRef(null);
	return [
		function submitForm(e) {
			let action;
			if (e?.preventDefault) {
				e.preventDefault();
				action = e.nativeEvent.submitter.name;
			} else {
				action = e;
			}

			if (action == "") {
				action = undefined;
			}
			usedAction.current = action;
			// transform the field definitions into an object with just their values 

			const { mutationData, updatedFields } = getFormMutations(form, { changedOnly });

			if (updatedFields.length == 0) {
				return;
			}

			mutationData.action = action;

			return Promise.try(() => {
				return runMutation(mutationData);
			}).then((res) => {
				if (onFinish) {
					return onFinish(res);
				}
			});
		},
		{
			...result,
			action: usedAction.current
		}
	];
};