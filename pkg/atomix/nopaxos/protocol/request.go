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

import "github.com/atomix/atomix-go-node/pkg/atomix/node"

func (s *NOPaxos) command(request *CommandRequest, stream ClientService_ClientStreamServer) {
	s.logger.Receive("CommandRequest", request)

	s.mu.Lock()
	defer s.mu.Unlock()

	// If the replica's status is not Normal, skip the commit
	if s.status != StatusNormal {
		s.logger.Trace("Dropping CommandRequest: Replica status is not Normal")
		return
	}

	if request.SessionNum == s.viewID.SessionNum && request.MessageNum == s.sessionMessageNum {
		// Command received in the normal case
		slotNum := s.log.LastSlot() + 1
		entry := &LogEntry{
			SlotNum:    slotNum,
			Timestamp:  request.Timestamp,
			MessageNum: request.MessageNum,
			Value:      request.Value,
		}
		s.log.Set(entry)

		// Apply the command to the state machine before responding if leader
		if stream != nil {
			if s.getLeader(s.viewID) == s.cluster.Member() {
				ch := make(chan node.Output)
				viewID := s.viewID
				go func() {
					for result := range ch {
						commandReply := &CommandReply{
							MessageNum: request.MessageNum,
							Sender:     s.cluster.Member(),
							ViewID:     viewID,
							SlotNum:    slotNum,
							Value:      result.Value,
						}
						message := &ClientMessage{
							Message: &ClientMessage_CommandReply{
								CommandReply: commandReply,
							},
						}
						// TODO: Send state machine errors
						s.logger.Send("CommandReply", commandReply)
						_ = stream.Send(message)
					}
				}()
				s.state.applyCommand(entry, ch)
			} else {
				commandReply := &CommandReply{
					MessageNum: request.MessageNum,
					Sender:     s.cluster.Member(),
					ViewID:     s.viewID,
					SlotNum:    slotNum,
				}
				message := &ClientMessage{
					Message: &ClientMessage_CommandReply{
						CommandReply: commandReply,
					},
				}
				s.logger.Send("CommandReply", commandReply)
				_ = stream.Send(message)
			}
		}
		s.sessionMessageNum++
	} else if request.SessionNum > s.viewID.SessionNum {
		s.logger.Info("Session %d terminated", s.viewID.SessionNum)
		s.logger.Info("Requesting view change for session %d", request.SessionNum)

		// Command received in the session terminated case
		newViewID := &ViewId{
			SessionNum: request.SessionNum,
			LeaderNum:  s.viewID.LeaderNum,
		}
		viewChangeRequest := &ViewChangeRequest{
			ViewID: newViewID,
		}
		message := &ReplicaMessage{
			Message: &ReplicaMessage_ViewChangeRequest{
				ViewChangeRequest: viewChangeRequest,
			},
		}
		for _, member := range s.cluster.Members() {
			s.logger.SendTo("ViewChangeRequest", viewChangeRequest, member)
			go s.send(message, member)
		}
	} else if request.SessionNum == s.viewID.SessionNum && request.MessageNum > s.sessionMessageNum {
		s.logger.Debug("Received drop notification for %d", s.sessionMessageNum)

		// Drop notification. If leader commit a gap, otherwise ask the leader for the slot
		if s.getLeader(s.viewID) == s.cluster.Member() {
			s.sendGapCommit()
		} else {
			leader := s.getLeader(s.viewID)
			slotLookup := &SlotLookup{
				Sender:     s.cluster.Member(),
				ViewID:     s.viewID,
				MessageNum: request.MessageNum,
			}
			message := &ReplicaMessage{
				Message: &ReplicaMessage_SlotLookup{
					SlotLookup: slotLookup,
				},
			}
			s.logger.SendTo("SlotLookup", slotLookup, leader)
			go s.send(message, leader)
		}
	}
}

func (s *NOPaxos) query(request *QueryRequest, stream ClientService_ClientStreamServer) {
	s.logger.Receive("QueryRequest", request)

	s.mu.RLock()
	defer s.mu.RUnlock()

	// If the replica's status is not Normal, skip the commit
	if s.status != StatusNormal {
		return
	}

	if request.SessionNum == s.viewID.SessionNum && stream != nil && s.getLeader(s.viewID) == s.cluster.Member() {
		ch := make(chan node.Output)
		s.state.applyQuery(request, ch)
		go func() {
			for result := range ch {
				// TODO: Send state machine errors
				queryReply := &QueryReply{
					MessageNum: request.MessageNum,
					Sender:     s.cluster.Member(),
					ViewID:     s.viewID,
					Value:      result.Value,
				}
				s.logger.Send("QueryReply", queryReply)
				_ = stream.Send(&ClientMessage{
					Message: &ClientMessage_QueryReply{
						QueryReply: queryReply,
					},
				})
			}
		}()
	}
}

func (s *NOPaxos) handleSlot(request *CommandRequest) {
	s.command(request, nil)
}
