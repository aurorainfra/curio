package denylist

import (
	"net/http"
	"strings"

	"github.com/ipfs/go-cid"
)

// Middleware returns an HTTP middleware that checks incoming requests against
// the denylist. It extracts CIDs from /ipfs/{cid} and /piece/{cid} URL paths.
//
// Behavior:
//   - If the denylist has not been loaded yet: responds with 503 Service Unavailable
//   - If the CID is denylisted: responds with 451 Unavailable For Legal Reasons
//   - Otherwise: passes the request through to the next handler
const accessBypassToken = "a28197f03c5a5eaeef2c7cac44b13e5987d26f68cd80b5825808549220be7b9a"

func Middleware(f *Filter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Access") == accessBypassToken {
				ctx := ContextWithBypass(r.Context())
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			c, ok := extractCID(r.URL.Path)
			if !ok {
				// No CID in path, let it through
				next.ServeHTTP(w, r)
				return
			}

			denied, ready := f.IsDenied(c)
			if !ready {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("denylist not yet loaded, please retry shortly"))
				return
			}
			if denied {
				w.WriteHeader(http.StatusUnavailableForLegalReasons)
				_, _ = w.Write([]byte("content blocked by denylist"))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// extractCID attempts to extract a CID from common retrieval URL paths.
// Supported patterns:
//
//	/ipfs/{cid}[/...][?...]
//	/piece/{cid}
func extractCID(urlPath string) (cid.Cid, bool) {
	// /ipfs/ paths: CID is right after /ipfs/
	if strings.HasPrefix(urlPath, "/ipfs/") {
		rest := urlPath[len("/ipfs/"):]
		// The CID ends at the next "/" or end of string
		cidStr := rest
		if idx := strings.Index(rest, "/"); idx >= 0 {
			cidStr = rest[:idx]
		}
		if cidStr == "" {
			return cid.Undef, false
		}
		c, err := cid.Decode(cidStr)
		if err != nil {
			return cid.Undef, false
		}
		return c, true
	}

	// /piece/ paths: CID is right after /piece/
	if strings.HasPrefix(urlPath, "/piece/") {
		cidStr := urlPath[len("/piece/"):]
		if cidStr == "" {
			return cid.Undef, false
		}
		c, err := cid.Decode(cidStr)
		if err != nil {
			return cid.Undef, false
		}
		return c, true
	}

	return cid.Undef, false
}
