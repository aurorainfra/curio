package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/deps"
	"github.com/filecoin-project/curio/deps/config"
)

func hashTok(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

const (
	tokA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestTokenAuth(t *testing.T) {
	toks := config.NewDynamic([]config.AuroraMCPToken{
		{Name: "agent-a", TokenSHA256: hashTok(tokA)},
		{Name: "agent-b", TokenSHA256: hashTok(tokB), Disabled: true},
		{Name: "broken", TokenSHA256: "nothex"},
	})
	a := newTokenAuth(toks)

	agent, ok := a.authenticate(tokA)
	require.True(t, ok)
	require.Equal(t, "agent-a", agent)

	_, ok = a.authenticate(tokB)
	require.False(t, ok, "disabled token")
	_, ok = a.authenticate(hashTok(tokA))
	require.False(t, ok, "the hash is not the token")
	_, ok = a.authenticate("short")
	require.False(t, ok)
	_, ok = a.authenticate("")
	require.False(t, ok)

	// rotation: replace agent-a's token at runtime
	toks.Set([]config.AuroraMCPToken{{Name: "agent-a", TokenSHA256: hashTok(tokB)}})
	_, ok = a.authenticate(tokA)
	require.False(t, ok)
	agent, ok = a.authenticate(tokB)
	require.True(t, ok)
	require.Equal(t, "agent-a", agent)
}

func testServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := config.DefaultCurioConfig()
	cfg.Aurora.MCP.Enable = true
	cfg.Aurora.MCP.Tokens.Set([]config.AuroraMCPToken{{Name: "agent-a", TokenSHA256: hashTok(tokA)}})
	return Handler(&deps.Deps{Cfg: cfg})
}

type bearer struct {
	tok  string
	next http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.tok != "" {
		r.Header.Set("Authorization", "Bearer "+b.tok)
	}
	return b.next.RoundTrip(r)
}

func connect(ctx context.Context, url, tok string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	return c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: bearer{tok: tok, next: http.DefaultTransport}},
	}, nil)
}

func TestMCPEndToEnd(t *testing.T) {
	srv := httptest.NewServer(testServer(t))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// no / wrong token
	for _, tok := range []string{"", tokB} {
		req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		require.NoError(t, err)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}
	_, err := connect(ctx, srv.URL, tokB)
	require.Error(t, err)

	cs, err := connect(ctx, srv.URL, tokA)
	require.NoError(t, err)
	defer func() { _ = cs.Close() }()

	tools, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		require.NotNil(t, tl.Annotations)
		require.True(t, tl.Annotations.ReadOnlyHint, tl.Name)
		require.NotEmpty(t, tl.Description, tl.Name)
	}
	for _, want := range []string{
		"cluster_machines", "alerts", "tasks_active", "tasks_stuck", "task_history", "task_detail", "task_timing_stats",
		"wdpost_deadlines", "wdpost_partition", "storage_paths", "sector_locate", "storage_path_sectors", "missing_refs",
		"storage_diag", "log_files", "log_read", "log_grep", "log_tail", "log_find_time", "goroutines",
		"log_query", "log_job", "log_job_lines", "log_job_cancel", "log_jobs",
		"storage_overview", "storage_disks", "storage_disk_detail", "storage_sas", "partition_sectors", "path_partitions",
		"market_backpressure", "market_decisions", "market_pipeline", "node_metrics", "config_effective",
	} {
		require.Contains(t, names, want)
	}

	// schema validation rejects bad input before the handler runs
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "task_detail", Arguments: map[string]any{"task_id": "x"}})
	require.NoError(t, err)
	require.True(t, res.IsError)

	// handler errors come back as tool errors
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "wdpost_deadlines", Arguments: map[string]any{"sp": "f01000"}})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Content[0].(*mcp.TextContent).Text, "no chain connection")
}

func TestMCPDisabled(t *testing.T) {
	cfg := config.DefaultCurioConfig()
	h := Handler(&deps.Deps{Cfg: cfg})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestQueryCatalogReadOnly checks every SQL constant in queries.go is a
// single read-only SELECT that calls only builtin functions.
func TestQueryCatalogReadOnly(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "queries.go", nil, 0)
	require.NoError(t, err)

	forbidden := regexp.MustCompile(`(?i)\b(insert|update|delete|merge|upsert|truncate|alter|create|drop|grant|revoke|call|do|copy|lock|vacuum|analyze|refresh|listen|notify|set|reset|prepare|execute|nextval|setval|lo_\w+|dblink\w*|pg_\w+|into|for\s+update|for\s+share|returning)\b`)
	// function calls: identifier followed by "("
	callRe := regexp.MustCompile(`(?i)\b([a-z_][a-z0-9_.]*)\s*\(`)
	allowedFuncs := map[string]bool{
		"count": true, "min": true, "max": true, "sum": true, "avg": true, "coalesce": true, "nullif": true,
		"greatest": true, "least": true, "now": true, "extract": true, "left": true, "bool_or": true,
		"percentile_cont": true, "to_jsonb": true, "any": true, "in": true, "filter": true, "exists": true,
		"lateral": true, "unnest": true, "over": true, "as": true, "using": true, "select": true, "from": true,
		"where": true, "and": true, "or": true, "not": true, "when": true, "then": true, "else": true, "on": true,
		"join": true, "values": true, "by": true, "group": true, "within": true, "distinct": true,
		"date_trunc": true, "strpos": true, "lower": true,
	}

	n := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		require.Equal(t, token.CONST, gd.Tok, "queries.go must only contain constants")
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, v := range vs.Values {
				name := vs.Names[i].Name
				lit, ok := v.(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "%s must be a string literal", name)
				q, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				n++

				// strip string literals ('WdPost') before checking keywords
				stripped := regexp.MustCompile(`'[^']*'`).ReplaceAllString(q, "''")
				trimmed := strings.ToUpper(strings.TrimSpace(stripped))
				require.True(t, strings.HasPrefix(trimmed, "SELECT") || strings.HasPrefix(trimmed, "WITH"), "%s must start with SELECT or WITH", name)
				require.NotContains(t, stripped, ";", "%s must be a single statement", name)
				require.NotContains(t, stripped, "--", "%s must not contain comments", name)
				require.NotContains(t, stripped, "/*", "%s must not contain comments", name)
				m := forbidden.FindString(stripped)
				require.Empty(t, m, "%s contains forbidden keyword %q", name, m)
				for _, c := range callRe.FindAllStringSubmatch(stripped, -1) {
					fn := strings.ToLower(c[1])
					require.True(t, allowedFuncs[fn], "%s calls non-allowlisted function %q", name, fn)
				}
			}
		}
	}
	require.Greater(t, n, 20)
}

func TestRedact(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJBbGxvdyI6WyJhZG1pbiJdfQ.c2lnbmF0dXJlc2lnbmF0dXJl"
	in := `{"err":"call failed: Authorization: Bearer ` + jwt + ` and secret c2VjcmV0c2VjcmV0 ok","api":"` + jwt + `:/ip4/1.2.3.4/tcp/1234/http"}`
	out := redact(in, []string{"c2VjcmV0c2VjcmV0"})
	require.NotContains(t, out, jwt)
	require.NotContains(t, out, "c2VjcmV0c2VjcmV0")
	require.Contains(t, out, "call failed")
	require.Contains(t, out, redacted)

	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----"
	require.Equal(t, "x "+redacted+" y", redact("x "+pem+" y", nil))

	s, tr := capOutput(strings.Repeat("é", 10), 5)
	require.True(t, tr)
	require.True(t, strings.HasPrefix(s, "éé"))
}

func TestParseTime(t *testing.T) {
	tm, err := parseTime("2026-10-18T12:00:00Z")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), *tm)

	tm, err = parseTime("2h")
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(-2*time.Hour), *tm, time.Minute)
	tm2, err := parseTime("-2h")
	require.NoError(t, err)
	require.WithinDuration(t, *tm, *tm2, time.Second)

	tm, err = parseTime("")
	require.NoError(t, err)
	require.Nil(t, tm)

	_, err = parseTime("yesterday-ish")
	require.Error(t, err)

	start, end, err := timeWindow("90d", "", time.Hour)
	require.Error(t, err) // "90d" is not a Go duration
	_ = start
	_ = end
	start, end, err = timeWindow("2000h", "", time.Hour)
	require.NoError(t, err)
	require.Equal(t, maxWindow, end.Sub(start))
}
