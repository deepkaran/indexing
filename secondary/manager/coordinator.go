// Copyright (c) 2014 Couchbase, Inc.

// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package manager

import (
	"fmt"
	common "github.com/couchbase/gometa/common"
	message "github.com/couchbase/gometa/message"
	protocol "github.com/couchbase/gometa/protocol"
	r "github.com/couchbase/gometa/repository"
	co "github.com/couchbase/indexing/secondary/common"
	"math"
	"sync"
	"time"
)

/////////////////////////////////////////////////////////////////////////////
// Type Declaration
/////////////////////////////////////////////////////////////////////////////

const (
	OPCODE_ADD_IDX_DEFN common.OpCode = iota
	OPCODE_DEL_IDX_DEFN
	OPCODE_NOTIFY_TIMESTAMP
)

type Coordinator struct {
	state      *CoordinatorState
	repo       *MetadataRepo
	env        *env
	txn        *common.TxnState
	config     *r.ServerConfig
	configRepo *r.Repository
	site       *protocol.ElectionSite
	listener   *common.PeerListener
	factory    protocol.MsgFactory
	skillch    chan bool
	idxMgr     *IndexManager

	mutex sync.Mutex
	cond  *sync.Cond
	ready bool
}

type CoordinatorState struct {
	incomings chan *protocol.RequestHandle

	// mutex protected variables
	mutex     sync.Mutex
	done      bool
	status    protocol.PeerStatus
	pendings  map[uint64]*protocol.RequestHandle       // key : request id
	proposals map[common.Txnid]*protocol.RequestHandle // key : txnid
}

/////////////////////////////////////////////////////////////////////////////
// Public API
/////////////////////////////////////////////////////////////////////////////

func NewCoordinator(repo *MetadataRepo, idxMgr *IndexManager) *Coordinator {
	coordinator := new(Coordinator)
	coordinator.repo = repo
	coordinator.ready = false
	coordinator.cond = sync.NewCond(&coordinator.mutex)
	coordinator.idxMgr = idxMgr

	return coordinator
}

//
// Run Coordinator
//
func (s *Coordinator) Run(config string) {

	repeat := true
	for repeat {
		pauseTime := s.runOnce(config)
		if !s.IsDone() {
			if pauseTime > 0 {
				// wait before restart
				time.Sleep(time.Duration(pauseTime) * time.Millisecond)
			}
		} else {
			repeat = false
		}
	}
}

//
// Terminate the Coordinator
//
func (s *Coordinator) Terminate() {

	defer func() {
		if r := recover(); r != nil {
			co.Warnf("panic in MetadataRepo.Close() : %s.  Ignored.\n", r)
		}
	}()

	s.state.mutex.Lock()
	defer s.state.mutex.Unlock()

	if s.state.done {
		return
	}

	s.state.done = true

	s.site.Close()
	s.site = nil

	s.configRepo.Close()
	s.configRepo = nil

	s.skillch <- true // kill leader/follower server
}

//
// Check if server is terminated
//
func (s *Coordinator) IsDone() bool {

	s.state.mutex.Lock()
	defer s.state.mutex.Unlock()

	return s.state.done
}

//
// Handle a new request.  This function will block until the request is being processed
// (by returning true) or until the request is being interrupted (by returning false).
// If request is interrupted, then the request may still be processed by some other
// nodes.  So the outcome of the request is unknown when this function returns false.
//
func (s *Coordinator) NewRequest(id uint64, opCode uint32, key string, content []byte) bool {

	s.waitForReady()

	req := s.factory.CreateRequest(id, opCode, key, content)

	handle := &protocol.RequestHandle{Request: req, Err: nil}
	handle.CondVar = sync.NewCond(&handle.Mutex)

	handle.CondVar.L.Lock()
	defer handle.CondVar.L.Unlock()

	s.state.incomings <- handle

	handle.CondVar.Wait()

	return handle.Err == nil
}

/////////////////////////////////////////////////////////////////////////////
// Main Control Loop
/////////////////////////////////////////////////////////////////////////////

//
// Run the server until it stop.  Will not attempt to re-run.
//
func (c *Coordinator) runOnce(config string) int {

	co.Debugf("Coordinator.runOnce() : Start Running Coordinator")

	pauseTime := 0

	defer func() {
		if r := recover(); r != nil {
			co.Warnf("panic in Coordinator.runOnce() : %s\n", r)
		}

		common.SafeRun("Coordinator.cleanupState()",
			func() {
				c.cleanupState()
			})
	}()

	err := c.bootstrap(config)
	if err != nil {
		pauseTime = 200
	}

	// Check if the server has been terminated explicitly. If so, don't run.
	if !c.IsDone() {

		// runElection() finishes if there is an error, election result is known or
		// it being terminated. Unless being killed explicitly, a goroutine
		// will continue to run to responds to other peer election request
		leader, err := c.runElection()
		if err != nil {
			co.Warnf("Coordinator.runOnce() : Error Encountered During Election : %s", err.Error())
			pauseTime = 100
		} else {

			// Check if the server has been terminated explicitly. If so, don't run.
			if !c.IsDone() {
				// runCoordinator() is done if there is an error	or being terminated explicitly (killch)
				err := c.runProtocol(leader)
				if err != nil {
					co.Warnf("Coordinator.RunOnce() : Error Encountered From Coordinator : %s", err.Error())
				}
			}
		}
	} else {
		co.Infof("Coordinator.RunOnce(): Coordinator has been terminated explicitly. Terminate.")
	}

	return pauseTime
}

/////////////////////////////////////////////////////////////////////////////
// Bootstrap and cleanup
/////////////////////////////////////////////////////////////////////////////

//
// Bootstrp
//
func (s *Coordinator) bootstrap(config string) (err error) {

	s.env, err = newEnv(config)
	if err != nil {
		return err
	}

	// Initialize server state
	s.state = newCoordinatorState()

	// Initialize various callback facility for leader election and
	// voting protocol.
	s.factory = message.NewConcreteMsgFactory()
	s.skillch = make(chan bool, 1) // make it buffered to unblock sender
	s.site = nil

	// Create and initialize new txn state.
	s.txn = common.NewTxnState()

	// Initialize the state to enable voting
	s.configRepo, err = r.OpenRepositoryWithName(COORDINATOR_CONFIG_STORE)
	if err != nil {
		return err
	}

	s.config = r.NewServerConfig(s.configRepo)
	lastLoggedTxid, err := s.config.GetLastLoggedTxnId()
	if err != nil {
		return err
	}
	s.txn.InitCurrentTxnid(common.Txnid(lastLoggedTxid))

	// Need to start the peer listener before election. A follower may
	// finish its election before a leader finishes its election. Therefore,
	// a follower node can request a connection to the leader node before that
	// node knows it is a leader.  By starting the listener now, it allows the
	// follower to establish the connection and let the leader handles this
	// connection at a later time (when it is ready to be a leader).
	s.listener, err = common.StartPeerListener(s.getHostTCPAddr())
	if err != nil {
		return NewError(ERROR_COOR_LISTENER_FAIL, NORMAL, COORDINATOR, err,
			fmt.Sprintf("Index Coordinator : Fail to start PeerListener"))
	}

	// tell boostrap is ready
	s.markReady()

	return nil
}

//
// Cleanup internal state upon exit
//
func (s *Coordinator) cleanupState() {

	// tell that coordinator is no longer ready
	s.markNotReady()

	s.state.mutex.Lock()
	defer s.state.mutex.Unlock()

	common.SafeRun("Coordinator.cleanupState()",
		func() {
			if s.listener != nil {
				s.listener.Close()
			}
		})

	common.SafeRun("Coordinator.cleanupState()",
		func() {
			if s.site != nil {
				s.site.Close()
			}
		})

	for len(s.state.incomings) > 0 {
		request := <-s.state.incomings
		request.Err = fmt.Errorf("Terminate Request due to server termination")

		common.SafeRun("Coordinator.cleanupState()",
			func() {
				request.CondVar.L.Lock()
				defer request.CondVar.L.Unlock()
				request.CondVar.Signal()
			})
	}

	for _, request := range s.state.pendings {
		request.Err = fmt.Errorf("Terminate Request due to server termination")

		common.SafeRun("Coordinator.cleanupState()",
			func() {
				request.CondVar.L.Lock()
				defer request.CondVar.L.Unlock()
				request.CondVar.Signal()
			})
	}

	for _, request := range s.state.proposals {
		request.Err = fmt.Errorf("Terminate Request due to server termination")

		common.SafeRun("Coordinator.cleanupState()",
			func() {
				request.CondVar.L.Lock()
				defer request.CondVar.L.Unlock()
				request.CondVar.Signal()
			})
	}
}

func (s *Coordinator) markReady() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.ready = true
	s.cond.Signal()
}

func (s *Coordinator) markNotReady() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.ready = false
}

func (s *Coordinator) isReady() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.ready
}

func (s *Coordinator) waitForReady() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	for !s.ready {
		s.cond.Wait()
	}
}

/////////////////////////////////////////////////////////////////////////////
//  Election
/////////////////////////////////////////////////////////////////////////////

//
// run election
//
func (s *Coordinator) runElection() (leader string, err error) {

	host := s.getHostUDPAddr()
	peers := s.getPeerUDPAddr()

	// Create an election site to start leader election.
	co.Debugf("Coordinator.runElection(): Local Coordinator %s start election", host)
	co.Debugf("Coordinator.runElection(): Peer in election")
	for _, peer := range peers {
		co.Debugf("	peer : %s", peer)
	}

	s.site, err = protocol.CreateElectionSite(host, peers, s.factory, s, false)
	if err != nil {
		return "", err
	}

	// blocked until leader is elected. coordinator.Terminate() will unblock this.
	resultCh := s.site.StartElection()
	leader, ok := <-resultCh
	if !ok {
		return "", NewError(ERROR_COOR_ELECTION_FAIL, NORMAL, COORDINATOR, nil,
			fmt.Sprintf("Index Coordinator Election Fails"))
	}

	return leader, nil
}

/////////////////////////////////////////////////////////////////////////////
//  Run Leader/Follower protocol
/////////////////////////////////////////////////////////////////////////////

//
// run server (as leader or follower)
//
func (s *Coordinator) runProtocol(leader string) (err error) {

	host := s.getHostUDPAddr()

	// If this host is the leader, then start the leader server.
	// Otherwise, start the followerCoordinator.
	if leader == host {
		co.Debugf("Coordinator.runServer() : Local Coordinator %s is elected as leader. Leading ...", leader)
		s.state.setStatus(protocol.LEADING)

		// start other master services if this node is a candidate as master
		s.idxMgr.startMasterService()
		defer s.idxMgr.stopMasterService()

		err = protocol.RunLeaderServer(s.getHostTCPAddr(), s.listener, s, s, s.factory, s.skillch)
	} else {
		co.Debugf("Coordinator.runServer() : Remote Coordinator %s is elected as leader. Following ...", leader)
		s.state.setStatus(protocol.FOLLOWING)
		leaderAddr := s.findMatchingPeerTCPAddr(leader)
		if len(leaderAddr) == 0 {
			return NewError(ERROR_COOR_ELECTION_FAIL, NORMAL, COORDINATOR, nil,
				fmt.Sprintf("Index Coordinator cannot find matching TCP addr for leader "+leader))
		}
		err = protocol.RunFollowerServer(s.getHostTCPAddr(), leaderAddr, s, s, s.factory, s.skillch)
	}

	return err
}

/////////////////////////////////////////////////////////////////////////////
// CoordinatorState
/////////////////////////////////////////////////////////////////////////////

//
// Create a new CoordinatorState
//
func newCoordinatorState() *CoordinatorState {

	incomings := make(chan *protocol.RequestHandle, common.MAX_PROPOSALS)
	pendings := make(map[uint64]*protocol.RequestHandle)
	proposals := make(map[common.Txnid]*protocol.RequestHandle)
	state := &CoordinatorState{
		incomings: incomings,
		pendings:  pendings,
		proposals: proposals,
		status:    protocol.ELECTING,
		done:      false}

	return state
}

func (s *CoordinatorState) getStatus() protocol.PeerStatus {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.status
}

func (s *CoordinatorState) setStatus(status protocol.PeerStatus) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.status = status
}

/////////////////////////////////////////////////////////////////////////////
//  Coordinator Action (Callback)
/////////////////////////////////////////////////////////////////////////////

func (c *Coordinator) GetEnsembleSize() uint64 {
	return uint64(len(c.getPeerUDPAddr())) + 1 // including myself
}

func (c *Coordinator) GetLastLoggedTxid() (common.Txnid, error) {
	val, err := c.config.GetLastLoggedTxnId()
	return common.Txnid(val), err
}

func (c *Coordinator) GetLastCommittedTxid() (common.Txnid, error) {
	val, err := c.config.GetLastCommittedTxnId()
	return common.Txnid(val), err
}

func (c *Coordinator) GetStatus() protocol.PeerStatus {
	return c.state.getStatus()
}

func (c *Coordinator) GetCurrentEpoch() (uint32, error) {
	return c.config.GetCurrentEpoch()
}

func (c *Coordinator) GetAcceptedEpoch() (uint32, error) {
	return c.config.GetAcceptedEpoch()
}

func (c *Coordinator) GetCommitedEntries(txid1, txid2 common.Txnid) (<-chan protocol.LogEntryMsg, <-chan error, chan<- bool, error) {
	// The coordinator does not use the commit log.  So nothing to stream.
	return nil, nil, nil, nil
}

func (c *Coordinator) LogAndCommit(txid common.Txnid, op uint32, key string, content []byte, toCommit bool) error {
	// The coordinator does not use the commit log. So nothing to log.
	return nil
}

func (c *Coordinator) NotifyNewAcceptedEpoch(epoch uint32) error {

	oldEpoch, _ := c.GetAcceptedEpoch()

	// update only if the new epoch is larger
	if oldEpoch < epoch {
		err := c.config.SetAcceptedEpoch(epoch)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) NotifyNewCurrentEpoch(epoch uint32) error {

	oldEpoch, _ := c.GetCurrentEpoch()

	// update only if the new epoch is larger
	if oldEpoch < epoch {
		err := c.config.SetCurrentEpoch(epoch)
		if err != nil {
			return err
		}

		// update the election site with the new epoch, such that
		// for new incoming vote, the server can reply with the
		// new and correct epoch
		c.site.UpdateWinningEpoch(epoch)

		// any new tnxid from now on will use the new epoch
		c.txn.SetEpoch(epoch)
	}

	return nil
}

func (c *Coordinator) GetFollowerId() string {
	return c.getHostUDPAddr()
}

// TODO : what to do if createIndex returns error
func (c *Coordinator) LogProposal(proposal protocol.ProposalMsg) error {

	if c.GetStatus() == protocol.LEADING {
		switch common.OpCode(proposal.GetOpCode()) {
		case OPCODE_ADD_IDX_DEFN:
			success := c.createIndex(proposal.GetKey(), proposal.GetContent())
			co.Debugf("Coordinator.LogProposal(): (createIndex) success = %s", success)
		case OPCODE_DEL_IDX_DEFN:
			success := c.deleteIndex(proposal.GetKey())
			co.Debugf("Coordinator.LogProposal(): (deleteIndex) success = %s", success)
		}
	}

	switch common.OpCode(proposal.GetOpCode()) {
	case OPCODE_NOTIFY_TIMESTAMP:
		timestamp, err := unmarshallTimestampWrapper(proposal.GetContent())
		if err == nil {
			c.idxMgr.notifyNewTimestamp(timestamp)
		} else {
			co.Debugf("Coordinator.LogProposal(): error when unmarshalling timestamp. Ignore timestamp.  Error=%s", err.Error())
		}
	}

	c.updateRequestOnNewProposal(proposal)

	return nil
}

func (c *Coordinator) Commit(txid common.Txnid) error {
	c.updateRequestOnCommit(txid)
	return nil
}

func (c *Coordinator) GetQuorumVerifier() protocol.QuorumVerifier {
	return c
}

func (c *Coordinator) GetNextTxnId() common.Txnid {
	return c.txn.GetNextTxnId()
}

//  TODO : Quorum should be based on active participants
func (c *Coordinator) HasQuorum(count int) bool {
	ensembleSz := c.GetEnsembleSize()
	return count > int(ensembleSz/2)
}

/////////////////////////////////////////////////////////////////////////////
//  Request Handling
/////////////////////////////////////////////////////////////////////////////

// Return a channel of request for the leader to process on.
func (c *Coordinator) GetRequestChannel() <-chan *protocol.RequestHandle {
	return (<-chan *protocol.RequestHandle)(c.state.incomings)
}

// This is called when the leader has de-queued the request for processing.
func (c *Coordinator) AddPendingRequest(handle *protocol.RequestHandle) {
	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()

	c.state.pendings[handle.Request.GetReqId()] = handle
}

//
// Update the request upon new proposal.
//
func (c *Coordinator) updateRequestOnNewProposal(proposal protocol.ProposalMsg) {

	fid := proposal.GetFid()
	reqId := proposal.GetReqId()
	txnid := proposal.GetTxnid()

	co.Debugf("Coorindator.updateRequestOnNewProposal(): recieve proposal. Txnid %d, follower id %s, coorindator fid %s",
		txnid, fid, c.GetFollowerId())

	// If this host is the one that sends the request to the leader
	if fid == c.GetFollowerId() {
		c.state.mutex.Lock()
		defer c.state.mutex.Unlock()

		// look up the request handle from the pending list and
		// move it to the proposed list
		handle, ok := c.state.pendings[reqId]
		if ok {
			delete(c.state.pendings, reqId)
			c.state.proposals[common.Txnid(txnid)] = handle
		}
	}
}

//
// Update the request upon new commit
//
func (c *Coordinator) updateRequestOnCommit(txnid common.Txnid) {

	c.state.mutex.Lock()
	defer c.state.mutex.Unlock()

	co.Debugf("Coorindator.updateRequestOnCommit(): recieve proposal. Txnid %d, coorindator fid %s",
		txnid, c.GetFollowerId())

	// If I can find the proposal based on the txnid in this host, this means
	// that this host originates the request.   Get the request handle and
	// notify the waiting goroutine that the request is done.
	handle, ok := c.state.proposals[txnid]

	if ok {
		delete(c.state.proposals, txnid)

		handle.CondVar.L.Lock()
		defer handle.CondVar.L.Unlock()
		handle.CondVar.Signal()
	}
}

/////////////////////////////////////////////////////////////////////////////
//  Environment
/////////////////////////////////////////////////////////////////////////////

func (c *Coordinator) getHostUDPAddr() string {
	return c.env.getHostUDPAddr()
}

func (c *Coordinator) getHostTCPAddr() string {
	return c.env.getHostTCPAddr()
}

func (c *Coordinator) findMatchingPeerTCPAddr(udpAddr string) string {
	return c.env.findMatchingPeerTCPAddr(udpAddr)
}

func (c *Coordinator) getPeerUDPAddr() []string {
	return c.env.getPeerUDPAddr()
}

/////////////////////////////////////////////////////////////////////////////
//  Metadata Operations
/////////////////////////////////////////////////////////////////////////////

//
// Handle create Index request in the dictionary.  If this function
// returns true, it means createIndex request completes successfully.
// If this function returns false, then the result is unknown.
//
func (c *Coordinator) createIndex(key string, content []byte) bool {

	defn, err := UnmarshallIndexDefn(content)
	if err != nil {
		return false
	}

	if err = c.repo.CreateIndex(defn); err != nil {
		co.Debugf("Coordinator.createIndex() : createIndex fails. Reason = %s", err.Error())
		return false
	}

	if err = c.addIndexToTopology(defn); err != nil {
		co.Debugf("Coordinator.createIndex() : setTopology fails. Reason = %s", err.Error())
		return false
	}

	return true
}

//
// Handle delete Index request in the dictionary.  If this function
// returns true, it means deleteIndex request completes successfully.
// If this function returns false, then the result is unknown.
//
func (c *Coordinator) deleteIndex(key string) bool {

	bucket := bucketFromIndexDefnName(key)
	name := nameFromIndexDefnName(key)
	co.Debugf("Coordinator.deleteIndex() : index to delete = %s, bucket = %s, name = %s", key)

	// Drop the index defnition before removing it from the topology.  If it fails to
	// remove the index defn from topology, it can mean that there is a dangling reference
	// in the topology with a deleted index defn, but it is easier to detect.
	if err := c.repo.DropIndexByName(bucket, name); err != nil {
		co.Debugf("Coordinator.deleteIndex() : deleteIndex fails. Reason = %s", err.Error())
		return false
	}

	if err := c.deleteIndexFromTopology(bucket, name); err != nil {
		co.Debugf("Coordinator.deleteIndex() : setTopology fails. Reason = %s", err.Error())
		return false
	}

	return true
}

//
// Add Index to Topology
//
func (c *Coordinator) addIndexToTopology(defn *co.IndexDefn) error {

	// get existing topology
	topology, err := c.repo.GetTopologyByBucket(defn.Bucket)
	if err != nil {
		// TODO: Need to check what type of error before creating a new topologyi
		topology = new(IndexTopology)
		topology.Bucket = defn.Bucket
		topology.Version = 0
	}

	// TODO: Get the host name from the indexDefn.
	host, err := c.findNextAvailNodeForIndex()
	if err != nil {
		return NewError(ERROR_MGR_DDL_CREATE_IDX, NORMAL, COORDINATOR, nil,
			fmt.Sprintf("Fail to find a host to store the index '%s'", defn.Name))
	}

	id := c.repo.GetNextIndexInstId()
	topology.AddIndexDefinition(defn.Bucket, defn.Name, uint64(defn.DefnId),
		uint64(id), uint32(co.INDEX_STATE_CREATED), host)

	// Add a reference of the bucket-level topology to the global topology.
	// If it fails later to create bucket-level topology, it will have
	// a dangling reference, but it is easier to discover this issue.  Otherwise,
	// we can end up having a bucket-level topology without being referenced.
	if err = c.addToGlobalTopologyIfNecessary(topology.Bucket); err != nil {
		return err
	}

	if err = c.repo.SetTopologyByBucket(topology.Bucket, topology); err != nil {
		return err
	}

	return nil
}

//
// Find next available node to host a new index for a particular bucket.
// This is purely only look at number of index definitons deployed for each
// node.  This does not take into account node availability as well as
// working set.  The node to deploy an index should really come from the
// DBA based on index runtime statistics.
//
func (c *Coordinator) findNextAvailNodeForIndex() (string, error) {

	// Get Global Topology
	globalTop, err := c.repo.GetGlobalTopology()
	if err != nil {
		// Use the local node
		return c.env.getLocalHost()
	}

	// initialize a map of indexCount per node
	indexCount := make(map[string]int)

	// Initialize the map with local index node
	host, err := c.env.getLocalHost()
	if err != nil {
		return "", err
	}
	indexCount[host] = 0

	// Intialize the map with peer index node
	hosts, err := c.env.getPeerHost()
	if err != nil {
		return "", err
	}
	for _, host := range hosts {
		indexCount[host] = 0
	}

	// Iterate through the topology for each bucket.  From the slice locator,
	// find out the node that host the index.   Increment the indexCount accordingly.
	for _, key := range globalTop.TopologyKeys {
		t, err := c.repo.GetTopologyByBucket(getBucketFromTopologyKey(key))
		if err != nil {
			return "", err
		}

		for _, defnRef := range t.Definitions {
			for _, inst := range defnRef.Instances {
				for _, partition := range inst.Partitions {
					singlePart := partition.SinglePartition
					for _, slice := range singlePart.Slices {
						count, ok := indexCount[slice.Host]
						if ok {
							indexCount[slice.Host] = count + 1
						}
					}
				}
			}
		}
	}

	// look for the host with the smallest count (least populated).
	minCount := math.MaxInt32
	chosenHost := ""
	for host, count := range indexCount {
		if count < minCount {
			minCount = count
			chosenHost = host
		}
	}

	return chosenHost, nil
}

//
// Delete Index from Topology
//
func (c *Coordinator) deleteIndexFromTopology(bucket string, name string) error {

	// get existing topology
	topology, err := c.repo.GetTopologyByBucket(bucket)
	if err != nil {
		return err
	}

	defn := topology.FindIndexDefinition(bucket, name)
	if defn != nil {
		topology.UpdateStateForIndexInstByDefn(co.IndexDefnId(defn.DefnId), co.INDEX_STATE_DELETED)
		if err = c.repo.SetTopologyByBucket(topology.Bucket, topology); err != nil {
			return err
		}
	}

	return nil
}

//
// Add a reference of the bucket-level index topology to global topology.
// If not exist, create a new one.
//
func (c *Coordinator) addToGlobalTopologyIfNecessary(bucket string) error {

	globalTop, err := c.repo.GetGlobalTopology()
	if err != nil {
		globalTop = new(GlobalTopology)
	}

	if globalTop.AddTopologyKeyIfNecessary(indexTopologyKey(bucket)) {
		return c.repo.SetGlobalTopology(globalTop)
	}

	return nil
}
