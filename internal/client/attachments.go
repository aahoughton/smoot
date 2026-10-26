package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"github.com/aahoughton/smoot/internal/core"
)

// Attach uploads data as file name to the conversation, as agent.
func (c *Client) Attach(ctx context.Context, ns, conv, agent, name string, data []byte) (core.AttachResult, error) {
	var out core.AttachResult
	q := url.Values{"agent": {agent}, "name": {name}}
	err := c.do(ctx, http.MethodPost, convPath(ns, conv)+"/attachments", q, octets{bytes.NewReader(data)}, &out)
	return out, err
}

// Fetch downloads the file attached at seq, as agent. It checks that the
// bytes match the sha256 the server sent and that the file name is one an
// attachment may have, so a caller can write the file under that name.
func (c *Client) Fetch(ctx context.Context, ns, conv, agent string, seq int64) (core.Download, error) {
	path := convPath(ns, conv) + "/attachments/" + strconv.FormatInt(seq, 10)
	resp, err := c.stream(ctx, path, url.Values{"agent": {agent}})
	if err != nil {
		return core.Download{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, core.MaxAttachmentBytes+1))
	if err != nil {
		return core.Download{}, err
	}
	if len(data) > core.MaxAttachmentBytes {
		return core.Download{}, fmt.Errorf("client: attachment #%d is over %d bytes", seq, core.MaxAttachmentBytes)
	}
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil {
		return core.Download{}, fmt.Errorf("client: attachment #%d has no usable file name: %w", seq, err)
	}
	name := params["filename"]
	if err := core.CheckFileName(name); err != nil {
		return core.Download{}, fmt.Errorf("client: attachment #%d: %w", seq, err)
	}
	sum := sha256.Sum256(data)
	got, want := hex.EncodeToString(sum[:]), resp.Header.Get(core.SHA256Header)
	if got != want {
		return core.Download{}, fmt.Errorf("client: attachment #%d does not match its sha256 (got %s, server sent %q)", seq, got, want)
	}
	return core.Download{Attachment: core.Attachment{Name: name, Size: int64(len(data)), SHA256: got}, Data: data}, nil
}

// AttachmentStorage reports attachment storage by conversation. Server
// admins only.
func (c *Client) AttachmentStorage(ctx context.Context) (core.AttachmentReport, error) {
	var out core.AttachmentReport
	err := c.do(ctx, http.MethodGet, "/v1/attachments", nil, nil, &out)
	return out, err
}

// DeleteAttachments drops stored attachment bytes: those with d.SHA256, or
// all in d.Namespace/d.Conversation. It returns how many files and bytes
// were dropped. Server admins only.
func (c *Client) DeleteAttachments(ctx context.Context, d core.AttachmentDelete) (blobs, size int64, err error) {
	path := "/v1/attachments/" + url.PathEscape(d.SHA256)
	if d.SHA256 == "" {
		path = convPath(d.Namespace, d.Conversation) + "/attachments"
	}
	var out struct {
		Blobs int64 `json:"blobs"`
		Bytes int64 `json:"bytes"`
	}
	err = c.do(ctx, http.MethodDelete, path, nil, nil, &out)
	return out.Blobs, out.Bytes, err
}
