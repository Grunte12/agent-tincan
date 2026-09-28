package client

import (
	"context"
	"errors"
	"net/url"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// ErrNeedsInputUnsupported means the relay must be upgraded for clarification.
var ErrNeedsInputUnsupported = errors.New("this relay does not support needs_input (upgrade the relay)")

func (r *Relay) requireNeedsInput(ctx context.Context) error {
	caps, err := r.Capabilities(ctx)
	if err != nil {
		return err
	}
	if !caps.NeedsInput {
		return ErrNeedsInputUnsupported
	}
	return nil
}

// Answer supplies a clarification to a request this agent originally sent.
func (r *Relay) Answer(ctx context.Context, id, body string) (envelope.Request, error) {
	if err := r.requireNeedsInput(ctx); err != nil {
		return envelope.Request{}, err
	}
	if err := envelope.ValidateInput(body); err != nil {
		return envelope.Request{}, err
	}
	var out envelope.Request
	err := r.call(ctx, r.api, "POST", "/v1/requests/"+url.PathEscape(id)+"/answer", map[string]string{"body": body}, &out)
	return out, err
}
