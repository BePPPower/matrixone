package meta

import (
	md "matrixone/pkg/vm/engine/aoe/storage/metadata/v1"
	// log "github.com/sirupsen/logrus"
)

func NewFlushTblOp(ctx *OpCtx, tbl *md.Table) *FlushTblOp {
	op := new(FlushTblOp)
	op.Table = tbl
	op.Op = *NewOp(op, ctx, ctx.Opts.Meta.Flusher)
	return op
}

type FlushTblOp struct {
	Op
	Table *md.Table
}

func (op *FlushTblOp) Execute() (err error) {
	err = op.Ctx.Opts.Meta.Checkpointer.PreCommit(op.Table)
	if err != nil {
		return err
	}
	err = op.Ctx.Opts.Meta.Checkpointer.Commit(op.Table)
	if err != nil {
		return err
	}
	_, err = op.Ctx.Opts.Meta.Info.ReferenceTable(op.Table.ID)
	if err != nil {
		panic(err)
	}

	return err
}
