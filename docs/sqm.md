# Smart queue management (SQM)

**What it is for.** When a connection is full (someone uploads a video, a game downloads), the packets of everything else wait in a queue.
At the provider's equipment that queue can hold hundreds of milliseconds of traffic ("bufferbloat"), and calls, games and page loads feel
broken. SQM makes the *router* hold the queue instead, a little below the line's real speed, where it can be kept short and shared fairly. Routers
only (an access point has no connection to shape).

## What a person sees

Internet page, card **Smart queue management** (a draft like every other change; shaping starts at the commit):

- **Shape the internet connection**: off by default. Off writes nothing and changes nothing on the connection.
- **Your line**: **Download speed** and **Upload speed** (Mbit/s), the speeds of **your plan** (what the provider sells), typed by hand, **or
  measured** with **Measure my speed**. Leave one empty to leave that direction alone; at least one is needed.
- **Measure my speed** runs a speed test from the router (about twenty seconds) with Cloudflare's public test (`speed.cloudflare.com`: a download
  of random bytes and an upload that is thrown away) and puts the two numbers into the fields: *a test changes nothing by itself*, the person
  checks them and saves. It uses up to about 400 MB each way (less on a slower line), says so on the page, and is an estimate: four connections,
  the first second left out while they warm up, the upload counted by what the other end **acknowledged** (TCP's own count, not what was handed
  to the send buffer, which at a slow upload would be seconds of data and pass for speed). **While shaping is on, it is taken off for the
  test and put back after** (otherwise the test would measure the shaping), whatever the outcome.
- **Share of your line (%)**: what the router shapes to is the line's speed times this, **95 % by default** (50 to 100). A line rarely
  delivers all of what it is sold at, and the queue only works while the router is the slowest point of the path. The page shows the result
  ("will shape to 475 Mbit/s down, 38 Mbit/s up") as it is typed. If a bufferbloat test (waveform.com/tools/bufferbloat) after committing does not
  improve, lower it a little (90, then 85). The default is not written to the file.
- **Kind of connection** (default **fibre or an Ethernet hand-off**): the link adds bytes to every packet that the router does not see
  (headers of the cable system, of DSL cells...), and cake counts them so the speeds mean the line's speeds. Cable (DOCSIS), DSL VDSL2 or
  ADSL (PPPoE) have their own. A PPPoE connection left on the Ethernet kind gets a note in the review.
- **Share the line fairly between devices**: on by default. One device with many connections cannot take the line from the others.
- While it runs: per direction the speed, how long the queue made traffic wait on average and at worst, and how many packets it dropped (a few
  dropped packets are how a full queue tells busy connections to slow down; it is what keeps the wait short).

## Per network: priorities and limits

By default the whole line is one queue, fair between devices whatever network they are on. Under **Per network** each network (the default one too)
can be given a **priority** and **limits**; until one is changed (all *Normal*, no limits) nothing below happens and the queue is the one above.

- **Priority** (*Low*, *Normal*, *High*): when the line is full, a network is **assured a share of it by its weight**, high 4, normal 2, low 1
  (and "everything else", such as the router's own traffic and the VPNs, normal 2); and the queue **serves a higher priority first**, so its
  packets wait less. A network that does not use its share lets the others use it (a share is a floor, not a wall).
- **Download limit** and **Upload limit** (Mbit/s, empty: none): the most the network may use in that direction whatever the others are doing,
  for a guest network that must not eat the line. A limit under the network's assured share is its share too.
- The card shows what each network is assured. Settings are kept while shaping is off, and a limit in a direction that is not shaped has nothing
  to limit: both are said so under the network ("does nothing while ... is off"), and neither stops a commit.
- The share is of the *shaped* line (the plan times the percentage), so a High network on a 450 Mbit/s shaped line is assured 4/7 of it
  when the LAN, a guest network and "everything else" all want it.

How: the one cake queue becomes a tree. An **htb** class for the whole line (counting the link's overhead through a size table), a class below it for each
network with its assured rate, its ceiling and its priority, and in each a **cake** queue (`unlimited`, so it only shares the class between the
devices and keeps its delay short). The packets are told apart by a **mark on their connection**: the firewall (a table `peakroute_qos` in
the same file as the rules, so a reload cannot lose it) marks every connection a network starts, and every one started towards it (a forwarded
port), with the network's number in bits 16 to 23 (`ct mark`, which nothing else here uses). What goes **out** carries the mark on the packet
(`meta mark set ct mark`) and the queue sorts by it. What comes **in** reaches the `ifb` before the firewall has seen it, so the `ctinfo` action
reads the mark from the connection tracking on its way there, and the same kind of tree sorts it. A connection that was open before the marks were
written is marked when its next packet passes. The file for the helper gets one line per network (`SQM_CLASSES`).


- **Out** (upload): the `cake` queue discipline on the connection's device (`WanDevice`: the port, its VLAN interface, or `ppp0`) at the upload
  speed, with `diffserv3`, `nat` (cake looks into the connection tracking, so it knows which device behind the NAT a packet belongs to; it is what
  makes the per-device fairness work) and `dual-srchost` (or `triple-isolate` without the per-device switch).
- **In** (download): what arrives is redirected (`tc ... matchall action mirred`) to an **ifb** device (`ifb-wan`) and cake shapes it there in
  `ingress` mode, with `dual-dsthost`, `nat` and `wash` (the provider's DSCP marks are cleared, since they mean nothing here).
- The connection kind picks cake's own keyword (`ethernet`, `docsis`, `pppoe-ptm` for VDSL, `pppoe-vcmux` for ADSL) which sets the overhead and the
  smallest packet it counts.
- **Files and service.** The translator writes `/etc/default/peakroute-sqm` (`SQM_DEV`, `SQM_DOWN_KBIT`, `SQM_UP_KBIT`, `SQM_LINK`,
  `SQM_PERHOST`; the speeds are what the router shapes to, the line's speeds times the percentage) while shaping is on and removes it when it is off. The action (service `sqm`, verb *apply*) runs
  `/usr/lib/peakroute/sqm reconcile`: with the file the kernel is made to match (replacing, never stacking), without it everything is
  taken away. Switching it on, off or changing a speed touches **nothing else**: no networking restart, no firewall reload, no DHCP restart.
- **Coming back up.** `tc` settings do not survive an interface going away. `if-up.d/peakroute-sqm` (every interface that comes up) and
  `ppp/ip-up.d/peakroute-sqm` (a PPPoE session is a new `ppp0` each time) run `sqm start-if <device>`, which shapes it only if it is the connection's device
  and never fails the interface coming up. A device that is not there yet is shaped when it appears.
- Every cake keyword is given every time: replacing a queue keeps what is not named, so a setting taken away would otherwise stay.
- The image's kernel has cake, ifb, mirred and matchall as modules (Debian's); the helper loads them. A commit on an image from before this
  feature fails with "smart queue management is not in this system image: update to a version that has it", changing nothing.

## Checked, and not

**Per network, with the real kernel** (three network namespaces: a client on each of two networks behind a router with NAT, and a server on the
internet side; the helper and the firewall table as the translator writes them): a guest network limited to **8 Mbit/s down and 6 up** got **7.7 and 5.6**,
while the other network, alone, got **57.4 of the 60 down and 38.2 of the 40 up** the line was shaped to; and with both networks downloading flat
out over a 20 Mbit/s line, the **high** priority network got **16.4** and the **low** one **2.7**. The same tests are in the suite (about 35 seconds).

Checked with the real kernel's cake and tc in throw-away network namespaces: the helper puts the queues on the device and the ifb with the
right options, a second run does not stack, another device is left alone, upload-only removes the ifb, a device that is not there is not an
error, and taking the file away takes everything away. **Shaping really limits the rate:** a TCP stream across a veth pair that ran at 13.7 Gbit/s
unshaped was held to 7.5 Mbit/s at a setting of 8 Mbit/s (goodput, with the Ethernet overhead counted) and to 47.1 at 50, and an 8 Mbit/s shaping of
the incoming side did the same. The speed test is checked against a server that moves a known rate (it measures within 30 %), the data cap, a failing server, and a cancelled test; the two Cloudflare endpoints answered a 100 kB download and a 100 kB upload with 200. **Not checked:** a real provider's line, the test's accuracy on one, the bufferbloat grade, CPU cost on the router's hardware (cake is
the heavier queue: a small router may not keep up at gigabit speeds), and the interface hooks under real ifupdown and pppd.

## Left out for now

Automatic adjustment of the speeds over time (cake-autorate), choosing the speed test server, limits per device, limits per time of day, traffic of the VPNs and of the router itself as classes of their own (they are "everything else"), a choice of DSCP handling (diffserv4,
besteffort), ACK filtering, and shaping a second internet connection (when there is one).
