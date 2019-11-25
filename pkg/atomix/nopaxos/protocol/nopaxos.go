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
	"github.com/atomix/atomix-go-node/pkg/atomix/node"
	"github.com/atomix/atomix-go-node/pkg/atomix/service"
	"github.com/atomix/atomix-nopaxos-node/pkg/atomix/nopaxos/config"
	"io"
	"math"
	"sync"
	"time"
)

// NewNOPaxos returns a new NOPaxos protocol state struct
func NewNOPaxos(cluster Cluster, registry *node.Registry, config *config.ProtocolConfig) *NOPaxos {
	return &NOPaxos{
		config:  config,
		cluster: cluster,
		state:   newStateMachine(string(cluster.Member()), registry),
		viewID: &ViewId{
			SessionNum: 1,
			LeaderNum:  1,
		},
		lastNormView: &ViewId{
			SessionNum: 1,
			LeaderNum:  1,
		},
		sessionMessageCount: 1,
		log:                 make(map[LogSlotID]*LogEntry),
		firstSlotNum:        1,
		viewChanges:         make(map[MemberID]*ViewChange),
		gapCommitReps:       make(map[MemberID]*GapCommitReply),
		syncReps:            make(map[MemberID]*SyncReply),
	}
}

// MemberID is the ID of a NOPaxos cluster member
type MemberID string

// LeaderID is the leader identifier
type LeaderID uint64

// SessionID is a sequencer session ID
type SessionID uint64

// MessageID is a sequencer message ID
type MessageID uint64

// LogSlotID is a log slot number
type LogSlotID uint64

// Status is the protocol status
type Status int

const (
	// StatusNormal is the normal status
	StatusNormal Status = iota
	// StatusViewChange is the view change status
	StatusViewChange
	// StatusGapCommit is the gap commit status
	StatusGapCommit
)

// NOPaxos is an interface for managing the state of the NOPaxos consensus protocol
type NOPaxos struct {
	config              *config.ProtocolConfig
	cluster             Cluster
	state               *stateMachine
	sequencer           ClientService_ClientStreamServer
	log                 map[LogSlotID]*LogEntry
	firstSlotNum        LogSlotID
	lastSlotNum         LogSlotID
	status              Status
	sessionMessageCount MessageID
	viewID              *ViewId
	lastNormView        *ViewId
	viewChanges         map[MemberID]*ViewChange
	currentGapSlot      LogSlotID
	gapCommitReps       map[MemberID]*GapCommitReply
	tentativeSync       LogSlotID
	syncPoint           LogSlotID
	syncReps            map[MemberID]*SyncReply
	mu                  sync.RWMutex
}

func (s *NOPaxos) ClientStream(stream ClientService_ClientStreamServer) error {
	s.mu.Lock()
	s.sequencer = stream
	s.mu.Unlock()
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		go s.handleClient(message, stream)
	}
}

func (s *NOPaxos) handleClient(message *ClientMessage, stream ClientService_ClientStreamServer) {
	switch m := message.Message.(type) {
	case *ClientMessage_Command:
		s.command(m.Command, stream)
	case *ClientMessage_Query:
		s.query(m.Query)
	}
}

func (s *NOPaxos) ReplicaStream(stream ReplicaService_ReplicaStreamServer) error {
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		go s.handleReplica(message, stream)
	}
}

func (s *NOPaxos) handleReplica(message *ReplicaMessage, stream ReplicaService_ReplicaStreamServer) {
	switch m := message.Message.(type) {
	case *ReplicaMessage_Command:
		s.slot(m.Command)
	case *ReplicaMessage_SlotLookup:
		s.slotLookup(m.SlotLookup, stream)
	case *ReplicaMessage_GapCommit:
		s.gapCommit(m.GapCommit, stream)
	case *ReplicaMessage_GapCommitReply:
		s.gapCommitReply(m.GapCommitReply)
	case *ReplicaMessage_ViewChangeRequest:
		s.viewChangeRequest(m.ViewChangeRequest)
	case *ReplicaMessage_ViewChange:
		s.viewChange(m.ViewChange)
	case *ReplicaMessage_StartView:
		s.startView(m.StartView)
	case *ReplicaMessage_SyncPrepare:
		s.syncPrepare(m.SyncPrepare, stream)
	case *ReplicaMessage_SyncReply:
		s.syncReply(m.SyncReply)
	case *ReplicaMessage_SyncCommit:
		s.syncCommit(m.SyncCommit)
	}
}

func (s *NOPaxos) getLeader(viewID *ViewId) MemberID {
	members := s.cluster.Members()
	return members[int(uint64(viewID.LeaderNum)%uint64(len(members)))]
}

func (s *NOPaxos) slot(request *CommandRequest) {
	s.command(request, nil)
}

func (s *NOPaxos) command(request *CommandRequest, stream ClientService_ClientStreamServer) {
	// If the replica's status is not Normal, skip the commit
	if s.status != StatusNormal {
		return
	}

	if request.SessionNum == s.viewID.SessionNum && request.MessageNum == s.sessionMessageCount {
		// Command received in the normal case
		slotID := s.lastSlotNum + 1
		s.log[slotID] = &LogEntry{
			SlotNum:    slotID,
			Timestamp:  time.Now(),
			MessageNum: request.MessageNum,
			Value:      request.Value,
		}
		s.lastSlotNum = slotID

		// TODO: Apply the command to the state machine before responding if leader
		if stream != nil {
			_ = stream.Send(&ClientMessage{
				Message: &ClientMessage_CommandReply{
					CommandReply: &CommandReply{
						MessageNum: request.MessageNum,
						Sender:     s.cluster.Member(),
						ViewID:     s.viewID,
						SlotNum:    slotID,
						Value:      nil, // TODO
					},
				},
			})
		}
	} else if request.SessionNum > s.viewID.SessionNum {
		// Command received in the session terminated case
		newViewID := &ViewId{
			SessionNum: request.SessionNum,
			LeaderNum:  s.viewID.LeaderNum,
		}
		for _, member := range s.cluster.Members() {
			if stream, err := s.cluster.GetStream(member); err == nil {
				_ = stream.Send(&ReplicaMessage{
					Message: &ReplicaMessage_ViewChangeRequest{
						ViewChangeRequest: &ViewChangeRequest{
							ViewID: newViewID,
						},
					},
				})
			}
		}
	} else if request.SessionNum == s.viewID.SessionNum && request.MessageNum > s.sessionMessageCount {
		// Drop notification. If leader commit a gap, otherwise ask the leader for the slot
		if s.getLeader(s.viewID) == s.cluster.Member() {
			s.sendGapCommit()
		} else {
			stream, err := s.cluster.GetStream(s.getLeader(s.viewID))
			if err != nil {
				return
			}
			_ = stream.Send(&ReplicaMessage{
				Message: &ReplicaMessage_SlotLookup{
					SlotLookup: &SlotLookup{
						Sender:     s.cluster.Member(),
						ViewID:     s.viewID,
						MessageNum: request.MessageNum,
					},
				},
			})
		}
	}
}

func (s *NOPaxos) query(request *QueryRequest) {

}

func (s *NOPaxos) slotLookup(request *SlotLookup, stream ReplicaService_ReplicaStreamServer) {
	// If the view ID does not match the sender's view ID, skip the message
	if s.viewID == nil || s.viewID.LeaderNum != request.ViewID.LeaderNum || s.viewID.SessionNum != request.ViewID.SessionNum {
		return
	}

	// If this replica is not the leader, skip the message
	if s.getLeader(s.viewID) != s.cluster.Member() {
		return
	}

	// If the replica's status is not Normal, skip the message
	if s.status != StatusNormal {
		return
	}

	slotID := s.lastSlotNum + 1 - LogSlotID(s.sessionMessageCount-request.MessageNum)

	if slotID <= s.lastSlotNum {
		for i := slotID; i <= s.lastSlotNum; i++ {
			entry := s.log[i]
			if entry != nil {
				_ = stream.Send(&ReplicaMessage{
					Message: &ReplicaMessage_Command{
						Command: &CommandRequest{
							SessionNum: s.viewID.SessionNum,
							MessageNum: entry.MessageNum,
							Value:      entry.Value,
						},
					},
				})
			}
		}
	} else if slotID == s.lastSlotNum+1 {
		s.sendGapCommit()
	}
}

func (s *NOPaxos) sendGapCommit() {
	// If this replica is not the leader, skip the commit
	if s.getLeader(s.viewID) != s.cluster.Member() {
		return
	}

	// If the replica's status is not Normal, skip the commit
	if s.status != StatusNormal {
		return
	}

	slotID := s.lastSlotNum + 1

	// Set the replica's status to GapCommit
	s.status = StatusGapCommit

	// Set the current gap slot
	s.currentGapSlot = slotID

	// Send a GapCommit to each replica
	for _, member := range s.cluster.Members() {
		if stream, err := s.cluster.GetStream(member); err == nil {
			_ = stream.Send(&ReplicaMessage{
				Message: &ReplicaMessage_GapCommit{
					GapCommit: &GapCommitRequest{
						ViewID:  s.viewID,
						SlotNum: slotID,
					},
				},
			})
		}
	}
}

func (s *NOPaxos) gapCommit(request *GapCommitRequest, stream ReplicaService_ReplicaStreamServer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If the view ID does not match the sender's view ID, skip the message
	if s.viewID == nil || s.viewID.LeaderNum != request.ViewID.LeaderNum || s.viewID.SessionNum != request.ViewID.SessionNum {
		return
	}

	// If the replica's status is not Normal or GapCommit, skip the message
	if s.status != StatusNormal && s.status != StatusGapCommit {
		return
	}

	// If the request slot ID is not the next slot in the replica's log, skip the message
	lastSlotID := s.lastSlotNum
	if request.SlotNum > lastSlotID+1 {
		return
	}

	// A no-op entry is represented as a missing entry
	delete(s.log, request.SlotNum)

	// Increment the session message ID if necessary
	if request.SlotNum > s.lastSlotNum {
		s.sessionMessageCount++
	}

	_ = stream.SendMsg(&GapCommitReply{
		Sender:  s.cluster.Member(),
		ViewID:  s.viewID,
		SlotNum: request.SlotNum,
	})
}

func (s *NOPaxos) gapCommitReply(reply *GapCommitReply) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If the view ID does not match the sender's view ID, skip the message
	if s.viewID == nil || s.viewID.LeaderNum != reply.ViewID.LeaderNum || s.viewID.SessionNum != reply.ViewID.SessionNum {
		return
	}

	// If this replica is not the leader, skip the message
	if s.getLeader(s.viewID) != s.cluster.Member() {
		return
	}

	// If the replica's status is not Normal or GapCommit, skip the message
	if s.status != StatusGapCommit {
		return
	}

	// If the gap commit slot does not match the current gap slot, skip the message
	if reply.SlotNum != s.currentGapSlot {
		return
	}

	s.gapCommitReps[reply.Sender] = reply

	// Get the set of gap commit replies for the current slot
	gapCommits := make([]*GapCommitReply, 0, len(s.gapCommitReps))
	for _, gapCommit := range s.gapCommitReps {
		if gapCommit.ViewID.SessionNum == s.viewID.SessionNum && gapCommit.ViewID.LeaderNum == s.viewID.LeaderNum && gapCommit.SlotNum == s.currentGapSlot {
			gapCommits = append(gapCommits, gapCommit)
		}
	}

	// If a quorum of gap commits has been received for the slot, return the status to normal
	if len(gapCommits) >= s.cluster.QuorumSize() {
		s.status = StatusNormal
	}
}

func (s *NOPaxos) startLeaderChange() {
	newViewID := &ViewId{
		SessionNum: s.viewID.SessionNum,
		LeaderNum:  s.viewID.LeaderNum + 1,
	}

	for _, member := range s.cluster.Members() {
		if member != s.getLeader(newViewID) {
			if stream, err := s.cluster.GetStream(member); err == nil {
				_ = stream.Send(&ReplicaMessage{
					Message: &ReplicaMessage_ViewChangeRequest{
						ViewChangeRequest: &ViewChangeRequest{
							ViewID: newViewID,
						},
					},
				})
			}
		}
	}
}

func (s *NOPaxos) viewChangeRequest(request *ViewChangeRequest) {
	newLeaderID := LeaderID(math.Max(float64(s.viewID.LeaderNum), float64(request.ViewID.LeaderNum)))
	newSessionID := SessionID(math.Max(float64(s.viewID.SessionNum), float64(request.ViewID.SessionNum)))
	newViewID := &ViewId{
		LeaderNum:  newLeaderID,
		SessionNum: newSessionID,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// If the view IDs match, ignore the request
	if s.viewID != nil && s.viewID.LeaderNum == newViewID.LeaderNum && s.viewID.SessionNum == newViewID.SessionNum {
		return
	}

	// Set the replica's status to ViewChange
	s.status = StatusViewChange

	// Set the replica's view ID to the new view ID
	s.viewID = newViewID

	// Reset the view changes
	s.viewChanges = make(map[MemberID]*ViewChange)

	// Send a ViewChange to the new leader
	stream, err := s.cluster.GetStream(s.getLeader(newViewID))
	if err != nil {
		return
	}

	// TODO: We probably can't send the entire log in a single message here
	log := make([]*LogEntry, 0, len(s.log))
	for _, entry := range s.log {
		log = append(log, entry)
	}
	_ = stream.Send(&ReplicaMessage{
		Message: &ReplicaMessage_ViewChange{
			ViewChange: &ViewChange{
				Sender:     s.cluster.Member(),
				ViewID:     newViewID,
				LastNormal: s.lastNormView,
				MessageNum: s.sessionMessageCount,
				Log:        log,
			},
		},
	})

	// Send a view change request to all replicas other than the leader
	for _, member := range s.cluster.Members() {
		if member != s.getLeader(newViewID) {
			if stream, err := s.cluster.GetStream(member); err == nil {
				_ = stream.Send(&ReplicaMessage{
					Message: &ReplicaMessage_ViewChangeRequest{
						ViewChangeRequest: &ViewChangeRequest{
							ViewID: newViewID,
						},
					},
				})
			}
		}
	}
}

func (s *NOPaxos) viewChange(request *ViewChange) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If the view IDs do not match, ignore the request
	if s.viewID == nil || s.viewID.LeaderNum != request.ViewID.LeaderNum || s.viewID.SessionNum != request.ViewID.SessionNum {
		return
	}

	// If the replica's status is not ViewChange, ignore the request
	if s.status != StatusViewChange {
		return
	}

	// If this replica is not the leader of the view, ignore the request
	if s.getLeader(request.ViewID) != s.cluster.Member() {
		return
	}

	// Add the view change to the set of view changes
	s.viewChanges[request.Sender] = request

	// Aggregate the view changes for the current view
	viewChanges := make([]*ViewChange, 0, len(s.viewChanges))
	for _, viewChange := range s.viewChanges {
		if viewChange.ViewID.SessionNum == s.viewID.SessionNum && viewChange.ViewID.LeaderNum == s.viewID.LeaderNum {
			viewChanges = append(viewChanges, viewChange)
		}
	}

	// If the view changes have reached a quorum, start the new view
	if len(viewChanges) >= s.cluster.QuorumSize() {
		// Create the state for the new view
		var lastNormal *ViewChange
		for _, viewChange := range viewChanges {
			if viewChange.ViewID.SessionNum <= s.viewID.SessionNum && viewChange.ViewID.LeaderNum <= s.viewID.LeaderNum {
				lastNormal = viewChange
			}
		}

		var newMessageID MessageID
		goodLogs := make([][]*LogEntry, 0, len(viewChanges))
		for _, viewChange := range viewChanges {
			if viewChange.LastNormal.SessionNum == lastNormal.ViewID.SessionNum && viewChange.LastNormal.LeaderNum == lastNormal.ViewID.LeaderNum {
				goodLogs = append(goodLogs, viewChange.Log)

				// If the session has changed, take the maximum message ID
				if lastNormal.ViewID.SessionNum == s.viewID.SessionNum && viewChange.MessageNum > newMessageID {
					newMessageID = viewChange.MessageNum
				}
			}
		}

		var firstSlotID, lastSlotID LogSlotID
		newLog := make(map[LogSlotID]*LogEntry)
		for _, goodLog := range goodLogs {
			for _, entry := range goodLog {
				newEntry := newLog[entry.SlotNum]
				if newEntry == nil {
					newLog[entry.SlotNum] = entry
					if firstSlotID == 0 || entry.SlotNum < firstSlotID {
						firstSlotID = entry.SlotNum
					}
					if lastSlotID == 0 || entry.SlotNum > lastSlotID {
						lastSlotID = entry.SlotNum
					}
				}
			}
		}

		log := make([]*LogEntry, 0, len(newLog))
		for slotID := firstSlotID; slotID <= lastSlotID; slotID++ {
			entry := newLog[slotID]
			if entry != nil {
				log = append(log, entry)
			}
		}

		// Send a StartView to each replica
		for _, member := range s.cluster.Members() {
			if stream, err := s.cluster.GetStream(member); err == nil {
				_ = stream.Send(&ReplicaMessage{
					Message: &ReplicaMessage_StartView{
						StartView: &StartView{
							ViewID:     s.viewID,
							MessageNum: newMessageID,
							Log:        log,
						},
					},
				})
			}
		}
	}
}

func (s *NOPaxos) startView(request *StartView) {

}

func (s *NOPaxos) startSync() {
	// If this replica is not the leader of the view, ignore the request
	if s.getLeader(s.viewID) != s.cluster.Member() {
		return
	}

	// If the replica's status is not Normal, do not attempt the sync
	if s.status != StatusNormal {
		return
	}

	s.syncReps = make(map[MemberID]*SyncReply)
	s.tentativeSync = s.lastSlotNum

	log := make([]*LogEntry, 0, len(s.log))
	for _, entry := range s.log {
		log = append(log, entry)
	}

	for _, member := range s.cluster.Members() {
		if member != s.cluster.Member() {
			if stream, err := s.cluster.GetStream(member); err == nil {
				_ = stream.Send(&ReplicaMessage{
					Message: &ReplicaMessage_SyncPrepare{
						SyncPrepare: &SyncPrepare{
							Sender:     s.cluster.Member(),
							ViewID:     s.viewID,
							MessageNum: s.sessionMessageCount,
							Log:        log,
						},
					},
				})
			}
		}
	}
}

func (s *NOPaxos) syncPrepare(request *SyncPrepare, stream ReplicaService_ReplicaStreamServer) {
	// If the replica's status is not Normal, ignore the request
	if s.status != StatusNormal {
		return
	}

	// If the view IDs do not match, ignore the request
	if s.viewID == nil || s.viewID.LeaderNum != request.ViewID.LeaderNum || s.viewID.SessionNum != request.ViewID.SessionNum {
		return
	}

	// If the sender is not the leader for the current view, ignore the request
	if request.Sender != s.getLeader(request.ViewID) {
		return
	}

	newLog := make(map[LogSlotID]*LogEntry)
	var firstSlotID LogSlotID
	if len(request.Log) > 0 {
		firstSlotID = request.Log[0].SlotNum
	}

	var lastSlotID = firstSlotID
	for _, entry := range request.Log {
		newLog[entry.SlotNum] = entry
		if entry.SlotNum > lastSlotID {
			lastSlotID = entry.SlotNum
		}
	}

	for i := lastSlotID; i < s.lastSlotNum; i++ {
		entry := s.log[i]
		if entry != nil {
			newLog[entry.SlotNum] = entry
		}
	}
	s.sessionMessageCount = s.sessionMessageCount + MessageID(lastSlotID-s.lastSlotNum)
	s.log = newLog
	s.firstSlotNum = firstSlotID
	s.lastSlotNum = lastSlotID

	// Send a SyncReply back to the leader
	_ = stream.Send(&ReplicaMessage{
		Message: &ReplicaMessage_SyncReply{
			SyncReply: &SyncReply{
				Sender:  s.cluster.Member(),
				ViewID:  s.viewID,
				SlotNum: lastSlotID,
			},
		},
	})

	// Send a RequestReply for all entries in the new log
	s.mu.RLock()
	sequencer := s.sequencer
	s.mu.RUnlock()

	if sequencer != nil {
		for slotNum := s.firstSlotNum; slotNum <= s.lastSlotNum; slotNum++ {
			entry := s.log[slotNum]
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

func (s *NOPaxos) syncReply(reply *SyncReply) {
	// If the view IDs do not match, ignore the request
	if s.viewID == nil || s.viewID.LeaderNum != reply.ViewID.LeaderNum || s.viewID.SessionNum != reply.ViewID.SessionNum {
		return
	}

	// If the replica's status is not Normal, ignore the request
	if s.status != StatusNormal {
		return
	}

	// Add the reply to the set of sync replies
	s.syncReps[reply.Sender] = reply

	syncReps := make([]*SyncReply, 0, len(s.syncReps))
	for _, syncRep := range s.syncReps {
		if syncRep.ViewID.LeaderNum == s.viewID.LeaderNum && syncRep.ViewID.SessionNum == s.viewID.SessionNum && syncRep.SlotNum == s.tentativeSync {
			syncReps = append(syncReps, syncRep)
		}
	}

	if len(syncReps) >= s.cluster.QuorumSize() {
		log := make([]*LogEntry, 0, len(s.log))
		for _, entry := range s.log {
			log = append(log, entry)
		}

		for _, member := range s.cluster.Members() {
			if member != s.cluster.Member() {
				if stream, err := s.cluster.GetStream(member); err == nil {
					_ = stream.Send(&ReplicaMessage{
						Message: &ReplicaMessage_SyncCommit{
							SyncCommit: &SyncCommit{
								Sender:     s.cluster.Member(),
								ViewID:     s.viewID,
								MessageNum: s.sessionMessageCount,
								Log:        log,
							},
						},
					})
				}
			}
		}
	}
}

func (s *NOPaxos) syncCommit(request *SyncCommit) {
	// If the replica's status is not Normal, ignore the request
	if s.status != StatusNormal {
		return
	}

	// If the view IDs do not match, ignore the request
	if s.viewID == nil || s.viewID.LeaderNum != request.ViewID.LeaderNum || s.viewID.SessionNum != request.ViewID.SessionNum {
		return
	}

	// If the sender is not the leader for the current view, ignore the request
	if request.Sender != s.getLeader(request.ViewID) {
		return
	}

	newLog := make(map[LogSlotID]*LogEntry)
	var firstSlotID LogSlotID
	if len(request.Log) > 0 {
		firstSlotID = request.Log[0].SlotNum
	}

	var lastSlotID = firstSlotID
	for _, entry := range request.Log {
		newLog[entry.SlotNum] = entry
		if entry.SlotNum > lastSlotID {
			lastSlotID = entry.SlotNum
		}
	}

	for i := lastSlotID; i < s.lastSlotNum; i++ {
		entry := s.log[i]
		if entry != nil {
			newLog[entry.SlotNum] = entry
		}
	}
	s.sessionMessageCount = s.sessionMessageCount + MessageID(lastSlotID-s.lastSlotNum)
	s.log = newLog
	s.firstSlotNum = firstSlotID
	s.lastSlotNum = lastSlotID
}

func newStateMachine(node string, registry *node.Registry) *stateMachine {
	sm := &stateMachine{
		node:     node,
		registry: registry,
	}
	sm.reset()
	return sm
}

// stateMachine is the replica state machine
type stateMachine struct {
	node     string
	registry *node.Registry
	state    node.StateMachine
	context  node.Context
}

// reset resets the state machine
func (s *stateMachine) reset() {
	s.context = &stateMachineContext{}
	s.state = node.NewPrimitiveStateMachine(s.registry, s.context)
}

// stateMachineContext is a replica state machine context
type stateMachineContext struct {
	node      string
	index     uint64
	timestamp time.Time
	op        service.OperationType
}

func (c *stateMachineContext) Node() string {
	return c.node
}

func (c *stateMachineContext) Index() uint64 {
	return c.index
}

func (c *stateMachineContext) Timestamp() time.Time {
	return c.timestamp
}

func (c *stateMachineContext) OperationType() service.OperationType {
	return c.op
}
