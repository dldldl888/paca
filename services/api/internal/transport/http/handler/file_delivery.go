package handler

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/delivery"
	"github.com/Paca-AI/api/internal/platform/storage"
	"github.com/Paca-AI/api/internal/transport/http/middleware"
	"github.com/Paca-AI/api/internal/transport/http/presenter"
	"github.com/aws/smithy-go"
)

// FileDelivery shares the existing authorization gate across all business file writes.
type FileDelivery struct {
	gate       *delivery.Gate
	store      storage.Client
	bucket     string
	authorizer *authz.Authorizer
}

func NewFileDelivery(gate *delivery.Gate, store storage.Client, bucket string, authorizer *authz.Authorizer) *FileDelivery {
	return &FileDelivery{gate, store, bucket, authorizer}
}

var singleRange = regexp.MustCompile(`^bytes=([0-9]+-[0-9]*|-[0-9]+)$`)

const fileWriteTimeout = 5 * time.Second

func (d *FileDelivery) serve(w http.ResponseWriter, r *http.Request, permission authz.Permission, resolve func(context.Context) (*attachmentdom.File, error), bounded bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	unavailable := func() { http.Error(w, "file delivery unavailable", http.StatusServiceUnavailable) }
	if d == nil || d.gate == nil || d.store == nil {
		unavailable()
		return
	}
	// Only this derived context carries a lease. Never keep it after leave.
	ctx, epoch, leave, err := d.gate.Enter(r.Context())
	if err != nil {
		unavailable()
		return
	}
	if permission != "" && !middleware.EnforcePermissions(w, r.WithContext(ctx), d.authorizer, middleware.ProjectScopeFromParam("projectId"), permission) {
		leave()
		return
	}
	f, err := resolve(ctx)
	leave()
	if err != nil {
		presenter.Error(w, r, err)
		return
	}
	rg := r.Header.Get("Range")
	if rg != "" && (!singleRange.MatchString(rg) || bounded) {
		http.Error(w, "unsupported range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	bucket := f.Bucket
	if bucket == "" {
		bucket = d.bucket
	}
	obj, err := d.store.OpenObject(r.Context(), bucket, f.StorageKey, rg)
	if err != nil {
		var apierr smithy.APIError
		if errors.As(err, &apierr) && apierr.ErrorCode() == "InvalidRange" {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		} else {
			http.Error(w, "file unavailable", http.StatusNotFound)
		}
		return
	}
	defer obj.Body.Close()
	if bounded && obj.Size > attachmentdom.MaxAttachmentContentSize {
		presenter.Error(w, r, attachmentdom.ErrAttachmentContentTooLarge)
		return
	}
	ct, _, parseErr := mime.ParseMediaType(f.ContentType)
	if parseErr != nil {
		ct = "application/octet-stream"
	}
	disposition := "attachment"
	// Active MIME types (including SVG/HTML/PDF) never execute on the API origin.
	if !bounded && r.URL.Query().Get("download") != "true" {
		switch ct {
		case "image/png", "image/jpeg", "image/gif", "image/webp", "audio/mpeg", "audio/ogg", "video/mp4", "video/webm":
			disposition = "inline"
		}
	}
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, f.FileName)
	controller := http.NewResponseController(w)
	started := false
	remaining := obj.Size
	buf := make([]byte, 64*1024)
	for {
		n := 0
		readErr := io.EOF
		if r.Method != http.MethodHead && remaining > 0 {
			limit := int64(len(buf))
			if remaining < limit {
				limit = remaining
			}
			n, readErr = io.ReadFull(obj.Body, buf[:limit])
		}
		_, current, release, e := d.gate.Enter(r.Context())
		if e != nil {
			if !started {
				unavailable()
			} else {
				panic(http.ErrAbortHandler)
			}
			return
		}
		if current != epoch {
			release()
			if !started {
				unavailable()
			} else {
				panic(http.ErrAbortHandler)
			}
			return
		}
		// A deadline bounds the lease even when a client stops consuming bytes.
		e = controller.SetWriteDeadline(time.Now().Add(fileWriteTimeout))
		if e != nil {
			release()
			if !started {
				unavailable()
			} else {
				panic(http.ErrAbortHandler)
			}
			return
		}
		if !started {
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
			w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
			w.Header().Set("Accept-Ranges", "bytes")
			status := http.StatusOK
			if obj.ContentRange != "" {
				w.Header().Set("Content-Range", obj.ContentRange)
				status = http.StatusPartialContent
			}
			w.WriteHeader(status)
			started = true
		}
		if n > 0 {
			_, e = w.Write(buf[:n])
		}
		if e == nil {
			e = controller.Flush()
		}
		release()
		if e != nil {
			panic(http.ErrAbortHandler)
		}
		remaining -= int64(n)
		if r.Method == http.MethodHead || remaining == 0 {
			_ = controller.SetWriteDeadline(time.Time{})
			return
		}
		if readErr != nil {
			panic(http.ErrAbortHandler)
		}
	}
}
