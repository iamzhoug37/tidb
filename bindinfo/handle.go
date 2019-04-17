package bindinfo

import (
	"bytes"
	"context"
	"fmt"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
	"sync"
	"sync/atomic"

	"github.com/pingcap/parser"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/store/tikv/oracle"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/sqlexec"
)

// BindHandle is used to handle all the sql bind operations.
type BindHandle struct {
	sctx struct {
		sync.Mutex
		sessionctx.Context
	}

	// bindInfo caches the sql bind info from storage.
	//
	// The Mutex protects that there is only one goroutine changes the content
	// of atmoic.Value.
	//
	// NOTE: Concurrent Value Write:
	//
	//    bindInfo.Lock()
	//    newCache := bindInfo.Value.Load()
	//    do the write operation on the newCache
	//    bindInfo.Value.Store(newCache)
	//
	// NOTE: Concurrent Value Read:
	//
	//    cache := bindInfo.Load().
	//    read the content
	//
	bindInfo struct {
		sync.Mutex
		atomic.Value
	}

	parser         *parser.Parser
	lastUpdateTime types.Time
}

// NewBindHandle creates a new BindHandle.
func NewBindHandle(ctx sessionctx.Context, parser *parser.Parser) *BindHandle {
	handle := &BindHandle{parser: parser}
	handle.sctx.Context = ctx
	handle.bindInfo.Value.Store(make(cache, 32))
	return handle
}

// Update updates the global sql bind cache.
func (h *BindHandle) Update(fullLoad bool) (err error) {
	sql := "select original_sql, bind_sql, default_db, status, create_time, update_time, charset, collation from mysql.bind_info"
	if !fullLoad {
		sql += " where update_time > \"" + h.lastUpdateTime.String() + "\""
	}

	// No need to acquire the session context lock for ExecRestrictedSQL, it
	// uses another background session.
	rows, _, err := h.sctx.Context.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(nil, sql)
	if err != nil {
		return err
	}

	// Make sure there is only one goroutine writes the cache.
	h.bindInfo.Lock()
	newCache := h.bindInfo.Value.Load().(cache).copy()
	defer func() {
			h.bindInfo.Value.Store(newCache)
			h.bindInfo.Unlock()
	}()

	for _, row := range rows {
		hash, meta, err := h.newBindMeta(newBindRecord(row))
		// Update lastUpdateTime to the newest one.
		if meta.UpdateTime.Compare(h.lastUpdateTime) > 0 {
			h.lastUpdateTime = meta.UpdateTime
		}
		if err != nil {
			logutil.Logger(context.Background()).Error("update bindinfo failed", zap.Error(err))
			continue
		}

		newCache.removeStaleBindMetas(hash, meta)
		newCache[hash] = append(newCache[hash], meta)
	}
	return nil
}

// AddBindRecord adds a BindRecord to the storage and bindMeta to the cache.
func (h *BindHandle) AddBindRecord(record *BindRecord) (err error) {
	exec, _ := h.sctx.Context.(sqlexec.SQLExecutor)
	h.sctx.Lock()
	_, err = exec.Execute(context.TODO(), "BEGIN")
	if err != nil {
		h.sctx.Unlock()
		return
	}

	defer func() {
		if err != nil {
			_, err1 := exec.Execute(context.TODO(), "ROLLBACK")
			h.sctx.Unlock()
			terror.Log(err1)
			return
		}

		_, err = exec.Execute(context.TODO(), "COMMIT")
		h.sctx.Unlock()
		if err != nil {
			return
		}

		// update the bindMeta to the cache.
		hash, meta, err1 := h.newBindMeta(record)
		if err1 != nil {
			err = err1
			return
		}

		h.appendBindMeta(hash, meta)
	}()

	// remove all the unused sql binds.
	_, err = exec.Execute(context.TODO(), h.deleteBindInfoSQL(record.OriginalSQL, record.Db))
	if err != nil {
		return err
	}

	txn, err1 := h.sctx.Context.Txn(true)
	if err1 != nil {
		return err1
	}
	record.CreateTime = types.Time{
		Time: types.FromGoTime(oracle.GetTimeFromTS(txn.StartTS())),
		Type: mysql.TypeDatetime,
		Fsp:  3,
	}
	record.UpdateTime = record.CreateTime
	record.Status = using
	record.BindSQL = h.getEscapeCharacter(record.BindSQL)

	// insert the BindRecord to the storage.
	_, err = exec.Execute(context.TODO(), h.insertBindInfoSQL(record))
	return err
}

// Size return the size of bind info cache.
func (h *BindHandle) Size() int {
	size := 0
	for _, bindRecords := range h.bindInfo.Load().(cache) {
		size += len(bindRecords)
	}
	return size
}

// GetBindRecord return the bindMeta of the (normdOrigSQL,db) if bindMeta exist.
func (h *BindHandle) GetBindRecord(normdOrigSQL, db string) *bindMeta {
	hash := parser.DigestHash(normdOrigSQL)
	bindRecords := h.bindInfo.Load().(cache)[hash]
	if bindRecords != nil {
		for _, bindRecord := range bindRecords {
			if bindRecord.OriginalSQL == normdOrigSQL && bindRecord.Db == db {
				return bindRecord
			}
		}
	}
	return nil
}

func (h *BindHandle) newBindMeta(record *BindRecord) (hash string, meta *bindMeta, err error) {
	hash = parser.DigestHash(record.OriginalSQL)
	stmtNodes, _, err := h.parser.Parse(record.BindSQL, record.Charset, record.Collation)
	if err != nil {
		return "", nil, err
	}
	meta = &bindMeta{BindRecord: record, ast: stmtNodes[0]}
	return hash, meta, nil
}

// appendBindMeta addes the bindMeta to the cache, all the stale bindMetas are
// removed from the cache after this operation.
func (h *BindHandle) appendBindMeta(hash string, meta *bindMeta) {
	// Make sure there is only one goroutine writes the cache.
	h.bindInfo.Lock()
	newCache := h.bindInfo.Value.Load().(cache).copy()
	defer func() {
		h.bindInfo.Value.Store(newCache)
		h.bindInfo.Unlock()
	}()

	newCache.removeStaleBindMetas(hash, meta)
	newCache[hash] = append(newCache[hash], meta)
}

// removeStaleBindMetas removes all the stale bindMeta in the cache.
func (c cache) removeStaleBindMetas(hash string, meta *bindMeta) {
	metas, ok := c[hash]
	if !ok {
		return
	}

	// remove stale bindMetas.
	for i := len(metas) - 1; i >= 0; i-- {
		if metas[i].isStale(meta) {
			metas = append(metas[:i], metas[i+1:]...)
			if len(metas) == 0 {
				delete(c, hash)
				return
			}
		}
	}
}

func (c cache) copy() cache {
	newCache := make(cache, len(c))
	for k, v := range c {
		newCache[k] = v
	}
	return newCache
}

// isStale checks whether this bindMeta is stale compared with the other bindMeta.
func (m *bindMeta) isStale(other *bindMeta) bool {
	return m.OriginalSQL == other.OriginalSQL && m.Db == other.Db &&
		m.UpdateTime.Compare(other.UpdateTime) < 0
}

func (h *BindHandle) deleteBindInfoSQL(normdOrigSQL, db string) string {
	return fmt.Sprintf(
		"DELETE FROM mysql.bind_info WHERE original_sql='%s' AND default_db='%s'",
		normdOrigSQL,
		db,
	)
}

func (h *BindHandle) insertBindInfoSQL(record *BindRecord) string {
	return fmt.Sprintf(`INSERT INTO mysql.bind_info VALUES ('%s', '%s', '%s', '%s', '%s', '%s','%s', '%s')`,
		record.OriginalSQL,
		record.BindSQL,
		record.Db,
		record.Status,
		record.CreateTime,
		record.UpdateTime,
		record.Charset,
		record.Collation,
	)
}

func (h *BindHandle) getEscapeCharacter(str string) string {
	var buffer bytes.Buffer
	for _, v := range str {
		if v == '\'' || v == '"' || v == '\\' {
			buffer.WriteString("\\")
		}
		buffer.WriteString(string(v))
	}
	return buffer.String()
}
