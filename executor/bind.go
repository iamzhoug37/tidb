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

package executor

import (
	"context"

	"github.com/pingcap/errors"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/tidb/bindinfo"
	"github.com/pingcap/tidb/util/chunk"
)

type CreateBindExec struct {
	baseExecutor

	originSQL string
	bindSQL   string
	defaultDB string
	charset   string
	collation string
	done      bool
	isGlobal  bool
	bindAst   ast.StmtNode
}

// Next implements the Executor Next interface.
func (e *CreateBindExec) Next(ctx context.Context, req *chunk.RecordBatch) error {
	req.Reset()
	if e.done {
		return nil
	}
	e.done = true

	bm := bindinfo.GetBindManager(e.ctx)
	if bm == nil {
		return errors.New("bind manager is nil")
	}

	var err error
	if e.isGlobal {
		err = bm.AddGlobalBind(e.originSQL, e.bindSQL, e.defaultDB, e.charset, e.collation)
	}
	return errors.Trace(err)
}
