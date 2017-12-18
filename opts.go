package jrpc2

import (
	"context"
	"fmt"
	"io"
	"log"

	"golang.org/x/sync/semaphore"
)

const logFlags = log.LstdFlags | log.Lshortfile

// An ServerOption controls an optional behaviour of a Server.
type ServerOption func(*Server)

// ServerLog enables debug logging to the specified writer.
func ServerLog(w io.Writer) ServerOption {
	logger := log.New(w, "[jrpc2.Server] ", logFlags)
	return func(s *Server) {
		s.log = func(msg string, args ...interface{}) { logger.Output(2, fmt.Sprintf(msg, args...)) }
	}
}

// AllowV1 instructs the server whether to tolerate requests that do not
// include the required "jsonrpc" version marker.
func AllowV1(ok bool) ServerOption { return func(s *Server) { s.allow1 = ok } }

// Concurrency allows up to the specified number of concurrent goroutines to
// execute when processing requests. A value less than 1 is treated as 1, which
// is also the default if this option is not provided.
func Concurrency(n int) ServerOption {
	if n < 1 {
		n = 1
	}
	return func(s *Server) { s.sem = semaphore.NewWeighted(int64(n)) }
}

// ReqContext provides a function that the server will call to obtain a context
// value to use for each inbound request. By default the server uses background
// context.
func ReqContext(f func(*Request) context.Context) ServerOption {
	return func(s *Server) { s.reqctx = f }
}

// A ClientOption controls an optional behaviour of a Client.
type ClientOption func(*Client)

// ClientLog enables debug logging to the specified writer.
func ClientLog(w io.Writer) ClientOption {
	logger := log.New(w, "[jrpc2.Client] ", logFlags)
	return func(c *Client) {
		c.log = func(msg string, args ...interface{}) { logger.Output(2, fmt.Sprintf(msg, args...)) }
	}
}