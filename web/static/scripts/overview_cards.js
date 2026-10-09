// web/static/scripts/overview_cards.js
// The Internet and Network cards at the top of the Overview. The system statistics and the DHCP
// leases are already fetched every second by telemetry.js, which hands them over (onStats,
// onLeases); the cards fetch the slower-moving things (health, addresses) themselves.

window.OverviewCards = (function () {
    const HEALTH_MS = 10000;
    const NETWORK_MS = 30000;

    const STATE = { online: 'Online', offline: 'Offline', unknown: 'Checking…', not_configured: 'Not set up yet' };

    let healthTimer = null;
    let networkTimer = null;
    let history = null;
    let network = null;        // { lan, uplink }
    let leases = [];
    let resizeHandler = null;

    const $ = (id) => document.getElementById(id);

    // ---------------------------------------------------------------- from telemetry.js

    function onStats(data) {
        const wan = Object.entries(data.interfaces || {}).find(([, m]) => m.role === 'wan');
        $('ov-wan-down').textContent = wan ? formatRate(wan[1].rx_bytes_sec) : '–';
        $('ov-wan-up').textContent = wan ? formatRate(wan[1].tx_bytes_sec) : '–';
    }

    function onLeases(list) {
        leases = Array.isArray(list) ? list : [];
        renderNetworks();
    }

    // ---------------------------------------------------------------- internet

    function renderHealth() {
        if (!history) return;
        const st = history.status || {};
        const gw = st.gateway || {};
        const state = STATE[st.state] ? st.state : 'unknown';

        $('ov-dot').className = 'inet-dot inet-dot--' + state;
        $('ov-state-text').textContent = STATE[state];

        const note = st.diagnosis || (state === 'not_configured' ? 'Choose an internet port on the Ports page to start measuring.' : '');
        $('ov-note').textContent = note;
        $('ov-note').classList.toggle('hidden', !note);

        $('ov-latency').textContent = state === 'offline' ? 'No answer' : formatMs(st.rtt_ms);
        $('ov-gateway').textContent = gw.state === 'online' ? formatMs(gw.rtt_ms)
            : gw.state === 'offline' ? 'No answer' : gw.state === 'unknown' ? '–' : 'Not measured';
        const up = history.availability ? history.availability.uptime_24h : null;
        $('ov-uptime').textContent = formatPercent(up);
        $('ov-uptime').classList.toggle('inet-bad', up !== null && up !== undefined && up < 99);

        drawSpark();
    }

    function drawSpark() {
        const canvas = $('ov-spark');
        if (!canvas) return;
        const box = canvas.parentElement.getBoundingClientRect();
        if (!box.width || !box.height) return; // hidden: drawn when shown

        // The plot fills whatever height the card has left beside its taller neighbour
        const dpr = window.devicePixelRatio || 1;
        const w = box.width, h = box.height;
        canvas.width = Math.round(w * dpr);
        canvas.height = Math.round(h * dpr);
        const ctx = canvas.getContext('2d');
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        ctx.clearRect(0, 0, w, h);

        const dns = history ? history.dns : null;
        const range = $('ov-spark-range');
        if (!dns || !dns.timestamps.length) {
            ctx.fillStyle = '#64748B';
            ctx.font = '11px system-ui, sans-serif';
            ctx.textAlign = 'center';
            ctx.fillText(history ? 'No measurements yet' : '', w / 2, h / 2 + 4);
            range.textContent = ' ';
            return;
        }

        const ts = dns.timestamps, avg = dns.avg_ms, loss = dns.loss_pct;
        const now = Date.now(), span = 3600e3, minT = now - span;
        const answered = avg.filter(v => v !== null);
        const top = Math.max(10, ...answered) * 1.15;
        const x = (t) => Math.max(0, Math.min(1, (t - minT) / span)) * (w - 4) + 2;
        const y = (v) => h - 4 - (v / top) * (h - 10);
        const gap = 150e3;

        // lost minutes: a red column, as on the Internet page
        for (let i = 0; i < ts.length; i++) {
            if (loss[i] < 1) continue;
            ctx.fillStyle = `rgba(239, 68, 68, ${Math.min(0.7, 0.1 + (loss[i] / 100) * 0.9)})`;
            ctx.fillRect(x(ts[i]) - 1.5, 0, 3, h);
        }

        // the average line, broken where nothing answered or nothing was measured
        let run = [];
        const runs = [];
        for (let i = 0; i < ts.length; i++) {
            const broken = avg[i] === null || (run.length && ts[i] - ts[run[run.length - 1]] > gap);
            if (broken && run.length) { runs.push(run); run = []; }
            if (avg[i] !== null) run.push(i);
        }
        if (run.length) runs.push(run);

        ctx.strokeStyle = '#38bdf8';
        ctx.fillStyle = '#38bdf8';
        ctx.lineWidth = 1.75;
        ctx.lineJoin = 'round';
        runs.forEach(r => {
            if (r.length === 1) { ctx.beginPath(); ctx.arc(x(ts[r[0]]), y(avg[r[0]]), 2, 0, Math.PI * 2); ctx.fill(); return; }
            ctx.beginPath();
            r.forEach((i, k) => (k ? ctx.lineTo(x(ts[i]), y(avg[i])) : ctx.moveTo(x(ts[i]), y(avg[i]))));
            ctx.stroke();
        });

        range.textContent = answered.length
            ? `${Math.round(Math.min(...answered))}–${Math.round(Math.max(...answered))} ms` : ' ';
    }

    async function loadHealth() {
        const res = await apiFetch('/api/v1/uplinks/wan/health?range=1h');
        if (!res || !res.ok) return;
        history = await res.json();
        renderHealth();
    }

    // ---------------------------------------------------------------- network

    const ipToInt = (ip) => ip.split('.').reduce((a, o) => a * 256 + Number(o), 0);

    function prefixOf(mask) {
        const bits = mask.split('.').map(Number).map(n => n.toString(2).padStart(8, '0')).join('');
        return bits.indexOf('0') === -1 ? 32 : bits.indexOf('0');
    }

    // The leases that fall inside a network's subnet
    function leasesIn(n) {
        if (!n.ip || !n.netmask) return [];
        const mask = ipToInt(n.netmask), net = ipToInt(n.ip) & mask;
        return leases.filter(l => {
            const ip = l.ip_address || l.ip;
            return ip && (ipToInt(ip) & mask) === net;
        });
    }

    function renderWan() {
        const uplink = network && network.uplink;
        if (!uplink) return;
        const live = uplink.live;
        $('ov-wan-port').textContent = uplink.port || 'none';
        $('ov-wan-link').textContent = !uplink.port ? 'choose a port on the Ports page' : live ? (live.up ? 'connected' : 'no link') : '';
        $('ov-wan-addr').textContent = (live && live.address) || (uplink.port ? 'Waiting for an address' : '\u2013');
        $('ov-wan-gw').textContent = (live && live.gateway) || '\u2013';
    }

    function networkHtml(n) {
        const mine = leasesIn(n);
        const chips = (n.ports || []).map(p => `<span class="chip">${escapeHtml(p)}</span>`)
            .concat((n.tagged_ports || []).map(p => `<span class="chip chip--tag">${escapeHtml(p)} <small>tagged</small></span>`)).join('');
        const tags = [];
        if (!n.default) tags.push(`<span class="tag">VLAN ${n.vlan}</span>`);
        if (!n.default) tags.push(n.internet ? '<span class="tag tag-ok">Internet</span>' : '<span class="tag tag-warn">No internet</span>');
        if (n.isolated) tags.push('<span class="tag">Isolated</span>');
        if (n.pending) tags.push('<span class="tag tag-draft">Draft</span>');

        let dhcp = '';
        const d = n.dhcp;
        if (d && d.enabled && d.range_start && d.range_end) {
            const start = ipToInt(d.range_start), end = ipToInt(d.range_end);
            const size = Math.max(0, end - start + 1);
            const used = mine.filter(l => { const v = ipToInt(l.ip_address || l.ip); return v >= start && v <= end; }).length;
            const pct = size ? Math.min(100, Math.round((used / size) * 100)) : 0;
            dhcp = `<div class="ov-dhcp"><div class="metric-header"><span class="metric-label">DHCP addresses in use</span>
                <span class="metric-subtext">${used} of ${size}</span></div>
                <div class="progress-track"><div class="progress-fill ${pct >= 90 ? 'fill-orange' : 'fill-green'}" style="width: ${pct}%"></div></div></div>`;
        } else if (n.default && d === null) {
            dhcp = '';
        } else {
            dhcp = '<p class="form-hint">No DHCP server on this network.</p>';
        }

        return `<div class="ov-row">
            <div class="ov-row-title"><span class="traffic-badge lan">${escapeHtml(n.name)}</span>${tags.join('')}</div>
            <div class="ov-chips">${chips || '<span class="form-hint">no ports yet</span>'}</div>
            <dl class="ov-kv">
                <div><dt>Router address</dt><dd>${n.ip ? escapeHtml(n.ip) + '/' + prefixOf(n.netmask || '255.255.255.0') : '\u2013'}</dd></div>
                <div><dt>Clients</dt><dd>${mine.length === 1 ? '1 connected' : mine.length + ' connected'}</dd></div>
            </dl>
            ${dhcp}
        </div>`;
    }

    function renderNetworks() {
        const box = $('ov-networks');
        if (!box || !network) return;
        box.innerHTML = (network.networks || []).map(networkHtml).join('');
    }

    function renderNetwork() {
        renderWan();
        renderNetworks();
    }

    async function loadNetwork() {
        const [u, n] = await Promise.all([apiFetch('/api/v1/uplinks'), apiFetch('/api/v1/networks')]);
        if (!u || !u.ok || !n || !n.ok) return;
        const uplinks = await u.json(), networks = await n.json();
        network = {
            uplink: (uplinks.uplinks || [])[0] || null,
            networks: networks.networks || [],
        };
        renderNetwork();
    }

    // ---------------------------------------------------------------- lifecycle

    function start() {
        history = null;
        loadHealth();
        loadNetwork();
        healthTimer = setInterval(loadHealth, HEALTH_MS);
        networkTimer = setInterval(loadNetwork, NETWORK_MS);
        resizeHandler = () => drawSpark();
        window.addEventListener('resize', resizeHandler);
    }

    function stop() {
        clearInterval(healthTimer);
        clearInterval(networkTimer);
        healthTimer = networkTimer = null;
        if (resizeHandler) window.removeEventListener('resize', resizeHandler);
        resizeHandler = null;
    }

    return { start, stop, onStats, onLeases };
})();
