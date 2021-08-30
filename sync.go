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
	"context"
	"errors"
	"fmt"
	"time"

	"maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var everything = []event.Type{{Type: "*"}}
var nothing = mautrix.FilterPart{NotTypes: everything}
var syncFilter = &mautrix.Filter{
	Presence:    nothing,
	AccountData: nothing,
	Room: mautrix.RoomFilter{
		IncludeLeave: false,
		Ephemeral:    nothing,
		AccountData:  nothing,
		State:        nothing,
		Timeline:     nothing,
	},
}

const initialSyncRetrySleep = 2 * time.Second
const maxSyncRetryInterval = 120 * time.Second

func (target *SyncTarget) sync(ctx context.Context) error {
	var filterID string
	if resp, err := target.client.CreateFilter(syncFilter); err != nil {
		return fmt.Errorf("failed to create filter: %w", err)
	} else {
		filterID = resp.FilterID
	}

	var otkCountSent bool
	var prevOTKCount mautrix.OTKCount
	syncLog := ctx.Value(logContextKey).(maulogger.Logger)
	retryIn := initialSyncRetrySleep

	for {
		resp, err := target.client.SyncRequest(30000, target.NextBatch, filterID, false, event.PresenceOffline, ctx)
		if err != nil {
			if errors.Is(err, mautrix.MUnknownToken) {
				return err
			} else if ctx.Err() != nil {
				if err != ctx.Err() {
					syncLog.Debugfln("Sync returned error %v, but context had different error %v", err, ctx.Err())
				}
				return ctx.Err()
			}
			syncLog.Warnfln("Error syncing: %v. Retrying in %v", err, retryIn)
			select {
			case <-time.After(retryIn):
			case <-ctx.Done():
				syncLog.Debugfln("Context returned error while waiting to retry sync")
				return ctx.Err()
			}
			retryIn *= 2
			if retryIn > maxSyncRetryInterval {
				retryIn = maxSyncRetryInterval
			}
			continue
		}
		retryIn = initialTransactionRetrySleep
		if len(resp.ToDevice.Events) > 0 || resp.DeviceOTKCount != prevOTKCount || !otkCountSent || len(resp.DeviceLists.Changed) > 0 {
			txn := syncToTransaction(resp, target.UserID, target.DeviceID, resp.DeviceOTKCount != prevOTKCount || !otkCountSent)
			prevOTKCount = resp.DeviceOTKCount
			otkCountSent = true
			err = target.tryPostTransaction(ctx, txn, nil)
			if err != nil {
				return fmt.Errorf("error sending transaction: %w", err)
			}
		}
		syncLog.Debugln("Storing new next batch token:", resp.NextBatch)
		err = target.SetNextBatch(resp.NextBatch)
		if err != nil {
			syncLog.Warnln("Failed to store next batch in database:", err)
		}
	}
}

func syncToTransaction(resp *mautrix.RespSync, userID id.UserID, deviceID id.DeviceID, sendOTKs bool) *appservice.Transaction {
	var txn appservice.Transaction
	if resp != nil {
		if len(resp.ToDevice.Events) > 0 {
			txn.EphemeralEvents = resp.ToDevice.Events
			txn.MSC2409EphemeralEvents = txn.EphemeralEvents
			for _, evt := range txn.EphemeralEvents {
				evt.ToUserID = userID
				evt.ToDeviceID = deviceID
			}
		}
		if len(resp.DeviceLists.Changed) > 0 || len(resp.DeviceLists.Left) > 0 {
			txn.DeviceLists = &resp.DeviceLists
			txn.MSC3202DeviceLists = txn.DeviceLists
		}
		if sendOTKs {
			txn.DeviceOTKCount = map[id.UserID]mautrix.OTKCount{
				userID: resp.DeviceOTKCount,
			}
			txn.MSC3202DeviceOTKCount = txn.DeviceOTKCount
		}
	}
	return &txn
}
