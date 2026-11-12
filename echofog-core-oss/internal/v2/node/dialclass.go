package node

import (
	"context"
	"errors"
	"net"

	"github.com/echofog/echofog-core/internal/v2/session"
)

// Failure classes of a dial for the connection log (T11.2, V2-FIELD-1). A class never names an
// address, an entry line or a key: the log says why one entry gave nothing, not where it points.
const (
	FailPolicy     = "policy"     // the country policy left no allowed carrier
	FailBackground = "background" // the app is suspended in the background
	FailProxy      = "proxy"      // the configured proxy or its network is unusable
	FailNoAnswer   = "no-answer"  // a carrier connected but the peer closed or never answered
	FailBadEntry   = "bad-entry"  // the entry line or the card it names is not usable
	FailTimeout    = "timeout"    // the dial ran out of time
	FailOther      = "other"      // nothing more precise; still never the address
)

// DialFailureClass maps a dial error to one of the classes the journal names. The checks are
// ordered: a policy refusal that wraps a timeout is a policy refusal, and a proxy error that
// wraps a network error is a proxy error.
func DialFailureClass(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrNoDomesticEntry), errors.Is(err, ErrRouteUnsupported), errors.Is(err, errPolicyBlocked):
		return FailPolicy
	case errors.Is(err, ErrBackground):
		return FailBackground
	case errors.Is(err, ErrEgressConfig), errors.Is(err, ErrEgressNetwork):
		return FailProxy
	case errors.Is(err, ErrBootstrapTarget), errors.Is(err, ErrNoPeerCard), errors.Is(err, ErrPeerCard), errors.Is(err, ErrConfig):
		return FailBadEntry
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return FailTimeout
	case errors.Is(err, session.ErrSilent), errors.Is(err, ErrNoRoute), errors.Is(err, ErrNoSession):
		return FailNoAnswer
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return FailTimeout
	}
	return FailOther
}
