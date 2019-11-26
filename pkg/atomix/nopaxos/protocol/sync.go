// Copyright 2019-present Open Networking Foundation.
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

package protocol

import (
	"encoding/binary"
	"encoding/json"
	"github.com/willf/bloom"
)

func (s *NOPaxos) startSync() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	// If this replica is not the leader of the view, ignore the request
	if s.getLeader(s.viewID) != s.cluster.Member() {
		return
	}

	// If the replica's status is not Normal, do not attempt the sync
	if s.status != StatusNormal {
		return
	}

	s.syncReps = make(map[MemberID]*SyncReply)
	s.tentativeSync = s.log.LastSlot()

	// Create a bloom filter of the log and add non-empty entries
	noOpFilter := bloom.New(uint(s.log.LastSlot()-s.log.FirstSlot()+1), bloomFilterHashFunctions)
	for slotNum := s.log.FirstSlot(); slotNum <= s.log.LastSlot(); slotNum++ {
		if entry := s.log.Get(slotNum); entry == nil {
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, uint64(slotNum))
			noOpFilter.Add(key)
		}
	}

	// Marshall the bloom filter to bytes
	noOpFilterBytes, err := json.Marshal(noOpFilter)
	if err != nil {
		s.logger.Error("Failed to marshal bloom filter", err)
		return
	}

	message := &ReplicaMessage{
		Message: &ReplicaMessage_SyncPrepare{
			SyncPrepare: &SyncPrepare{
				Sender:          s.cluster.Member(),
				ViewID:          s.viewID,
				MessageNum:      s.sessionMessageNum,
				NoOpFilter:      noOpFilterBytes,
				FirstLogSlotNum: s.log.FirstSlot(),
				LastLogSlotNum:  s.log.LastSlot(),
			},
		},
	}

	for _, member := range s.cluster.Members() {
		if member != s.cluster.Member() {
			if stream, err := s.cluster.GetStream(member); err == nil {
				s.logger.SendTo("SyncPrepare", message, member)
				_ = stream.Send(message)
			}
		}
	}
}

func (s *NOPaxos) handleSyncPrepare(request *SyncPrepare) {
	s.logger.ReceiveFrom("SyncPrepare", request, request.Sender)

	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	// If the replica's status is not Normal, ignore the request
	if s.status != StatusNormal {
		return
	}

	// If the view IDs do not match, ignore the request
	if s.viewID.LeaderNum != request.ViewID.LeaderNum || s.viewID.SessionNum != request.ViewID.SessionNum {
		return
	}

	// If the sender is not the leader for the current view, ignore the request
	if request.Sender != s.getLeader(request.ViewID) {
		return
	}

	// Unmarshal the leader's no-op filter
	noOpFilter := &bloom.BloomFilter{}
	if err := json.Unmarshal(request.NoOpFilter, noOpFilter); err != nil {
		s.logger.Error("Failed to decode bloom filter", err)
		return
	}

	newLog := newLog(request.FirstLogSlotNum)
	entrySlots := make([]LogSlotID, 0)
	for slotNum := request.FirstLogSlotNum; slotNum <= request.LastLogSlotNum; slotNum++ {
		// If the entry is greater than the last in the replica's log, request it.
		if entry := s.log.Get(slotNum); entry != nil {
			// If the entry is missing from the leader's log, request it. Otherwise add it to the new log.
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, uint64(slotNum))
			if noOpFilter.Test(key) {
				entrySlots = append(entrySlots, slotNum)
			} else {
				newLog.Set(entry)
			}
		} else if slotNum > s.log.LastSlot() {
			entrySlots = append(entrySlots, slotNum)
		}
	}

	// If any entries need to be requested from the leader, request them. Otherwise, send a SyncReply
	if len(entrySlots) > 0 {
		leader := s.getLeader(s.viewID)
		if stream, err := s.cluster.GetStream(leader); err == nil {
			repair := &SyncRepair{
				Sender:   s.cluster.Member(),
				ViewID:   s.viewID,
				SlotNums: entrySlots,
			}
			message := &ReplicaMessage{
				Message: &ReplicaMessage_SyncRepair{
					SyncRepair: repair,
				},
			}
			s.syncRepair = repair
			s.logger.SendTo("SyncRepair", message, leader)
			_ = stream.Send(message)
		}
	} else {
		s.sessionMessageNum = s.sessionMessageNum + MessageID(newLog.LastSlot()-s.log.LastSlot())
		s.log = newLog

		// Send a SyncReply back to the leader
		if stream, err := s.cluster.GetStream(request.Sender); err == nil {
			message := &ReplicaMessage{
				Message: &ReplicaMessage_SyncReply{
					SyncReply: &SyncReply{
						Sender:  s.cluster.Member(),
						ViewID:  s.viewID,
						SlotNum: s.log.LastSlot(),
					},
				},
			}
			s.logger.SendTo("SyncReply", message, request.Sender)
			_ = stream.Send(message)
		}

		// Send a RequestReply for all entries in the new log
		sequencer := s.sequencer

		if sequencer != nil {
			for slotNum := s.log.FirstSlot(); slotNum <= s.log.LastSlot(); slotNum++ {
				entry := s.log.Get(slotNum)
				if entry != nil {
					_ = sequencer.Send(&ClientMessage{
						Message: &ClientMessage_CommandReply{
							CommandReply: &CommandReply{
								MessageNum: entry.MessageNum,
								Sender:     s.cluster.Member(),
								ViewID:     s.viewID,
								SlotNum:    slotNum,
							},
						},
					})
				}
			}
		}
	}
}

func (s *NOPaxos) handleSyncRepair(request *SyncRepair) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	// If the request views do not match, ignore the request
	if s.viewID.SessionNum != request.ViewID.SessionNum || s.viewID.LeaderNum != request.ViewID.LeaderNum {
		return
	}

	// Lookup entries for the requested slots
	entries := make([]*LogEntry, 0, len(request.SlotNums))
	for _, slotNum := range request.SlotNums {
		if entry := s.log.Get(slotNum); entry != nil {
			entries = append(entries, entry)
		}
	}

	// Send non-nil entries back to the sender
	if stream, err := s.cluster.GetStream(request.Sender); err == nil {
		message := &ReplicaMessage{
			Message: &ReplicaMessage_SyncRepairReply{
				SyncRepairReply: &SyncRepairReply{
					Sender:  s.cluster.Member(),
					ViewID:  s.viewID,
					Entries: entries,
				},
			},
		}
		s.logger.SendTo("SyncRepairReply", message, request.Sender)
		_ = stream.Send(message)
	}
}

func (s *NOPaxos) handleSyncRepairReply(reply *SyncRepairReply) {
	// If the request views do not match, ignore the reply
	if s.viewID.SessionNum != reply.ViewID.SessionNum || s.viewID.LeaderNum != reply.ViewID.LeaderNum {
		return
	}

	// If no sync repair request is stored, ignore the reply
	request := s.syncRepair
	if request == nil || s.syncLog == nil {
		return
	}

	// Create a map of log entries
	entries := make(map[LogSlotID]*LogEntry)
	for _, entry := range reply.Entries {
		entries[entry.SlotNum] = entry
	}

	// For each requested slot, store the entry if one was returned. Otherwise, remove the entry
	for _, slotNum := range request.SlotNums {
		if entry := entries[slotNum]; entry != nil {
			s.syncLog.Set(entry)
		} else {
			s.syncLog.Delete(slotNum)
		}
	}

	// Once the repair is complete, send a SyncReply
	s.sessionMessageNum = s.sessionMessageNum + MessageID(s.syncLog.LastSlot()-s.log.LastSlot())
	s.log = s.syncLog

	// Send a SyncReply back to the leader
	if stream, err := s.cluster.GetStream(reply.Sender); err == nil {
		message := &ReplicaMessage{
			Message: &ReplicaMessage_SyncReply{
				SyncReply: &SyncReply{
					Sender:  s.cluster.Member(),
					ViewID:  s.viewID,
					SlotNum: s.log.LastSlot(),
				},
			},
		}
		s.logger.SendTo("SyncReply", message, reply.Sender)
		_ = stream.Send(message)
	}

	// Send a RequestReply for all entries in the new log
	sequencer := s.sequencer

	if sequencer != nil {
		for slotNum := s.log.FirstSlot(); slotNum <= s.log.LastSlot(); slotNum++ {
			entry := s.log.Get(slotNum)
			if entry != nil {
				_ = sequencer.Send(&ClientMessage{
					Message: &ClientMessage_CommandReply{
						CommandReply: &CommandReply{
							MessageNum: entry.MessageNum,
							Sender:     s.cluster.Member(),
							ViewID:     s.viewID,
							SlotNum:    slotNum,
						},
					},
				})
			}
		}
	}
}

func (s *NOPaxos) handleSyncReply(reply *SyncReply) {
	s.logger.ReceiveFrom("SyncReply", reply, reply.Sender)

	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	// If the view IDs do not match, ignore the request
	if s.viewID.LeaderNum != reply.ViewID.LeaderNum || s.viewID.SessionNum != reply.ViewID.SessionNum {
		return
	}

	// If the replica's status is not Normal, ignore the request
	if s.status != StatusNormal {
		return
	}

	// Add the reply to the set of sync replies
	s.syncReps[reply.Sender] = reply

	localSynced := false
	syncReps := make([]*SyncReply, 0, len(s.syncReps))
	for _, syncRep := range s.syncReps {
		if syncRep.ViewID.LeaderNum == s.viewID.LeaderNum && syncRep.ViewID.SessionNum == s.viewID.SessionNum && syncRep.SlotNum == s.tentativeSync {
			syncReps = append(syncReps, syncRep)
			if syncRep.Sender == s.cluster.Member() {
				localSynced = true
			}
		}
	}

	if localSynced && len(syncReps) >= s.cluster.QuorumSize() {
		for _, member := range s.cluster.Members() {
			if member != s.cluster.Member() {
				if stream, err := s.cluster.GetStream(member); err == nil {
					_ = stream.Send(&ReplicaMessage{
						Message: &ReplicaMessage_SyncCommit{
							SyncCommit: &SyncCommit{
								Sender:     s.cluster.Member(),
								ViewID:     s.viewID,
								MessageNum: s.sessionMessageNum,
								SyncPoint:  s.tentativeSync,
							},
						},
					})
				}
			}
		}
	}
}

func (s *NOPaxos) handleSyncCommit(request *SyncCommit) {
	s.logger.ReceiveFrom("SyncCommit", request, request.Sender)

	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	// If the replica's status is not Normal, ignore the request
	if s.status != StatusNormal {
		return
	}

	// If the view IDs do not match, ignore the request
	if s.viewID.LeaderNum != request.ViewID.LeaderNum || s.viewID.SessionNum != request.ViewID.SessionNum {
		return
	}

	// If the sender is not the leader for the current view, ignore the request
	if request.Sender != s.getLeader(request.ViewID) {
		return
	}

	for slotNum := s.applied + 1; slotNum <= request.SyncPoint; slotNum++ {
		entry := s.log.Get(slotNum)
		if entry != nil {
			s.state.applyCommand(entry, nil)
		}
		s.applied = slotNum
	}
}