// mautrix-syncproxy - A /sync proxy for encrypted Matrix appservices.
// Copyright (C) 2021 Tulir Asokan
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
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/appservice"
)

var (
	errTargetNotFound = appservice.Error{
		HTTPStatus: http.StatusNotFound,
		ErrorCode:  "M_NOT_FOUND",
		Message:    "No appservice found with that ID",
	}
	errTargetNotActive = appservice.Error{
		HTTPStatus: http.StatusNotFound,
		ErrorCode:  "FI.MAU.SYNCPROXY.NOT_ACTIVE",
		Message:    "That appservice is not active",
	}
	errUpsertFailed = appservice.Error{
		HTTPStatus: http.StatusInternalServerError,
		ErrorCode:  "FI.MAU.SYNCPROXY.UPSERT_FAILED",
		Message:    "Failed to insert appservice details into database",
	}
)

func startSync(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(w, r) {
		return
	}
	vars := mux.Vars(r)
	appserviceID := vars["appserviceID"]

	switch r.Method {
	case http.MethodPut:
		var req SyncTarget
		if !getJSON(w, r, &req) {
			return
		}
		log.Debugfln("Received PUT request with %+v", &req)
		req.AppserviceID = appserviceID
		target := GetOrSetTarget(appserviceID, &req)
		changed := true
		if target == nil {
			target = &req
			err := target.Init()
			if err != nil {
				target.log.Warnln("Failed to initialize new target:", err)
				appservice.Error{
					HTTPStatus: http.StatusNotFound,
					ErrorCode:  "FI.MAU.SYNCPROXY.INVALID_ADDRESS",
					Message:    fmt.Sprintf("Failed to initialize target: %v", err),
				}.Write(w)
				return
			}
		} else if target.BotAccessToken != req.BotAccessToken || target.HSToken != req.HSToken ||
			target.Address != req.Address || target.UserID != req.UserID || target.DeviceID != req.DeviceID {
			target.BotAccessToken = req.BotAccessToken
			target.HSToken = req.HSToken
			target.Address = req.Address
			target.UserID = req.UserID
			target.DeviceID = req.DeviceID
			if target.client != nil {
				target.client.AccessToken = target.BotAccessToken
				target.client.UserID = target.UserID
				target.client.DeviceID = target.DeviceID
			}
		} else {
			changed = false
		}
		if changed {
			target.log.Debugln("Upserting target for PUT request")
			err := target.Upsert()
			if err != nil {
				target.log.Warnln("Failed to upsert target:", err)
				errUpsertFailed.Write(w)
				return
			}
		}
		target.log.Debugln("Starting target for PUT request")
		go target.Start()
		appservice.WriteBlankOK(w)
	case http.MethodDelete:
		target := GetOrSetTarget(appserviceID, nil)
		if target == nil {
			log.Debugln("Client requested stopping unknown appservice", appserviceID)
			errTargetNotFound.Write(w)
			return
		} else if !target.Active {
			log.Debugln("Client requested stopping inactive appservice", appserviceID)
			errTargetNotActive.Write(w)
			return
		}
		target.Stop()
		target.log.Debugln("Waiting for syncing to stop")
		target.wg.Wait()
		target.log.Infoln("Target stopped after DELETE request")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func checkAuth(w http.ResponseWriter, r *http.Request) bool {
	var token string
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		token = r.URL.Query().Get("access_token")
	} else {
		token = authHeader[len("Bearer "):]
	}
	w.Header().Add("Content-Type", "application/json")
	if len(token) == 0 {
		appservice.Error{
			HTTPStatus: http.StatusUnauthorized,
			ErrorCode:  "M_MISSING_TOKEN",
			Message:    "Missing authorization header",
		}.Write(w)
		return false
	}
	if token != cfg.SharedSecret {
		appservice.Error{
			HTTPStatus: http.StatusUnauthorized,
			ErrorCode:  "M_UNKNOWN_TOKEN",
			Message:    "Unknown authorization token",
		}.Write(w)
		return false
	}
	return true
}

func getJSON(w http.ResponseWriter, r *http.Request, into interface{}) bool {
	err := json.NewDecoder(r.Body).Decode(&into)
	if err != nil {
		appservice.Error{
			HTTPStatus: http.StatusBadRequest,
			ErrorCode:  "M_BAD_JSON",
			Message:    "Failed to decode request JSON",
		}.Write(w)
		return false
	}
	return true
}
