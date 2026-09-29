package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

// ErrUploadFailed marks a SendEach entry that was not sent because one of
// its files could not be uploaded (the relay stores no attachments, or a
// quota is full). The caller may resend it without files.
var ErrUploadFailed = errors.New("attachment upload failed")

// Outgoing is one request of a SendEach batch: its own target, body, and
// files.
type Outgoing struct {
	To    string
	Body  string
	Files []OutgoingFile
}

// OutgoingFile is a file to upload and attach to one Outgoing request.
// MIME may be "" to let the relay detect it.
type OutgoingFile struct {
	Name string
	MIME string
	Data []byte
}

// SendEach sends a different body and file set to each target under one
// new group id, each as a child of parent ("" lets the relay infer it).
// It takes 1 to 8 distinct targets, the relay's per-group cap. Files are
// uploaded separately for each target, since an attachment is bound to
// one request. errs lines up with outs: a target whose upload or send
// failed is a failed entry in the group, and its error wraps
// ErrUploadFailed when nothing was sent because of an upload. err is set
// only when the batch itself is refused and nothing was sent.
func (r *Relay) SendEach(ctx context.Context, outs []Outgoing, kind envelope.Kind, parent string) (g GroupResult, errs []error, err error) {
	targets := make([]string, len(outs))
	for i, o := range outs {
		targets[i] = o.To
	}
	norm, err := NormalizeTargets(targets, r.agent)
	if err != nil {
		return GroupResult{}, nil, err
	}
	if len(norm) != len(outs) {
		return GroupResult{}, nil, errors.New("each target may appear once in a batch")
	}
	g = GroupResult{Group: "group-" + rand.Text()}
	errs = make([]error, len(outs))
	for i, o := range outs {
		req := envelope.Request{To: o.To, Body: o.Body, Kind: kind, ParentID: parent, Group: g.Group}
		err := r.uploadEach(ctx, o.Files, &req)
		if err == nil {
			var sent envelope.Request
			if err = r.call(ctx, r.api, "POST", "/v1/send", req, &sent); err == nil {
				req = sent
			}
		}
		res := Result{Request: req, Status: sentStatus(req)}
		if err != nil {
			errs[i] = err
			res.Status = envelope.StatusFailed
			res.Reply = &envelope.Reply{From: o.To, Status: envelope.StatusFailed, Body: err.Error()}
		}
		g.Results = append(g.Results, GroupEntry{Result: res})
	}
	g.summarize()
	r.groupsMu.Lock()
	r.cacheGroupLocked(g)
	r.groupsMu.Unlock()
	return g, errs, nil
}

// uploadEach uploads files and names them on req. An error wraps
// ErrUploadFailed.
func (r *Relay) uploadEach(ctx context.Context, files []OutgoingFile, req *envelope.Request) error {
	if len(files) == 0 {
		return nil
	}
	if _, err := r.requireAttachments(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrUploadFailed, err)
	}
	for _, f := range files {
		up, err := r.upload(ctx, f.Name, f.MIME, bytes.NewReader(f.Data), int64(len(f.Data)))
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUploadFailed, f.Name, err)
		}
		req.Attachments = append(req.Attachments, envelope.Attachment{ID: up.ID})
	}
	return nil
}
