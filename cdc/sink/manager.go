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

package sink

import (
	"context"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/pingcap/log"
	"go.uber.org/zap"

	"github.com/pingcap/errors"
	"github.com/pingcap/ticdc/cdc/model"
)

// Manager manages table sinks, maintains the relationship between table sinks and backendSink
type Manager struct {
	backendSink  Sink
	checkpointTs model.Ts
	tableSinks   map[model.TableID]*tableSink
	tableSinksMu sync.Mutex
	flushMu      sync.Mutex
}

// NewManager creates a new Sink manager
func NewManager(backendSink Sink, checkpointTs model.Ts) *Manager {
	return &Manager{
		backendSink:  backendSink,
		checkpointTs: checkpointTs,
		tableSinks:   make(map[model.TableID]*tableSink),
	}
}

// CreateTableSink creates a table sink
func (m *Manager) CreateTableSink(tableID model.TableID, checkpointTs model.Ts) Sink {
	if _, exist := m.tableSinks[tableID]; exist {
		log.Panic("the table sink already exists", zap.Uint64("tableID", uint64(tableID)))
	}
	sink := &tableSink{
		tableID:   tableID,
		manager:   m,
		buffer:    make([]*model.RowChangedEvent, 0, 128),
		emittedTs: checkpointTs,
	}
	m.tableSinksMu.Lock()
	defer m.tableSinksMu.Unlock()
	m.tableSinks[tableID] = sink
	return sink
}

// Close closes the Sink manager and backend Sink
func (m *Manager) Close() error {
	return m.backendSink.Close()
}

func (m *Manager) getMinEmittedTs() model.Ts {
	if len(m.tableSinks) == 0 {
		return m.getCheckpointTs()
	}
	minTs := model.Ts(math.MaxUint64)
	m.tableSinksMu.Lock()
	defer m.tableSinksMu.Unlock()
	for _, tableSink := range m.tableSinks {
		emittedTs := tableSink.getEmittedTs()
		if minTs > emittedTs {
			minTs = emittedTs
		}
	}
	return minTs
}

func (m *Manager) flushBackendSink(ctx context.Context) (model.Ts, error) {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	minEmittedTs := m.getMinEmittedTs()
	checkpointTs, err := m.backendSink.FlushRowChangedEvents(ctx, minEmittedTs)
	if err != nil {
		return m.getCheckpointTs(), errors.Trace(err)
	}
	atomic.StoreUint64(&m.checkpointTs, checkpointTs)
	return checkpointTs, nil
}

func (m *Manager) destroyTableSink(tableID model.TableID) {
	m.tableSinksMu.Lock()
	defer m.tableSinksMu.Unlock()
	delete(m.tableSinks, tableID)
}

func (m *Manager) getCheckpointTs() uint64 {
	return atomic.LoadUint64(&m.checkpointTs)
}

type tableSink struct {
	tableID   model.TableID
	manager   *Manager
	buffer    []*model.RowChangedEvent
	emittedTs model.Ts
}

func (t *tableSink) Initialize(ctx context.Context, tableInfo []*model.SimpleTableInfo) error {
	// do nothing
	return nil
}

func (t *tableSink) EmitRowChangedEvents(ctx context.Context, rows ...*model.RowChangedEvent) error {
	t.buffer = append(t.buffer, rows...)
	return nil
}

func (t *tableSink) EmitDDLEvent(ctx context.Context, ddl *model.DDLEvent) error {
	// the table sink doesn't receive the DDL event
	return nil
}

func (t *tableSink) FlushRowChangedEvents(ctx context.Context, resolvedTs uint64) (uint64, error) {
	i := sort.Search(len(t.buffer), func(i int) bool {
		return t.buffer[i].CommitTs > resolvedTs
	})
	if i == 0 {
		atomic.StoreUint64(&t.emittedTs, resolvedTs)
		return t.manager.flushBackendSink(ctx)
	}
	resolvedRows := t.buffer[:i]
	t.buffer = t.buffer[i:]
	err := t.manager.backendSink.EmitRowChangedEvents(ctx, resolvedRows...)
	if err != nil {
		return t.manager.getCheckpointTs(), errors.Trace(err)
	}
	atomic.StoreUint64(&t.emittedTs, resolvedTs)
	return t.manager.flushBackendSink(ctx)
}

func (t *tableSink) getEmittedTs() uint64 {
	return atomic.LoadUint64(&t.emittedTs)
}

func (t *tableSink) EmitCheckpointTs(ctx context.Context, ts uint64) error {
	// the table sink doesn't receive the checkpoint event
	return nil
}

func (t *tableSink) Close() error {
	t.manager.destroyTableSink(t.tableID)
	return nil
}
