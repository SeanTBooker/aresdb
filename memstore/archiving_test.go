//  Copyright (c) 2017-2018 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package memstore

import (
	"sort"

	diskMocks "code.uber.internal/data/ares/diskstore/mocks"
	metaMocks "code.uber.internal/data/ares/metastore/mocks"
	utilsMocks "code.uber.internal/data/ares/utils/mocks"

	memCom "code.uber.internal/data/ares/memstore/common"
	metaCom "code.uber.internal/data/ares/metastore/common"
	"code.uber.internal/data/ares/utils"
	"github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/mock"
)

var _ = ginkgo.Describe("archiving", func() {
	var batch99, batch101, batch110 *Batch
	var archiveBatch0 *ArchiveBatch
	var vs LiveStore
	var dataTypes []memCom.DataType
	var archivingJob *ArchivingJob
	var scheduler *schedulerImpl
	var jobManager *archiveJobManager
	var cutoff uint32 = 140
	var oldCutoff uint32 = 100
	var m *memStoreImpl
	table := "table1"
	shardID := 0
	key := getIdentifier(table, shardID, memCom.ArchivingJobType)
	day := 0

	m = getFactory().NewMockMemStore()
	hostMemoryManager := NewHostMemoryManager(m, 1<<32)
	shard := NewTableShard(&TableSchema{
		Schema: metaCom.Table{
			Name: table,
			Config: metaCom.TableConfig{
				ArchivingDelayMinutes:    500,
				ArchivingIntervalMinutes: 300,
			},
			IsFactTable:          true,
			ArchivingSortColumns: []int{1, 2},
			Columns: []metaCom.Column{
				{Deleted: false},
				{Deleted: false},
				{Deleted: false},
			},
		},
		ValueTypeByColumn: []memCom.DataType{memCom.Uint32, memCom.Bool, memCom.Float32},
		DefaultValues:     []*memCom.DataValue{&memCom.NullDataValue, &memCom.NullDataValue, &memCom.NullDataValue},
	}, nil, nil, hostMemoryManager, shardID)

	shard.ArchiveStore = ArchiveStore{CurrentVersion: &ArchiveStoreVersion{
		ArchivingCutoff: 0,
		Batches:         map[int32]*ArchiveBatch{},
	}}

	shardMap := map[int]*TableShard{
		shardID: shard,
	}

	ginkgo.BeforeEach(func() {
		dataTypes = []memCom.DataType{memCom.Uint32, memCom.Bool, memCom.Float32}
		var err error
		batch110, err = getFactory().ReadLiveBatch("archiving/batch-110")
		Ω(err).Should(BeNil())
		batch101, err = getFactory().ReadLiveBatch("archiving/batch-101")
		Ω(err).Should(BeNil())
		batch99, err = getFactory().ReadLiveBatch("archiving/batch-99")
		Ω(err).Should(BeNil())
		tmpBatch, err := getFactory().ReadArchiveBatch("archiving/archiveBatch0")
		Ω(err).Should(BeNil())
		archiveBatch0 = &ArchiveBatch{
			Version: 0,
			Size:    5,
			Shard:   shardMap[0],
			Batch:   *tmpBatch,
		}
		vs = LiveStore{
			LastReadRecord: RecordID{-101, 3},
			Batches: map[int32]*LiveBatch{
				-110: {
					*batch110,
					5,
					nil,
				},
				-101: {
					*batch101,
					5,
					nil,
				},
				-99: {
					*batch99,
					5,
					nil,
				},
			},
			PrimaryKey:        NewPrimaryKey(16, true, 0, hostMemoryManager),
			HostMemoryManager: hostMemoryManager,
		}

		shardMap[shardID].diskStore = m.diskStore
		shardMap[shardID].metaStore = m.metaStore
		shardMap[shardID].LiveStore = &vs
		shardMap[shardID].ArchiveStore.CurrentVersion.ArchivingCutoff = 100
		shardMap[shardID].ArchiveStore.CurrentVersion.shard = shardMap[shardID]
		shardMap[shardID].ArchiveStore.CurrentVersion.Batches[0] = archiveBatch0
		// Map from max event time to file creation time.
		shardMap[shardID].LiveStore.RedoLogManager = NewRedoLogManager(10800, 1<<30, m.diskStore, table, shardID)
		shardMap[shardID].LiveStore.RedoLogManager.MaxEventTimePerFile = make(map[int64]uint32)
		shardMap[shardID].LiveStore.RedoLogManager.MaxEventTimePerFile[1] = 1
		// make purge to pass
		shardMap[shardID].LiveStore.BackfillManager = NewBackfillManager(table, shardID, metaCom.TableConfig{
			BackfillMaxBufferSize:    1 << 32,
			BackfillThresholdInBytes: 1 << 21,
		})
		shardMap[shardID].LiveStore.BackfillManager.LastRedoFile = 2
		shardMap[shardID].LiveStore.BackfillManager.LastBatchOffset = 1
		m.TableShards[table] = shardMap

		archivingJob = &ArchivingJob{
			tableName: table,
			shardID:   shardID,
			cutoff:    cutoff,
			memStore:  m,
		}

		scheduler = newScheduler(m)
		jobManager = scheduler.jobManagers[memCom.ArchivingJobType].(*archiveJobManager)
	})

	ginkgo.AfterEach(func() {
		batch110.SafeDestruct()
		batch101.SafeDestruct()
		batch99.SafeDestruct()
	})

	ginkgo.It("snapshots live vector store", func() {
		ss := vs.snapshot()
		Ω(ss).Should(Equal(liveStoreSnapshot{
			numRecordsInLastBatch: 3,
			batches: [][]memCom.VectorParty{
				batch110.Columns,
				batch101.Columns,
			},
			batchIDs: []int32{-110, -101},
		}))
	})

	ginkgo.It("creates archiving patches", func() {
		ss := vs.snapshot()
		patchByDay := ss.createArchivingPatches(cutoff, oldCutoff, []int{1, 2},
			jobManager.reportArchiveJobDetail, key, table, shardID)
		Ω(patchByDay[0].sortColumns).Should(Equal(
			[]int{1, 2},
		))
		Ω(patchByDay[0].recordIDs).Should(Equal(
			[]RecordID{
				{0, 1},
				{0, 2},
				{0, 3},
				{0, 4},
				{1, 0},
				{1, 1},
				{1, 2},
			},
		))
		scheduler.RLock()
		Ω(*(jobManager.getJobDetail(key))).Should(Equal(ArchiveJobDetail{
			JobDetail: JobDetail{
				Current:    2,
				Total:      2,
				NumRecords: 7,
			},
			Stage: "create patch",
		}))
		scheduler.RUnlock()
	})

	ginkgo.It("sorts", func() {
		ss := vs.snapshot()
		patchByDay := ss.createArchivingPatches(cutoff, oldCutoff, []int{1, 2}, jobManager.reportArchiveJobDetail, key, table, shardID)
		sort.Sort(patchByDay[0])
		Ω(patchByDay[0].recordIDs).Should(Equal(
			[]RecordID{
				{0, 3}, // null, 1.2
				{1, 0}, // false, null
				{0, 1}, // false, 1.0
				{1, 2}, // false, 1.2
				{0, 4}, // false, 1.3
				{0, 2}, // true, null
				{1, 1}, // true, 1.1
			},
		))
		Ω(patchByDay[0].sortColumns).Should(Equal(
			[]int{1, 2},
		))
	})

	ginkgo.It("archive", func() {
		tableShard := shardMap[shardID]

		// Following calls are expected.
		oldVersion := tableShard.ArchiveStore.CurrentVersion
		(m.metaStore).(*metaMocks.MetaStore).On(
			"AddArchiveBatchVersion", table, shardID, day, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		(m.metaStore).(*metaMocks.MetaStore).On(
			"UpdateArchivingCutoff", table, shardID, mock.Anything).Return(nil)
		(m.diskStore).(*diskMocks.DiskStore).On(
			"DeleteBatchVersions", table, shardID, day, mock.Anything, mock.Anything).Return(nil)
		(m.diskStore).(*diskMocks.DiskStore).On(
			"DeleteLogFile", table, shardID, int64(1)).Return(nil)

		writer := new(utilsMocks.WriteCloser)
		writer.On("Write", mock.Anything).Return(0, nil)
		writer.On("Close").Return(nil)

		(m.diskStore).(*diskMocks.DiskStore).On(
			"OpenVectorPartyFileForWrite", table, mock.Anything, shardID, day, mock.Anything, mock.Anything).Return(writer, nil)

		tableShard.LiveStore.RedoLogManager.CurrentFileCreationTime = 2

		timeIncrementer := &utils.TimeIncrementer{IncBySecond: 1}
		utils.SetClockImplementation(timeIncrementer.Now)
		err := m.Archive(table, shardID, cutoff, jobManager.reportArchiveJobDetail)
		jobManager.RLock()
		jobManager.jobDetails[key].LastDuration = 0
		Ω(*(jobManager.jobDetails[key])).Should(Equal(ArchiveJobDetail{
			JobDetail: JobDetail{
				Current:         1,
				Total:           1,
				NumRecords:      7,
				NumAffectedDays: 1,
			},
			Stage:         "complete",
			CurrentCutoff: 140,
			RunningCutoff: 140,
		}))
		jobManager.RUnlock()
		Ω(err).Should(BeNil())

		// New version of archiving store should be as expected
		Ω(tableShard.ArchiveStore.CurrentVersion).ShouldNot(BeIdenticalTo(oldVersion))
		Ω(tableShard.ArchiveStore.CurrentVersion.ArchivingCutoff).Should(BeEquivalentTo(cutoff))
		Ω(tableShard.ArchiveStore.CurrentVersion.Batches).Should(HaveKey(int32(0)))
		mergedBatch := tableShard.ArchiveStore.CurrentVersion.Batches[0]
		Ω(mergedBatch.Size).Should(BeEquivalentTo(12))
		Ω(mergedBatch.Columns).Should(HaveLen(3))

		timeColumn := mergedBatch.Columns[0]
		Ω(timeColumn.GetLength()).Should(BeEquivalentTo(12))
		Ω(timeColumn.(memCom.CVectorParty).GetMode()).Should(BeEquivalentTo(memCom.AllValuesPresent))

		// Old version of archiving store should be purged.
		for _, column := range archiveBatch0.Columns {
			Ω(column.(*archiveVectorParty).values).Should(BeNil())
			Ω(column.(*archiveVectorParty).nulls).Should(BeNil())
			Ω(column.(*archiveVectorParty).counts).Should(BeNil())
		}

		// If a batch is partially read, it should not be purged
		for _, column := range batch101.Columns {
			Ω(column.(*cLiveVectorParty).GetMode()).ShouldNot(BeEquivalentTo(memCom.AllValuesDefault))
			Ω(column.(*cLiveVectorParty).values).ShouldNot(BeNil())
		}

		// MaxEventTimePerFile should be purged.
		Ω(tableShard.LiveStore.RedoLogManager.MaxEventTimePerFile).ShouldNot(HaveKey(int64(1)))

		// Archive again, there should be no crashes or errors.
		Ω(m.Archive(table, shardID, cutoff+100, jobManager.reportArchiveJobDetail)).Should(BeNil())
		utils.ResetClockImplementation()
	})
})