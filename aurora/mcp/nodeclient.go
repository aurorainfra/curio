package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gbrlsnchs/jwt/v3"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-jsonrpc/auth"

	"github.com/filecoin-project/curio/aurora/nodediag"
	"github.com/filecoin-project/curio/deps"
	"github.com/filecoin-project/curio/harmony/harmonydb"
)

const (
	// machines not seen for this long are skipped by "all" selectors
	liveMachineWindow = 2 * time.Minute
	maxNodeResponse   = 32 << 20
	maxFanOut         = 16
	// time reserved for a node to send back partial results
	nodeResponseMargin = 5 * time.Second
)

// nodeClient calls the nodediag endpoint of cluster machines with a JWT that
// grants only nodediag.PermDiag.
type nodeClient struct {
	d      *deps.Deps
	client *http.Client

	tokLk sync.Mutex
	tok   string
}

func newNodeClient(d *deps.Deps) *nodeClient {
	return &nodeClient{d: d, client: &http.Client{}}
}

func (n *nodeClient) token() (string, error) {
	n.tokLk.Lock()
	defer n.tokLk.Unlock()
	if n.tok != "" {
		return n.tok, nil
	}
	key, err := base64.StdEncoding.DecodeString(n.d.Cfg.Apis.StorageRPCSecret)
	if err != nil {
		return "", xerrors.Errorf("decoding storage rpc secret: %w", err)
	}
	tok, err := jwt.Sign(&deps.JwtPayload{Allow: []auth.Permission{nodediag.PermDiag}}, jwt.NewHS256(key))
	if err != nil {
		return "", xerrors.Errorf("signing diag token: %w", err)
	}
	n.tok = string(tok)
	return n.tok, nil
}

func (n *nodeClient) call(ctx context.Context, m machine, op string, req, resp any) error {
	tok, err := n.token()
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	u := "http://" + m.HostPort + nodediag.PathPrefix + "/" + op
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Authorization", "Bearer "+tok)
	hreq.Header.Set("Content-Type", "application/json")
	if dl, ok := ctx.Deadline(); ok {
		// leave time to return partial results before our own deadline
		budget := time.Until(dl) - nodeResponseMargin
		if budget < time.Second {
			budget = time.Second
		}
		hreq.Header.Set(nodediag.TimeoutHeader, strconv.FormatInt(budget.Milliseconds(), 10))
	}

	hresp, err := n.client.Do(hreq)
	if err != nil {
		return xerrors.Errorf("calling %s: %w", m.label(), err)
	}
	defer func() { _ = hresp.Body.Close() }()
	rb, err := io.ReadAll(io.LimitReader(hresp.Body, maxNodeResponse))
	if err != nil {
		return xerrors.Errorf("reading response from %s: %w", m.label(), err)
	}
	if hresp.StatusCode != http.StatusOK {
		var er nodediag.ErrorResponse
		if json.Unmarshal(rb, &er) == nil && er.Error != "" {
			return xerrors.Errorf("%s: %s", m.label(), er.Error)
		}
		if hresp.StatusCode == http.StatusNotFound {
			return xerrors.Errorf("%s: diag endpoint not found (node runs a build without aurora/nodediag?)", m.label())
		}
		return xerrors.Errorf("%s: HTTP %d", m.label(), hresp.StatusCode)
	}
	return json.Unmarshal(rb, resp)
}

// NodeResult is the result of one machine in a fan-out call.
type NodeResult[T any] struct {
	MachineID int64
	Machine   string
	Result    *T     `json:",omitempty"`
	Error     string `json:",omitempty"`
}

func callNodes[T any](ctx context.Context, s *Server, ms []machine, op string, req any) []NodeResult[T] {
	out := make([]NodeResult[T], len(ms))
	sem := make(chan struct{}, maxFanOut)
	var wg sync.WaitGroup
	for i, m := range ms {
		out[i] = NodeResult[T]{MachineID: m.ID, Machine: m.Name}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var r T
			if err := s.nodes.call(ctx, m, op, req, &r); err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Result = &r
		}()
	}
	wg.Wait()
	return out
}

// callNode calls one machine and returns its result.
func callNode[T any](ctx context.Context, s *Server, m machine, op string, req any) (*NodeResult[T], error) {
	var r T
	if err := s.nodes.call(ctx, m, op, req, &r); err != nil {
		return nil, err
	}
	return &NodeResult[T]{MachineID: m.ID, Machine: m.Name, Result: &r}, nil
}

type machine struct {
	ID          int64     `db:"id"`
	HostPort    string    `db:"host_and_port"`
	Name        string    `db:"name"`
	LastContact time.Time `db:"last_contact"`
}

func (m machine) label() string {
	if m.Name != "" && m.Name != m.HostPort {
		return fmt.Sprintf("%s (%s)", m.Name, m.HostPort)
	}
	return m.HostPort
}

func (m machine) live() bool { return time.Since(m.LastContact) < liveMachineWindow }

func (s *Server) machines(ctx context.Context) ([]machine, error) {
	var ms []machine
	err := s.ro(ctx, func(tx *harmonydb.Tx) error {
		return tx.Select(&ms, qMachineList)
	})
	return ms, err
}

// selectMachines resolves a machine selector: a machine ID, name or
// host:port; "all" selects every live machine when allowAll is set.
func (s *Server) selectMachines(ctx context.Context, sel string, allowAll bool) ([]machine, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return nil, xerrors.New("machine is required (ID, name or host:port from cluster_machines)")
	}
	ms, err := s.machines(ctx)
	if err != nil {
		return nil, err
	}
	if sel == "all" {
		if !allowAll {
			return nil, xerrors.New(`this tool needs a single machine, not "all"`)
		}
		var out []machine
		for _, m := range ms {
			if m.live() {
				out = append(out, m)
			}
		}
		if len(out) == 0 {
			return nil, xerrors.New("no live machines")
		}
		return out, nil
	}
	id, idErr := strconv.ParseInt(sel, 10, 64)
	var out []machine
	for _, m := range ms {
		if (idErr == nil && m.ID == id) || m.HostPort == sel {
			return []machine{m}, nil
		}
		if m.Name == sel {
			out = append(out, m)
		}
	}
	switch len(out) {
	case 0:
		return nil, xerrors.Errorf("no machine %q; use cluster_machines to list machines", sel)
	case 1:
		return out, nil
	default:
		return nil, xerrors.Errorf("machine name %q is ambiguous (%d machines); use the machine ID", sel, len(out))
	}
}

func (s *Server) selectMachine(ctx context.Context, sel string) (machine, error) {
	ms, err := s.selectMachines(ctx, sel, false)
	if err != nil {
		return machine{}, err
	}
	return ms[0], nil
}
