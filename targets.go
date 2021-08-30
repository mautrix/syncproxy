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
	"runtime/debug"
	"sync"
	"sync/atomic"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

var targets = make(map[string]*SyncTarget)
var targetLock sync.Mutex

type SyncTarget struct {
	AppserviceID   string      `json:"appservice_id"`
	BotAccessToken string      `json:"bot_access_token"`
	HSToken        string      `json:"hs_token"`
	Address        string      `json:"address"`
	UserID         id.UserID   `json:"user_id"`
	DeviceID       id.DeviceID `json:"device_id"`
	IsProxy        bool        `json:"is_proxy"`

	NextBatch string `json:"-"`
	Active    bool   `json:"-"`

	client  *mautrix.Client
	log     log.Logger
	running bool
	cancel  func()
	wg      sync.WaitGroup
	lock    sync.Mutex
}

func (target *SyncTarget) Upsert() error {
	query := `
		INSERT INTO targets (appservice_id, bot_access_token, hs_token, address, user_id, device_id, is_proxy, next_batch, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (appservice_id) DO UPDATE
		SET bot_access_token=$2, hs_token=$3, address=$4, user_id=$5, device_id=$6, is_proxy=$7
	`
	if db.scheme == "sqlite3" {
		query = "INSERT OR REPLACE INTO targets (appservice_id, bot_access_token, hs_token, address, user_id, device_id, is_proxy, next_batch, active)"
	}
	_, err := db.conn.Exec(query, target.AppserviceID, target.BotAccessToken, target.HSToken, target.Address, target.UserID, target.DeviceID, target.IsProxy, target.NextBatch, target.Active)
	return err
}

func (target *SyncTarget) SetActive(active bool) error {
	if target.Active == active {
		return nil
	}
	target.Active = active
	_, err := db.conn.Exec("UPDATE targets SET active=$2 WHERE appservice_id=$1", target.AppserviceID, target.Active)
	return err
}

func (target *SyncTarget) SetNextBatch(nextBatch string) error {
	if target.NextBatch == nextBatch {
		return nil
	}
	target.NextBatch = nextBatch
	_, err := db.conn.Exec("UPDATE targets SET next_batch=$2 WHERE appservice_id=$1", target.AppserviceID, target.NextBatch)
	return err
}

func GetOrSetTarget(appserviceID string, newTarget *SyncTarget) *SyncTarget {
	targetLock.Lock()
	defer targetLock.Unlock()
	target, ok := targets[appserviceID]
	if !ok {
		if newTarget != nil {
			targets[appserviceID] = newTarget
		}
		return nil
	}
	return target
}

func LoadTargets() error {
	res, err := db.conn.Query("SELECT appservice_id, bot_access_token, hs_token, address, is_proxy, user_id, device_id, active FROM targets")
	if err != nil {
		return fmt.Errorf("failed to query targets: %w", err)
	}
	targetLock.Lock()
	defer targetLock.Unlock()
	for res.Next() {
		var target SyncTarget
		err = res.Scan(&target.AppserviceID, &target.BotAccessToken, &target.HSToken, &target.Address, &target.IsProxy, &target.UserID, &target.DeviceID, &target.Active)
		if err != nil {
			return fmt.Errorf("failed to scan target: %w", err)
		}
		err = target.Init()
		if err != nil {
			target.log.Warnln("Failed to initialize target (startup):", err)
		} else {
			targets[target.AppserviceID] = &target
		}
	}
	return nil
}

var globalSyncID uint64

const logContextKey = "log"

func (target *SyncTarget) Init() error {
	target.log = log.Sub(fmt.Sprintf("Target-%s", target.AppserviceID))
	var err error
	target.client, err = mautrix.NewClient(cfg.HomeserverURL, target.UserID, target.BotAccessToken)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	return nil
}

func (target *SyncTarget) Start() {
	syncLog := target.log.Sub(fmt.Sprintf("Sync-%d", atomic.AddUint64(&globalSyncID, 1)))
	if target.running {
		syncLog.Debugln("There seems to be an existing syncer running, stopping it first")
		target.Stop()
	}

	syncLog.Debugln("Locking mutex to start syncing")
	target.lock.Lock()
	target.wg = sync.WaitGroup{}
	target.wg.Add(1)
	target.running = true

	defer func() {
		target.running = false
		target.cancel = nil
		target.wg.Done()
		syncLog.Debugln("Unlocking mutex")
		target.lock.Unlock()
		err := recover()
		if err != nil {
			syncLog.Errorfln("Syncing panicked: %v\n%s", err, debug.Stack())
		}
	}()

	if err := target.SetActive(true); err != nil {
		syncLog.Warnln("Failed to mark target as active:", err)
	}
	defer func() {
		if err := target.SetActive(false); err != nil {
			syncLog.Warnln("Failed to mark target as inactive:", err)
		}
	}()

	ctx, cancelFunc := context.WithCancel(context.WithValue(context.Background(), logContextKey, syncLog))
	target.cancel = cancelFunc

	syncLog.Infoln("Starting syncing")
	err := target.sync(ctx)
	if errors.Is(err, context.Canceled) {
		syncLog.Infoln("Syncing stopped")
	} else if err != nil {
		syncLog.Errorfln("Syncing failed: %v, notifying target...", err)
		proxyErr := &errorRequest{
			Error:   ProxyErrorUnknown,
			Message: err.Error(),
		}
		if errors.Is(err, mautrix.MUnknownToken) {
			proxyErr.Error = ProxyErrorLoggedOut
		}
		err = target.tryPostTransaction(ctx, nil, proxyErr)
		if err != nil {
			syncLog.Warnln("Failed to notify target about sync error:", err)
		}
	}
}

func (target *SyncTarget) Stop() {
	if cancelFn := target.cancel; cancelFn != nil {
		target.log.Debugln("Stopping syncing...")
		cancelFn()
	}
}
