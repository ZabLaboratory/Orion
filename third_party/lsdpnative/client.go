// Package lsdpnative is a generic, persistent native-wire language binding.
// It carries operator types and LSML as data; it has no application-specific logic.
package lsdpnative

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const wire = "LSDP-TCP/2.0-draft2"

type frame struct {
	kind    byte
	channel uint32
	id      uint64
	body    []byte
}
type WireError struct {
	Code    string `json:"code"`
	Outcome string `json:"outcome"`
}

func (e *WireError) Error() string { return e.Code + " (" + e.Outcome + ")" }

// Client serializes exchanges on one connection. A transport failure closes it;
// reconnect and retry the same port request ID to recover the producer's outbox.
type Client struct {
	mu                     sync.Mutex
	conn                   net.Conn
	send, receive, channel uint32
	remote                 struct{ FrameBytes, MessageBytes, WindowBytes, ChannelWindowBytes int }
	credit                 int
}

func Dial(ctx context.Context, address, token string, security *tls.Config) (*Client, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if security == nil && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return nil, errors.New("TLS_REQUIRED")
	}
	if security != nil && security.InsecureSkipVerify {
		return nil, errors.New("TLS_VERIFICATION_REQUIRED")
	}
	var conn net.Conn
	if security == nil {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", address)
	} else {
		secure := security.Clone()
		secure.MinVersion = tls.VersionTLS13
		conn, err = (&tls.Dialer{Config: secure}).DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	hello := map[string]any{"wire": wire, "role": "client", "limits": map[string]any{
		"frameBytes": 65536, "windowBytes": 1048576, "channelWindowBytes": 262144, "mutationReserveBytes": 65536,
		"messageBytes": 16777216, "bufferedMessageBytes": 33554432, "streamBytes": 1073741824, "channels": 32,
		"controlBytes": 8192, "receiptBytes": 8192, "timeoutMs": 30000, "handshakeMs": 5000, "idleMs": 120000}}
	if token != "" {
		hello["authToken"] = token
	}
	body, _ := json.Marshal(hello)
	if err = c.write(frame{kind: 1, body: body}); err != nil {
		conn.Close()
		return nil, err
	}
	response, err := c.read()
	if err != nil {
		conn.Close()
		return nil, err
	}
	var reply struct {
		Wire, Role string
		Limits     struct{ FrameBytes, MessageBytes, WindowBytes, ChannelWindowBytes int }
	}
	if err = json.Unmarshal(response.body, &reply); err != nil || response.kind != 1 || response.channel != 0 || response.id != 0 || reply.Wire != wire || reply.Role != "server" || reply.Limits.FrameBytes < 1024 || reply.Limits.FrameBytes > 1048576 || reply.Limits.MessageBytes < 1 || reply.Limits.MessageBytes > 67108864 || reply.Limits.WindowBytes < reply.Limits.FrameBytes || reply.Limits.WindowBytes > 67108864 || reply.Limits.ChannelWindowBytes < 1 || reply.Limits.ChannelWindowBytes > reply.Limits.WindowBytes {
		conn.Close()
		return nil, errors.New("INCOMPATIBLE_HELLO")
	}
	c.remote = reply.Limits
	c.credit = reply.Limits.WindowBytes
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}
func (c *Client) Close() error { return c.conn.Close() }
func (c *Client) write(f frame) error {
	if c.send == ^uint32(0) {
		return errors.New("SEQUENCE_EXHAUSTED")
	}
	c.send++
	header := make([]byte, 32)
	copy(header, "LSDP")
	header[4] = 2
	header[5] = f.kind
	binary.BigEndian.PutUint32(header[8:12], f.channel)
	binary.BigEndian.PutUint64(header[12:20], f.id)
	binary.BigEndian.PutUint32(header[20:24], uint32(len(f.body)))
	binary.BigEndian.PutUint32(header[24:28], c.send)
	chunks := net.Buffers{header, f.body}
	_, err := chunks.WriteTo(c.conn)
	return err
}
func (c *Client) read() (frame, error) {
	var f frame
	header := make([]byte, 32)
	if _, err := io.ReadFull(c.conn, header); err != nil {
		return f, err
	}
	length := binary.BigEndian.Uint32(header[20:24])
	sequence := binary.BigEndian.Uint32(header[24:28])
	if string(header[:4]) != "LSDP" || header[4] != 2 || header[5] < 1 || header[5] > 10 || header[6] != 0 || header[7] != 0 || binary.BigEndian.Uint32(header[28:]) != 0 || c.receive == ^uint32(0) || sequence != c.receive+1 || length > 65536 {
		return f, errors.New("INVALID_FRAME")
	}
	c.receive = sequence
	f = frame{kind: header[5], channel: binary.BigEndian.Uint32(header[8:12]), id: binary.BigEndian.Uint64(header[12:20]), body: make([]byte, length)}
	_, err := io.ReadFull(c.conn, f.body)
	return f, err
}
func (c *Client) consume(f frame, channelCredit *int) error {
	if f.kind != 5 || len(f.body) != 4 || f.id != 0 {
		return errors.New("INVALID_CREDIT")
	}
	amount := int(binary.BigEndian.Uint32(f.body))
	if amount < 1 {
		return errors.New("INVALID_CREDIT")
	}
	if f.channel == 0 {
		c.credit += amount
		if c.credit > c.remote.WindowBytes {
			return errors.New("INVALID_CREDIT")
		}
	} else if f.channel == c.channel {
		*channelCredit += amount
		if *channelCredit > c.remote.ChannelWindowBytes {
			return errors.New("INVALID_CREDIT")
		}
	} else if f.channel > c.channel {
		return errors.New("INVALID_CREDIT")
	}
	return nil
}
func (c *Client) Exchange(ctx context.Context, value any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.conn.Close() })
	defer stop()
	defer c.conn.SetDeadline(time.Time{})
	result, err := c.exchange(value)
	if err != nil {
		var remote *WireError
		if !errors.As(err, &remote) {
			c.conn.Close()
		}
	}
	return result, err
}
func (c *Client) exchange(value any) (json.RawMessage, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(body) > c.remote.MessageBytes || len(body) > 16777216 {
		return nil, errors.New("MESSAGE_LIMIT")
	}
	if c.channel == 0 {
		c.channel = 1
	} else {
		if c.channel > ^uint32(0)-2 {
			return nil, errors.New("CHANNEL_EXHAUSTED")
		}
		c.channel += 2
	}
	header, _ := json.Marshal(map[string]any{"kind": "json", "length": len(body), "metadata": nil})
	if err = c.write(frame{kind: 2, channel: c.channel, id: uint64(c.channel), body: header}); err != nil {
		return nil, err
	}
	channelCredit := c.remote.ChannelWindowBytes
	chunk := min(65536, c.remote.FrameBytes, c.remote.ChannelWindowBytes)
	for offset := 0; offset < len(body); {
		n := min(chunk, len(body)-offset)
		for n > c.credit || n > channelCredit {
			f, e := c.read()
			if e != nil {
				return nil, e
			}
			if e = c.consume(f, &channelCredit); e != nil {
				return nil, e
			}
		}
		c.credit -= n
		channelCredit -= n
		if err = c.write(frame{kind: 3, channel: c.channel, id: uint64(c.channel), body: body[offset : offset+n]}); err != nil {
			return nil, err
		}
		offset += n
	}
	commit := make([]byte, 40)
	binary.BigEndian.PutUint64(commit, uint64(len(body)))
	hash := sha256.Sum256(body)
	copy(commit[8:], hash[:])
	if err = c.write(frame{kind: 4, channel: c.channel, id: uint64(c.channel), body: commit}); err != nil {
		return nil, err
	}
	for {
		f, e := c.read()
		if e != nil {
			return nil, e
		}
		if f.kind == 5 {
			if e = c.consume(f, &channelCredit); e != nil {
				return nil, e
			}
			continue
		}
		if f.channel != c.channel || f.id != uint64(c.channel) {
			return nil, errors.New("INVALID_RESPONSE")
		}
		if f.kind == 7 {
			var wireError WireError
			if e = json.Unmarshal(f.body, &wireError); e != nil {
				return nil, e
			}
			return nil, &wireError
		}
		if f.kind != 6 {
			return nil, errors.New("INVALID_RESPONSE")
		}
		var response struct {
			Result json.RawMessage `json:"result"`
		}
		if e = json.Unmarshal(f.body, &response); e != nil || response.Result == nil {
			return nil, errors.New("INVALID_ACK")
		}
		var descriptor struct {
			Profile, Target string
			Length          int
		}
		_ = json.Unmarshal(response.Result, &descriptor)
		if descriptor.Profile == "lsdp.state.read/1" {
			return c.readState(descriptor.Target, descriptor.Length, &channelCredit)
		}
		return response.Result, nil
	}
}
func (c *Client) readState(target string, length int, channelCredit *int) (json.RawMessage, error) {
	if length < 0 || length > 16777216 {
		return nil, errors.New("SNAPSHOT_LIMIT")
	}
	var begin frame
	for {
		f, err := c.read()
		if err != nil {
			return nil, err
		}
		if f.kind == 5 {
			if err = c.consume(f, channelCredit); err != nil {
				return nil, err
			}
			continue
		}
		begin = f
		break
	}
	var header struct {
		Kind     string
		Length   int
		Metadata struct {
			Profile, Target string
			Length          int
		}
	}
	if err := json.Unmarshal(begin.body, &header); err != nil || begin.kind != 2 || begin.channel == 0 || begin.channel%2 != 0 || begin.id == 0 || header.Kind != "json" || header.Length != length || header.Metadata.Profile != "lsdp.state.read/1" || header.Metadata.Target != target || header.Metadata.Length != length {
		return nil, errors.New("INVALID_SNAPSHOT")
	}
	body := bytes.NewBuffer(make([]byte, 0, length))
	for {
		f, err := c.read()
		if err != nil {
			return nil, err
		}
		if f.kind == 5 {
			if err = c.consume(f, channelCredit); err != nil {
				return nil, err
			}
			continue
		}
		if f.channel != begin.channel || f.id != begin.id {
			return nil, errors.New("INVALID_SNAPSHOT")
		}
		if f.kind == 3 {
			if len(f.body) == 0 || body.Len()+len(f.body) > length {
				return nil, errors.New("INVALID_SNAPSHOT")
			}
			body.Write(f.body)
			amount := make([]byte, 4)
			binary.BigEndian.PutUint32(amount, uint32(len(f.body)))
			for _, channel := range []uint32{0, begin.channel} {
				if err = c.write(frame{kind: 5, channel: channel, body: amount}); err != nil {
					return nil, err
				}
			}
		} else if f.kind == 4 {
			hash := sha256.Sum256(body.Bytes())
			if len(f.body) != 40 || body.Len() != length || binary.BigEndian.Uint64(f.body) != uint64(length) || !bytes.Equal(f.body[8:], hash[:]) || !json.Valid(body.Bytes()) {
				return nil, errors.New("INTEGRITY_FAILED")
			}
			if err = c.write(frame{kind: 6, channel: begin.channel, id: begin.id, body: []byte(`{"result":{"level":"received"}}`)}); err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"target": target, "state": json.RawMessage(body.Bytes())})
		} else {
			return nil, errors.New("INVALID_SNAPSHOT")
		}
	}
}
func NewID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
func (c *Client) SendPort(ctx context.Context, id, port, kind string, payload any) (json.RawMessage, error) {
	if id == "" {
		var err error
		id, err = NewID()
		if err != nil {
			return nil, err
		}
	}
	return c.Exchange(ctx, map[string]any{"kind": "route", "id": id, "port": port, "type": kind, "payload": payload})
}
func (c *Client) ConfigureSettings(ctx context.Context, settings any, expectedVersion string) (json.RawMessage, error) {
	value := map[string]any{"kind": "configuration.settings", "settings": settings}
	if expectedVersion != "" {
		value["expectedVersion"] = expectedVersion
	}
	return c.Exchange(ctx, value)
}

// Typed receipt access is deliberately explicit: a completed route confirms
// its destinations, not pixels rendered or a provider's external side effects.
func Completed(value json.RawMessage) error {
	var status struct{ Status string }
	if err := json.Unmarshal(value, &status); err != nil {
		return err
	}
	if status.Status != "completed" {
		return fmt.Errorf("route status: %s", status.Status)
	}
	return nil
}
