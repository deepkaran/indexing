// admin server to handle admin and system messages.
//
// Example server {
//      reqch  := make(chan adminport.Request)
//      server := adminport.NewHTTPServer("projector", "localhost:9999", "/adminport", reqch)
//      server.Register(&protobuf.RequestMessage{})
//
//      loop:
//      for {
//          select {
//          case req, ok := <-reqch:
//              if ok {
//                  msg := req.GetMessage()
//                  // interpret request and compose a response
//                  respMsg := &protobuf.ResponseMessage{}
//                  err := msg.Send(respMsg)
//              } else {
//                  break loop
//              }
//          }
//      }
// }

// TODO: IMPORTANT:
//  Go 1.3 is supposed to have graceful shutdown of http server.
//  Refer https://code.google.com/p/go/issues/detail?id=4674

package adminport

import (
	"encoding/json"
	"fmt"
	c "github.com/couchbase/indexing/secondary/common"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

// httpServer is a concrete type implementing adminport Server interface.
type httpServer struct {
	mu        sync.Mutex   // handle concurrent updates to this object
	lis       net.Listener // TCP listener
	srv       *http.Server // http server
	urlPrefix string       // URL path prefix for adminport
	messages  map[string]MessageMarshaller
	reqch     chan<- Request // request channel back to application

	logPrefix string
	stats     *c.ComponentStat
}

// NewHTTPServer creates an instance of admin-server. Start() will actually
// start the server.
func NewHTTPServer(name, connAddr, urlPrefix string, reqch chan<- Request) Server {
	s := &httpServer{
		reqch:     reqch,
		messages:  make(map[string]MessageMarshaller),
		urlPrefix: urlPrefix,
		logPrefix: fmt.Sprintf("[%s:%s]", name, connAddr),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(s.urlPrefix, s.systemHandler)
	s.srv = &http.Server{
		Addr:           connAddr,
		Handler:        mux,
		ReadTimeout:    c.AdminportReadTimeout * time.Millisecond,
		WriteTimeout:   c.AdminportWriteTimeout * time.Millisecond,
		MaxHeaderBytes: 1 << 20,
	}
	return s
}

// Register is part of Server interface.
func (s *httpServer) Register(msg MessageMarshaller) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lis != nil {
		return ErrorRegisteringRequest
	}
	key := fmt.Sprintf("%v%v", s.urlPrefix, msg.Name())
	s.messages[key] = msg
	c.Infof("%s registered %s\n", s.logPrefix, s.getURL(msg))
	return
}

// Unregister is part of Server interface.
func (s *httpServer) Unregister(msg MessageMarshaller) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lis != nil {
		return ErrorRegisteringRequest
	}
	name := msg.Name()
	if s.messages[name] == nil {
		return ErrorMessageUnknown
	}
	delete(s.messages, name)
	c.Infof("%s unregistered %s\n", s.logPrefix, s.getURL(msg))
	return
}

// Start is part of Server interface.
func (s *httpServer) Start() (err error) {
	s.stats = s.newStats() // initialize statistics

	if s.lis, err = net.Listen("tcp", s.srv.Addr); err != nil {
		return err
	}

	// Server routine
	go func() {
		defer s.shutdown()

		c.Infof("%s starting ...\n", s.logPrefix)
		err := s.srv.Serve(s.lis) // serve until listener is closed.
		if err != nil {
			c.Errorf("%s %v\n", s.logPrefix, err)
		}
	}()
	return
}

func (s *httpServer) GetStatistics() *c.ComponentStat {
	return s.stats
}

// Stop is part of Server interface.
func (s *httpServer) Stop() {
	s.shutdown()
	c.Infof("%s ... stopped\n", s.logPrefix)
}

func (s *httpServer) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lis != nil {
		s.lis.Close()
		close(s.reqch)
		s.lis = nil
	}
}

func (s *httpServer) getURL(msg MessageMarshaller) string {
	return s.urlPrefix + msg.Name()
}

// handle incoming request.
func (s *httpServer) systemHandler(w http.ResponseWriter, r *http.Request) {
	var err error
	var statPath string

	c.Infof("%s Request %q\n", s.logPrefix, r.URL.Path)

	// Fault-tolerance. No need to crash the server in case of panic.
	defer func() {
		if r := recover(); r != nil {
			c.Errorf("%s, adminport.request.recovered `%v`\n", s.logPrefix, r)
			s.stats.Incrs(statPath, 0, 0, 1) // count error
		} else if err != nil {
			c.Errorf("%s %v\n", s.logPrefix, err)
			s.stats.Incrs(statPath, 0, 1, 1) // count response&error
		} else {
			s.stats.Incrs(statPath, 0, 1, 0) // count response
		}
	}()

	var msg MessageMarshaller

	// check wether it is for stats.
	prefix := c.StatsURLPath(s.urlPrefix, "")
	if strings.HasPrefix(r.URL.Path, prefix) {
		msg = &c.ComponentStat{}
	} else {
		msg = s.messages[r.URL.Path]
		data := make([]byte, r.ContentLength, r.ContentLength)
		r.Body.Read(data)
		// Get an instance of request type and decode request into that.
		typeOfMsg := reflect.ValueOf(msg).Elem().Type()
		msg = reflect.New(typeOfMsg).Interface().(MessageMarshaller)
		if err = msg.Decode(data); err != nil {
			err = fmt.Errorf("%v %v", ErrorDecodeRequest, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	statPath = "/request." + msg.Name()
	s.stats.Incrs(statPath, 1, 0, 0) // count request

	if msg == nil {
		err = ErrorPathNotFound
		http.Error(w, "path not found", http.StatusNotFound)
		return
	}

	waitch := make(chan interface{}, 1)
	// send and wait
	s.reqch <- &httpAdminRequest{srv: s, msg: msg, waitch: waitch}
	val := <-waitch

	switch v := (val).(type) {
	case *c.ComponentStat:
		val = v.Get(c.ParseStatsPath(r.URL.Path))
		if data, err := json.Marshal(&val); err == nil {
			header := w.Header()
			// TODO: no magic
			header["Content-Type"] = []string{"application/json"}
			w.Write(data)
			s.stats.Incrs("/payload", 0, len(data))
		} else {
			err = fmt.Errorf("%v %v", ErrorDecodeRequest, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			c.Errorf("%v %v", s.logPrefix, err)
		}

	case MessageMarshaller:
		if data, err := v.Encode(); err == nil {
			header := w.Header()
			header["Content-Type"] = []string{v.ContentType()}
			w.Write(data)
			s.stats.Incrs("/payload", 0, len(data))
		} else {
			err = fmt.Errorf("%v %v", ErrorDecodeRequest, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			c.Errorf("%v %v", s.logPrefix, err)
		}

	case error:
		http.Error(w, v.Error(), http.StatusInternalServerError)
		err = fmt.Errorf("%v %v", ErrorInternal, v)
		c.Errorf("%v %v", s.logPrefix, err)
	}
}

// concrete type implementing Request interface
type httpAdminRequest struct {
	srv    *httpServer
	msg    MessageMarshaller
	waitch chan interface{}
}

// GetMessage is part of Request interface.
func (r *httpAdminRequest) GetMessage() MessageMarshaller {
	return r.msg
}

// Send is part of Request interface.
func (r *httpAdminRequest) Send(msg MessageMarshaller) error {
	r.waitch <- msg
	close(r.waitch)
	return nil
}

// SendError is part of Request interface.
func (r *httpAdminRequest) SendError(err error) error {
	r.waitch <- err
	close(r.waitch)
	return nil
}
