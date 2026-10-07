package lsdpnative

// Orion-owned subscription extension. client.go remains the exact upstream pin;
// this reuses its negotiated framing, checksums and credit implementation.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
)

func (c *Client) Subscribe(ctx context.Context, target, stateHash string) error {
	_, err := c.Exchange(ctx, map[string]any{"kind": "subscribe", "target": target, "stateHash": stateHash})
	return err
}

// Next receives one server-initiated subscription transaction on a dedicated
// connection. Callback validation precedes its application acknowledgement.
func (c *Client) Next(ctx context.Context, target string, apply func(json.RawMessage) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { c.conn.Close() })
	defer stop()
	credit := 0
	var begin frame
	for {
		f, err := c.read()
		if err != nil {
			return err
		}
		if f.kind == 5 {
			if err = c.consume(f, &credit); err != nil {
				return err
			}
			continue
		}
		begin = f
		break
	}
	var header struct {
		Kind     string
		Length   int
		Metadata struct{ Profile, Target string }
	}
	if err := json.Unmarshal(begin.body, &header); err != nil || begin.kind != 2 || begin.channel == 0 || begin.channel%2 != 0 || begin.id == 0 || header.Kind != "json" || header.Length < 0 || header.Length > 16777216 || header.Length > c.remote.MessageBytes || header.Metadata.Profile != "lsdp.state.subscription/1" || header.Metadata.Target != target {
		return errors.New("INVALID_SUBSCRIPTION")
	}
	body := bytes.NewBuffer(make([]byte, 0, header.Length))
	for {
		f, err := c.read()
		if err != nil {
			return err
		}
		if f.kind == 5 {
			if err = c.consume(f, &credit); err != nil {
				return err
			}
			continue
		}
		if f.channel != begin.channel || f.id != begin.id {
			return errors.New("INVALID_SUBSCRIPTION")
		}
		switch f.kind {
		case 3:
			if len(f.body) == 0 || body.Len()+len(f.body) > header.Length {
				return errors.New("INVALID_SUBSCRIPTION")
			}
			body.Write(f.body)
			amount := make([]byte, 4)
			binary.BigEndian.PutUint32(amount, uint32(len(f.body)))
			for _, channel := range []uint32{0, begin.channel} {
				if err = c.write(frame{kind: 5, channel: channel, body: amount}); err != nil {
					return err
				}
			}
		case 4:
			hash := sha256.Sum256(body.Bytes())
			if len(f.body) != 40 || body.Len() != header.Length || binary.BigEndian.Uint64(f.body) != uint64(header.Length) || !bytes.Equal(f.body[8:], hash[:]) || !json.Valid(body.Bytes()) {
				return errors.New("INTEGRITY_FAILED")
			}
			if err = apply(body.Bytes()); err != nil {
				return err
			}
			return c.write(frame{kind: 6, channel: begin.channel, id: begin.id, body: []byte(`{"result":{"level":"applied"}}`)})
		default:
			return errors.New("INVALID_SUBSCRIPTION")
		}
	}
}
