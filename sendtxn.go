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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
)

const txnIDFormat = "fi.mau.syncproxy_%d_%d"
const wrapperTxnIDFormat = "fi.mau.syncproxy.wrapper_%d_%d"

const initialTransactionRetrySleep = 2 * time.Second
const maxTransactionRetryInterval = 120 * time.Second

var errFiMauWsNotConnected = mautrix.RespError{ErrCode: "FI.MAU.WS_NOT_CONNECTED"}
var errWebsocketNotConnected = fmt.Errorf("server said the transaction websocket is not connected")

type SendStatus string

const (
	SendStatusOK                    SendStatus = "ok"
	SendStatusWebsocketNotConnected SendStatus = "websocket-not-connected"
)

type transactionRequest struct {
	*appservice.Transaction
	WrappedTxnID  string   `json:"fi.mau.syncproxy.transaction_id,omitempty"`
	SynchronousTo []string `json:"com.beeper.asmux.synchronous_to,omitempty"`
}

type ProxyError string

const (
	ProxyErrorLoggedOut ProxyError = "FI.MAU.CLIENT_LOGGED_OUT"
	ProxyErrorUnknown   ProxyError = "M_UNKNOWN"
)

type errorRequest struct {
	Error        ProxyError `json:"errcode"`
	Message      string     `json:"error"`
	WrappedTxnID string     `json:"fi.mau.syncproxy.transaction_id,omitempty"`
}

type transactionResponse struct {
	Synchronous bool                  `json:"com.beeper.asmux.synchronous"`
	SentTo      map[string]SendStatus `json:"com.beeper.asmux.sent_to,omitempty"`
}

var lastTxnID uint64

func nextTxnID(format string) (uint64, string) {
	txnIDCounter := atomic.AddUint64(&lastTxnID, 1)
	return txnIDCounter, fmt.Sprintf(format,
		time.Now().UnixNano(),
		txnIDCounter)
}

func (target *SyncTarget) tryPostTransaction(ctx context.Context, txn *appservice.Transaction, error *errorRequest) error {
	counter, txnID := nextTxnID(txnIDFormat)
	txnLog := ctx.Value(logContextKey).(maulogger.Logger).Sub(fmt.Sprintf("Txn-%d", counter))
	ctx = context.WithValue(ctx, logContextKey, txnLog)

	if txn != nil {
		deviceListChanges := 0
		if txn.DeviceLists != nil {
			deviceListChanges = len(txn.DeviceLists.Changed)
		}
		txnLog.Debugfln("Sending %d to-device events, %d device list changes and %d OTK counts to %s in transaction %s",
			len(txn.EphemeralEvents), deviceListChanges, len(txn.DeviceOTKCount), target.AppserviceID, txnID)
	} else {
		txnLog.Debugfln("Sending error '%s' to %s in transaction %s", error.Error, target.AppserviceID, txnID)
	}

	retryIn := initialTransactionRetrySleep
	attemptNo := 1
	for {
		err := target.postTransaction(ctx, txn, error, txnID, attemptNo)
		attemptNo += 1
		if err == nil {
			return nil
		} else if ctx.Err() != nil {
			if err != ctx.Err() {
				txnLog.Debugfln("Sending transaction %s returned error %v, but context had different error %v", txnID, err, ctx.Err())
			}
			return ctx.Err()
		} else if errors.Is(err, errWebsocketNotConnected) {
			// Assume that the server will ask as to restart syncing when the websocket does connect again.
			return err
		}

		txnLog.Warnfln("Failed to send transaction %s: %v. Retrying in %v", txnID, err, retryIn)
		select {
		case <-time.After(retryIn):
		case <-ctx.Done():
			txnLog.Debugfln("Context returned error while waiting to retry transaction %s", txnID)
			return ctx.Err()
		}
		retryIn *= 2
		if retryIn > maxTransactionRetryInterval {
			retryIn = maxTransactionRetryInterval
		}
	}
}

func createTxnURL(address, appserviceID, txnID string, isError bool) (string, error) {
	parsedURL, err := url.Parse(address)
	if err != nil {
		return "", fmt.Errorf("failed to parse target URL: %w", err)
	}
	if isError {
		parsedURL.Path = fmt.Sprintf("/_matrix/app/unstable/fi.mau.syncproxy/error/%s", txnID)
	} else {
		parsedURL.Path = fmt.Sprintf("/_matrix/app/v1/transactions/%s", txnID)
	}
	q := parsedURL.Query()
	q.Add("appservice_id", appserviceID)
	parsedURL.RawQuery = q.Encode()
	return parsedURL.String(), nil
}

func closeBody(body io.ReadCloser) {
	_ = body.Close()
}

func (target *SyncTarget) postTransaction(ctx context.Context, txn *appservice.Transaction, error *errorRequest, txnID string, attemptNo int) error {
	txnLog := ctx.Value(logContextKey).(maulogger.Logger)
	var buf bytes.Buffer
	var req *http.Request
	var resp *http.Response
	var respData transactionResponse
	var txnData interface{}
	if txn != nil {
		txnData = &transactionRequest{
			Transaction:   txn,
			WrappedTxnID:  txnID,
			SynchronousTo: []string{target.AppserviceID},
		}
	} else {
		error.WrappedTxnID = txnID
		txnData = error
	}

	pathTxnID := txnID
	if target.IsProxy {
		_, pathTxnID = nextTxnID(wrapperTxnIDFormat)
	}
	txnLog.Debugfln("Attempt #%d for transaction %s (path: %s)", attemptNo, txnID, pathTxnID)

	if txnURL, err := createTxnURL(target.Address, target.AppserviceID, pathTxnID, error != nil); err != nil {
		return fmt.Errorf("failed to form transaction URL: %w", err)
	} else if err = json.NewEncoder(&buf).Encode(txnData); err != nil {
		return fmt.Errorf("failed to encode transaction JSON: %w", err)
	} else if req, err = http.NewRequestWithContext(ctx, http.MethodPut, txnURL, &buf); err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	} else if req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", target.HSToken)); len(target.HSToken) == 0 {
		return fmt.Errorf("target is missing hs_token")
	} else if resp, err = http.DefaultClient.Do(req); err != nil {
		return fmt.Errorf("failed to send transaction: %w", err)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		var respErr mautrix.RespError
		if err := json.NewDecoder(resp.Body).Decode(&respErr); err != nil {
			return fmt.Errorf("transaction returned HTTP %d and non-JSON body", resp.StatusCode)
		} else if errors.Is(respErr, errFiMauWsNotConnected) {
			return errWebsocketNotConnected
		} else {
			return fmt.Errorf("transaction returned HTTP %d: %w", resp.StatusCode, err)
		}
	} else if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		return fmt.Errorf("transaction returned HTTP %d, but had non-JSON body: %v", resp.StatusCode, err)
	} else if !respData.Synchronous && cfg.ExpectSynchronous {
		return fmt.Errorf("transaction returned HTTP %d, but EXPECT_SYNCHRONOUS is set and server didn't confirm support for synchronous delivery", resp.StatusCode)
	} else if respData.Synchronous && respData.SentTo == nil {
		return fmt.Errorf("transaction returned HTTP %d, but synchronous delivery confirmation was missing `com.beeper.asmux.sent_to` field", resp.StatusCode)
	} else if respData.Synchronous {
		status, ok := respData.SentTo[target.AppserviceID]
		if status == SendStatusOK {
			txnLog.Debugfln("Successfully sent transaction %s with synchronous delivery confirmation for %s on attempt #%d", txnID, target.AppserviceID, attemptNo)
			return nil
		} else if status == SendStatusWebsocketNotConnected {
			return errWebsocketNotConnected
		} else if ok {
			return fmt.Errorf("transaction returned HTTP %d, but server said it didn't reach the appservice (status %s)", resp.StatusCode, status)
		} else {
			return fmt.Errorf("transaction returned HTTP %d, but server didn't confirm synchronous delivery", resp.StatusCode)
		}
	} else {
		txnLog.Debugfln("Successfully sent transaction %s on attempt #%d", txnID, attemptNo)
		return nil
	}
}
