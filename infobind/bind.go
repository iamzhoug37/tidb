// Copyright 2013 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

// Copyright 2015 PingCAP, Inc.
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
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/sessionctx"
)

var _ Manager = (*BindManager)(nil)

// BindManager use to manage both global bind info and session bind info
type BindManager struct {
	is            infoschema.InfoSchema
	currentDB     string
	SessionHandle *Handle //session handle
	*Handle               //global handle
	copy          bool
}

type keyType int

func (k keyType) String() string {
	return "bind-key"
}

// Manager is the interface for providing bind related operations.
type Manager interface {
}

const key keyType = 0

//BindBinderManager binds Manager to context.
func BindBinderManager(ctx sessionctx.Context, pc Manager) {
	ctx.SetValue(key, pc)
}
