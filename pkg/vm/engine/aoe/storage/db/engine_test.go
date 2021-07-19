package db

import (
	"context"
	"fmt"
	"matrixone/pkg/vm/engine/aoe/storage/dbi"
	md "matrixone/pkg/vm/engine/aoe/storage/metadata/v1"
	"matrixone/pkg/vm/engine/aoe/storage/mock/type/chunk"
	"matrixone/pkg/vm/mempool"
	"matrixone/pkg/vm/mmu/guest"
	"matrixone/pkg/vm/mmu/host"
	"matrixone/pkg/vm/process"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panjf2000/ants/v2"

	"github.com/stretchr/testify/assert"
)

var (
	DIR = "/tmp/engine_test"
)

func TestEngine(t *testing.T) {
	initDBTest()
	inst := initDB()
	tableInfo := md.MockTableInfo(2)
	tid, err := inst.CreateTable(tableInfo, dbi.TableOpCtx{TableName: "mockcon"})
	assert.Nil(t, err)
	tblMeta, err := inst.Opts.Meta.Info.ReferenceTable(tid)
	assert.Nil(t, err)
	blkCnt := inst.Store.MetaInfo.Conf.SegmentMaxBlocks
	rows := inst.Store.MetaInfo.Conf.BlockMaxRows * blkCnt
	baseCk := chunk.MockBatch(tblMeta.Schema.Types(), rows)
	insertCh := make(chan dbi.AppendCtx)
	searchCh := make(chan *dbi.GetSnapshotCtx)

	p, _ := ants.NewPool(40)
	attrs := []string{}
	cols := make([]int, 0)
	for i, colDef := range tblMeta.Schema.ColDefs {
		attrs = append(attrs, colDef.Name)
		cols = append(cols, i)
	}
	refs := make([]uint64, len(attrs))
	hm := host.New(1 << 40)
	gm := guest.New(1<<40, hm)
	proc := process.New(gm, mempool.New(1<<48, 8))

	tableCnt := 100
	var twg sync.WaitGroup
	for i := 0; i < tableCnt; i++ {
		twg.Add(1)
		f := func(idx int) func() {
			return func() {
				tInfo := *tableInfo
				_, err := inst.CreateTable(&tInfo, dbi.TableOpCtx{TableName: fmt.Sprintf("%dxxxxxx%d", idx, idx)})
				assert.Nil(t, err)
				twg.Done()
			}
		}
		p.Submit(f(i))
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	var (
		loopWg   sync.WaitGroup
		searchWg sync.WaitGroup
		loadCnt  uint32
	)
	assert.Nil(t, err)
	task := func(ctx *dbi.GetSnapshotCtx) func() {
		return func() {
			defer searchWg.Done()
			rel, err := inst.Relation(tblMeta.Schema.Name)
			assert.Nil(t, err)
			for _, segId := range rel.SegmentIds().Ids {
				seg := rel.Segment(segId, proc)
				for _, id := range seg.Blocks() {
					blk := seg.Block(id, proc)
					bat, err := blk.Prefetch(refs, attrs, proc)
					assert.Nil(t, err)
					for _, attr := range attrs {
						bat.GetVector(attr, proc)
						atomic.AddUint32(&loadCnt, uint32(1))
					}
				}
			}
			rel.Close()
		}
	}
	assert.NotNil(t, task)
	task2 := func(ctx *dbi.GetSnapshotCtx) func() {
		return func() {
			defer searchWg.Done()
			ss, err := inst.GetSnapshot(ctx)
			assert.Nil(t, err)
			segIt := ss.NewIt()
			assert.Nil(t, err)
			if segIt == nil {
				return
			}
			for segIt.Valid() {
				sh := segIt.GetHandle()
				blkIt := sh.NewIt()
				for blkIt.Valid() {
					blkHandle := blkIt.GetHandle()
					hh := blkHandle.Prefetch()
					for idx, _ := range attrs {
						hh.GetReaderByAttr(idx)
						atomic.AddUint32(&loadCnt, uint32(1))
					}
					hh.Close()
					// blkHandle.Close()
					blkIt.Next()
				}
				blkIt.Close()
				segIt.Next()
			}
			segIt.Close()
		}
	}
	assert.NotNil(t, task2)
	loopWg.Add(1)
	loop := func(ctx context.Context) {
		defer loopWg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-searchCh:
				p.Submit(task2(req))
			case req := <-insertCh:
				loopWg.Add(1)
				t := func() {
					rel, err := inst.Relation(req.TableName)
					assert.Nil(t, err)
					err = rel.Write(req)
					assert.Nil(t, err)
					loopWg.Done()
				}
				go t()
			}
		}
	}
	go loop(reqCtx)

	insertCnt := 8
	var driverWg sync.WaitGroup
	driverWg.Add(1)
	go func() {
		defer driverWg.Done()
		for i := 0; i < insertCnt; i++ {
			req := dbi.AppendCtx{
				TableName: tableInfo.Name,
				Data:      baseCk,
				OpIndex:   uint64(i),
			}
			insertCh <- req
		}
	}()

	time.Sleep(time.Duration(500) * time.Millisecond)
	searchCnt := 10000
	driverWg.Add(1)
	go func() {
		defer driverWg.Done()
		for i := 0; i < searchCnt; i++ {
			req := &dbi.GetSnapshotCtx{
				TableName: tableInfo.Name,
				ScanAll:   true,
				Cols:      cols,
			}
			searchWg.Add(1)
			searchCh <- req
		}
	}()
	driverWg.Wait()
	searchWg.Wait()
	cancel()
	loopWg.Wait()
	twg.Wait()
	t.Log(inst.WorkersStatsString())
	t.Log(inst.MTBufMgr.String())
	t.Log(inst.SSTBufMgr.String())
	t.Log(inst.MemTableMgr.String())
	t.Logf("Load: %d", loadCnt)
	tbl, err := inst.Store.DataTables.WeakRefTable(tid)
	t.Logf("tbl %v, tid %d, err %v", tbl, tid, err)
	assert.Equal(t, tbl.GetRowCount(), rows*uint64(insertCnt))
	t.Log(tbl.GetRowCount())
	attr := tblMeta.Schema.ColDefs[0].Name
	t.Log(tbl.Size(attr))
	attr = tblMeta.Schema.ColDefs[1].Name
	t.Log(tbl.Size(attr))
	t.Log(tbl.String())
	rel, err := inst.Relation(tblMeta.Schema.Name)
	assert.Nil(t, err)
	t.Logf("Rows: %d, Size: %d", rel.Rows(), rel.Size(tblMeta.Schema.ColDefs[0].Name))
	t.Log(inst.GetSegmentIds(dbi.GetSegmentsCtx{TableName: tblMeta.Schema.Name}))
	inst.Close()
}

func TestLogIndex(t *testing.T) {
	initDBTest()
	inst := initDB()
	tableInfo := md.MockTableInfo(2)
	tid, err := inst.CreateTable(tableInfo, dbi.TableOpCtx{TableName: "mockcon", OpIndex: md.NextGloablSeqnum()})
	assert.Nil(t, err)
	tblMeta, err := inst.Opts.Meta.Info.ReferenceTable(tid)
	assert.Nil(t, err)
	rows := inst.Store.MetaInfo.Conf.BlockMaxRows * 2 / 5
	baseCk := chunk.MockBatch(tblMeta.Schema.Types(), rows)

	// p, _ := ants.NewPool(40)

	for i := 0; i < 50; i++ {
		rel, err := inst.Relation(tblMeta.Schema.Name)
		assert.Nil(t, err)
		err = rel.Write(dbi.AppendCtx{
			OpIndex:   md.NextGloablSeqnum(),
			Data:      baseCk,
			TableName: tblMeta.Schema.Name,
		})
		assert.Nil(t, err)
	}

	time.Sleep(time.Duration(100) * time.Millisecond)

	tbl, err := inst.Store.DataTables.WeakRefTable(tid)
	assert.Nil(t, err)
	logIndex, ok := tbl.GetSegmentedIndex()
	assert.True(t, ok)
	_, ok = tblMeta.Segments[len(tblMeta.Segments)-1].Blocks[1].GetAppliedIndex()
	assert.False(t, ok)
	expectIdx, ok := tblMeta.Segments[len(tblMeta.Segments)-1].Blocks[0].GetAppliedIndex()
	assert.True(t, ok)
	assert.Equal(t, expectIdx, logIndex)

	dropLogIndex := md.NextGloablSeqnum()
	_, err = inst.DropTable(dbi.DropTableCtx{TableName: tblMeta.Schema.Name, OpIndex: dropLogIndex})
	assert.Nil(t, err)
	time.Sleep(time.Duration(10) * time.Millisecond)
	// tbl, err = inst.Store.DataTables.WeakRefTable(tid)
	logIndex, ok = tbl.GetSegmentedIndex()
	assert.True(t, ok)
	assert.Equal(t, dropLogIndex, logIndex)

	inst.Close()
}
