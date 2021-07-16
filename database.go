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
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/jackc/pgx/v4/stdlib"
	_ "github.com/mattn/go-sqlite3"

	log "maunium.net/go/maulogger/v2"
)

type Database struct {
	conn   *sql.DB
	scheme string
}

var knownSchemes = map[string]string{
	"sqlite": "sqlite3",
	"sqlite3": "sqlite3",
	"postgres": "pgx",
	"postgresql": "pgx",
	"pgx": "pgx",
}

// Connect creates a new pgx connection pool.
func Connect(dbURL string) (*Database, error) {
	var localDB Database
	parsedURL, err := url.Parse(dbURL)
	if err != nil {
		return nil, err
	}
	var ok bool
	localDB.scheme, ok = knownSchemes[parsedURL.Scheme]
	if !ok {
		return nil, fmt.Errorf("unsupported database scheme '%s'", parsedURL.Scheme)
	}
	if localDB.scheme == "sqlite3" {
		newDBURL := strings.TrimPrefix(parsedURL.Path, "/")
		if len(newDBURL) == 0 {
			return nil, fmt.Errorf("invalid database URL '%s', missing a slash?", dbURL)
		}
		dbURL = newDBURL
	}
	localDB.conn, err = sql.Open(localDB.scheme, dbURL)
	return &localDB, err
}

type Upgrade struct {
	Message string
	Func    func(conn *sql.Tx) error
}

var upgrades = []Upgrade{{
	"Initial version",
	func(conn *sql.Tx) error {
		_, err := conn.Exec(`
			CREATE TABLE targets (
				appservice_id    TEXT    PRIMARY KEY,
				bot_access_token TEXT    NOT NULL,
				hs_token         TEXT    NOT NULL,
				address          TEXT    NOT NULL,
				user_id          TEXT    NOT NULL,
				device_id        TEXT    NOT NULL,
				is_proxy         BOOLEAN NOT NULL,
				next_batch       TEXT    NOT NULL,
				active           BOOLEAN DEFAULT false
			);
		`)
		return err
	},
}}

func setVersion(conn *sql.Tx, version int) error {
	_, err := conn.Exec("DELETE FROM version")
	if err != nil {
		return fmt.Errorf("failed to delete current version row: %w", err)
	}
	_, err = conn.Exec("INSERT INTO version VALUES ($1)", version)
	if err != nil {
		return fmt.Errorf("failed to insert new version row: %w", err)
	}
	return nil
}

// Upgrade updates the database schema to the latest version.
func (db *Database) Upgrade() error {
	_, err := db.conn.Exec("CREATE TABLE IF NOT EXISTS version (version INTEGER PRIMARY KEY)")
	if err != nil {
		return fmt.Errorf("failed to ensure version table exists: %w", err)
	}
	var version int
	err = db.conn.QueryRow("SELECT version FROM version").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("failed to get current database schema version: %w", err)
	}

	if len(upgrades) > version {
		for index, upgrade := range upgrades[version:] {
			newVersion := version + index + 1
			log.Infofln("Updating database schema to v%d: %s", newVersion, upgrade.Message)
			var tx *sql.Tx
			if tx, err = db.conn.Begin(); err != nil {
				return fmt.Errorf("failed to begin transaction to upgrade database schema to v%d: %w", newVersion, err)
			} else if err = upgrade.Func(tx); err != nil {
				return fmt.Errorf("failed to upgrade database schema to v%d: %w", newVersion, err)
			} else if err = setVersion(tx, newVersion); err != nil {
				return fmt.Errorf("failed to store new version v%d in database: %w", newVersion, err)
			} else if err = tx.Commit(); err != nil {
				return fmt.Errorf("failed to commit upgrade of database schema to v%d: %w", newVersion, err)
			}
		}
		log.Infofln("Database schema update to v%d", len(upgrades))
	}
	return nil
}
