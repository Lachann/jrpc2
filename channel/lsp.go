package channel

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// NewLSP constructs a jrpc2.Channel that transmits and receives messages on
// rwc using the Language Server Protocol (LSP) framing, defined by the LSP
// specification at http://github.com/Microsoft/language-server-protocol.
func NewLSP(rwc io.ReadWriteCloser) *LSP { return &LSP{rwc: rwc, rd: bufio.NewReader(rwc)} }

// LSP implements jrpc2.Channel. Messages sent on a LSP channel are framed as a
// header/body transaction, similar to HTTP.
//
//    Content-Length: n<CRLF>
//    ...other headers...
//    <CRLF>
//    <n-byte message>
//
type LSP struct {
	rwc io.ReadWriteCloser
	rd  *bufio.Reader
	buf []byte
}

// Send implements part of jrpc2.Channel.
func (c *LSP) Send(msg []byte) error {
	if _, err := fmt.Fprintf(c.rwc, "Content-Length: %d\r\n\r\n", len(msg)); err != nil {
		return err
	}
	_, err := c.rwc.Write(msg)
	return err
}

// Recv implements part of jrpc2.Channel.
func (c *LSP) Recv() ([]byte, error) {
	h := make(map[string]string)
	for {
		raw, err := c.rd.ReadString('\n')
		line := strings.TrimRight(raw, "\r\n")
		if line != "" {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				h[strings.ToLower(parts[0])] = strings.TrimSpace(parts[1])
			} else {
				return nil, errors.New("invalid header line")
			}
		}
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		} else if line == "" {
			break
		}
	}

	// Parse out the required content-length field.  This implementation
	// ignores unknown header fields.
	clen, ok := h["content-length"]
	if !ok {
		return nil, errors.New("missing required content-length")
	}
	size, err := strconv.Atoi(clen)
	if err != nil {
		return nil, fmt.Errorf("invalid content-length: %v", err)
	} else if size < 0 {
		return nil, errors.New("negative content-length")
	}

	// We need to use ReadFull here because the buffered reader may not have a
	// big enough buffer to deliver the whole message, and will only issue a
	// single read to the underlying source.
	data := make([]byte, size)
	if _, err := io.ReadFull(c.rd, data); err != nil {
		return nil, err
	}
	return data, nil
}

// Close implements part of jrpc2.Channel.
func (c *LSP) Close() error { return c.rwc.Close() }
