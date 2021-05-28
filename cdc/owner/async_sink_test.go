package owner

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/pingcap/check"
	"github.com/pingcap/errors"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/cdc/sink"
	"github.com/pingcap/ticdc/pkg/config"
	cdcContext "github.com/pingcap/ticdc/pkg/context"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/retry"
)

var _ = check.Suite(&asyncSinkSuite{})

type asyncSinkSuite struct {
}

type mockSink struct {
	sink.Sink
	initTableInfo []*model.SimpleTableInfo
	checkpointTs  model.Ts
	ddl           *model.DDLEvent
	ddlError      error
}

func (m *mockSink) Initialize(ctx context.Context, tableInfo []*model.SimpleTableInfo) error {
	m.initTableInfo = tableInfo
	return nil
}

func (m *mockSink) EmitCheckpointTs(ctx context.Context, ts uint64) error {
	atomic.StoreUint64(&m.checkpointTs, ts)
	return nil
}

func (m *mockSink) EmitDDLEvent(ctx context.Context, ddl *model.DDLEvent) error {
	time.Sleep(1 * time.Second)
	m.ddl = ddl
	return m.ddlError
}

func (m *mockSink) Close() error {
	return nil
}

func newAsyncSink4Test(ctx cdcContext.Context, c *check.C) (cdcContext.Context, AsyncSink, *mockSink) {
	ctx = cdcContext.WithChangefeedVars(ctx, &cdcContext.ChangefeedVars{
		ID:   "test-changefeed",
		Info: &model.ChangeFeedInfo{SinkURI: "blackhole://", Config: config.GetDefaultReplicaConfig()},
	})
	sink, err := newAsyncSink(ctx)
	c.Assert(err, check.IsNil)
	mockSink := &mockSink{}
	sink.(*asyncSinkImpl).sink = mockSink
	return ctx, sink, mockSink
}

func (s *asyncSinkSuite) TestInitialize(c *check.C) {
	ctx := cdcContext.NewBackendContext4Test(false)
	ctx, sink, mockSink := newAsyncSink4Test(ctx, c)
	defer sink.Close()
	tableInfos := []*model.SimpleTableInfo{{Schema: "test"}}
	err := sink.Initialize(ctx, tableInfos)
	c.Assert(err, check.IsNil)
	c.Assert(tableInfos, check.DeepEquals, mockSink.initTableInfo)
}

func (s *asyncSinkSuite) TestCheckpoint(c *check.C) {
	ctx := cdcContext.NewBackendContext4Test(false)
	ctx, sink, mSink := newAsyncSink4Test(ctx, c)
	defer sink.Close()

	waitCheckpointGrowingUp := func(m *mockSink, targetTs model.Ts) error {
		return retry.Run(100*time.Millisecond, 30, func() error {
			if targetTs != atomic.LoadUint64(&m.checkpointTs) {
				return errors.New("targetTs!=checkpointTs")
			}
			return nil
		})
	}
	sink.EmitCheckpointTs(ctx, 1)
	c.Assert(waitCheckpointGrowingUp(mSink, 1), check.IsNil)
	sink.EmitCheckpointTs(ctx, 10)
	c.Assert(waitCheckpointGrowingUp(mSink, 10), check.IsNil)
}

func (s *asyncSinkSuite) TestExecDDL(c *check.C) {
	ctx := cdcContext.NewBackendContext4Test(false)
	ctx, sink, mSink := newAsyncSink4Test(ctx, c)
	defer sink.Close()
	ddl1 := &model.DDLEvent{CommitTs: 1}
	for {
		done, err := sink.EmitDDLEvent(ctx, ddl1)
		c.Assert(err, check.IsNil)
		if done {
			c.Assert(mSink.ddl, check.DeepEquals, ddl1)
			break
		}
	}
	ddl2 := &model.DDLEvent{CommitTs: 2}
	ddl3 := &model.DDLEvent{CommitTs: 3}
	_, err := sink.EmitDDLEvent(ctx, ddl2)
	c.Assert(err, check.IsNil)
	_, err = sink.EmitDDLEvent(ctx, ddl3)
	c.Assert(err, check.IsNil)
	for {
		done, err := sink.EmitDDLEvent(ctx, ddl2)
		c.Assert(err, check.IsNil)
		if done {
			c.Assert(mSink.ddl, check.DeepEquals, ddl2)
			break
		}
	}
	for {
		done, err := sink.EmitDDLEvent(ctx, ddl3)
		c.Assert(err, check.IsNil)
		if done {
			c.Assert(mSink.ddl, check.DeepEquals, ddl3)
			break
		}
	}
}

func (s *asyncSinkSuite) TestExecDDLError(c *check.C) {
	ctx := cdcContext.NewBackendContext4Test(false)
	var resultErr error
	ctx = cdcContext.WithErrorHandler(ctx, func(err error) error {
		resultErr = err
		return nil
	})
	ctx, sink, mSink := newAsyncSink4Test(ctx, c)
	defer sink.Close()
	mSink.ddlError = cerror.ErrDDLEventIgnored.GenWithStackByArgs()
	ddl1 := &model.DDLEvent{CommitTs: 1}
	for {
		done, err := sink.EmitDDLEvent(ctx, ddl1)
		c.Assert(err, check.IsNil)
		if done {
			c.Assert(mSink.ddl, check.DeepEquals, ddl1)
			break
		}
	}
	c.Assert(resultErr, check.IsNil)
	mSink.ddlError = cerror.ErrExecDDLFailed.GenWithStackByArgs()
	ddl2 := &model.DDLEvent{CommitTs: 2}
	for {
		done, err := sink.EmitDDLEvent(ctx, ddl2)
		c.Assert(err, check.IsNil)
		if done || resultErr != nil {
			c.Assert(mSink.ddl, check.DeepEquals, ddl2)
			break
		}
	}
	c.Assert(cerror.ErrExecDDLFailed.Equal(errors.Cause(resultErr)), check.IsTrue)
}