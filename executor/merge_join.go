// Copyright 2017 PingCAP, Inc.
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
	"fmt"
	"github.com/pingcap/tidb/sessionctx"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/memory"
)

// MergeJoinExec implements the merge join algorithm.
// This operator assumes that two iterators of both sides
// will provide required order on join condition:
// 1. For equal-join, one of the join key from each side
// matches the order given.
// 2. For other cases its preferred not to use SMJ and operator
// will throw error.
type MergeJoinExec struct {
	baseExecutor

	stmtCtx      *stmtctx.StatementContext
	compareFuncs []chunk.CompareFunc
	joiner       joiner

	prepared bool
	outerIdx int

	innerTable *mergeJoinInnerTable
	outerTable *mergeJoinOuterTable

	innerRows     []chunk.Row
	innerIter4Row chunk.Iterator

	childrenResults []*chunk.Chunk

	memTracker *memory.Tracker

	closeCh chan struct{}

	innerMergeJoinFetchWorker *innerMergeJoinFetchWorker
	outerMergeJoinFetchWorker *outerMergeJoinFetchWorker
	mergeJoinMergeWorker      *mergeJoinCompareWorker

	outerReader		Executor
	outerKeys		[]*expression.Column
	outerFilter		[]expression.Expression

	innerReader		Executor
	innerKeys		[]*expression.Column

	mergeTaskCh  	<-chan *mergeTask

	curTask *mergeTask
}

type mergeJoinOuterTable struct {
	mergeJoinTable

	//	reader Executor
	filter []expression.Expression
	//	keys   []*expression.Column

	//	chk      *chunk.Chunk
	//	selected []bool

	//	iter     *chunk.Iterator4Chunk
	//	row      chunk.Row
	//	hasMatch bool

}

type mergeJoinTable struct {
	reader   Executor
	joinKeys []*expression.Column
	ctx      context.Context

	// for chunk executions
	sameKeyRows    []chunk.Row
	compareFuncs   []chunk.CompareFunc
	firstRow4Key   chunk.Row
	curRow         chunk.Row
	curResult      *chunk.Chunk
	curIter        *chunk.Iterator4Chunk
	curResultInUse bool

/*	usingResultQueue []*chunk.Chunk 	//被发送给mergeWorker的chunk
	unusedResultQueue    []*chunk.Chunk	//未使用的chunk(还没有发送给mergeWorker的chunk)
	resourceQueue  []*chunk.Chunk
*/
	memTracker *memory.Tracker
}
// mergeJoinInnerTable represents the inner table of merge join.
// All the inner rows which have the same join key are returned when function
// "rowsWithSameKey()" being called.
type mergeJoinInnerTable struct {
	mergeJoinTable

	/*	reader   Executor
		joinKeys []*expression.Column
		ctx      context.Context

		// for chunk executions
		sameKeyRows    []chunk.Row
		compareFuncs   []chunk.CompareFunc
		firstRow4Key   chunk.Row
		curRow         chunk.Row
		curResult      *chunk.Chunk
		curIter        *chunk.Iterator4Chunk
		curResultInUse bool

		usingResultQueue []*chunk.Chunk 	//被发送给mergeWorker的chunk
		unusedResultQueue    []*chunk.Chunk	//未使用的chunk(还没有发送给mergeWorker的chunk)
		resourceQueue  []*chunk.Chunk

		memTracker *memory.Tracker
	*/}

type mergejoinWorkerResult struct {
	chk *chunk.Chunk
	err error
	src chan<- *chunk.Chunk
}

type mergeJoinCompareWorker struct {
	innerFetchResultCh <-chan *innerFetchResult
	outerFetchResultCh <-chan *outerFetchResult

	innerFetchResult *innerFetchResult
	outerFetchResult *outerFetchResult

	//	outerTable *mergeJoinOuterTable
	//	innerTable *mergeJoinInnerTable

	compareFuncs   []chunk.CompareFunc

	joinKeys     []*expression.Column
//	joiner       joiner
//	maxChunkSize int


	ctx           sessionctx.Context

	innerRow 	  chunk.Row
	innerRows     []chunk.Row
	innerIter4Row chunk.Iterator
	innerJoinKeys []*expression.Column

	outerRow      chunk.Row
	outerRows	  []chunk.Row
	outerIter4Row chunk.Iterator
	outerSelected    []bool
	outerRowIdx	  int
	outerJoinKeys []*expression.Column

	hasMatch bool

	mergeTaskListWorkerCh chan<- *mergeTaskList
	mergeTaskListCh            chan<- *mergeTaskList

	maxChunkSize int
}

type mergeJoinMergeWorker struct {
	workerId int

	innerRows     []chunk.Row
	innerRow 	  chunk.Row
	innerIter4Row chunk.Iterator
	innerJoinKeys []*expression.Column

	outerRows	  []chunk.Row
	outerRow	  chunk.Row
	outerSelected    []bool
	outerRowIdx	  int
	outerIter4Row chunk.Iterator
	outerJoinKeys []*expression.Column

	closeCh <-chan struct{}

	mergeTaskListCh <-chan *mergeTaskList

	joinChkResourceCh chan *chunk.Chunk

	joiner       joiner

	maxChunkSize  int

}

func (mw *mergeJoinMergeWorker) run() {
	fmt.Println("merge workerId:" , mw.workerId , " run")
	defer func() {
	}()
	ok, joinResult := mw.getNewJoinResult()
	if !ok {
		return
	}

	for {
		mergeTask,ok := <- mw.mergeTaskCh
		fmt.Println(mw.workerId , "processing task")
		if !ok {
			return
		}

		hasLeftSend := false
		hasMatch := false
		if mergeTask.cmp < 0 {
			mw.outerRows = mergeTask.outerRows
			mw.outerIter4Row = chunk.NewIterator4Slice(mw.outerRows)
			mw.outerRow = mw.outerIter4Row.Begin()
			mw.outerSelected = mergeTask.outerSelected
			mw.outerRowIdx = 0

			for ; mw.outerRow != mw.outerIter4Row.End() ; {
				if !mw.outerSelected[mw.outerRowIdx] {
					mw.outerRow = mw.outerIter4Row.Next()
					mw.outerRowIdx = mw.outerRowIdx + 1
					continue
				}

				mw.joiner.onMissMatch(mw.outerRow, joinResult.chk)
				hasLeftSend = true
				mw.outerRow = mw.outerIter4Row.Next()
				mw.outerRowIdx = mw.outerRowIdx + 1

				if joinResult.chk.NumRows() == mw.maxChunkSize {
					mergeTask.joinResultCh <- joinResult
					hasLeftSend = false
					ok, joinResult = mw.getNewJoinResult()
					if !ok {
						return
					}
				}
			}
		} else {
			mw.innerRows = mergeTask.innerRows
			mw.innerIter4Row = chunk.NewIterator4Slice(mw.innerRows)
			mw.innerIter4Row.Begin()

			mw.outerRows = mergeTask.outerRows
			mw.outerIter4Row = chunk.NewIterator4Slice(mw.outerRows)
			mw.outerRow = mw.outerIter4Row.Begin()
			mw.outerSelected = mergeTask.outerSelected
			mw.outerRowIdx = 0

			for ; mw.outerRow != mw.outerIter4Row.End() ; {
				if !mw.outerSelected[mw.outerRowIdx] {
					mw.outerRow = mw.outerIter4Row.Next()
					mw.outerRowIdx = mw.outerRowIdx + 1
					continue
				}

				matched, err := mw.joiner.tryToMatch(mw.outerRow, mw.innerIter4Row, joinResult.chk)
				if err != nil {
					joinResult.err = errors.Trace(err)
					mergeTask.joinResultCh <- joinResult
					return
				}
				hasMatch = hasMatch || matched

				if mw.innerIter4Row.Current() == mw.innerIter4Row.End() {
					if !hasMatch {
						mw.joiner.onMissMatch(mw.outerRow, joinResult.chk)
					}
					mw.outerRow = mw.outerIter4Row.Next()
					mw.outerRowIdx = mw.outerRowIdx + 1
					hasMatch = false
					mw.innerIter4Row.Begin()
				}

				if joinResult.chk.NumRows() >= mw.maxChunkSize {
					mergeTask.joinResultCh <- joinResult
					hasLeftSend = false
					ok, joinResult = mw.getNewJoinResult()
					if !ok {
						return
					}
				}
			}
		}

		if hasLeftSend {
			mergeTask.joinResultCh <- joinResult
			ok, joinResult = mw.getNewJoinResult()
			if !ok {
				return
			}
		}

		close(mergeTask.joinResultCh)
	}
}

type mergeTask struct {
	innerRows     []chunk.Row

	outerRows	  []chunk.Row
	outerSelected    []bool

	cmp 		int

	buildErr 	error
}

type mergeTaskList struct {
	taskList []*mergeTask

	outerRowNum int

	joinResultCh chan *mergejoinWorkerResult
}

const closed int64 = 1
const opened int64 = 0

type outerFetchResult struct {
	selected    []bool
	fetchRow 	[]chunk.Row
	err         error
}

type innerFetchResult struct {
	fetchRow []chunk.Row
	err           error
}

type innerMergeJoinFetchWorker struct {
	innerResultCh chan<- *innerFetchResult

	innerTable    *mergeJoinInnerTable
	isClosed      int64
}

type outerMergeJoinFetchWorker struct {
	outerFetchResultCh chan<- *outerFetchResult

	outerTable *mergeJoinOuterTable

	isClosed int64

	ctx           sessionctx.Context
}

func (ow outerMergeJoinFetchWorker) run(ctx context.Context) {//row with the same key
	defer func() {
		close(ow.outerFetchResultCh)
	}()

	for atomic.LoadInt64(&ow.isClosed) != closed {
		outerRows, err := ow.outerTable.rowsWithSameKey()
		fetchResult := &outerFetchResult{fetchRow:outerRows , err : err}
		fetchResult.selected, err = expression.VectorizedFilterByRow(ow.ctx, ow.outerTable.filter, outerRows, fetchResult.selected)
		if err != nil {
			fetchResult.err = err
		}

		ow.outerFetchResultCh <- fetchResult
		if err != nil || len(outerRows) == 0 {
			return
		}
	}
}

func (iw innerMergeJoinFetchWorker) run(ctx context.Context) {//row with the same key
	defer func() {
		close(iw.innerResultCh)
	}()

	for atomic.LoadInt64(&iw.isClosed) != closed {
		innerRows, err := iw.innerTable.rowsWithSameKey()
		fetchResult := &innerFetchResult{fetchRow: innerRows, err : err}
		iw.innerResultCh <- fetchResult
		if err != nil || len(innerRows) == 0 {
			return
		}
	}
}

func (mw *mergeJoinCompareWorker) run(ctx context.Context) { //只compare 双方的first key，然后就开始merge
	defer func() {
		close(mw.mergeTaskListCh)
		close(mw.mergeTaskListWorkerCh)
	}()
	mw.joinToChunk(ctx)
}

func (mw *mergeJoinCompareWorker) fetchNextInnerRows() (bool,error) {
	innerResult, ok := <- mw.innerFetchResultCh

	if !ok {
		return false, nil
	}

	if innerResult.err != nil {
		return false, errors.Trace(innerResult.err)
	}

	mw.innerRows = innerResult.fetchRow
	mw.innerIter4Row = chunk.NewIterator4Slice(mw.innerRows)
	mw.innerIter4Row.Begin()

	return true, nil
}

func (mw *mergeJoinCompareWorker) fetchNextOuterRows() (bool, error) {
	outerResult, ok := <- mw.outerFetchResultCh

	if !ok {
		return false, nil
	}
	if outerResult.err != nil {
		return false, errors.Trace(outerResult.err)
	}

	mw.outerRows = outerResult.fetchRow
	mw.outerIter4Row = chunk.NewIterator4Slice(mw.outerRows)
	mw.outerRow = mw.outerIter4Row.Begin()
	mw.outerSelected = outerResult.selected
	mw.outerRowIdx = 0

	return ok, nil
}

func (mw *mergeJoinCompareWorker) joinToChunk(ctx context.Context) {
	var err error
	var ok	bool

	mergeJoinTaskList := &mergeTaskList{
		joinResultCh : make(chan *mergejoinWorkerResult),
		outerRowNum: 0,
	}

	if ok, err = mw.fetchNextOuterRows() ; err != nil {
		mt := &mergeTask{}
		mt.buildErr = errors.Trace(err)
		mergeJoinTaskList.taskList = append(mergeJoinTaskList.taskList , mt)
		mw.mergeTaskListCh <- mergeJoinTaskList
		return
	}

	if !ok {
		return
	}

	if ok,err = mw.fetchNextInnerRows(); err != nil {
		mt := &mergeTask{}
		mt.buildErr = errors.Trace(err)
		mergeJoinTaskList.taskList = append(mergeJoinTaskList.taskList , mt)
		mw.mergeTaskListCh <- mergeJoinTaskList
		return
	}

	for {
		for ;mw.outerRow != mw.outerIter4Row.End() && !mw.outerSelected[mw.outerRowIdx];{
			mw.outerRow = mw.outerIter4Row.Next()
			mw.outerRowIdx = mw.outerRowIdx + 1
		}
		if mw.outerRow == mw.outerIter4Row.End() {
			continue
		}

		cmpResult := -1
		if len(mw.innerRows) > 0 {
			cmpResult = compareChunkRow(mw.compareFuncs, mw.outerRow , mw.innerRows[0], mw.outerJoinKeys, mw.innerJoinKeys)
		}

		if cmpResult > 0 {
			if ok,err = mw.fetchNextInnerRows(); err != nil {
				mt := &mergeTask{}
				mt.buildErr = errors.Trace(err)
				mergeJoinTaskList.taskList = append(mergeJoinTaskList.taskList , mt)
				mw.mergeTaskListCh <- mergeJoinTaskList
				return
			}

			continue
		}

		mt := &mergeTask{}
		mt.outerRows = mw.outerRows
		mt.outerSelected = mw.outerSelected
		mt.cmp = cmpResult

		if cmpResult == 0 {
			mt.innerRows = mw.innerRows
		}

		if mergeJoinTaskList.outerRowNum + len(mt.outerRows) < mw.maxChunkSize {
			mergeJoinTaskList.taskList = append(mergeJoinTaskList.taskList , mt)
			mergeJoinTaskList.outerRowNum = mergeJoinTaskList.outerRowNum + len(mt.outerRows)
			continue
		} else if mergeJoinTaskList.outerRowNum + len(mt.outerRows) == mw.maxChunkSize {
			mergeJoinTaskList.taskList = append(mergeJoinTaskList.taskList , mt)
			mergeJoinTaskList.outerRowNum = mergeJoinTaskList.outerRowNum + len(mt.outerRows)
			mw.mergeTaskListWorkerCh <- mergeJoinTaskList
			mw.mergeTaskListCh <- mergeJoinTaskList
		} else {
			mw.mergeTaskListWorkerCh <- mergeJoinTaskList
			mw.mergeTaskListCh <- mergeJoinTaskList

			mergeJoinTaskList = &mergeTaskList{
				joinResultCh : make(chan *mergejoinWorkerResult),
				outerRowNum: 0,
			}

			mergeJoinTaskList.taskList = append(mergeJoinTaskList.taskList , mt)
			mergeJoinTaskList.outerRowNum = mergeJoinTaskList.outerRowNum + len(mt.outerRows)
		}
	}
}

func (mw *mergeJoinMergeWorker) getNewJoinResult() (bool, *mergejoinWorkerResult) {
	joinResult := &mergejoinWorkerResult{
		src: mw.joinChkResourceCh, //用来给next函数归还chunk的
	}
	ok := true
	select {
	case <-mw.closeCh:
		ok = false
	case joinResult.chk, ok = <-mw.joinChkResourceCh:
	}

	return ok, joinResult
}

func (t *mergeJoinTable) init(ctx context.Context, chk4Reader *chunk.Chunk) (err error) {
	if t.reader == nil || ctx == nil {
		return errors.Errorf("Invalid arguments: Empty arguments detected.")
	}
	t.ctx = ctx
	t.curResult = chk4Reader
	t.curIter = chunk.NewIterator4Chunk(t.curResult)
	t.curRow = t.curIter.End()
	t.curResultInUse = false
//	t.unusedResultQueue = append(t.unusedResultQueue, chk4Reader)
	t.memTracker.Consume(chk4Reader.MemoryUsage())
	t.firstRow4Key, err = t.nextRow()
	t.compareFuncs = make([]chunk.CompareFunc, 0, len(t.joinKeys))
	for i := range t.joinKeys {
		t.compareFuncs = append(t.compareFuncs, chunk.GetCompareFunc(t.joinKeys[i].RetType))
	}
	return errors.Trace(err)
}

func (t *mergeJoinTable) rowsWithSameKey() ([]chunk.Row, error) {
/*	defer func() {
		lastResultIdx := len(t.usingResultQueue) - 1
		if lastResultIdx > 0 {
			t.usingResultQueue = append(t.usingResultQueue , t.usingResultQueue[0:lastResultIdx]...)
			t.unusedResultQueue = t.unusedResultQueue[lastResultIdx:]
		}
	}()
	t.resourceQueue = append(t.resourceQueue, t.usingResultQueue...)
	t.usingResultQueue = t.usingResultQueue[:0]
*/
	// no more data.
	if t.firstRow4Key == t.curIter.End() {
		return nil, nil
	}
	t.sameKeyRows = make([]chunk.Row, 0)
	t.sameKeyRows = append(t.sameKeyRows, t.firstRow4Key)
	for {
		selectedRow, err := t.nextRow()
		// error happens or no more data.
		if err != nil || selectedRow == t.curIter.End() {
			t.firstRow4Key = t.curIter.End()
			return t.sameKeyRows, errors.Trace(err)
		}
		compareResult := compareChunkRow(t.compareFuncs, selectedRow, t.firstRow4Key, t.joinKeys, t.joinKeys)
		if compareResult == 0 {
			t.sameKeyRows = append(t.sameKeyRows, selectedRow)
		} else {
			t.firstRow4Key = selectedRow
			return t.sameKeyRows, nil
		}
	}
}

func (t *mergeJoinTable) nextRow() (chunk.Row, error) {
	for {
		if t.curRow == t.curIter.End() {
			t.reallocReaderResult()
			oldMemUsage := t.curResult.MemoryUsage()
			err := t.reader.Next(t.ctx, chunk.NewRecordBatch(t.curResult))
			// error happens or no more data.
			if err != nil || t.curResult.NumRows() == 0 {
				t.curRow = t.curIter.End()
				return t.curRow, errors.Trace(err)
			}
			newMemUsage := t.curResult.MemoryUsage()
			t.memTracker.Consume(newMemUsage - oldMemUsage)
			t.curRow = t.curIter.Begin()
		}

		result := t.curRow
		t.curResultInUse = true
		t.curRow = t.curIter.Next()

		if !t.hasNullInJoinKey(result) {
			return result, nil
		}
	}
}

func (t *mergeJoinTable) hasNullInJoinKey(row chunk.Row) bool {
	for _, col := range t.joinKeys {
		ordinal := col.Index
		if row.IsNull(ordinal) {
			return true
		}
	}
	return false
}

// reallocReaderResult resets "t.curResult" to an empty Chunk to buffer the result of "t.reader".
// It pops a Chunk from "t.resourceQueue" and push it into "t.resultQueue" immediately.
func (t *mergeJoinTable) reallocReaderResult() {
	if !t.curResultInUse {
		// If "t.curResult" is not in use, we can just reuse it.
		t.curResult.Reset()
		return
	}

	// Create a new Chunk and append it to "resourceQueue" if there is no more
	// available chunk in "resourceQueue".
/*	if len(t.resourceQueue) == 0 {
		newChunk := t.reader.newFirstChunk()
		t.memTracker.Consume(newChunk.MemoryUsage())
		t.resourceQueue = append(t.resourceQueue, newChunk)
	}
*/
	// NOTE: "t.curResult" is always the last element of "resultQueue".
//	t.curResult = t.resourceQueue[0]
	t.curResult = t.reader.newFirstChunk()
	t.curIter = chunk.NewIterator4Chunk(t.curResult)
	//t.resourceQueue = t.resourceQueue[1:]
	//t.unusedResultQueue = append(t.unusedResultQueue, t.curResult)
	t.curResult.Reset()
	t.curResultInUse = false
}

// Close implements the Executor Close interface.
func (e *MergeJoinExec) Close() error {
	e.memTracker.Detach()
	e.childrenResults = nil
	e.memTracker = nil

	close(e.closeCh)
	atomic.StoreInt64(&e.innerMergeJoinFetchWorker.isClosed, closed)
	atomic.StoreInt64(&e.outerMergeJoinFetchWorker.isClosed, closed)

	for range e.mergeJoinMergeWorker.outerFetchResultCh {
	}

	for range e.mergeJoinMergeWorker.innerFetchResultCh {
	}

	return errors.Trace(e.baseExecutor.Close())
}

// Open implements the Executor Open interface.
func (e *MergeJoinExec) Open(ctx context.Context) error {
	if err := e.baseExecutor.Open(ctx); err != nil {
		return errors.Trace(err)
	}

	e.prepared = false
	e.memTracker = memory.NewTracker(e.id, e.ctx.GetSessionVars().MemQuotaMergeJoin)
	e.memTracker.AttachTo(e.ctx.GetSessionVars().StmtCtx.MemTracker)

	e.childrenResults = make([]*chunk.Chunk, 0, len(e.children))
	for _, child := range e.children {
		e.childrenResults = append(e.childrenResults, child.newFirstChunk())
	}

	err := e.innerTable.init(ctx, e.childrenResults[e.outerIdx^1])
	if err != nil {
		return errors.Trace(err)
	}
	e.innerTable.memTracker = memory.NewTracker("innerTable", -1)
	e.innerTable.memTracker.AttachTo(e.memTracker)

	err = e.outerTable.init(ctx, e.childrenResults[e.outerIdx^1])
	if err != nil {
		return errors.Trace(err)
	}
	e.outerTable.memTracker = memory.NewTracker("outerTable", -1)
	e.outerTable.memTracker.AttachTo(e.memTracker)

	innerResultCh := make(chan *innerFetchResult)
	innerMergeJoinFetchWorker := &innerMergeJoinFetchWorker{
		innerResultCh: innerResultCh,
		isClosed:      opened,
		innerTable:    e.innerTable,
	}

	e.innerMergeJoinFetchWorker = innerMergeJoinFetchWorker
	go innerMergeJoinFetchWorker.run(ctx)

	outerFetchResultCh := make(chan *outerFetchResult)
	outerMergeJoinFetchWorker := &outerMergeJoinFetchWorker{
		outerTable:			e.outerTable,
		outerFetchResultCh: outerFetchResultCh,
		isClosed:           opened,
	}
	e.outerMergeJoinFetchWorker = outerMergeJoinFetchWorker
	go outerMergeJoinFetchWorker.run(ctx)

	closeCh := make(chan struct{})
	mergeTaskCh := make(chan *mergeTask, 4)
	for i := 0 ; i<4 ; i++ {
		joinChkResourceCh := make(chan *chunk.Chunk, 1)
		joinChkResourceCh <- e.newFirstChunk()
		mw := mergeJoinMergeWorker{
			workerId: i,
			closeCh: closeCh,
			mergeTaskCh: mergeTaskCh,
			joinChkResourceCh : joinChkResourceCh,
			joiner:e.joiner,
			maxChunkSize: e.maxChunkSize,
		}

		go mw.run()
	}

	taskCh := make(chan *mergeTask, 4)
	joinChkResourceCh := make(chan *chunk.Chunk, 1)
	joinChkResourceCh <- e.newFirstChunk()
	mergeJoinMergeWorker := &mergeJoinCompareWorker{
		innerFetchResultCh: innerResultCh,
		outerFetchResultCh: outerFetchResultCh,
		joinKeys:           e.innerTable.joinKeys,
//		joiner:             e.joiner,
		maxChunkSize:       e.maxChunkSize,
		ctx:           		e.ctx,
		mergeWorkerTaskCh: mergeTaskCh,
		taskCh:			taskCh,

	}
	e.mergeTaskCh = taskCh
	e.closeCh = closeCh
	e.mergeJoinMergeWorker = mergeJoinMergeWorker

	go mergeJoinMergeWorker.run(ctx)

	return nil
}

func compareChunkRow(cmpFuncs []chunk.CompareFunc, lhsRow, rhsRow chunk.Row, lhsKey, rhsKey []*expression.Column) int {
	for i := range lhsKey {
		cmp := cmpFuncs[i](lhsRow, lhsKey[i].Index, rhsRow, rhsKey[i].Index)
		if cmp != 0 {
			return cmp
		}
	}
	return 0
}

func (e *MergeJoinExec) Next(ctx context.Context, req *chunk.RecordBatch) error {
	if e.runtimeStats != nil {
		start := time.Now()
		defer func() { e.runtimeStats.Record(time.Since(start), req.NumRows()) }()
	}
	req.Reset()
	if !e.prepared {
		//todo 进行prepared，应该做懒加载，不过现在先不管啦
	}

	var err error

	for {
		if e.curTask == nil {
			e.curTask = e.getNextTask(ctx)
			if e.curTask == nil { //index merge join all complete
				break
			}

			if e.curTask.buildErr != nil {
				return e.curTask.buildErr
			}
		}

		joinResult, ok := <-e.curTask.joinResultCh
		if !ok { //curTask process complete,we need getNextTask,so set curTask = nil
			e.curTask = nil
			continue
		}

		if joinResult.err != nil {
			err = errors.Trace(joinResult.err)
			break
		}

		req.SwapColumns(joinResult.chk)
		joinResult.src <- joinResult.chk
		break
	}

	return err

}

func (e *MergeJoinExec) getNextTask(ctx context.Context) *mergeTask {
	select {
	case task, ok := <-e.mergeTaskCh:
		if ok {
			return task
		}
	case <-ctx.Done():
		return nil
	}

	return nil
}
