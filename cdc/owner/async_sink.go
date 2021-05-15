package owner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/cdc/sink"
	cdcContext "github.com/pingcap/ticdc/pkg/context"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/filter"
	"go.uber.org/zap"
)

type AsyncSink interface {
	Initialize(ctx cdcContext.Context, tableInfo []*model.SimpleTableInfo) error
	EmitCheckpointTs(ctx cdcContext.Context, ts uint64)
	EmitDDLEvent(ctx cdcContext.Context, ddl *model.DDLEvent) (bool, error)
	SinkSyncpoint(ctx cdcContext.Context, checkpointTs uint64) error
	Close() error
}

type asyncSinkImpl struct {
	sink           sink.Sink
	syncpointStore sink.SyncpointStore

	checkpointTs model.Ts

	ddlCh         chan *model.DDLEvent
	ddlFinishedTs model.Ts
	ddlSentTs     model.Ts

	cancel context.CancelFunc
	wg     sync.WaitGroup
	errCh  chan error
}

func newAsyncSink(ctx cdcContext.Context) (AsyncSink, error) {
	ctx, cancel := cdcContext.WithCancel(ctx)
	changefeedID := ctx.ChangefeedVars().ID
	changefeedInfo := ctx.ChangefeedVars().Info
	filter, err := filter.NewFilter(changefeedInfo.Config)
	if err != nil {
		return nil, errors.Trace(err)
	}
	errCh := make(chan error, defaultErrChSize)
	s, err := sink.NewSink(ctx, changefeedID, changefeedInfo.SinkURI, filter, changefeedInfo.Config, changefeedInfo.Opts, errCh)
	if err != nil {
		return nil, errors.Trace(err)
	}
	asyncSink := &asyncSinkImpl{
		sink:   s,
		ddlCh:  make(chan *model.DDLEvent, 1),
		errCh:  errCh,
		cancel: cancel,
	}
	if changefeedInfo.SyncPointEnabled {
		asyncSink.syncpointStore, err = sink.NewSyncpointStore(ctx, changefeedID, changefeedInfo.SinkURI)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if err := asyncSink.syncpointStore.CreateSynctable(ctx); err != nil {
			return nil, errors.Trace(err)
		}
	}
	asyncSink.wg.Add(1)
	go asyncSink.run(ctx)
	return asyncSink, nil
}

func (s *asyncSinkImpl) Initialize(ctx cdcContext.Context, tableInfo []*model.SimpleTableInfo) error {
	return s.sink.Initialize(ctx, tableInfo)
}

func (s *asyncSinkImpl) run(ctx cdcContext.Context) {
	defer s.wg.Done()
	// TODO make the tick duration configurable
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastCheckpointTs model.Ts
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-s.errCh:
			ctx.Throw(err)
			return
		case <-ticker.C:
			checkpointTs := atomic.LoadUint64(&s.checkpointTs)
			if checkpointTs == 0 || checkpointTs <= lastCheckpointTs {
				continue
			}
			lastCheckpointTs = checkpointTs
			if err := s.sink.EmitCheckpointTs(ctx, checkpointTs); err != nil {
				ctx.Throw(errors.Trace(err))
				return
			}
		case ddl := <-s.ddlCh:
			err := s.sink.EmitDDLEvent(ctx, ddl)
			failpoint.Inject("InjectChangefeedDDLError", func() {
				err = cerror.ErrExecDDLFailed.GenWithStackByArgs()
			})
			if err == nil {
				log.Info("Execute DDL succeeded", zap.String("changefeed", ctx.ChangefeedVars().ID), zap.Reflect("ddl", ddl))
				atomic.StoreUint64(&s.ddlFinishedTs, ddl.CommitTs)
			} else if cerror.ErrDDLEventIgnored.Equal(errors.Cause(err)) {
				// If DDL executing failed, and the error can be ignored, mark the ddl is executed successfully and print a log
				log.Info("Execute DDL ignored", zap.String("changefeed", ctx.ChangefeedVars().ID), zap.Reflect("ddl", ddl))
				atomic.StoreUint64(&s.ddlFinishedTs, ddl.CommitTs)
			} else {
				// If DDL executing failed, and the error can not be ignored, throw an error and pause the changefeed
				log.Error("Execute DDL failed",
					zap.String("ChangeFeedID", ctx.ChangefeedVars().ID),
					zap.Error(err),
					zap.Reflect("ddl", ddl))
				ctx.Throw(errors.Trace(err))
				return
			}
		}
	}
}

func (s *asyncSinkImpl) EmitCheckpointTs(ctx cdcContext.Context, ts uint64) {
	atomic.StoreUint64(&s.checkpointTs, ts)
}

func (s *asyncSinkImpl) EmitDDLEvent(ctx cdcContext.Context, ddl *model.DDLEvent) (bool, error) {
	ddlFinishedTs := atomic.LoadUint64(&s.ddlFinishedTs)
	if ddl.CommitTs <= ddlFinishedTs {
		return true, nil
	}
	if ddl.CommitTs <= s.ddlSentTs {
		return false, nil
	}
	select {
	case <-ctx.Done():
		return false, errors.Trace(ctx.Err())
	case s.ddlCh <- ddl:
	}
	s.ddlSentTs = ddl.CommitTs
	return false, nil
}

func (s *asyncSinkImpl) SinkSyncpoint(ctx cdcContext.Context, checkpointTs uint64) error {
	// TODO implement async sink syncpoint
	return s.syncpointStore.SinkSyncpoint(ctx, ctx.ChangefeedVars().ID, checkpointTs)
}

func (s *asyncSinkImpl) Close() (err error) {
	s.cancel()
	err = s.sink.Close()
	if s.syncpointStore != nil {
		err = s.syncpointStore.Close()
	}
	s.wg.Wait()
	return
}
