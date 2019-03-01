// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package infobind

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/sqlexec"
	log "github.com/sirupsen/logrus"
)

const (
	using   = "using"
	deleted = "deleted"
)

// bindRecord store the basic bind info and bindSql astNode.
type bindRecord struct {
	*bindMeta
	ast ast.StmtNode //the ast will be used to do query sql bind check
}

// bindCache holds a bindDataMap
//   key: original sql
//   value: a slice of bindRecords.
type bindCache map[string][]*bindRecord

// Handler hold an atomic bindCache.
type Handler struct {
	atomic.Value
}

// BindCacheUpdater is used to update the global bindCache.
// BindCacheUpdater will update the bind cache pre 3 second in domain gorountine loop. When the tidb server first startup, the updater will load all bind info in memory; then load diff bind info pre 3 second.
type BindCacheUpdater struct {
	ctx sessionctx.Context

	parser         *parser.Parser
	lastUpdateTime types.Time
	globalHandler  *Handler
}

type bindMeta struct {
	OriginalSQL string
	BindSQL     string
	Db          string
	// Status will only be 0 or 1:
	//   deleted: bindMeta has been marked as deleted status.
	//   using: bindMeta is in using status.
	Status     string
	CreateTime types.Time
	UpdateTime types.Time
	Charset    string
	Collation  string
}

// NewBindCacheUpdater create a new BindCacheUpdater.
func NewBindCacheUpdater(ctx sessionctx.Context, handler *Handler, parser *parser.Parser) *BindCacheUpdater {
	return &BindCacheUpdater{
		globalHandler: handler,
		parser:        parser,
		ctx:           ctx,
	}
}

// NewHandler create a Handler with a bindCache.
func NewHandler() *Handler {
	handler := &Handler{}
	return handler
}

// Get get bindCache from a Handler.
func (h *Handler) Get() bindCache {
	bc := h.Load()

	if bc != nil {
		return bc.(map[string][]*bindRecord)
	}

	return make(map[string][]*bindRecord)
}

// LoadDiff use to load new bind info to bindCache bc.
func (bindCacheUpdater *BindCacheUpdater) loadDiff(sql string, bc bindCache) error {
	recordSets, err := bindCacheUpdater.ctx.(sqlexec.SQLExecutor).Execute(context.Background(), sql)
	if err != nil {
		return errors.Trace(err)
	}

	rs := recordSets[0]
	defer terror.Call(rs.Close)
	chkBatch := rs.NewRecordBatch()
	for {
		err = rs.Next(context.TODO(), chkBatch)
		if err != nil {
			return errors.Trace(err)
		}

		if chkBatch.NumRows() == 0 {
			return nil
		}
		it := chunk.NewIterator4Chunk(chkBatch.Chunk)
		for row := it.Begin(); row != it.End(); row = it.Next() {
			record := newBindMeta(row)
			err = bc.appendNode(record, bindCacheUpdater.parser)
			if err != nil {
				return err
			}

			if record.UpdateTime.Compare(bindCacheUpdater.lastUpdateTime) == 1 {
				bindCacheUpdater.lastUpdateTime = record.UpdateTime
			}
		}
	}
}

// Update update the BindCacheUpdater's bindCache if tidb first startup,the fullLoad is true,otherwise fullLoad is false.
func (bindCacheUpdater *BindCacheUpdater) Update(fullLoad bool) (err error) {
	var sql string
	bc := bindCacheUpdater.globalHandler.Get()
	newBc := make(map[string][]*bindRecord, len(bc))
	for hash, bindDataArr := range bc {
		newBc[hash] = append(newBc[hash], bindDataArr...)
	}

	if fullLoad {
		sql = "select original_sql, bind_sql, default_db, status, create_time, update_time, charset, collation from mysql.bind_info"
	} else {
		sql = fmt.Sprintf("select original_sql, bind_sql, default_db, status, create_time, update_time, charset, collation from mysql.bind_info where update_time > \"%s\"", bindCacheUpdater.lastUpdateTime.String())
	}
	err = bindCacheUpdater.loadDiff(sql, newBc)
	if err != nil {
		return errors.Trace(err)
	}

	bindCacheUpdater.globalHandler.Store(newBc)
	return nil
}

func newBindMeta(row chunk.Row) *bindMeta {
	return &bindMeta{
		OriginalSQL: row.GetString(0),
		BindSQL:     row.GetString(1),
		Db:          row.GetString(2),
		Status:      row.GetString(3),
		CreateTime:  row.GetTime(4),
		UpdateTime:  row.GetTime(5),
		Charset:     row.GetString(6),
		Collation:   row.GetString(7),
	}
}

func (b bindCache) appendNode(newBindRecord *bindMeta, sparser *parser.Parser) error {
	hash := parser.DigestHash(newBindRecord.OriginalSQL)

	if bindArr, ok := b[hash]; ok {
		for idx, v := range bindArr {
			if v.OriginalSQL == newBindRecord.OriginalSQL && v.Db == newBindRecord.Db {
				b[hash] = append(b[hash][:idx], b[hash][idx+1:]...)
				if len(b[hash]) == 0 {
					delete(b, hash)
				}
				break
			}
		}
	}

	if newBindRecord.Status == deleted {
		return nil
	}

	stmtNodes, _, err := sparser.Parse(newBindRecord.BindSQL, newBindRecord.Charset, newBindRecord.Collation)
	if err != nil {
		log.Warnf("parse error:\n%v\n%s", err, newBindRecord.BindSQL)
		return errors.Trace(err)
	}

	newNode := &bindRecord{
		bindMeta: newBindRecord,
		ast:      stmtNodes[0],
	}

	b[hash] = append(b[hash], newNode)
	return nil
}
