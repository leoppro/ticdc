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

package kv

import (
	"context"

	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/pkg/regionspan"
	"github.com/pingcap/ticdc/pkg/txnutil"
)

type MockKVClient struct {
}

func NewMockKVClient() (*MockKVClient, error) {
	panic("unimplemented")
}

func (m *MockKVClient) EventFeed(
	ctx context.Context,
	span regionspan.ComparableSpan,
	ts uint64,
	enableOldValue bool,
	lockResolver txnutil.LockResolver,
	isPullerInit PullerInitialization,
	eventCh chan<- *model.RegionFeedEvent,
) error {
	panic("unimplemented")
}

func (m *MockKVClient) Close() error {
	panic("unimplemented")
}