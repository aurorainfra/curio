// Package nodediag serves read-only diagnostics of the local Curio node
// (log files, goroutine dumps, storage health) at /aurora/diag/v0 on the
// node RPC port. It is called by the Aurora MCP server running on a GUI node
// and is authenticated with a JWT signed with the cluster storage RPC secret
// carrying exactly the PermDiag permission.
//
// Nothing in this package accepts file paths or command arguments from the
// caller: log files are chosen by name from a server-side list, storage
// diagnostics by the ID of a storage path attached to this node.
package nodediag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/filecoin-project/go-jsonrpc/auth"
	logging "github.com/ipfs/go-log/v2"

	"github.com/filecoin-project/curio/deps"
)

var log = logging.Logger("aurora/nodediag")

// PermDiag is the only permission accepted by the diag endpoint. It is not
// granted to any other API, and the diag endpoint accepts no other
// permission (not even admin).
const PermDiag auth.Permission = "aurora-diag"

// TimeoutHeader carries the caller's remaining time budget in milliseconds.
// Long operations (log scans) stop when it runs out and return partial
// results, instead of being cut off with the caller's connection.
const TimeoutHeader = "X-Aurora-Diag-Timeout-Ms"

// PathPrefix is where Handler is mounted on the node RPC mux.
const PathPrefix = "/aurora/diag/v0"

const maxRequestBytes = 64 << 10

// Endpoint names, relative to PathPrefix.
const (
	OpLogFiles   = "log/files"
	OpLogRead    = "log/read"
	OpLogGrep    = "log/grep"
	OpLogTail    = "log/tail"
	OpLogFind    = "log/findtime"
	OpGoroutines = "goroutines"
	OpStorage    = "storage"
)

// ErrorResponse is returned with a non-200 status.
type ErrorResponse struct {
	Error string
}

// errBadRequest marks errors caused by invalid caller input.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

func badRequest(msg string) error { return errBadRequest{msg: msg} }

type handler struct {
	d    *deps.Deps
	logs *logFiles
}

// Handler returns the diag HTTP handler. Every request must carry a JWT
// granting PermDiag; requests without one are rejected.
func Handler(d *deps.Deps) http.Handler {
	h := &handler{d: d, logs: newLogFiles(d)}
	return http.HandlerFunc(h.serve)
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	if !auth.HasPerm(r.Context(), nil, PermDiag) {
		writeErr(w, http.StatusForbidden, errors.New("missing aurora-diag permission"))
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}

	op := strings.TrimPrefix(r.URL.Path, PathPrefix+"/")
	body := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	ctx := r.Context()
	if ms, err := strconv.ParseInt(r.Header.Get(TimeoutHeader), 10, 64); err == nil && ms > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
		defer cancel()
	}

	var resp any
	var err error
	switch op {
	case OpLogFiles:
		resp, err = h.logs.list()
	case OpLogRead:
		resp, err = handle(ctx, body, h.logs.read)
	case OpLogGrep:
		resp, err = handle(ctx, body, h.logs.grep)
	case OpLogTail:
		resp, err = handle(ctx, body, h.logs.tail)
	case OpLogFind:
		resp, err = handle(ctx, body, h.logs.findTime)
	case OpGoroutines:
		resp, err = handle(ctx, body, goroutines)
	case OpStorage:
		resp, err = handle(ctx, body, h.storageDiag)
	default:
		writeErr(w, http.StatusNotFound, errors.New("unknown diag endpoint"))
		return
	}
	if err != nil {
		code := http.StatusInternalServerError
		var br errBadRequest
		if errors.As(err, &br) {
			code = http.StatusBadRequest
		}
		writeErr(w, code, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Debugw("writing diag response", "op", op, "error", err)
	}
}

func handle[Req, Resp any](ctx context.Context, body interface{ Read([]byte) (int, error) }, fn func(context.Context, Req) (Resp, error)) (Resp, error) {
	var req Req
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var zero Resp
		return zero, badRequest("decoding request: " + err.Error())
	}
	return fn(ctx, req)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: err.Error()})
}
