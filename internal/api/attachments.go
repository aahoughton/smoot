package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/aahoughton/smoot/internal/core"
)

// attachReadTimeout bounds how long a client may take to send a file, which
// may be much larger than a JSON body.
const attachReadTimeout = 5 * time.Minute

// attachmentRoutes are registered only when the server enables
// attachments; openapi.yaml describes them with the rest.
var attachmentRoutes = []route{
	{"POST", convPath + "/attachments", (*handler).attach},
	{"GET", convPath + "/attachments/{seq}", (*handler).fetch},
	{"GET", "/v1/attachments", (*handler).attachmentStorage},
	{"DELETE", "/v1/attachments/{sha256}", (*handler).deleteAttachmentsBySHA},
	{"DELETE", convPath + "/attachments", (*handler).deleteConversationAttachments},
}

// attach takes the file as the raw request body; agent and name are query
// parameters.
func (h *handler) attach(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	q := r.URL.Query()
	agent, name := q.Get("agent"), q.Get("name")
	if agent == "" || name == "" {
		return invalid("query parameters agent and name are required")
	}
	if err := core.CheckFileName(name); err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(attachReadTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, core.MaxAttachmentBytes))
	if err != nil {
		w.Header().Set("Connection", "close")
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return &core.Error{Code: core.CodeAttachmentTooLarge, Message: fmt.Sprintf("file is over %d bytes", core.MaxAttachmentBytes)}
		}
		return bodyError(err)
	}
	_ = rc.SetReadDeadline(time.Time{})
	res, err := h.svc.Attach(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.AttachParams{Agent: agent, Name: name, Data: data})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, res)
	return nil
}

// fetch sends an attachment's bytes, as a download, never for display:
// participants download with ?agent=, anyone who may read the
// conversation without it (recorded as ?observer= under auth mode none).
func (h *handler) fetch(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq < 1 {
		return invalid("seq must be a positive integer")
	}
	q := r.URL.Query()
	d, err := h.svc.Fetch(r.Context(), p, r.PathValue("ns"), r.PathValue("conv"), core.FetchParams{
		Seq: seq, Agent: q.Get("agent"), Observer: q.Get("observer"),
	})
	if err != nil {
		return err
	}
	WriteDownload(w, d)
	return nil
}

// WriteDownload sends d as a file to save: an opaque type, never sniffed,
// never cached, with its name and sha256 in headers. The viewer shares it.
func WriteDownload(w http.ResponseWriter, d core.Download) {
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": d.Attachment.Name}))
	hd.Set("Content-Length", strconv.Itoa(len(d.Data)))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "no-store")
	hd.Set("Content-Security-Policy", "sandbox")
	hd.Set(core.SHA256Header, d.Attachment.SHA256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(d.Data)
}

func (h *handler) attachmentStorage(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	rep, err := h.svc.AttachmentStorage(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rep)
	return nil
}

// deleted is the response to an attachment delete.
type deleted struct {
	Blobs int64 `json:"blobs"`
	Bytes int64 `json:"bytes"`
}

func (h *handler) deleteAttachmentsBySHA(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	return h.deleteAttachments(w, r, p, core.AttachmentDelete{SHA256: r.PathValue("sha256")})
}

func (h *handler) deleteConversationAttachments(w http.ResponseWriter, r *http.Request, p core.Principal) error {
	return h.deleteAttachments(w, r, p, core.AttachmentDelete{Namespace: r.PathValue("ns"), Conversation: r.PathValue("conv")})
}

func (h *handler) deleteAttachments(w http.ResponseWriter, r *http.Request, p core.Principal, d core.AttachmentDelete) error {
	blobs, size, err := h.svc.DeleteAttachments(r.Context(), p, d)
	if err != nil {
		return err
	}
	target := d.SHA256
	if target == "" {
		target = d.Namespace + "/" + d.Conversation
	}
	h.audit(r, p, "delete attachments", target, "blobs", blobs, "bytes", size)
	writeJSON(w, http.StatusOK, deleted{Blobs: blobs, Bytes: size})
	return nil
}
