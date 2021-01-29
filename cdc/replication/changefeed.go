// Copyright 2021 PingCAP, Inc.
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

package replication

import (
	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/model"
	"go.uber.org/zap"
	"math"
)

// changeFeed is part of the replication model that implements the control logic of a changeFeed
type changeFeed struct {
	TableTasks    map[model.TableID]*tableTask
	CheckpointTs  uint64
	DDLResolvedTs uint64
	Barriers      []*barrier

	Scheduler scheduler
}

type tableTask struct {
	TableID      model.TableID
	CheckpointTs uint64
	ResolvedTs   uint64
}

type barrierType = int

const (
	DDLBarrier = barrierType(iota)
)

type barrier struct {
	BarrierType barrierType
	BarrierTs   uint64
}

type ddlResultAction = string

const (
	AddTableAction  = ddlResultAction("add")
	DropTableAction = ddlResultAction("drop")
)

type ddlResult struct {
	FinishTs uint64
	Action   ddlResultAction
	tableID  model.TableID
}

func (cf *changeFeed) SetDDLResolvedTs(ddlResolvedTs uint64) {
	cf.DDLResolvedTs = ddlResolvedTs
}

func (cf *changeFeed) AddDDLBarrier(barrierTs uint64) {
	if len(cf.Barriers) > 0 && barrierTs > cf.Barriers[len(cf.Barriers)-1].BarrierTs {
		log.Panic("changeFeed: DDLBarrier too large",
			zap.Uint64("last-barrier-ts", cf.Barriers[0].BarrierTs),
			zap.Uint64("new-barrier-ts", barrierTs))
	}

	if barrierTs < cf.DDLResolvedTs {
		log.Panic("changeFeed: DDLBarrier too small",
			zap.Uint64("cur-ddl-resolved-ts", cf.DDLResolvedTs),
			zap.Uint64("new-barrier-ts", barrierTs))
	}

	cf.Barriers = append(cf.Barriers, &barrier{
		BarrierType: DDLBarrier,
		BarrierTs:   barrierTs,
	})
}

func (cf *changeFeed) ShouldRunDDL() *barrier {
	if len(cf.Barriers) > 0 {
		if cf.Barriers[0].BarrierTs == cf.CheckpointTs+1 &&
			cf.Barriers[0].BarrierType == DDLBarrier {

			return cf.Barriers[0]
		}

		if cf.Barriers[0].BarrierTs <= cf.CheckpointTs {
			log.Panic("changeFeed: Checkpoint run past barrier",
				zap.Uint64("cur-checkpoint-ts", cf.CheckpointTs),
				zap.Reflect("barriers", cf.Barriers))
		}
	}

	return nil
}

func (cf *changeFeed) MarkDDLDone(result ddlResult) {
	if cf.CheckpointTs != result.FinishTs-1 {
		log.Panic("changeFeed: Unexpected checkpoint when DDL is done",
			zap.Uint64("cur-checkpoint-ts", cf.CheckpointTs),
			zap.Reflect("ddl-result", result))
	}

	if len(cf.Barriers) == 0 ||
		cf.Barriers[0].BarrierType != DDLBarrier ||
		cf.Barriers[0].BarrierTs != result.FinishTs {

		log.Panic("changeFeed: no DDL barrier found",
			zap.Reflect("barriers", cf.Barriers),
			zap.Reflect("ddl-result", result))
	}

	cf.Barriers = cf.Barriers[1:]

	switch result.Action {
	case AddTableAction:
		cf.TableTasks[result.tableID] = &tableTask{
			TableID:      result.tableID,
			CheckpointTs: cf.CheckpointTs,
			ResolvedTs:   0,
		}
	case DropTableAction:
		if _, ok := cf.TableTasks[result.tableID]; !ok {
			log.Panic("changeFeed: Dropping unknown table", zap.Int64("table-id", result.tableID))
		}

		delete(cf.TableTasks, result.tableID)
	default:
		log.Panic("changeFeed: unknown action")
	}

	cf.Scheduler.SyncTasks(cf.TableTasks)
}

func (cf *changeFeed) ResolvedTs() uint64 {
	resolvedTs := uint64(math.MaxUint64)

	for _, table := range cf.TableTasks {
		if resolvedTs > table.ResolvedTs {
			resolvedTs = table.ResolvedTs
		}
	}

	if len(cf.Barriers) > 0 && resolvedTs > cf.Barriers[0].BarrierTs-1 {
		resolvedTs = cf.Barriers[0].BarrierTs - 1
	}

	if resolvedTs > cf.DDLResolvedTs {
		resolvedTs = cf.DDLResolvedTs
	}

	return resolvedTs
}