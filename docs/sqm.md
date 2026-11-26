# Smart queue management (SQM)

**What it is for.** When a connection is full (someone uploads a video, a game downloads), the packets of everything else wait in a queue.
At the provider's equipment that queue can hold hundreds of milliseconds of traffic ("bufferbloat"), and calls, games and page loads feel
broken. SQM makes the *router* hold the queue instead, a little below the line's real speed, where it can be kept short and shared fairly. Routers
only (an access point has no connection to shape).

## What a person sees

Internet page, card **Smart queue management** (a draft like every other change; shaping starts at the commit):

- **Shape the internet connection**: off by default. Off writes nothing and changes nothing on the connection.
- **Download speed** and **Upload speed** (Mbit/s): *there is no sensible default*, because it is the speed of **your** line, and a number that is
  too high does nothing while one that is too low wastes speed. The page says how to find it: start at **90 to 95 %** of what a speed test shows
  with nothing else on the line, test with a bufferbloat test (waveform.com/tools/bufferbloat), and lower it a little if the grade does not rise.
  Leave one empty to leave that direction alone (a line that only needs the upload shaped, say). At least one is needed.
- **Kind of connection** (default **fibre or an Ethernet hand-off**): the link adds bytes to every packet that the router does not see
  (headers of the cable system, of DSL cells...), and cake counts them so the speeds mean the line's speeds. Cable (DOCSIS), DSL VDSL2 or
  ADSL (PPPoE) have their own. A PPPoE connection left on the Ethernet kind gets a note in the review.
- **Share the line fairly between devices**: on by default. One device with many connections cannot take the line from the others.
- While it runs: per direction the speed, how long the queue made traffic wait on average and at worst, and how many packets it dropped (a few
  dropped packets are how a full queue tells busy connections to slow down; it is what keeps the wait short).

## How it works

- **Out** (upload): the `cake` queue discipline on the connection's device (`WanDevice`: the port, its VLAN interface, or `ppp0`) at the upload
  speed, with `diffserv3`, `nat` (cake looks into the connection tracking, so it knows which device behind the NAT a packet belongs to; it is what
  makes the per-device fairness work) and `dual-srchost` (or `triple-isolate` without the per-device switch).
- **In** (download): what arrives is redirected (`tc ... matchall action mirred`) to an **ifb** device (`ifb-wan`) and cake shapes it there in
  `ingress` mode, with `dual-dsthost`, `nat` and `wash` (the provider's DSCP marks are cleared, since they mean nothing here).
- The connection kind picks cake's own keyword (`ethernet`, `docsis`, `pppoe-ptm` for VDSL, `pppoe-vcmux` for ADSL) which sets the overhead and the
  smallest packet it counts.
- **Files and service.** The translator writes `/etc/default/peakroute-sqm` (`SQM_DEV`, `SQM_DOWN_KBIT`, `SQM_UP_KBIT`, `SQM_LINK`,
  `SQM_PERHOST`) while shaping is on and removes it when it is off. The action (service `sqm`, verb *apply*) runs
  `/usr/lib/peakroute/sqm reconcile`: with the file the kernel is made to match (replacing, never stacking), without it everything is
  taken away. Switching it on, off or changing a speed touches **nothing else**: no networking restart, no firewall reload, no DHCP restart.
- **Coming back up.** `tc` settings do not survive an interface going away. `if-up.d/peakroute-sqm` (every interface that comes up) and
  `ppp/ip-up.d/peakroute-sqm` (a PPPoE session is a new `ppp0` each time) run `sqm start-if <device>`, which shapes it only if it is the connection's device
  and never fails the interface coming up. A device that is not there yet is shaped when it appears.
- Every cake keyword is given every time: replacing a queue keeps what is not named, so a setting taken away would otherwise stay.
- The image's kernel has cake, ifb, mirred and matchall as modules (Debian's); the helper loads them. A commit on an image from before this
  feature fails with "smart queue management is not in this system image: update to a version that has it", changing nothing.

## Checked, and not

Checked with the real kernel's cake and tc in throw-away network namespaces: the helper puts the queues on the device and the ifb with the
right options, a second run does not stack, another device is left alone, upload-only removes the ifb, a device that is not there is not an
error, and taking the file away takes everything away. **Shaping really limits the rate:** a TCP stream across a veth pair that ran at 13.7 Gbit/s
unshaped was held to 7.5 Mbit/s at a setting of 8 Mbit/s (goodput, with the Ethernet overhead counted) and to 47.1 at 50, and an 8 Mbit/s shaping of
the incoming side did the same. **Not checked:** a real provider's line, the bufferbloat grade, CPU cost on the router's hardware (cake is
the heavier queue: a small router may not keep up at gigabit speeds), and the interface hooks under real ifupdown and pppd.

## Left out for now

Automatic speed measurement or adjustment (cake-autorate), per-network or per-device speed limits, a choice of DSCP handling (diffserv4,
besteffort), ACK filtering, and shaping a second internet connection (when there is one).
