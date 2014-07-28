// A gen server behavior for dataport consumer.
//
// Daemon listens for new connections and spawns a reader routine
// for each new connection.
//
// concurrency model:
//
//                                   Application back channels,
//                               mutation channel and sideband channel
//                          -----------------------------------------------
//                                ^                      ^
//                             (sideband)            (mutation)
//     NewServer() ----------*    |                      |
//             |             |    |                      | []*VbKeyVersions
//          (spawn)          |    | []*ShutdownDataport  |
//             |             |    | []*RestartVbuckets   |
//             |          (spawn) | error                |
//           listener()      |    |                      |
//                 |         |    |                      |
//  serverCmdNewConnection   |    |                      |
//                 |         |    |                      |
//  Close() -------*------->gen-server()------*---- doReceive()----*
//          serverCmdClose        ^           |                    |
//                                |           *---- doReceive()----*
//                serverCmdVbmap  |           |                    |
//            serverCmdVbcontrol  |           *---- doReceive()----*
//                serverCmdError  |                                |
//                                *--------------------------------*
//                                          (control & faults)
//
// server behavior:
//
// 1. can handle more than one connection from same router.
// 2. whenever a connection with router fails, all active connections with
//    that router will be closed and the same will be intimated to
//    application for catchup connection, using []*RestartVbuckets message.
// 3. side band information to application,
//    a. []*RestartVbuckets, connection with an upstream host is closed.
//    b. []*ShutdownDataport, all connections with an upstream host is closed.
//    c. error string, describing the cause of the error.
//    in case of b and c, all connections with all upstream host will be
//    closed and dataport-instance will be shutdown.
// 4. when ever dataport-instance shuts down it will signal application by
//    side-band channel.

package dataport

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	c "github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/protobuf"
)

// Error codes

// ErrorPayload
var ErrorPayload = errors.New("dataport.daemonPayload")

// ErrorVbmap
var ErrorVbmap = errors.New("dataport.vbmap")

type bucketVbno struct {
	bucket string
	vbno   uint16
}

// Side band information

// RestartVbuckets to restart a subset of vuckets.
type RestartVbuckets struct {
	Bucket   string
	Vbuckets []uint16
}

// ShutdownDataport tells daemon has shutdown, provides vbuckets to restart.
type ShutdownDataport struct {
	Bucket   string
	Vbuckets []uint16
}

// messages to gen-server
type serverMessage struct {
	cmd   byte          // gen server command
	raddr string        // routine for remote connection, optional
	args  []interface{} // command arguments
	err   error         // in case the message is to notify error
}

// maintain information about each remote connection.
type netConn struct {
	conn   net.Conn
	worker chan interface{}
	active bool
}

// Server handles an active dataport server of mutation for all vbuckets.
type Server struct {
	laddr string // address to listen
	lis   net.Listener
	mutch chan<- []*protobuf.VbKeyVersions // backchannel to application
	sbch  chan<- interface{}               // sideband channel to application

	// gen-server management
	reqch     chan []interface{}
	finch     chan bool
	conns     map[string]netConn // resolve <host:port> to conn. obj
	logPrefix string
}

// NewServer creates a new dataport daemon.
func NewServer(
	laddr string,
	mutch chan []*protobuf.VbKeyVersions,
	sbch chan<- interface{}) (s *Server, err error) {

	s = &Server{
		laddr: laddr,
		mutch: mutch,
		sbch:  sbch,

		// Managing vbuckets and connections for all routers
		reqch:     make(chan []interface{}, c.GenserverChannelSize),
		finch:     make(chan bool),
		conns:     make(map[string]netConn),
		logPrefix: fmt.Sprintf("[dataport %q]", laddr),
	}
	if s.lis, err = net.Listen("tcp", laddr); err != nil {
		return nil, err
	}
	go listener(laddr, s.lis, s.reqch) // spawn daemon
	go s.genServer(s.reqch, sbch)      // spawn gen-server
	c.Infof("%v started ...", s.logPrefix)
	return s, nil
}

func (s *Server) addUuids(started, uuids []*bucketVbno) ([]*bucketVbno, error) {
	for _, adduuid := range started {
		for _, uuid := range uuids {
			if uuid.bucket == adduuid.bucket && uuid.vbno == adduuid.vbno {
				c.Errorf("%v duplicate vbucket %v\n", s.logPrefix, uuid)
				return nil, ErrorDuplicateStreamBegin
			}
		}
		c.Infof("%v added vbucket %v\n", s.logPrefix, adduuid)
	}
	uuids = append(uuids, started...)
	return uuids, nil
}

func (s *Server) delUuids(finished, uuids []*bucketVbno) ([]*bucketVbno, error) {
	newUuids := make([]*bucketVbno, 0, len(uuids))
	for _, uuid := range uuids {
		for _, finuuid := range finished {
			if uuid.bucket == finuuid.bucket && uuid.vbno == finuuid.vbno {
				uuid = nil
				break
			}
		}
		if uuid != nil {
			newUuids = append(newUuids, uuid)
		}
	}
	return newUuids, nil
}

// Close the daemon listening for new connections and shuts down all read
// routines for this dataport server. synchronous call.
func (s *Server) Close() (err error) {
	respch := make(chan []interface{}, 1)
	cmd := []interface{}{serverMessage{cmd: serverCmdClose}, respch}
	resp, err := c.FailsafeOp(s.reqch, respch, cmd, s.finch)
	return c.OpError(err, resp, 0)
}

// gen-server commands
const (
	serverCmdNewConnection byte = iota + 1
	serverCmdVbmap
	serverCmdVbcontrol
	serverCmdError
	serverCmdClose
)

// gen server routine for dataport server.
func (s *Server) genServer(reqch chan []interface{}, sbch chan<- interface{}) {
	var appmsg interface{}

	defer func() { // panic safe
		if r := recover(); r != nil {
			msg := serverMessage{cmd: serverCmdClose}
			s.handleClose(msg)
			c.Errorf("%v gen-server fatal panic: %v\n", s.logPrefix, r)
		}
	}()

	remoteUuids := make(map[string][]*bucketVbno) // indexed by `raddr`
loop:
	for {
		appmsg = nil
		select {
		case cmd := <-reqch:
			msg := cmd[0].(serverMessage)
			switch msg.cmd {
			case serverCmdNewConnection:
				var err error
				conn := msg.args[0].(net.Conn)
				if appmsg, err = s.handleNewConnection(msg); err != nil {
					conn.Close()
				} else {
					// TODO: avoid magic numbers.
					remoteUuids[msg.raddr] = make([]*bucketVbno, 0, 4)
					s.startWorker(msg.raddr)
				}

			case serverCmdVbmap:
				vbmap := msg.args[0].(*protobuf.VbConnectionMap)
				b, raddr := vbmap.GetBucket(), msg.raddr
				for _, vbno := range vbmap.GetVbuckets() {
					remoteUuids[raddr] =
						append(remoteUuids[raddr], &bucketVbno{b, uint16(vbno)})
				}
				s.startWorker(msg.raddr)

			case serverCmdVbcontrol:
				var err error
				started := msg.args[0].([]*bucketVbno)
				finished := msg.args[1].([]*bucketVbno)
				uuids := remoteUuids[msg.raddr]
				if len(started) > 0 { // new vbucket stream(s) have started
					if uuids, err = s.addUuids(started, uuids); err != nil {
						panic(err)
					}
				}
				if len(finished) > 0 { // vbucket stream(s) have finished
					if uuids, err = s.delUuids(finished, uuids); err != nil {
						panic(err)
					}
				}
				remoteUuids[msg.raddr] = uuids
				s.startWorker(msg.raddr)

			case serverCmdClose:
				respch := cmd[1].(chan []interface{})
				appmsg = s.handleClose(msg)
				respch <- []interface{}{nil}
				break loop

			case serverCmdError:
				raddr, err := msg.raddr, msg.err
				appmsg = s.jumboErrorHandler(raddr, remoteUuids, err)
			}
			if appmsg != nil {
				s.sbch <- appmsg
				c.Infof("appmsg: %T:%+v\n", appmsg, appmsg)
			}
		}
	}
}

/**** gen-server handlers ****/

// handle new connection
func (s *Server) handleNewConnection(msg serverMessage) (interface{}, error) {
	conn := msg.args[0].(net.Conn)
	c.Infof("%v connection request from %q\n", s.logPrefix, msg.raddr)
	if _, _, err := net.SplitHostPort(msg.raddr); err != nil {
		c.Errorf("%v unable to parse %q\n", s.logPrefix, msg.raddr)
		return nil, err
	}
	if _, ok := s.conns[msg.raddr]; ok {
		c.Errorf("%v %q already active\n", s.logPrefix, msg.raddr)
		return nil, ErrorDuplicateClient
	}
	// connection accepted
	worker := make(chan interface{}, 10) // TODO: avoid magic numbers
	s.conns[msg.raddr] = netConn{conn: conn, worker: worker, active: false}
	c.Infof("%v total active connections %v\n", s.logPrefix, len(s.conns))
	return nil, nil
}

// shutdown this gen server and all its routines.
func (s *Server) handleClose(msg serverMessage) (appmsg interface{}) {
	defer func() {
		if r := recover(); r != nil {
			c.Errorf("%v handleClose fatal panic: %v\n", s.logPrefix, r)
		}
	}()

	c.Infof("%v shutting down\n", s.logPrefix)
	s.lis.Close()                          // close listener daemon
	closeConnections(s.logPrefix, s.conns) // close workers
	s.lis, s.conns = nil, nil
	close(s.finch)
	return
}

// start a connection worker to read mutation message for a subset of vbuckets.
func (s *Server) startWorker(raddr string) {
	c.Infof("%v starting worker for connection %q\n", s.logPrefix, raddr)
	nc := s.conns[raddr]
	go doReceive(s.logPrefix, nc, s.mutch, s.reqch)
	nc.active = true
}

// jumbo size error handler, it either closes all connections and shutdown the
// server or it closes all open connections with faulting remote-host and
// returns back a message for application.
func (s *Server) jumboErrorHandler(
	raddr string,
	remoteUuids map[string][]*bucketVbno,
	err error) (msg interface{}) {

	var whatJumbo string

	// connection is already gone. TODO: make the following error message as
	// fatal.
	if _, ok := s.conns[raddr]; ok == false {
		c.Errorf("%v fatal remote %q already gone\n", s.logPrefix, raddr)
		return nil
	}

	if err == io.EOF {
		c.Errorf("%v remote %q closed\n", s.logPrefix, raddr)
		whatJumbo = "closeremote"
	} else if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
		c.Errorf("%v remote %q timedout\n", s.logPrefix, raddr)
		whatJumbo = "closeremote"
	} else if err != nil {
		c.Errorf("%v `%v` from %q\n", s.logPrefix, err, raddr)
		whatJumbo = "closeall"
	} else {
		c.Errorf("%v no error why did you call jumbo !!!\n", s.logPrefix)
		return
	}

	switch whatJumbo {
	case "closeremote":
		var closed []string
		buckets := make(map[string]*RestartVbuckets)
		closed, s.conns = closeRemoteHost(s.logPrefix, raddr, s.conns)
		for _, raddr := range closed {
			msg = vbucketsForRemote(remoteUuids[raddr], buckets)
		}

	case "closeall":
		msg = vbucketsForRemotes(remoteUuids)
		go s.Close()
	}
	return
}

// close all connections and worker routines
func closeConnections(prefix string, conns map[string]netConn) {
	var closed []string

	raddrs := make([]string, 0, len(conns))
	for raddr := range conns {
		raddrs = append(raddrs, raddr)
	}
	for len(raddrs) > 0 {
		closed, conns = closeRemoteHost(prefix, raddrs[0], conns)
		raddrs = c.ExcludeStrings(raddrs, closed)
	}
}

// close all connections with remote host.
func closeRemoteHost(prefix string, raddr string, conns map[string]netConn) ([]string, map[string]netConn) {
	recoverClose := func(conn net.Conn, craddr string) {
		defer func() {
			if r := recover(); r != nil {
				c.Errorf("%v panic closing connection %q", prefix, craddr)
			}
			conn.Close()
		}()
	}

	clientRaddrs := make([]string, 0, len(conns))
	if _, ok := conns[raddr]; ok {
		host, _, _ := net.SplitHostPort(raddr)
		// close all connections and worker routines for this remote host
		for _, craddr := range remoteConnections(host, conns) {
			nc := conns[craddr]
			recoverClose(nc.conn, craddr)
			close(nc.worker)
			delete(conns, craddr)
			clientRaddrs = append(clientRaddrs, craddr)
			c.Infof("%v closed connection %q\n", prefix, craddr)
		}
	} else {
		c.Errorf("%v fatal unknown connection %q\n", prefix, raddr)
	}
	return clientRaddrs, conns
}

// get all remote connections for `host`
func remoteConnections(host string, conns map[string]netConn) []string {
	raddrs := make([]string, 0)
	for s := range conns {
		if h, _, _ := net.SplitHostPort(s); h == host {
			raddrs = append(raddrs, s)
		}
	}
	return raddrs
}

// gather vbuckets to restart for all remote connections
func vbucketsForRemotes(remotes map[string][]*bucketVbno) []*ShutdownDataport {
	var rs []*RestartVbuckets
	buckets := make(map[string]*RestartVbuckets)
	for _, uuids := range remotes {
		rs = vbucketsForRemote(uuids, buckets)
	}
	ss := make([]*ShutdownDataport, 0, len(rs))
	for _, r := range rs {
		s := &ShutdownDataport{Bucket: r.Bucket, Vbuckets: r.Vbuckets}
		ss = append(ss, s)
	}
	return ss
}

// gather vbuckets to restart for a single remote-connection.
func vbucketsForRemote(uuids []*bucketVbno, buckets map[string]*RestartVbuckets) []*RestartVbuckets {
	for _, uuid := range uuids { // ids are from to remote-connection
		r, ok := buckets[uuid.bucket]
		if !ok {
			r = &RestartVbuckets{
				Bucket:   uuid.bucket,
				Vbuckets: make([]uint16, 0, 4), // TODO: avoid magic numbers
			}
			buckets[uuid.bucket] = r
		}
		r.Vbuckets = append(r.Vbuckets, uuid.vbno)
	}
	rs := make([]*RestartVbuckets, 0, 10) // TODO: avoid magic numbers
	for _, r := range buckets {
		rs = append(rs, r)
	}
	return rs
}

// go-routine to listen for new connections, if this routine goes down -
// server is shutdown and reason notified back to application.
func listener(laddr string, lis net.Listener, reqch chan []interface{}) {
	defer func() {
		c.Errorf("%v listener fatal panic: %v", laddr, recover())
		msg := serverMessage{cmd: serverCmdError, err: ErrorDaemonExit}
		reqch <- []interface{}{msg}
	}()
	for {
		// TODO: handle `err` for lis.Close() and avoid panic(err)
		if conn, err := lis.Accept(); err != nil {
			panic(err)
		} else {
			msg := serverMessage{
				cmd:   serverCmdNewConnection,
				raddr: conn.RemoteAddr().String(),
				args:  []interface{}{conn},
			}
			reqch <- []interface{}{msg}
		}
	}
}

// per connection go-routine to read []*VbKeyVersions.
func doReceive(prefix string, nc netConn, mutch chan<- []*protobuf.VbKeyVersions, reqch chan<- []interface{}) {
	conn, worker := nc.conn, nc.worker
	flags := TransportFlag(0).SetProtobuf() // TODO: make it configurable
	pkt := NewTransportPacket(c.MaxDataportPayload, flags)
	msg := serverMessage{raddr: conn.RemoteAddr().String()}

	started := make([]*bucketVbno, 0, 4)  // TODO: avoid magic numbers
	finished := make([]*bucketVbno, 0, 4) // TODO: avoid magic numbers

	// detect StreamBegin and StreamEnd messages.
	// TODO: function uses 2 level of loops, figure out a smart way to identify
	//       presence of StreamBegin/StreamEnd so that we can avoid looping.
	updateActiveVbuckets := func(vbs []*protobuf.VbKeyVersions) {
		for _, vb := range vbs {
			bucket, vbno := vb.GetBucketname(), uint16(vb.GetVbucket())
			kvs := vb.GetKvs()

			commands := make([]uint32, 0, len(kvs))
			seqnos := make([]uint64, 0, len(kvs))

			// TODO: optimize this.
			for _, kv := range kvs {
				seqnos = append(seqnos, kv.GetSeqno())
				commands = append(commands, kv.GetCommands()...)
				commands = append(commands, 17)

				if byte(kv.GetCommands()[0]) == c.StreamBegin {
					started = append(started, &bucketVbno{bucket, vbno})
				} else if byte(kv.GetCommands()[0]) == c.StreamEnd {
					finished = append(finished, &bucketVbno{bucket, vbno})
				}
			}
			c.Tracef("%v {%v, %v}\n", prefix, bucket, vbno)
		}
	}

loop:
	for {
		timeoutMs := c.DataportReadDeadline * time.Millisecond
		conn.SetReadDeadline(time.Now().Add(timeoutMs))
		msg.cmd, msg.err, msg.args = 0, nil, nil
		if payload, err := pkt.Receive(conn); err != nil {
			msg.cmd, msg.err = serverCmdError, err
			reqch <- []interface{}{msg}
			c.Errorf("%v worker %q exited %v\n", prefix, msg.raddr, err)
			break loop

		} else if vbmap, ok := payload.(*protobuf.VbConnectionMap); ok {
			msg.cmd, msg.args = serverCmdVbmap, []interface{}{vbmap}
			reqch <- []interface{}{msg}
			c.Infof(
				"%v worker %q exiting with `serverCmdVbmap`\n",
				prefix, msg.raddr)
			break loop

		} else if vbs, ok := payload.([]*protobuf.VbKeyVersions); ok {
			updateActiveVbuckets(vbs)
			select {
			case mutch <- vbs:
				if len(started) > 0 || len(finished) > 0 {
					msg.cmd = serverCmdVbcontrol
					msg.args = []interface{}{started, finished}
					reqch <- []interface{}{msg}
					c.Infof(
						"%v worker %q exiting with `serverCmdVbcontrol` %v\n",
						prefix, msg.raddr, len(started))
					break loop
				}

			case <-worker:
				msg.cmd, msg.err = serverCmdError, ErrorWorkerKilled
				reqch <- []interface{}{msg}
				c.Errorf("%v worker %q exited %v\n", prefix, msg.raddr, err)
				break loop
			}

		} else {
			msg.cmd, msg.err = serverCmdError, ErrorPayload
			reqch <- []interface{}{msg}
			c.Errorf("%v worker %q exited %v\n", prefix, msg.raddr, err)
			break loop
		}
	}
	nc.active = false
}

func vbucketSchedule(vb *protobuf.VbKeyVersions) (s, e *protobuf.KeyVersions) {
	for _, kv := range vb.GetKvs() {
		commands := kv.GetCommands()
		if len(commands) == 1 {
			switch byte(commands[0]) {
			case c.StreamBegin:
				s, e = kv, nil
			case c.StreamEnd:
				s, e = nil, kv
			}
		}
	}
	return s, e
}