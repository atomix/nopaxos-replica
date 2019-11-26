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

func (s *NOPaxos) handleStartView(request *StartView) {
	s.logger.ReceiveFrom("StartView", request, request.Sender)

	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	// If the local view is newer than the request view, skip the view
	if s.viewID.SessionNum > request.ViewID.SessionNum && s.viewID.LeaderNum > request.ViewID.LeaderNum {
		return
	}

	// If the views match and the replica is not in the ViewChange state, skip the view
	if s.viewID.SessionNum == request.ViewID.SessionNum && s.viewID.LeaderNum == request.ViewID.LeaderNum && s.status != StatusViewChange {
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
			repair := &ViewRepair{
				Sender:     s.cluster.Member(),
				ViewID:     s.viewID,
				MessageNum: request.MessageNum,
				SlotNums:   entrySlots,
			}
			message := &ReplicaMessage{
				Message: &ReplicaMessage_ViewRepair{
					ViewRepair: repair,
				},
			}
			s.viewRepair = repair
			s.logger.SendTo("ViewRepair", message, leader)
			_ = stream.Send(message)
		}
	} else {
		s.log = newLog
		s.sessionMessageNum = request.MessageNum
		s.status = StatusNormal
		s.viewID = request.ViewID
		s.lastNormView = request.ViewID

		// Send a reply for all commands in the log
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

	s.resetTimeout()
}

func (s *NOPaxos) handleViewRepair(request *ViewRepair) {
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
			Message: &ReplicaMessage_ViewRepairReply{
				ViewRepairReply: &ViewRepairReply{
					Sender:  s.cluster.Member(),
					ViewID:  s.viewID,
					Entries: entries,
				},
			},
		}
		s.logger.SendTo("ViewRepairReply", message, request.Sender)
		_ = stream.Send(message)
	}
}

func (s *NOPaxos) handleViewRepairReply(reply *ViewRepairReply) {
	// If the request views do not match, ignore the reply
	if s.viewID.SessionNum != reply.ViewID.SessionNum || s.viewID.LeaderNum != reply.ViewID.LeaderNum {
		return
	}

	// If no view repair request is stored, ignore the reply
	request := s.viewRepair
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
			s.viewLog.Set(entry)
		} else {
			s.viewLog.Delete(slotNum)
		}
	}

	s.log = s.viewLog
	s.sessionMessageNum = request.MessageNum
	s.status = StatusNormal
	s.viewID = request.ViewID
	s.lastNormView = request.ViewID

	// Send a reply for all commands in the log
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