package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"github.com/echofog/echofog-core/internal/v2/tun/platform"
)

// Replaceable for the tests: the real ones touch the firewall of the computer.
var (
	killswitchRemove = platform.RemoveAll
	killswitchStands = platform.FiltersStand
)

// cmdKillswitch is the manual way back when the application cannot be started and its kill switch
// holds the computer closed: efv2 killswitch remove (from an elevated terminal) deletes every filter,
// the sublayer and the provider of the kill switch, the DNS rule and the DNS policy values.
func cmdKillswitch(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: efv2 killswitch remove|status")
	}
	ctx := context.Background()
	switch args[0] {
	case "status":
		if killswitchStands(ctx) {
			fmt.Fprintln(out, "the filters of the kill switch stand (remove them: efv2 killswitch remove, from an elevated terminal)")
		} else {
			fmt.Fprintln(out, "no filters of the kill switch are known to stand")
		}
		return nil
	case "remove":
		rep, err := killswitchRemove(ctx)
		switch {
		case errors.Is(err, tun.ErrNoPrivilege):
			return errors.New("administrator rights are needed: open an elevated terminal (Run as administrator) and repeat")
		case errors.Is(err, tun.ErrUnsupported):
			return errors.New("the kill switch has no driver on this system")
		case err != nil:
			return fmt.Errorf("removal incomplete (%d filters removed): %w", rep.Filters, err)
		case rep.Filters == 0:
			fmt.Fprintln(out, "nothing to remove: no filters of the kill switch stand")
		default:
			fmt.Fprintf(out, "removed %d filters of the kill switch; the network is back\n", rep.Filters)
		}
		return nil
	}
	return fmt.Errorf("unknown killswitch action %q (remove or status)", args[0])
}
