// web/static/scripts/firewall_connections.js
// Connections page: the kernel's table of connections passing through the router, the busiest first, with the
// device names from DHCP, what the router translated (NAT) and how much data each carried. Read only.

window.FirewallConnectionsModule = (function () {
    const STEP = 200;
    let report = null;          // GET /api/v1/firewall/connections
    let names = new Map();      // IP address -> device name, from the DHCP leases
    let timer = null;
    let paused = false;
    let shown = STEP;
    let wired = false;

    const $ = (id) => document.getElementById(id);

    const TCP_STATES = {
        ESTABLISHED: 'Established', SYN_SENT: 'Connecting', SYN_RECV: 'Connecting', FIN_WAIT: 'Closing', CLOSE_WAIT: 'Closing',
        LAST_ACK: 'Closing', TIME_WAIT: 'Closed, waiting', CLOSE: 'Closed',
    };

    // "4d 23h", "5m 20s": two units at most, a connection's last seconds do not matter when it lives for days
    function coarse(sec) {
        const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
        if (d) return `${d}d ${h}h`;
        if (h) return `${h}h ${m}m`;
        if (m) return `${m}m ${s}s`;
        return `${s}s`;
    }

    function stateText(c) {
        if (c.proto === 'tcp') return TCP_STATES[c.state] || c.state || 'Active';
        return c.unreplied ? 'Waiting for a reply' : 'Active';
    }

    const known = (c) => (['tcp', 'udp', 'icmp'].includes(c.proto) ? c.proto : 'other');

    function hostName(ip) {
        const n = names.get(ip);
        return n ? `<strong>${escapeHtml(n)}</strong> <span class="form-hint">${escapeHtml(ip)}</span>` : escapeHtml(ip);
    }

    function endpoint(ip, port) {
        return `${hostName(ip)}${port ? `<span class="cx-port">:${port}</span>` : ''}`;
    }

    // the text a search looks in
    function haystack(c) {
        return [c.proto, c.state, stateText(c), c.src, c.sport || '', names.get(c.src) || '', c.dst, c.dport || '', names.get(c.dst) || '',
            c.source_nat, c.dest_nat, names.get(c.dest_nat) || ''].join(' ').toLowerCase();
    }

    function matches() {
        const q = $('cx-filter').value.trim().toLowerCase();
        const proto = $('cx-proto').value;
        return (report.connections || []).filter(c => (!proto || known(c) === proto) && (!q || haystack(c).includes(q)));
    }

    function rowHtml(c) {
        const to = [endpoint(c.dst, c.dport)];
        if (c.dest_nat) to.push(`<div class="form-hint">sent on to ${endpoint(c.dest_nat, c.dest_nat_port)} (port forward)</div>`);
        const from = [endpoint(c.src, c.sport)];
        if (c.source_nat) from.push(`<div class="form-hint">seen outside as ${escapeHtml(c.source_nat)}</div>`);
        const data = report.accounting ? `${formatBytes(c.bytes)} / ${formatBytes(c.reply_bytes)}` : '–';
        return `<tr><td><span class="chip">${escapeHtml(c.proto)}</span></td><td>${from.join('')}</td><td>${to.join('')}</td>
            <td>${escapeHtml(stateText(c))}</td><td>${escapeHtml(coarse(c.expires_in))}</td><td>${data}</td></tr>`;
    }

    function render() {
        if (!report) return;
        const body = $('cx-body');
        const note = $('cx-note');
        if (!report.known) {
            $('cx-summary').textContent = '';
            body.innerHTML = '<tr><td colspan="6" class="table-placeholder">The connection table cannot be read on this system.</td></tr>';
            note.textContent = 'It needs the router’s connection tracking (which the firewall uses) and permission to read it, which the router has when it runs as the appliance.';
            $('cx-more').classList.add('hidden');
            return;
        }
        const list = matches();
        const pct = report.max ? Math.round(100 * report.count / report.max) : null;
        $('cx-summary').textContent = `${report.count.toLocaleString()} connections tracked` + (pct !== null ? ` (the table is ${pct}% full)` : '') +
            (report.shown < report.total ? `; the busiest ${report.shown.toLocaleString()} of ${report.total.toLocaleString()} are loaded` : '') +
            ((list.length !== report.shown) ? `; ${list.length.toLocaleString()} match` : '') + '.';
        body.innerHTML = list.length ? list.slice(0, shown).map(rowHtml).join('') :
            `<tr><td colspan="6" class="table-placeholder">${report.connections.length ? 'No connection matches.' : 'No connections right now.'}</td></tr>`;
        $('cx-more').classList.toggle('hidden', list.length <= shown);
        note.textContent = report.accounting ? '' : 'The router has just started counting data: it shows for connections that start from now on.';
    }

    async function load() {
        if (paused) return;
        const [res, leases] = await Promise.all([apiFetch('/api/v1/firewall/connections?limit=2000'), apiFetch('/api/v1/dhcp/leases')]);
        if (!res || !res.ok) return;
        if (leases && leases.ok) {
            names = new Map();
            ((await leases.json()) || []).forEach(l => { if (l.hostname && l.hostname !== 'Unknown Device') names.set(l.ip_address, l.hostname); });
        }
        report = await res.json();
        render();
    }

    function wire() {
        if (wired) return;
        wired = true;
        $('cx-filter').addEventListener('input', () => { shown = STEP; render(); });
        $('cx-proto').addEventListener('change', () => { shown = STEP; render(); });
        $('cx-more').addEventListener('click', () => { shown += STEP; render(); });
        $('cx-pause').addEventListener('click', () => {
            paused = !paused;
            $('cx-pause').textContent = paused ? 'Resume' : 'Pause';
            $('cx-pause').setAttribute('aria-pressed', String(paused));
            if (!paused) load();
        });
    }

    function init() {
        wire();
        report = null;
        paused = false;
        shown = STEP;
        $('cx-pause').textContent = 'Pause';
        $('cx-pause').setAttribute('aria-pressed', 'false');
        load();
        timer = setInterval(load, 5000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
