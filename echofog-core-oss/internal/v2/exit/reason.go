package exit

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/echofog/echofog-core/internal/network/exit_gateway/core"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Reason says why an exit refused or could not be used. It is what the facade (T11.4) shows
// instead of v1's single "exit relay is not connected". The payment reasons (PaymentRequired,
// FreeQuotaUsed, PriceChanged) belong to T10.3 and read as ReasonUnknown until then.
type Reason uint8

const (
	// ReasonUnknown is an error that is not an exit refusal.
	ReasonUnknown Reason = iota
	// ReasonNoCamouflage: the exit has no camouflaged address (reality2 or ws2) and could not be
	// reached; strangers cannot use it (D37).
	ReasonNoCamouflage
	// ReasonUnreachable: refused, reset, timed out or DNS failure, all alike (D19).
	ReasonUnreachable
	// ReasonBusy: the exit is out of tunnel slots for this client.
	ReasonBusy
	// ReasonPolicy: the exit's policy forbids the target or the request (denied, blocked port,
	// private range, CLOSE policy).
	ReasonPolicy
	// ReasonRateLimited: the client's own rate limit at the exit was hit.
	ReasonRateLimited
	// ReasonNoPath: no session to the exit could be set up.
	ReasonNoPath
)

var reasonText = map[Reason]struct{ code, msg string }{
	ReasonUnknown:      {"unknown", "Причина отказа неизвестна."},
	ReasonNoCamouflage: {"no-camouflage", "У сервера нет обложки: подключиться к нему не удалось."},
	ReasonUnreachable:  {"unreachable", "Узел назначения недоступен."},
	ReasonBusy:         {"busy", "Сервер занят: исчерпан лимит одновременных соединений."},
	ReasonPolicy:       {"policy", "Сервер не разрешает такое соединение."},
	ReasonRateLimited:  {"rate-limited", "Слишком много запросов к серверу: нужно немного подождать."},
	ReasonNoPath:       {"no-path", "Не удалось установить соединение с сервером."},
}

// String is the stable code of the reason (logs, events, the facade's enum).
func (r Reason) String() string {
	if t, ok := reasonText[r]; ok {
		return t.code
	}
	return reasonText[ReasonUnknown].code
}

// Message is the neutral Russian text for the user.
func (r Reason) Message() string {
	if t, ok := reasonText[r]; ok {
		return t.msg
	}
	return reasonText[ReasonUnknown].msg
}

// Refusal is an exit refusal with its reason. Its text is fixed per reason: it never carries the
// target, the exit's address or the underlying cause (AD20), so two causes that share a reason
// cannot be told apart by what the caller sees.
type Refusal struct {
	Reason Reason
	cause  error
}

func (e *Refusal) Error() string { return "exit refused: " + e.Reason.String() }

// Unwrap returns the underlying error, for errors.Is; it is never shown to users.
func (e *Refusal) Unwrap() error { return e.cause }

// refuse wraps err in a Refusal; an error that already is one is returned as it is.
func refuse(r Reason, err error) error {
	var ref *Refusal
	if errors.As(err, &ref) {
		return err
	}
	return &Refusal{Reason: r, cause: err}
}

// ReasonOf classifies an error returned by the client or read from a tunnel. Errors that are not
// exit refusals (cancellation, EOF, invalid target) are ReasonUnknown.
func ReasonOf(err error) Reason {
	if err == nil {
		return ReasonUnknown
	}
	var ref *Refusal
	if errors.As(err, &ref) {
		return ref.Reason
	}
	var ce *mux.CloseError
	if errors.As(err, &ce) && ce.Code == CodePolicy {
		return ReasonPolicy
	}
	switch {
	case errors.Is(err, core.ErrExitBusy):
		return ReasonBusy
	case errors.Is(err, core.ErrExitDenied), errors.Is(err, core.ErrExitDisabled):
		return ReasonPolicy
	case errors.Is(err, core.ErrExitDialFailed):
		return ReasonUnreachable
	}
	return ReasonUnknown
}

// CardHasCamouflage reports whether the card has an address strangers can reach through a cover
// (reality2-quic, reality2-tcp or ws2, D37).
func CardHasCamouflage(c *objects.Card) bool {
	if c == nil {
		return false
	}
	for _, a := range c.Addrs() {
		switch a.GetSuite() {
		case objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2:
			return true
		}
	}
	return false
}

// resultReason maps an exit/2 RESULT code to its reason; ResultOK has none.
func resultReason(code uint32) Reason {
	switch code {
	case ResultDenied, ResultBlockedPort:
		return ReasonPolicy
	case ResultUnreachable, ResultDNSFailed:
		return ReasonUnreachable
	case ResultBusy:
		return ReasonBusy
	case ResultRateLimited:
		return ReasonRateLimited
	}
	return ReasonUnknown
}

// noPath is the reason a failure to set up the session gets.
func (c *Client) noPath() Reason {
	if c.cfg.NoCamouflage {
		return ReasonNoCamouflage
	}
	return ReasonNoPath
}

// dialErr turns an error of the dial step into a Refusal. Cancellation, an invalid target and a
// closed client are the caller's own doing and stay as they are.
func (c *Client) dialErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, core.ErrInvalidTarget), errors.Is(err, ErrClientClosed),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	if r := ReasonOf(err); r != ReasonUnknown {
		return refuse(r, err)
	}
	if errors.Is(err, core.ErrPaymentRequired) {
		return err // T10.3
	}
	return refuse(c.noPath(), err)
}

// refusalStream gives the errors of a tunnel their Reason. It forwards what the SOCKS5 layer and
// the relay look for: the verdict callback, half-close and deadlines.
type refusalStream struct {
	io.ReadWriteCloser
	cs *clientStream
}

// mapErr turns a verdict or read error into a Refusal when it is one. The exit's own RESULT code
// decides: the v1 core folds codes it does not know into "dial failed".
func (s *refusalStream) mapErr(err error) error {
	if err == nil {
		return nil
	}
	if code, ok := s.cs.resultCode(); ok && code != ResultOK && errors.Is(err, core.ErrExitDialFailed) {
		if r := resultReason(code); r != ReasonUnknown {
			return refuse(r, err)
		}
	}
	if r := ReasonOf(err); r != ReasonUnknown {
		return refuse(r, err)
	}
	return err
}

func (s *refusalStream) Read(p []byte) (int, error) {
	n, err := s.ReadWriteCloser.Read(p)
	return n, s.mapErr(err)
}

func (s *refusalStream) Write(p []byte) (int, error) {
	n, err := s.ReadWriteCloser.Write(p)
	return n, s.mapErr(err)
}

// OnVerdict implements core.VerdictNotifier.
func (s *refusalStream) OnVerdict(f func(error)) {
	if v, ok := s.ReadWriteCloser.(core.VerdictNotifier); ok {
		v.OnVerdict(func(err error) { f(s.mapErr(err)) })
	}
}

func (s *refusalStream) CloseWrite() error {
	if cw, ok := s.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (s *refusalStream) SetDeadline(t time.Time) error {
	if d, ok := s.ReadWriteCloser.(interface{ SetDeadline(time.Time) error }); ok {
		return d.SetDeadline(t)
	}
	return nil
}

func (s *refusalStream) SetReadDeadline(t time.Time) error {
	if d, ok := s.ReadWriteCloser.(interface{ SetReadDeadline(time.Time) error }); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}
