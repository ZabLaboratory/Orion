package lsdpreception

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

// Producer serializes native transactions off the Blue/scene goroutines.
// Overflow and failed delivery are visible to Flush/Ready, never silently dropped.
// RAM only: no scene, token, outbox or mutated LSML is written to disk.
type Producer struct {
	reception  *Reception
	ctx        context.Context
	cancel     context.CancelFunc
	queue      chan delivery
	done       chan struct{}
	mu         sync.Mutex
	failure    error
	terminal   bool
	dial       func(context.Context) (producerClient, error)
	retryDelay time.Duration
	reconnects uint64
	delivered  uint64
}
type DeliveryStatus struct {
	Pending    int    `json:"pending"`
	Capacity   int    `json:"capacity"`
	Recovering bool   `json:"recovering"`
	Terminal   bool   `json:"terminal"`
	Reconnects uint64 `json:"reconnects"`
	Delivered  uint64 `json:"delivered"`
}

func (p *Producer) Status() DeliveryStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return DeliveryStatus{Pending: len(p.queue), Capacity: cap(p.queue), Recovering: p.failure != nil && !p.terminal, Terminal: p.terminal, Reconnects: p.reconnects, Delivered: p.delivered}
}

type producerClient interface {
	Exchange(context.Context, any) (json.RawMessage, error)
	Close() error
}
type delivery struct {
	target     string
	document   any
	operations []map[string]any
	barrier    chan error
}

func NewProducer(parent context.Context, reception *Reception) *Producer {
	ctx, cancel := context.WithCancel(parent)
	p := &Producer{reception: reception, ctx: ctx, cancel: cancel, queue: make(chan delivery, 256), done: make(chan struct{})}
	p.dial = func(ctx context.Context) (producerClient, error) { return native.Dial(ctx, reception.Address, "", nil) }
	p.retryDelay = 250 * time.Millisecond
	go p.run()
	return p
}
func (p *Producer) Close() { p.cancel(); <-p.done }
func (p *Producer) fail(err error) {
	p.mu.Lock()
	if !p.terminal {
		p.failure = err
		p.terminal = true
	}
	p.mu.Unlock()
}
func (p *Producer) Error() error { p.mu.Lock(); defer p.mu.Unlock(); return p.failure }
func (p *Producer) transient(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.terminal {
		p.failure = err
	}
}

// A dropped transport has an unknown outcome. Replay exactly the same request
// on the same receiver, never mint a fresh mutation identity during recovery.
func (p *Producer) exchange(client *producerClient, request any) (json.RawMessage, bool, error) {
	retried := false
	for {
		p.mu.Lock()
		terminal, failure := p.terminal, p.failure
		p.mu.Unlock()
		if terminal {
			return nil, retried, failure
		}
		ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
		var result json.RawMessage
		var err error
		if *client == nil {
			*client, err = p.dial(ctx)
		}
		if err == nil {
			result, err = (*client).Exchange(ctx, request)
		}
		cancel()
		if err == nil {
			p.transient(nil)
			return result, retried, nil
		}
		var wireError *native.WireError
		if errors.As(err, &wireError) {
			return nil, retried, err
		}
		var transport net.Error
		if !errors.As(err, &transport) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.DeadlineExceeded) && p.ctx.Err() == nil {
			return nil, retried, err
		}
		if *client != nil {
			(*client).Close()
			*client = nil
		}
		p.transient(fmt.Errorf("NATIVE_LSDP_RECONNECTING: %w", err))
		p.mu.Lock()
		p.reconnects++
		p.mu.Unlock()
		retried = true
		timer := time.NewTimer(p.retryDelay)
		select {
		case <-p.ctx.Done():
			timer.Stop()
			return nil, retried, p.ctx.Err()
		case <-timer.C:
		}
	}
}
func (p *Producer) enqueue(d delivery) {
	if p.ctx.Err() != nil {
		p.fail(p.ctx.Err())
		return
	}
	select {
	case p.queue <- d:
	default:
		p.fail(errors.New("NATIVE_LSDP_QUEUE_FULL"))
	}
}
func (p *Producer) Replace(target string, document any) {
	// Capture immutable bytes before the caller mutates its private scene map.
	data, err := json.Marshal(document)
	if err != nil {
		p.fail(err)
		return
	}
	p.enqueue(delivery{target: target, document: json.RawMessage(data)})
}
func (p *Producer) Apply(target string, operations []map[string]any) {
	if len(operations) == 0 {
		return
	}
	data, err := json.Marshal(operations)
	if err != nil {
		p.fail(err)
		return
	}
	var owned []map[string]any
	if err = json.Unmarshal(data, &owned); err != nil {
		p.fail(err)
		return
	}
	p.enqueue(delivery{target: target, operations: owned})
}
func (p *Producer) Flush(ctx context.Context) error {
	result := make(chan error, 1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("NATIVE_LSDP_STOPPED")
	case p.queue <- delivery{barrier: result}:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("NATIVE_LSDP_STOPPED")
	case err := <-result:
		return err
	}
}
func (p *Producer) Check(ctx context.Context) error {
	if err := p.Error(); err != nil {
		return err
	}
	return p.reception.Check(ctx)
}
func (p *Producer) run() {
	defer close(p.done)
	var client producerClient
	defer func() {
		if client != nil {
			client.Close()
		}
	}()
	// The cache is authoritative only after a real read/ACK, not a listening port.
	states := map[string]any{}
	for {
		select {
		case <-p.ctx.Done():
			return
		case job := <-p.queue:
			if job.barrier != nil {
				job.barrier <- p.Error()
				continue
			}
			if p.Error() != nil {
				continue
			}
			id, idErr := native.NewID()
			if idErr != nil {
				p.fail(idErr)
				continue
			}
			var request any
			var next any
			var err error
			if job.operations == nil {
				kind := "solar.lsml/1"
				if job.target != "solar/program" && job.target != "solar/preview" {
					kind = "orion.state/1"
				}
				request = map[string]any{"kind": "route", "id": id, "port": job.target, "type": kind, "payload": job.document}
				err = json.Unmarshal(job.document.(json.RawMessage), &next)
			} else {
				before, ok := states[job.target]
				if !ok {
					var raw json.RawMessage
					raw, _, err = p.exchange(&client, map[string]any{"kind": "state.read", "target": job.target})
					if err == nil {
						var snapshot struct {
							State any `json:"state"`
						}
						err = json.Unmarshal(raw, &snapshot)
						before = snapshot.State
					}
				}
				if err == nil {
					next, err = applyObjectOperations(before, job.operations)
					if err == nil {
						request = map[string]any{"format": "lsdp.apply/1", "id": id, "target": job.target, "beforeHash": treeHash(before), "operations": job.operations, "require": "applied"}
					}
				}
			}
			if err == nil {
				var result json.RawMessage
				var uncertain bool
				result, uncertain, err = p.exchange(&client, request)
			rebaseLoop:
				for rebase := 0; err != nil && rebase < 8; rebase++ {
					var wireError *native.WireError
					switch {
					case errors.As(err, &wireError) && wireError.Code == "BASE_MISMATCH" && job.operations != nil && uncertain:
						// A receiver restart may have lost deduplication receipts. Do not
						// rebase an array insertion or another possibly committed operation.
						err = errors.New("NATIVE_TRANSACTION_OUTCOME_UNKNOWN")
						break rebaseLoop
					case errors.As(err, &wireError) && wireError.Code == "BASE_MISMATCH" && job.operations != nil:
						// A concurrent explicit LSML edit may have changed another field.
						// Read its acknowledged baseline; preserve it, then retry only our
						// declared leaves with a NEW ID after a proven rejection.
						var raw json.RawMessage
						raw, _, err = p.exchange(&client, map[string]any{"kind": "state.read", "target": job.target})
						var snapshot struct {
							State any `json:"state"`
						}
						if err == nil {
							err = json.Unmarshal(raw, &snapshot)
						}
						if err == nil {
							next, err = applyObjectOperations(snapshot.State, job.operations)
						}
						if err == nil {
							id, err = native.NewID()
							if err == nil {
								request = map[string]any{"format": "lsdp.apply/1", "id": id, "target": job.target, "beforeHash": treeHash(snapshot.State), "operations": job.operations, "require": "applied"}
								result, uncertain, err = p.exchange(&client, request)
							}
						}
					default:
						break rebaseLoop
					}
				}
				if err == nil {
					var receipt struct {
						Status, Level, Target string
						TransactionID         string `json:"transactionId"`
					}
					err = json.Unmarshal(result, &receipt)
					if err == nil && job.operations == nil && receipt.Status != "completed" {
						err = fmt.Errorf("NATIVE_ROUTE_INCOMPLETE: %s", receipt.Status)
					}
					if err == nil && job.operations != nil && (receipt.Level != "applied" || receipt.Target != job.target || receipt.TransactionID != id) {
						err = errors.New("NATIVE_APPLICATION_NOT_ACKNOWLEDGED")
					}
				}
			}
			if err != nil {
				p.fail(fmt.Errorf("native delivery %s: %w", job.target, err))
			} else {
				states[job.target] = next
				p.mu.Lock()
				p.delivered++
				p.mu.Unlock()
			}
		}
	}
}
