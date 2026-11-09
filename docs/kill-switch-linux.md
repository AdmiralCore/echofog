Русская версия: [kill-switch-linux.ru.md](kill-switch-linux.ru.md)

# Kill switch on Linux (whole-computer mode, protocol 2)

In the whole-computer mode every application of the computer goes through the VPN. The kill switch
makes that a promise: if the tunnel, the helper or the application dies, **nothing leaves the
computer outside the tunnel**, and it stays so until the protection is turned off.

## What it is

- **An nftables table of the kernel.** The table `inet echofog_ks` has two chains, `out` (hook
  `output`, priority 0) and `in` (hook `input`, priority 0), both with policy `drop`. The kernel keeps
  the table, not the process that installed it: a killed helper, a killed application, a removed
  device and a crashed session leave the computer closed. Rules are installed before the tunnel comes
  up.
- **One transaction.** A replacement is one `nft -f` script (`add table`, `flush table`, new chains
  and rules), which nftables applies atomically: there is no moment without filters, and a failed
  script changes nothing.
- **What is permitted outside the tunnel:** loopback; the tunnel device `echofog0`; the first hop of
  the live session to the exit (the exit itself or the relay in the path) for exactly its address,
  port and protocol; DHCP when the plan allows it; IPv6 neighbour discovery with link-local addresses,
  only when the exit has an IPv6 address.
- **DNS** goes through the tunnel only: ports 53 and 853 are blocked on every interface but the
  tunnel, `resolvectl` makes `echofog0` the default route of systemd-resolved and takes the default
  route away from the links that had it. The servers, search domains and default-route flags of those
  links are saved before the change and given back by Restore, also from a new process after a crash.
- **Held state.** When the tunnel is lost, the filters stay and the application shows the banner with
  the button **Disable protection**. It is also what is shown after the application crashed and was
  started again.
- **A reboot clears the table.** Unlike the Windows filters, nftables rules are not restored by the
  kernel after a reboot: the network is open until the mode is turned on again. That is the same state
  as before the first start; nothing of EchoFog holds the computer closed in between.

## Identifiers

These never change: filters of an older version would be orphaned.

| What | Value |
|---|---|
| Table | `inet echofog_ks` |
| Chains | `out` = hook `output`, `in` = hook `input`; priority `0`; policy `drop` |
| Rule order (first match wins) | DNS ports `30`, endpoints `20`, permits (loopback, tunnel, DHCP, LAN, ND) `10`, block-all `1` |
| Tunnel device | `echofog0` (the TUN of v1 `tunmode`; the rules name it) |
| Marker file | `/run/echofog/killswitch.on` (written by the elevated helper) |
| DNS snapshot | `/run/echofog/killswitch-dns.json` (what `resolvectl` had before Protect) |

To see the rules: `sudo nft list table inet echofog_ks`.

The helper is the same executable started as `--ks-helper`; the prompt for root rights is `pkexec`
(polkit). The TUN device and tun2socks are started by that helper, so the computer is never asked for
rights twice.

## Taking the protection off when the application will not start

With `efv2` (it deletes the table **and** gives the DNS settings back):

```
sudo efv2 killswitch remove
efv2 killswitch status          # reads the marker, needs no root
```

Without any EchoFog program (only the filters, not DNS):

```
sudo nft delete table inet echofog_ks
sudo resolvectl revert echofog0
sudo resolvectl flush-caches
```

`efv2 killswitch remove` on a computer that has none of it removes nothing and succeeds. Without root
it says so and changes nothing.

## Requirements

`nft` (nftables), `resolvectl` (systemd-resolved), `ip` (iproute2), polkit for the prompt, and a
kernel with TUN and nftables. Without systemd-resolved the DNS part is refused and the mode does not
start: the filters would block DNS and the tunnel would have no resolver. A redirect fallback for
resolv.conf-only systems is not part of this version.

## Checking it (in WSL or a virtual machine only)

The table cuts the network of the computer it is installed on. **Never** run these checks on a
working or shared computer: use a WSL instance or a virtual machine (take a checkpoint first).

1. The integration test (root; installs real rules, kills the helper with `kill -9`, captures on
   `eth0`, then removes everything):

   ```
   cd echofog-core-oss
   sudo EF_KS_INTEGRATION=1 go test -tags kslinux ./internal/v2/tun/platform -run Integration -v
   ```

   It checks that the table stands after `kill -9`, that a permitted datagram reaches `eth0` while a
   datagram to port 53 does not, and that `efv2 killswitch remove` (the same `RemoveAll`) gives the
   DNS settings back. It uses a dummy `echofog0` for the DNS part; the real TUN and tun2socks need a
   live exit and are checked by hand.

2. The manual list (in the VM, with a live node and packet capture on the physical interface:
   `sudo tcpdump -i eth0 -n 'udp port 53 or port 853'`, at least 5 minutes after each step, with
   background applications running):

   - kill the helper (`kill -9` of the `--ks-helper` process), then the whole application;
   - remove the device (`sudo ip link del echofog0`);
   - DNS: `resolvectl query example.org` uses `198.18.0.2`; a query to a public resolver on port 53
     does not leave `eth0`; IPv6 outside the tunnel is blocked;
   - start without root (the polkit prompt declined): the tunnel does not come up and the network
     stays open; then start again in the held state: the banner shows and Disable protection works;
   - Wi-Fi to Ethernet and back; sleep and wake; change of the exit while the mode is on;
   - an IPv6-only network; a captive portal with DHCP;
   - Disable protection after a failed removal.

   Criterion: not one packet leaves on the physical interface except to the exit's address (and DHCP
   and neighbour discovery on the link), and nothing on ports 53 and 853 ever does.
