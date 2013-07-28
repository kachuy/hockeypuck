/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012, 2013  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package openpgp

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/cmars/sqlx"
	_ "github.com/lib/pq"
	"io"
	. "launchpad.net/hockeypuck"
	"launchpad.net/hockeypuck/hkp"
	"log"
	"strings"
)

const LOOKUP_RESULT_LIMIT = 100

type Worker struct {
	Service    *hkp.Service
	KeyChanges KeyChangeChan
	db         *sqlx.DB
}

func (s *OpenpgpSettings) Driver() string {
	return s.GetStringDefault("hockeypuck.openpgp.db.driver", "postgres")
}

func (s *OpenpgpSettings) DSN() string {
	return s.GetStringDefault("hockeypuck.openpgp.db.dsn",
		"dbname=hkp host=/var/run/postgresql sslmode=disable")
}

func StartWorker(service *hkp.Service) error {
	w := &Worker{Service: service}
	err := w.initDb()
	if err != nil {
		return err
	}
	go w.Run()
	return nil
}

func (w *Worker) Run() {
	for {
		select {
		case req, ok := <-w.Service.Requests:
			switch r := req.(type) {
			case *hkp.Lookup:
				w.Lookup(r)
			case *hkp.Add:
				w.Add(r)
			case *hkp.HashQuery:
				w.HashQuery(r)
			default:
				log.Println("Unsupported HKP service request:", req)
			}
			if !ok {
				return
			}
		}
	}
}

func (w *Worker) initDb() (err error) {
	w.db, err = sqlx.Connect(OpenpgpConfig().Driver(), OpenpgpConfig().DSN())
	if err != nil {
		return
	}
	// Create tables and indexes (idempotent).
	w.CreateTables()
	w.CreateIndexes()
	return
}

func (w *Worker) Lookup(l *hkp.Lookup) {
	// Dispatch the lookup operation to the correct query
	if l.Op == hkp.Stats {
		w.Stats(l)
		return
	} else if l.Op == hkp.UnknownOperation {
		l.Response() <- &ErrorResponse{hkp.ErrorUnknownOperation("")}
		return
	}
	var keys []*Pubkey
	var limit int = LOOKUP_RESULT_LIMIT
	var err error
	if l.Op == hkp.HashGet {
		keys, err = w.LookupHash(l.Search)
	} else {
		keys, err = w.LookupKeys(l.Search, limit)
	}
	if err != nil {
		l.Response() <- &ErrorResponse{err}
		return
	}
	// Formulate a response
	var resp hkp.Response
	switch l.Op {
	case hkp.Get:
		resp = &KeyringResponse{keys}
	case hkp.HashGet:
		resp = &KeyringResponse{keys}
	case hkp.Index:
		resp = &IndexResponse{Lookup: l, Keys: keys, Verbose: false}
	case hkp.Vindex:
		resp = &IndexResponse{Lookup: l, Keys: keys, Verbose: true}
	default:
		resp = &ErrorResponse{ErrUnsupportedOperation}
		return
	}
	l.Response() <- resp
}

func (w *Worker) HashQuery(hq *hkp.HashQuery) {
	var uuids []string
	for _, digest := range hq.Digests {
		uuid, err := w.lookupMd5Uuid(digest)
		if err != nil {
			hq.Response() <- &ErrorResponse{err}
		}
		uuids = append(uuids, uuid)
	}
	keys, err := w.fetchKeys(uuids)
	if err != nil {
		hq.Response() <- &ErrorResponse{err}
	}
	hq.Response() <- &HashQueryResponse{keys}
}

func (w *Worker) LookupKeys(search string, limit int) (keys []*Pubkey, err error) {
	uuids, err := w.lookupPubkeyUuids(search, limit)
	return w.fetchKeys(uuids)
}

func (w *Worker) LookupHash(digest string) ([]*Pubkey, error) {
	uuid, err := w.lookupMd5Uuid(digest)
	if err != nil {
		return nil, err
	}
	return w.fetchKeys([]string{uuid})
}

func (w *Worker) WriteKeys(wr io.Writer, uuids []string) error {
	// Stream OpenPGP binary packets directly out of the database.
	stmt, err := w.db.Preparex(`
SELECT bytea FROM openpgp_pubkey pk WHERE uuid = $1 UNION
SELECT bytea FROM openpgp_sig s
	JOIN openpgp_pubkey_sig pks ON (s.uuid = pks.sig_uuid)
	WHERE pks.pubkey_uuid = $1 ORDER BY creation UNION
SELECT bytea FROM (
	SELECT bytea, 1 AS level, uuid AS subkey_uuid
		FROM openpgp_subkey sk WHERE pubkey_uuid = $1 UNION
	SELECT bytea, 2 AS level, subkey_uuid FROM openpgp_sig s
		JOIN openpgp_subkey_sig sks ON (s.uuid = sks.sig_uuid)
		WHERE sks.pubkey_uuid = $1) ORDER BY subkey_uuid, level UNION
SELECT bytea FROM (
	SELECT bytea, 1 AS level, uuid AS uid_uuid, creation
		FROM openpgp_uid u WHERE pubkey_uuid = $1 UNION
	SELECT bytea, 2 AS level, uid_uuid, creation FROM openpgp_sig s
		JOIN openpgp_uid_sig us ON (s.uuid = us.sig_uuid)
		WHERE us.pubkey_uuid = $1) ORDER BY creation, uid_uuid, level UNION
SELECT bytea FROM (
	SELECT bytea, 1 AS level, uuid AS uat, creation
		FROM openpgp_uat u WHERE pubkey_uuid = $1 UNION
	SELECT bytea, 2 AS level, uat, creation FROM openpgp_sig s
		JOIN openpgp_uat_sig uas ON (s.uuid = uas.sig_uuid)
		WHERE uas.pubkey_uuid = $1) ORDER BY creation, uat_uuid, level`)
	if err != nil {
		return err
	}
	var packet struct{ data []byte }
	for _, uuid := range uuids {
		rows, err := stmt.Query(uuid)
		if err != nil {
			return err
		}
		for rows.Next() {
			err = rows.Scan(&packet)
			if err != nil {
				return err
			}
			_, err = wr.Write(packet.data)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Worker) lookupPubkeyUuids(search string, limit int) (uuids []string, err error) {
	if strings.HasPrefix(search, "0x") {
		return w.lookupKeywordUuids(search, limit)
	}
	return w.lookupKeyidUuids(search)
}

func (w *Worker) lookupMd5Uuid(hash string) (uuid string, err error) {
	rows, err := w.db.Queryx(`SELECT uuid FROM openpgp_pubkey WHERE md5 = $1`, hash)
	if err != nil {
		return
	}
	var uuids []string
	uuids, err = flattenUuidRows(rows)
	if err != nil {
		return
	}
	if len(uuids) < 1 {
		return "", ErrKeyNotFound
	}
	uuid = uuids[0]
	if len(uuids) > 1 {
		return uuid, ErrKeyIdCollision
	}
	return
}

func (w *Worker) lookupKeyidUuids(keyId string) (uuids []string, err error) {
	keyId = strings.ToLower(keyId)
	raw, err := hex.DecodeString(keyId)
	if err != nil {
		return nil, ErrInvalidKeyId
	}
	rKeyId := Reverse(keyId)
	var compareOp string
	switch len(raw) {
	case 4:
		compareOp = "LIKE $1 || '________________________________'"
	case 8:
		compareOp = "LIKE $1 || '________________________'"
	case 20:
		return []string{rKeyId}, nil
	default:
		return nil, ErrInvalidKeyId
	}
	rows, err := w.db.Queryx(fmt.Sprintf(`
SELECT uuid FROM openpgp_pubkey WHERE rfingerprint %s
	AND expiration > now() AND revsig_uuid IS NULL`, compareOp), rKeyId)
	if err != nil {
		return
	}
	return flattenUuidRows(rows)
}

func flattenUuidRows(rows *sqlx.Rows) (uuids []string, err error) {
	var row struct{ uuid string }
	for rows.Next() {
		err = rows.StructScan(&row)
		if err != nil {
			return
		}
		uuids = append(uuids, row.uuid)
	}
	return
}

func (w *Worker) lookupKeywordUuids(search string, limit int) (uuids []string, err error) {
	rows, err := w.db.Queryx(`
SELECT DISTINCT pk.uuid FROM openpgp_pubkey pk
	JOIN openpgp_uid u ON (pk.uuid = u.pubkey_uuid)
WHERE u.keywords_fulltext @@ to_tsquery($1)
	AND pk.expiration < now() AND pk.revsig_uuid IS NULL
	AND u.expiration < now()
	AND EXISTS (
		SELECT 1 FROM openpgp_uid_sig usig 
			JOIN openpgp_sig s ON (usig.sig_uuid = s.uuid)
		WHERE usig.uid_uuid = u.uuid AND s.expiration < now() 
			AND s.signer_uuid = pk.uuid AND sig_type BETWEEN 0x10 AND 0x13) LIMIT $2`,
		search, limit)
	if err != nil {
		return
	}
	return flattenUuidRows(rows)
}

var ErrInternalKeyInvalid error = errors.New("Internal integrity error matching key")

func (w *Worker) LookupKey(keyid string) (pubkey *Pubkey, err error) {
	uuids, err := w.lookupKeyidUuids(keyid)
	if err != nil {
		return nil, err
	}
	if len(uuids) < 1 {
		return nil, ErrKeyNotFound
	}
	if len(uuids) > 1 {
		return nil, ErrKeyIdCollision
	}
	keys, err := w.fetchKeys(uuids)
	if err != nil {
		return nil, err
	}
	if len(keys) != 1 {
		return nil, ErrInternalKeyInvalid
	}
	return keys[0], nil
}

func (w *Worker) fetchKeys(uuids []string) (keys []*Pubkey, err error) {
	var key *Pubkey
	for _, uuid := range uuids {
		key, err = w.fetchKey(uuid)
		if err != nil {
			return
		}
		keys = append(keys, key)
	}
	return
}

func (w *Worker) fetchKey(uuid string) (pubkey *Pubkey, err error) {
	pubkey = new(Pubkey)
	err = w.db.Get(pubkey, `SELECT * FROM openpgp_pubkey WHERE uuid = $1`, uuid)
	if err != nil {
		return
	}
	// Retrieve all signatures made directly on the primary public key
	sigs := []Signature{}
	err = w.db.Select(&sigs, `
SELECT sig.* FROM openpgp_sig sig
	JOIN openpgp_pubkey_sig pksig ON (sig.uuid = pksig.sig_uuid)
WHERE pksig.pubkey_uuid = $1`, uuid)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	pubkey.signatures = toSigPtrSlice(sigs)
	// Retrieve all uid records
	uids := []UserId{}
	err = w.db.Select(&uids, `
SELECT uuid, creation, expiration, state, packet,
	pubkey_uuid, revsig_uuid, keywords
FROM openpgp_uid WHERE pubkey_uuid = $1`, uuid)
	log.Println("uid select:", uids, err)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	pubkey.userIds = toUidPtrSlice(uids)
	for _, uid := range pubkey.userIds {
		sigs = []Signature{}
		err = w.db.Select(&sigs, `
SELECT sig.* FROM openpgp_sig sig
	JOIN openpgp_uid_sig usig ON (sig.uuid = usig.sig_uuid)
WHERE usig.uid_uuid = $1`, uid.ScopedDigest)
		if err != nil && err != sql.ErrNoRows {
			return
		}
		uid.signatures = toSigPtrSlice(sigs)
	}
	// Retrieve all user attribute records
	uats := []UserAttribute{}
	err = w.db.Select(&uats,
		`SELECT * FROM openpgp_uat WHERE pubkey_uuid = $1`, uuid)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	pubkey.userAttributes = toUatPtrSlice(uats)
	for _, uat := range pubkey.userAttributes {
		sigs = []Signature{}
		err = w.db.Select(&sigs, `
SELECT sig.* FROM openpgp_sig sig
	JOIN openpgp_uat_sig usig ON (sig.uuid = usig.sig_uuid)
WHERE usig.uat_uuid = $1`, uat.ScopedDigest)
		if err != nil && err != sql.ErrNoRows {
			return
		}
		uat.signatures = toSigPtrSlice(sigs)
	}
	// Retrieve all subkey records
	subkeys := []Subkey{}
	err = w.db.Select(&subkeys,
		`SELECT * FROM openpgp_subkey WHERE pubkey_uuid = $1`, uuid)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	pubkey.subkeys = toSubkeyPtrSlice(subkeys)
	for _, subkey := range pubkey.subkeys {
		sigs = []Signature{}
		err = w.db.Select(&sigs, `
SELECT sig.* FROM openpgp_sig sig
	JOIN openpgp_subkey_sig sksig ON (sig.uuid = sksig.sig_uuid)
WHERE sksig.subkey_uuid = $1`, subkey.RFingerprint)
		if err != nil && err != sql.ErrNoRows {
			return
		}
		subkey.signatures = toSigPtrSlice(sigs)
	}
	kv := ValidateKey(pubkey)
	return kv.Pubkey, kv.KeyError
}

func toSigPtrSlice(recs []Signature) (result []*Signature) {
	for i := 0; i < len(recs); i++ {
		result = append(result, &(recs[i]))
	}
	return
}

func toUidPtrSlice(recs []UserId) (result []*UserId) {
	for i := 0; i < len(recs); i++ {
		result = append(result, &(recs[i]))
	}
	return
}

func toUatPtrSlice(recs []UserAttribute) (result []*UserAttribute) {
	for i := 0; i < len(recs); i++ {
		result = append(result, &(recs[i]))
	}
	return
}

func toSubkeyPtrSlice(recs []Subkey) (result []*Subkey) {
	for i := 0; i < len(recs); i++ {
		result = append(result, &(recs[i]))
	}
	return
}
