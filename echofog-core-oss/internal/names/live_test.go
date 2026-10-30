package names

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/dns"
)

// TestLiveTONLookup reads real names from TON mainnet liteservers. It runs only with
// ECHOFOG_LIVE_TON=1; ECHOFOG_LIVE_TON_NAMES may list more names, separated by spaces.
func TestLiveTONLookup(t *testing.T) {
	if os.Getenv("ECHOFOG_LIVE_TON") != "1" {
		t.Skip("set ECHOFOG_LIVE_TON=1 to read names from TON mainnet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := liteclient.NewConnectionPool()
	require.NoError(t, pool.AddConnectionsFromConfigUrl(ctx, "https://ton.org/global.config.json"))
	api := ton.NewAPIClient(pool, ton.ProofCheckPolicyFast).WithRetry(2)
	root, err := dns.GetRootContractAddr(ctx, api)
	require.NoError(t, err)

	r := NewResolver(NewTONSource(api, root))
	names := []string{"foundation.ton", "news.t.me", "durov.t.me"}
	if extra := os.Getenv("ECHOFOG_LIVE_TON_NAMES"); extra != "" {
		names = append(names, strings.Fields(strings.ReplaceAll(extra, ",", " "))...)
	}
	for _, n := range names {
		res, err := r.Resolve(ctx, n)
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		t.Logf("%s: %s owner=%s identity=%s problem=%s", n, res.Status, res.Owner, res.Identity, res.Problem)
	}
}
