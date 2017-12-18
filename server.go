package jrpc2

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"bitbucket.org/creachadair/stringset"
	"bitbucket.org/creachadair/taskgroup"
	"golang.org/x/sync/semaphore"
)

// A Conn represents the ability to transmit JSON-RPC messages.
type Conn interface {
	io.Reader
	io.Writer
	io.Closer
}

// A Server is a JSON-RPC 2.0 server. The server receives requests and sends
// responses on a Conn provided by the caller, and dispatches requests to
// user-defined Method handlers.
type Server struct {
	wg     sync.WaitGroup               // ready when workers are done at shutdown time
	mux    Assigner                     // associates method names with handlers
	sem    *semaphore.Weighted          // bounds concurrent execution (default 1)
	allow1 bool                         // allow v1 requests with no version marker
	log    func(string, ...interface{}) // write debug logs here

	reqctx func(req *Request) context.Context // obtain a context for req

	mu     *sync.Mutex   // protects the fields below
	closer io.Closer     // close to terminate the connection
	err    error         // error from a previous operation
	work   *sync.Cond    // for signaling message availability
	inq    *list.List    // inbound requests awaiting processing
	outq   *json.Encoder // encoder for outbound replies

	used stringset.Set // IDs of requests being processed
}

// NewServer returns a new unstarted server that will dispatch incoming
// JSON-RPC requests according to mux. To start serving, call Start.  It is
// safe to modify mux after the server has been started if mux itself is safe
// for concurrent use by multiple goroutines.
//
// This function will panic if mux == nil.
func NewServer(mux Assigner, opts ...ServerOption) *Server {
	if mux == nil {
		panic("nil assigner")
	}
	s := &Server{
		mux:    mux,
		sem:    semaphore.NewWeighted(1),
		log:    func(string, ...interface{}) {},
		reqctx: func(*Request) context.Context { return context.Background() },
		mu:     new(sync.Mutex),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start enables processing of requests from conn.
func (s *Server) Start(conn Conn) (*Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closer != nil {
		return s, errors.New("T is already started")
	}

	// Set up the queues and condition variable used by the workers.
	s.closer = conn
	s.work = sync.NewCond(s.mu)
	s.inq = list.New()
	s.used = stringset.New()

	// Reset all the I/O structures and start up the workers.
	s.err = nil
	s.outq = json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	dec.UseNumber()
	// TODO(fromberger): Disallow extra fields once 1.10 lands.

	g := taskgroup.New(nil)
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.read(dec) }()
	go func() {
		defer s.wg.Done()
		for {
			next, err := s.nextRequest()
			if err != nil {
				s.log("Reading next request: %v", err)
				return
			}
			g.Go(next)
		}
	}()
	return s, nil
}

// nextRequest blocks until a request batch is available and returns a function
// dispatches it to the appropriate handlers. The result is only an error if
// the connection failed; errors reported by the handler are reported to the
// caller and not returned here.
//
// The caller must invoke the returned function to complete the request.
func (s *Server) nextRequest() (func() error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.closer != nil && s.inq.Len() == 0 {
		s.work.Wait()
	}
	if s.closer == nil {
		return nil, s.err
	}

	next := s.inq.Remove(s.inq.Front()).(jrequests)
	s.log("Processing %d requests", len(next))

	// Resolve all the task handlers or record errors.
	var tasks tasks
	for _, req := range next {
		s.log("Checking request for %q: %s", req.M, string(req.P))
		t := &task{req: req}
		if !s.versionOK(req.V) {
			t.err = Errorf(E_InvalidRequest, "incorrect version marker %q", req.V)
		} else if id := string(req.ID); id != "" && !s.used.Add(id) {
			t.err = Errorf(E_InvalidRequest, "duplicate request id %q", id)
		} else if req.M == "" {
			t.err = Errorf(E_InvalidRequest, "empty method name")
		} else if m := s.mux.Assign(req.M); m == nil {
			t.err = Errorf(E_MethodNotFound, "no such method %q", req.M)
		} else {
			t.m = m
		}
		tasks = append(tasks, t)
	}

	// Invoke the handlers outside the lock.
	return func() error {
		g := taskgroup.New(nil)
		for _, t := range tasks {
			if t.err != nil {
				continue // nothing to do here; this was a bogus one
			}
			t := t
			g.Go(func() error {
				s.sem.Acquire(context.Background(), 1)
				defer s.sem.Release(1)
				t.val, t.err = s.dispatch(t.m, &Request{
					id:     t.req.ID,
					method: t.req.M,
					params: json.RawMessage(t.req.P),
				})
				return nil
			})
		}
		g.Wait()
		rsps := tasks.responses()
		s.log("Completed %d responses", len(rsps))

		// Deliver any responses (or errors) we owe.
		if len(rsps) != 0 {
			s.log("Sending response: %v", rsps)
			return s.send(rsps)
		}
		return nil
	}, nil
}

// dispatch invokes m for the specified request type, and marshals the return
// value into JSON if there is one.
func (s *Server) dispatch(m Method, req *Request) (json.RawMessage, error) {
	v, err := m.Call(s.reqctx(req), req)
	if err != nil {
		if req.id == nil {
			s.log("Discarding error from notification to %q: %v", req.Method(), err)
			return nil, nil // a notification
		}
		return nil, err // a call reporting an error
	}
	return json.Marshal(v)
}

// Stop shuts down the server. It is safe to call this method multiple times or
// from concurrent goroutines; it will only take effect once.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop(ErrServerStopped)
}

// Wait blocks until the connection terminates and returns the resulting error.
func (s *Server) Wait() error {
	s.wg.Wait()
	s.work = nil
	s.used = nil
	return s.err
}

// stop shuts down the connection and records err as its final state.  The
// caller must hold s.mu. If multiple callers invoke stop, only the first will
// successfully record its error status.
func (s *Server) stop(err error) {
	if s.closer == nil {
		return // nothing is running
	}
	s.closer.Close()
	s.work.Broadcast()
	s.err = err
	s.closer = nil
}

func isRecoverableJSONError(err error) bool {
	switch err.(type) {
	case *json.UnmarshalTypeError, *json.UnsupportedTypeError:
		// Do not include syntax errors, as the decoder will not generally
		// recover from these without more serious help.
		return true
	default:
		return false
	}
}

func (s *Server) read(dec *json.Decoder) {
	for {
		// If the message is not sensible, report an error; otherwise enqueue
		// it for processing.
		var in jrequests
		err := dec.Decode(&in)
		s.mu.Lock()
		if isRecoverableJSONError(err) {
			s.pushError(nil, jerrorf(E_ParseError, "invalid JSON request message"))
		} else if err != nil {
			s.stop(err)
			break
		} else if len(in) == 0 {
			s.pushError(nil, jerrorf(E_InvalidRequest, "empty request batch"))
		} else {
			s.log("Received %d new requests", len(in))
			s.inq.PushBack(in)
			s.work.Broadcast()
		}
		s.mu.Unlock()
	}
	s.inq = nil
	s.mu.Unlock()
}

func (s *Server) pushError(id json.RawMessage, err *jerror) {
	s.log("Error for request %q: %v", string(id), err)
	if err := s.outq.Encode(jresponses{{
		V:  Version,
		ID: id,
		E:  err,
	}}); err != nil {
		s.log("Writing error response: %v", err)
	}
}

// send enqueues a request or a response for delivery. The caller must hold s.mu.
func (s *Server) send(msg jresponses) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outq.Encode(msg)
}

func (s *Server) versionOK(v string) bool {
	if v == "" {
		return s.allow1 // an empty version is OK if the server allows it
	}
	return v == Version // ... otherwise it must match the spec
}

type task struct {
	m   Method
	req *jrequest
	val json.RawMessage
	err error
}

type tasks []*task

func (ts tasks) responses() jresponses {
	var rsps jresponses
	for _, task := range ts {
		if task.req.ID == nil && task.err == nil {
			continue // non-error-causing notifications do not get responses
		}
		rsp := &jresponse{V: Version, ID: task.req.ID}
		if task.err == nil {
			rsp.R = task.val
		} else if e, ok := task.err.(*Error); ok {
			rsp.E = e.tojerror()
		} else if code, ok := task.err.(Code); ok {
			rsp.E = jerrorf(code, code.Error())
		} else {
			rsp.E = jerrorf(E_InternalError, "internal error: %v", task.err)
		}
		rsps = append(rsps, rsp)
	}
	return rsps
}