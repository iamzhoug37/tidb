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
	"github.com/pingcap/tidb/util"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/memory"
	log "github.com/sirupsen/logrus"
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

	outerReader Executor
	outerKeys   []*expression.Column
	outerFilter []expression.Expression

	innerReader Executor
	innerKeys   []*expression.Column

	//	mergeTaskCh  	<-chan *mergeTask
	joinResultCh chan *mergejoinWorkerResult

	curTask *mergeTask

	workerWg *sync.WaitGroup
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
	*/
}

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

	compareFuncs []chunk.CompareFunc

	joinKeys []*expression.Column
	//	joiner       joiner
	//	maxChunkSize int

	ctx sessionctx.Context

	innerRow      chunk.Row
	innerRows     []chunk.Row
	innerIter4Row chunk.Iterator
	innerJoinKeys []*expression.Column

	outerRow      chunk.Row
	outerRows     []chunk.Row
	outerIter4Row chunk.Iterator
	outerSelected []bool
	outerRowIdx   int
	outerJoinKeys []*expression.Column

	hasMatch bool
	workerWg *sync.WaitGroup

	outerRowsPreTask  int
	mergeWorkerTaskCh chan<- *mergeTask
	//	taskCh            chan<- *mergeTask
//	joinResultCh chan *mergejoinWorkerResult
}

type mergeJoinMergeWorker struct {
	workerId int

	innerRows     []chunk.Row
	innerRow      chunk.Row
	innerIter4Row chunk.Iterator
	innerJoinKeys []*expression.Column

	outerRows     []chunk.Row
	outerRow  chunk.Row
	//	outerSelected    []bool
	//	outerRowIdx	  int
	outerIter4Row chunk.Iterator
	outerJoinKeys []*expression.Column

	closeCh <-chan struct{}

	mergeTaskCh <-chan *mergeTask

	joinResultCh chan<- *mergejoinWorkerResult

	joinChkResourceCh chan *chunk.Chunk

	joiner joiner

	maxChunkSize int

	workerWg *sync.WaitGroup
}

func (mw *mergeJoinMergeWorker) run() {
	defer func() {
		close(mw.joinResultCh)
		mw.workerWg.Done()
		log.Info("mergeWorker:", mw.workerId, "exit")
	}()
	log.Info("mergeWorker:", mw.workerId, "start")
	ok, joinResult := mw.getNewJoinResult()
	if !ok {
		return
	}

	for {
		mergeTask, ok := <-mw.mergeTaskCh
		if !ok {
			return
		}

		//		fmt.Println("worker :" , mw.workerId , " begin process task")
		hasMatch := false

		if mergeTask.cmp < 0 {
			//			fmt.Println("join worker outerRow", len(mergeTask.outerRows), "," , mergeTask.outerRows[0].GetInt64(0))
			mw.outerRows = mergeTask.outerRows
			mw.outerIter4Row = chunk.NewIterator4Slice(mw.outerRows)
			mw.outerRow = mw.outerIter4Row.Begin()
			//			mw.outerSelected = mergeTask.outerSelected
			//			mw.outerRowIdx = 0

			for mw.outerRow != mw.outerIter4Row.End() {
				/*				if !mw.outerSelected[mw.outerRowIdx] {
									mw.outerRow = mw.outerIter4Row.Next()
									mw.outerRowIdx = mw.outerRowIdx + 1
									continue
								}
				*/
				mw.joiner.onMissMatch(mw.outerRow, joinResult.chk)
				mw.outerRow = mw.outerIter4Row.Next()
				//				mw.outerRowIdx = mw.outerRowIdx + 1

				if joinResult.chk.NumRows() == mw.maxChunkSize {
					mw.joinResultCh <- joinResult
					ok, joinResult = mw.getNewJoinResult()
					if !ok {
						return
					}
				}
			}
		} else {
			//			fmt.Println("join worker outerRow", len(mergeTask.outerRows), "," , mergeTask.outerRows[0].GetInt64(0))
			//			fmt.Println("join worker innerRow", len(mergeTask.outerRows), "," , mergeTask.innerRows[0].GetInt64(0))
			mw.innerRows = mergeTask.innerRows
			mw.innerIter4Row = chunk.NewIterator4Slice(mw.innerRows)
			mw.innerIter4Row.Begin()

			mw.outerRows = mergeTask.outerRows
			mw.outerIter4Row = chunk.NewIterator4Slice(mw.outerRows)
			mw.outerRow = mw.outerIter4Row.Begin()
			//			mw.outerSelected = mergeTask.outerSelected
			//			mw.outerRowIdx = 0

			for mw.outerRow != mw.outerIter4Row.End() {
				/*				if !mw.outerSelected[mw.outerRowIdx] {
									mw.outerRow = mw.outerIter4Row.Next()
									mw.outerRowIdx = mw.outerRowIdx + 1
									continue
								}
				*/
//				fmt.Println("outerRow:" , mw.outerRow)
				matched, err := mw.joiner.tryToMatch(mw.outerRow, mw.innerIter4Row, joinResult.chk)
				if err != nil {
					joinResult.err = errors.Trace(err)
					mw.joinResultCh <- joinResult
					return
				}
				hasMatch = hasMatch || matched

				if mw.innerIter4Row.Current() == mw.innerIter4Row.End() {
					if !hasMatch {
						mw.joiner.onMissMatch(mw.outerRow, joinResult.chk)
					}
					mw.outerRow = mw.outerIter4Row.Next()
					//					mw.outerRowIdx = mw.outerRowIdx + 1
					hasMatch = false
					mw.innerIter4Row.Begin()
				}

				if joinResult.chk.NumRows() >= mw.maxChunkSize {
					//					fmt.Println("worker :" , mw.workerId , " task result chunk size" , sortedJoinResult.chk.NumRows())
					mw.joinResultCh <- joinResult
					ok, joinResult = mw.getNewJoinResult()
					if !ok {
						return
					}
				}
			}
		}

		if joinResult.chk.NumRows() > 0 {
			//			fmt.Println("worker :" , mw.workerId , " task result chunk size" , sortedJoinResult.chk.NumRows())
			mw.joinResultCh <- joinResult
			ok, joinResult = mw.getNewJoinResult()
			if !ok {
				return
			}
		}

		//		close(mergeTask.joinResultCh)

		//		fmt.Println("worker :" , mw.workerId , " end process task")
	}
}

type mergeTask struct {
	innerRows []chunk.Row

	outerRows     []chunk.Row
	outerSelected []bool

	cmp int

	buildErr error

//	joinResultCh chan<- *mergejoinWorkerResult
}

const closed int64 = 1
const opened int64 = 0

type outerFetchResult struct {
	selected []bool
	fetchRow []chunk.Row
	err      error
}

type innerFetchResult struct {
	fetchRow []chunk.Row
	err      error
}

type innerMergeJoinFetchWorker struct {
	innerResultCh chan<- *innerFetchResult

	innerTable *mergeJoinInnerTable
	isClosed   int64
	workerWg   *sync.WaitGroup
}

type outerMergeJoinFetchWorker struct {
	outerFetchResultCh chan<- *outerFetchResult

	outerTable *mergeJoinOuterTable

	isClosed int64

	ctx sessionctx.Context

	workerWg *sync.WaitGroup
}

type sortWorker struct {
	joinResultChs         []chan *mergejoinWorkerResult
	sortedJoinResultCh    chan *mergejoinWorkerResult
	joinResultSlice       []*mergejoinWorkerResult
	sortedJoinResultSlice []*mergejoinWorkerResult
	joinResultChunkResource [] *chunk.Chunk
	joinChkResourceCh chan *chunk.Chunk

	compareFuncs  []chunk.CompareFunc
	innerJoinKeys []*expression.Column
	outerJoinKeys []*expression.Column

	waitGroup *sync.WaitGroup
	executorWaitGroup  *sync.WaitGroup
}

func (sw *sortWorker) run() {
	defer func() {
		sw.executorWaitGroup.Done()
	}()

	for {
		sw.sortedJoinResultSlice = sw.sortedJoinResultSlice[:0]
		sw.waitGroup.Add(len(sw.joinResultChs))

		for idx := 0; idx < len(sw.joinResultSlice); idx++ {
			go func(idx int) {
				sw.joinResultSlice[idx] = nil
				joinResult, ok := <-sw.joinResultChs[idx]
				if ok {
					sw.joinResultSlice[idx] = joinResult
					joinResult.src <- sw.joinResultChunkResource[idx]
				}

				sw.waitGroup.Done()
			}(idx)
		}

		sw.waitGroup.Wait()


		for idx := 0; idx < len(sw.joinResultChs); {
			if sw.joinResultSlice[idx] == nil {
				sw.joinResultSlice = append(sw.joinResultSlice[:idx], sw.joinResultSlice[idx+1:]...)
				sw.joinResultChs = append(sw.joinResultChs[:idx], sw.joinResultChs[idx+1:]...)
			} else {
				idx = idx+1
			}
		}

		if len(sw.joinResultSlice) == 0 {
			break
		}

		minIdx := 0
		sw.sortedJoinResultSlice = append(sw.sortedJoinResultSlice, sw.joinResultSlice[minIdx])
		for idx := 1; idx < len(sw.joinResultSlice); idx++ {
			joinResult := sw.joinResultSlice[idx]
			hasInserted := false
			for sortedIdx := 0; sortedIdx < len(sw.sortedJoinResultSlice); sortedIdx++ {
				sortedJoinResult := sw.sortedJoinResultSlice[sortedIdx]
				cmpResult := compareChunkRow(sw.compareFuncs, joinResult.chk.GetRow(0), sortedJoinResult.chk.GetRow(0), sw.outerJoinKeys, sw.outerJoinKeys) //先直接这么写哇，没啥办法
				if cmpResult > 0 {
					sw.sortedJoinResultSlice = append(sw.sortedJoinResultSlice[:sortedIdx+1], sw.sortedJoinResultSlice[sortedIdx:]...)
					sw.sortedJoinResultSlice[sortedIdx] = joinResult
					hasInserted = true
					break
				}
			}

			if !hasInserted {
				sw.sortedJoinResultSlice = append(sw.sortedJoinResultSlice, joinResult)
			}
		}

		var ok bool
		for idx := len(sw.sortedJoinResultSlice)-1; idx >= 0; idx-- {
			sw.sortedJoinResultSlice[idx].src = sw.joinChkResourceCh
			sw.sortedJoinResultCh <- sw.sortedJoinResultSlice[idx]
			sw.joinResultChunkResource[idx],ok = <- sw.joinChkResourceCh
			if !ok {
				fmt.Println("xxxx")
				return
			}
		}
	}
}

func (ow *outerMergeJoinFetchWorker) run(ctx context.Context) { //row with the same key
	defer func() {
		close(ow.outerFetchResultCh)
		ow.workerWg.Done()
	}()

	outerStart := time.Now()
	err := ow.outerTable.init(ctx, ow.outerTable.reader.newFirstChunk())
	if err != nil {
		log.Error("outer table init err:", errors.Trace(err))
	}
	/*	ow.outerTable.memTracker = memory.NewTracker("outerTable", -1)
		ow.outerTable.memTracker.AttachTo(e.memTracker)
	*/outerCost := time.Since(outerStart)
	log.Info("outer init cost:", outerCost)

	for atomic.LoadInt64(&ow.isClosed) != closed {
		outerRows, err := ow.outerTable.rowsWithSameKey()
		fetchResult := &outerFetchResult{fetchRow: outerRows, err: err}
		fetchResult.selected, err = expression.VectorizedFilterByRow(ow.ctx, ow.outerTable.filter, outerRows, fetchResult.selected)
		if err != nil {
			fetchResult.err = err
		}

		if len(outerRows) > 0 {
			ow.outerFetchResultCh <- fetchResult
		}

		if err != nil || len(outerRows) == 0 {
			return
		}
	}
}

func (iw innerMergeJoinFetchWorker) run(ctx context.Context) { //row with the same key
	defer func() {
		close(iw.innerResultCh)
		iw.workerWg.Done()
	}()

	innerStart := time.Now()
	err := iw.innerTable.init(ctx, iw.innerTable.reader.newFirstChunk())
	if err != nil {
		log.Error("inner worker err:", errors.Trace(err))
	}
	innerCost := time.Since(innerStart)
	log.Info("inner init cost:", innerCost)

	for atomic.LoadInt64(&iw.isClosed) != closed {
		innerRows, err := iw.innerTable.rowsWithSameKey()
		fetchResult := &innerFetchResult{fetchRow: innerRows, err: err}
		if len(innerRows) > 0 {
			iw.innerResultCh <- fetchResult
		}
		if err != nil || len(innerRows) == 0 {
			return
		}
	}
}

func (mw *mergeJoinCompareWorker) run(ctx context.Context) { //只compare 双方的first key，然后就开始merge
	defer func() {
		//		close(mw.taskCh)
		mw.workerWg.Done()
		close(mw.mergeWorkerTaskCh)
	}()
	mw.joinToChunk(ctx)
}

func (mw *mergeJoinCompareWorker) fetchNextInnerRows() (bool, error) {
	innerResult, ok := <-mw.innerFetchResultCh

	if !ok {
		mw.innerRows = make([]chunk.Row, 0)
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
	outerResult, ok := <-mw.outerFetchResultCh

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
	var ok bool

	mt := &mergeTask{/*joinResultCh: mw.joinResultCh*/}

	if ok, err = mw.fetchNextOuterRows(); err != nil {
		mt.buildErr = errors.Trace(err)
		//		mw.taskCh <- mt
		log.Error("build err", err)
		return
	}

	if !ok {
		return
	}

	if _, err = mw.fetchNextInnerRows(); err != nil {
		mt.buildErr = errors.Trace(err)
		//		mw.taskCh <- mt
		log.Error("build err", err)
		return
	}

	for {
		for mw.outerRow != mw.outerIter4Row.End() && !mw.outerSelected[mw.outerRowIdx] {
			mw.outerRow = mw.outerIter4Row.Next()
			mw.outerRowIdx = mw.outerRowIdx + 1
		}
		if mw.outerRow == mw.outerIter4Row.End() {
			continue
		}

		cmpResult := -1
		if len(mw.innerRows) > 0 {
			cmpResult = compareChunkRow(mw.compareFuncs, mw.outerRow, mw.innerRows[0], mw.outerJoinKeys, mw.innerJoinKeys)
		}

		if cmpResult > 0 {
			if _, err = mw.fetchNextInnerRows(); err != nil {
				mt.buildErr = errors.Trace(err)
				log.Error("build err", err)
				//				mw.taskCh <- mt
				return
			}
			continue
		}

		mt.cmp = cmpResult
		if cmpResult == 0 {
			mt.innerRows = mw.innerRows
		}

		for mw.outerRow != mw.outerIter4Row.End() {
			if mw.outerSelected[mw.outerRowIdx] {
				mt.outerRows = append(mt.outerRows, mw.outerRow)
			}

			if len(mt.outerRows) == mw.outerRowsPreTask {
				//if cmpResult == 0 {
				//	fmt.Println("before join-worker outerRow len", len(mt.outerRows), mt.outerRows[0].GetInt64(0))
				//	fmt.Println("before join-worker innerRows len", len(mt.innerRows), mt.innerRows[0].GetInt64(0))
				//} else {
				//	fmt.Println("before join-worker outerRow len", len(mt.outerRows), mt.outerRows[0].GetInt64(0))
				//}

				mw.mergeWorkerTaskCh <- mt
				//				mw.taskCh <- mt

				mt = &mergeTask{/*joinResultCh: mw.joinResultCh*/} //重新new一个
				mt.cmp = cmpResult
				if cmpResult == 0 {
					mt.innerRows = mw.innerRows
				}
			}
			mw.outerRow = mw.outerIter4Row.Next()
			mw.outerRowIdx = mw.outerRowIdx + 1
		}

		if len(mt.outerRows) != 0 {
			mw.mergeWorkerTaskCh <- mt
			//			mw.taskCh <- mt

			mt = &mergeTask{/*joinResultCh: mw.joinResultCh*/} //重新new一个
		}

		if cmpResult < 0 {
			if ok, err = mw.fetchNextOuterRows(); err != nil {
				mt.buildErr = errors.Trace(err)
				log.Error("build err", err)
				//				mw.taskCh <- mt
				return
			}

			if !ok {
				return
			}
		}

		if cmpResult == 0 {
			if ok, err = mw.fetchNextOuterRows(); err != nil {
				mt.buildErr = errors.Trace(err)
				log.Error("build err", err)
				//				mw.taskCh <- mt
				return
			}

			if !ok {
				return
			}

			if _, err = mw.fetchNextInnerRows(); err != nil {
				mt.buildErr = errors.Trace(err)
				log.Error("build err", err)
				//				mw.taskCh <- mt
				return
			}
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
		//		fmt.Println("joinChk cur row len:" , sortedJoinResult.chk.NumRows())
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
	start := time.Now()
	if err := e.baseExecutor.Open(ctx); err != nil {
		return errors.Trace(err)
	}

	e.prepared = false
	e.memTracker = memory.NewTracker(e.id, e.ctx.GetSessionVars().MemQuotaMergeJoin)
	e.memTracker.AttachTo(e.ctx.GetSessionVars().StmtCtx.MemTracker)
	e.workerWg = new(sync.WaitGroup)

	e.childrenResults = make([]*chunk.Chunk, 0, len(e.children))
	for _, child := range e.children {
		e.childrenResults = append(e.childrenResults, child.newFirstChunk())
	}

	/*	innerStart := time.Now()
		err := e.innerTable.init(ctx, e.childrenResults[e.outerIdx^1])
		if err != nil {
			return errors.Trace(err)
		}
		e.innerTable.memTracker = memory.NewTracker("innerTable", -1)
		e.innerTable.memTracker.AttachTo(e.memTracker)
		innerCost := time.Since(innerStart)
		log.Info("inner init cost:" , innerCost)
	*/

	innerResultCh := make(chan *innerFetchResult)
	innerMergeJoinFetchWorker := &innerMergeJoinFetchWorker{
		innerResultCh: innerResultCh,
		isClosed:      opened,
		innerTable:    e.innerTable,
		workerWg:      e.workerWg,
	}

	e.innerMergeJoinFetchWorker = innerMergeJoinFetchWorker
	e.workerWg.Add(1)
	go innerMergeJoinFetchWorker.run(ctx)

	/*	outerStart := time.Now()
		err = e.outerTable.init(ctx, e.childrenResults[e.outerIdx])
		if err != nil {
			return errors.Trace(err)
		}
		e.outerTable.memTracker = memory.NewTracker("outer", -1)
		e.outerTable.memTracker.AttachTo(e.memTracker)
		outerCost := time.Since(outerStart)
		log.Info("outerTable init cost:" , outerCost)
	*/
	outerFetchResultCh := make(chan *outerFetchResult)
	outerMergeJoinFetchWorker := &outerMergeJoinFetchWorker{
		outerTable:         e.outerTable,
		outerFetchResultCh: outerFetchResultCh,
		isClosed:           opened,
		workerWg:           e.workerWg,
	}
	e.outerMergeJoinFetchWorker = outerMergeJoinFetchWorker
	e.workerWg.Add(1)
	go outerMergeJoinFetchWorker.run(ctx)

	joinWorkerCount := 3
	closeCh := make(chan struct{})
	mergeTaskCh := make(chan *mergeTask, joinWorkerCount)
	mergejoinWorkerResultChSlice := make([]chan *mergejoinWorkerResult, joinWorkerCount)
	for i := 0; i < joinWorkerCount; i++ {
		joinChkResourceCh := make(chan *chunk.Chunk, 1)
		joinChkResourceCh <- e.newFirstChunk()
		joinResultCh := make(chan *mergejoinWorkerResult)
		mergejoinWorkerResultChSlice[i] = joinResultCh
		mw := mergeJoinMergeWorker{
			workerId:          i,
			closeCh:           closeCh,
			mergeTaskCh:       mergeTaskCh,
			joinChkResourceCh: joinChkResourceCh,
			joiner:            e.joiner,
			maxChunkSize:      e.maxChunkSize,
			workerWg:          e.workerWg,
			joinResultCh: joinResultCh,
		}

		e.workerWg.Add(1)
		go mw.run()
	}

	//	taskCh := make(chan *mergeTask, joinWorkerCount)
	joinResultCh := make(chan *mergejoinWorkerResult, joinWorkerCount)
	joinChkResourceCh := make(chan *chunk.Chunk, 1)
	joinChkResourceCh <- e.newFirstChunk()
	mergeJoinCompareWorker := &mergeJoinCompareWorker{
		innerFetchResultCh: innerResultCh,
		outerFetchResultCh: outerFetchResultCh,
		joinKeys:           e.innerTable.joinKeys,
		//		joiner:             e.joiner,
		//		maxChunkSize:       e.maxChunkSize,
		ctx:               e.ctx,
		mergeWorkerTaskCh: mergeTaskCh,
		//		taskCh:			taskCh,
//		joinResultCh:     joinResultCh,
		outerRowsPreTask: 1,
		workerWg:         e.workerWg,
	}
	//	e.mergeTaskCh = taskCh
	e.joinResultCh = joinResultCh
	e.closeCh = closeCh
	e.mergeJoinMergeWorker = mergeJoinCompareWorker

	e.workerWg.Add(1)
	go mergeJoinCompareWorker.run(ctx)

	cost := time.Since(start)

	log.Info("start merge join cost:", cost)

	joinResultChunkResource := make([] *chunk.Chunk, joinWorkerCount)
	for i :=0 ; i< len(joinResultChunkResource) ; i++ {
		joinResultChunkResource[i] = e.newFirstChunk()
	}

	sw := &sortWorker{
		joinResultChs:        mergejoinWorkerResultChSlice,
		sortedJoinResultCh:    joinResultCh,
		joinResultSlice:       make([]*mergejoinWorkerResult,joinWorkerCount),
		sortedJoinResultSlice: make([]*mergejoinWorkerResult,joinWorkerCount),
		joinChkResourceCh: make(chan *chunk.Chunk),
		joinResultChunkResource: joinResultChunkResource,
		compareFuncs:  mergeJoinCompareWorker.compareFuncs,
		innerJoinKeys: mergeJoinCompareWorker.innerJoinKeys,
		outerJoinKeys: mergeJoinCompareWorker.outerJoinKeys,

		waitGroup:   new(sync.WaitGroup),
		executorWaitGroup : e.workerWg,
	}

	e.workerWg.Add(1)
	go sw.run()


	go util.WithRecovery(e.waitInnerHashWorkersAndCloseResultChan, nil)
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
		/*		if e.curTask == nil {
				e.curTask = e.getNextTask(ctx)
				if e.curTask == nil { //index merge join all complete
					break
				}

				if e.curTask.buildErr != nil {
					return e.curTask.buildErr
				}
			}*/

		joinResult, ok := <-e.joinResultCh
		if !ok { //curTask process complete,we need getNextTask,so set curTask = nil
			/*e.curTask = nil
			continue*/
			return nil
		}

		if joinResult.err != nil {
			err = errors.Trace(joinResult.err)
			break
		}

		req.SwapColumns(joinResult.chk)
		//		fmt.Println("join result:" , req.Chunk.NumRows() , "," , req.Chunk.GetRow(0).GetInt64(0) , ":" , req.Chunk.GetRow(1).GetInt64(0))
				fmt.Println("join result:" , req.Chunk.NumRows() , "," , req.Chunk.GetRow(0).GetInt64(0))
		joinResult.src <- joinResult.chk
		break
	}

	return err

}

func (e *MergeJoinExec) waitInnerHashWorkersAndCloseResultChan() {
	e.workerWg.Wait()
	close(e.joinResultCh)
	fmt.Println("close join channel")
}

/*func (e *MergeJoinExec) getNextTask(ctx context.Context) *mergeTask {
	select {
	case task, ok := <-e.mergeTaskCh:
		if ok {
//			fmt.Println("getTask" , task.outerRows[0].GetInt64(0))
			return task
		}
	case <-ctx.Done():
		return nil
	}

	return nil
}
*/
/*
// fetchNextInnerRows fetches the next join group, within which all the rows
// have the same join key, from the inner table.
func (innerTable *mergeJoinInnerTable) fetchNextInnerRows() (err error) {
	innerTable.innerRows, err = innerTable.rowsWithSameKey()
	if err != nil {
		return errors.Trace(err)
	}
	innerTable.innerIter4Row = chunk.NewIterator4Slice(innerTable.innerRows)
	innerTable.innerIter4Row.Begin()
	return nil
}

func (outerTable *mergeJoinOuterTable) fetchNextOuterRows(ctx context.Context) (err error) {
	outerFetchResult,ok := <- outerTable.mergeJoinCompareWorker.outerFetchResultCh

	if !ok {
		return errors.Errorf("outer channel closed")
	}

	outerTable.chk = outerFetchResult.resultChunk

	outerTable.iter = chunk.NewIterator4Chunk(outerTable.chk)
	outerTable.iter.Begin()
	outerTable.selected, err = expression.VectorizedFilter(outerTable.mergeJoinCompareWorker.ctx, outerTable.filter, outerTable.iter, outerTable.selected)
	if err != nil {
		return errors.Trace(err)
	}
	outerTable.row = outerTable.iter.Begin()
	return nil
}
*/
