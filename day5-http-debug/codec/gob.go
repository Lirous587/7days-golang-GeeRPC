package codec

import (
	"bufio"
	"encoding/gob"
	"io"
	"log"
)

type GobCodec struct {
	conn io.ReadWriteCloser
	buf  *bufio.Writer
	dec  *gob.Decoder
	enc  *gob.Encoder
}

// Assert that the *GobCodec type implements the Codec interface.
// If it does not implement it,a compilation error will occur.
// This is a very good tip when developing a library.
var _ Codec = (*GobCodec)(nil)

func NewGobCodec(conn io.ReadWriteCloser) Codec {
	buf := bufio.NewWriter(conn)
	return &GobCodec{
		conn: conn,
		buf:  buf,
		dec:  gob.NewDecoder(conn),
		enc:  gob.NewEncoder(buf),
	}
}

func (c *GobCodec) ReadHeader(h *Header) error {
	// In the source code of Decode,it's clear that h must be used as a pointer and its value will be changed.
	return c.dec.Decode(h)
}

func (c *GobCodec) ReadBody(body interface{}) error {
	// Same as the previous one.
	return c.dec.Decode(body)
}

func (c *GobCodec) Write(h *Header, body interface{}) (err error) {
	defer func() {
		_ = c.buf.Flush()
		if err != nil {
			_ = c.Close()
		}
	}()

	if err := c.enc.Encode(h); err != nil {
		log.Println("rpc codec: gob error encoding header: ", err)
	}

	if err := c.enc.Encode(body); err != nil {
		log.Println("rpc codec: gob error encoding body: ", err)
	}

	return nil
}

func (c *GobCodec) Close() error {
	return c.conn.Close()
}
