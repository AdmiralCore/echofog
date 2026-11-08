Русская версия: [kill-switch-windows.ru.md](kill-switch-windows.ru.md)

# Kill switch on Windows (whole-computer mode, protocol 2)

In the whole-computer mode every application of the computer goes through the VPN. The kill switch
makes that a promise: if the tunnel, the helper or the application dies, **nothing leaves the
computer outside the tunnel**, and it stays so until the user turns the protection off.

## What it is

- **Persistent filters in the Windows Filtering Platform.** The Base Filtering Engine keeps them, not
  the process that installed them, so a killed helper, a killed application, a removed adapter and a
  reboot leave the computer closed. They are installed before the tunnel comes up.
- **What is permitted outside the tunnel:** loopback; the tunnel adapter; the first hop of the live
  session to the exit (the exit itself or the relay in the path) for exactly its address and port;
  DHCP on the physical interface (so the lease can renew); IPv6 neighbour discovery with link-local
  addresses, only when the exit has an IPv6 address. **Not** the other addresses of the exit's card.
- **DNS** goes through the tunnel only: ports 53 and 853 are blocked outside it; the DNS client
  policy `DisableSmartNameResolution` and `DisableParallelAandAAAA` are set; a name resolution rule
  for all names points at the resolver inside the tunnel. IPv6 outside the tunnel is blocked.
- **Held state.** When the tunnel is lost, the filters stay and the application shows a banner:
  *"The internet is blocked on purpose …"* with the button **Disable protection**. It is also what is
  shown after the application crashed and was started again.

## Identifiers

These never change: filters of an older version would be orphaned.

| What | Value |
|---|---|
| Provider | `{d2c88793-6bad-4fdf-881b-f515c796bdd7}` "EchoFog kill switch" |
| Sublayer | `{af629ec5-2cd7-4717-af0e-b4e3e4672d24}`, weight `0xFFFF` (first) |
| Filter names | `EchoFog: <rule>`, e.g. `EchoFog: block-all-v4-out` |
| Marker file | `%ProgramData%\EchoFog\killswitch.on` (written by the elevated helper) |
| DNS rule | name resolution rule for `.` with the comment `EchoFog kill switch` |
| Policy state | `HKLM\SOFTWARE\EchoFog\KillSwitchDNS` (original DNS policy values) |

Filter weights (within the sublayer the matching filter of the highest weight decides): block
everything `1`; permits (loopback, tunnel, LAN, DHCP, neighbour discovery) `10`; the exit's address
`20`; blocking DNS ports on the physical interface `30`. Layers: `ALE_AUTH_CONNECT_V4/V6` for what the
computer starts and `ALE_AUTH_RECV_ACCEPT_V4/V6` for what arrives.

To see them: `netsh wfp show filters file=filters.xml`, then look for the provider key.

## Taking the protection off when the application will not start

Both ways need an **elevated** terminal (Run as administrator) and remove the same things: every
filter, the sublayer and the provider, the DNS rule, the DNS policy values as they were.

1. With `efv2`:

   ```
   efv2 killswitch status
   efv2 killswitch remove
   ```

2. Without any EchoFog program, with PowerShell (the script is `echofog-core-oss/deploy/v2/killswitch-remove.ps1`;
   it touches only filters whose provider key is the one above):

   ```
   powershell -ExecutionPolicy Bypass -File .\killswitch-remove.ps1
   ```

   `-List` only counts the filters and removes nothing.

There is no built-in `netsh wfp` command that deletes filters, which is why the script uses the
filtering engine's own API. As a last resort, restoring a system restore point or a VM checkpoint also
works: the filters are ordinary BFE configuration.

## Checking it (in a virtual machine only)

The filters cut the network of the computer they are installed on. **Never** run these checks on a
working or shared computer; use a Hyper-V virtual machine (take a checkpoint first) or Windows Sandbox.

1. The integration test of the driver (build once on the development computer):

   ```
   cd echofog-core-oss
   set GOWORK=off
   go test -c -tags wfpvm -o wfpvm.test.exe ./internal/v2/tun/winwfp
   ```

   Copy `wfpvm.test.exe` and `internal/v2/tun/winwfp/vm/run-wfpvm.ps1` into the VM and run the script
   in an elevated PowerShell. It refuses to run outside a VM. It installs real filters, compares
   `netsh wfp show filters` with the expected names and the provider key, checks that the Internet is
   blocked and loopback is not, replaces the plan, removes everything and checks that no filter is left.

2. The manual list (with the application, in the VM, packet capture on the physical interface of the
   VM: `pktmon start --capture --comp nics` and `pktmon filter add -t tcp udp`, or the Wireshark
   display filter `not (ip.addr == <exit address>) && not arp && not dhcp`; capture at least 5 minutes
   after each step, with background applications running):

   - kill the tunnel helper (`--tun-helper` process), then the whole application;
   - remove the adapter (`netsh interface set interface EchoFog disable` or Device Manager);
   - DNS: `nslookup example.org 8.8.8.8` fails; `nslookup example.org` answers; IPv6: `ping -6` outside fails;
   - start without administrator rights (the prompt declined): the tunnel does not come up and the
     network stays open; then reboot in the held state: the banner shows and Disable protection works;
   - Wi-Fi to Ethernet and back; sleep and wake; change of the exit while the mode is on;
   - virtual adapters (Hyper-V, WSL, Docker): they count as physical and are blocked;
   - an IPv6-only network; a captive portal with DHCP;
   - Disable protection after a failed removal.

   Criterion: not one packet leaves on the physical interface except to the exit's address (and DHCP
   and neighbour discovery on the link).

   - close the application window with the mode on: the Internet must come back;
   - refuse the administrator prompt on Disable protection: one prompt, the state stays held;
   - after a failed DNS restore: `nslookup` works or not, and what the application shows;
   - run the application without rights and delete `killswitch.on` by hand: what the banner shows.
