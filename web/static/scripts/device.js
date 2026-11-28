// web/static/scripts/device.js
// A device's own page (#device/<hardware address>), reached from the Clients list: who it is, how it is connected, what it has used over
// time, when it was here and what it is doing now (docs/device-pages.md, phase 1).

window.DevicePage = (function () {
    const RANGE_MS = { '1h': 3600e3, '24h': 24 * 3600e3, '30d': 30 * 24 * 3600e3 };
    const REFRESH_MS = { '1h': 15000, '24h': 30000, '30d': 60000 };
    const COLOR = { down: '0, 114, 206', up: '0, 168, 107', grid: 'rgba(255, 255, 255, 0.05)', text: '#94A3B8', muted: '#64748B' };
    const PAD = { top: 16, right: 20, bottom: 32, left: 70 };
    const FONT = 'Inter, system-ui, sans-serif';

    let mac = null;
    let detail = null;
    let hist = null;
    let range = '24h';
    let timers = [];
    let wired = false;
    let resizeHandler = null;
    let gone = false;

    const $ = (id) => document.getElementById(id);

    // ---------------------------------------------------------------- formatting

    function bytesText(n) {
        if (!n) return '0 B';
        const u = ['B', 'KB', 'MB', 'GB', 'TB'];
        let i = 0, v = n;
        while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; }
        return (v >= 100 || i === 0 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2)) + ' ' + u[i];
    }
    function rateText(bps) {
        if (bps < 1) return '0 bit/s';
        if (bps < 1e3) return bps.toFixed(0) + ' bit/s';
        if (bps < 1e6) return (bps / 1e3).toFixed(bps < 1e4 ? 1 : 0) + ' kbit/s';
        if (bps < 1e9) return (bps / 1e6).toFixed(bps < 1e7 ? 1 : 0) + ' Mbit/s';
        return (bps / 1e9).toFixed(2) + ' Gbit/s';
    }
    function whenText(sec) {
        if (!sec) return 'never';
        const d = new Date(sec * 1000);
        const hm = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
        const ago = Date.now() / 1000 - sec;
        if (ago < 86400 * 1.5 && new Date().getDate() === d.getDate()) return 'today ' + hm;
        return d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + hm;
    }
    function durationText(sec) {
        if (sec < 90) return Math.round(sec) + ' s';
        const m = Math.round(sec / 60);
        if (m < 90) return m + ' min';
        const h = Math.floor(m / 60);
        return h < 48 ? h + ' h ' + (m % 60) + ' min' : Math.floor(h / 24) + ' d ' + (h % 24) + ' h';
    }
    function niceCeil(v) {
        if (v <= 0) return 1;
        const p = Math.pow(10, Math.floor(Math.log10(v)));
        const n = v / p;
        return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * p;
    }
    function signalText(dbm) {
        const q = dbm >= -55 ? 'excellent' : dbm >= -65 ? 'good' : dbm >= -75 ? 'fair' : 'weak';
        return `${dbm} dBm (${q})`;
    }
    // where the router's bridge has found the device: its own port, and the switch or access point plugged into it when that has said who it is
    function portText(at) {
        if (!at) return '<span class="form-hint">the switch port is not known yet</span>';
        let t = `<code>${escapeHtml(at.port)}</code>` + (at.vlan > 1 ? ` <span class="form-hint">VLAN ${at.vlan}</span>` : '');
        const n = at.neighbour;
        if (n) {
            const who = n.name || n.chassis_id || 'a switch';
            t += `<br><span class="form-hint">behind ${escapeHtml(who)}${n.port_id ? ', its port ' + escapeHtml(n.port_id) : ''}${n.port_desc ? ' (' + escapeHtml(n.port_desc) + ')' : ''}</span>`;
        } else {
            t += '<br><span class="form-hint">the router sees it on this port: a switch beyond it is not known yet</span>';
        }
        return t;
    }
    const row = (k, v) => v === '' || v == null ? '' : `<div><dt>${k}</dt><dd>${v}</dd></div>`;

    // ---------------------------------------------------------------- the device

    function showMessage(text) {
        const box = $('dev-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--error', !!text);
    }

    function renderDetail() {
        const d = detail;
        if (!d) return;
        $('dev-title').textContent = d.name;
        $('dev-subtitle').textContent = [d.vendor, d.private_address ? 'private hardware address' : '', d.network].filter(Boolean).join(' · ');
        const st = $('dev-status');
        st.textContent = d.status === 'online' ? 'Online' : d.status === 'offline' ? 'Offline' : 'Not known';
        st.className = 'tag ' + (d.status === 'online' ? 'tag-ok' : d.status === 'offline' ? 'tag-warn' : '');

        let mac = `<code class="code-mac">${escapeHtml(d.mac)}</code>`;
        if (d.private_address) mac += ' <span class="form-hint" title="The device made up this address itself, to be harder to follow from network to network. It may change, and it names no maker.">private address</span>';
        else if (d.vendor) mac += ` <span class="form-hint">${escapeHtml(d.vendor)}</span>`;
        let ip = escapeHtml(d.ip || '–');
        if (d.static) ip += ' <span class="tag" title="The device has an address of its own: it did not ask this router for one">Static</span>';
        if (d.seen_on) ip += ` <span class="tag tag-draft" title="The router sees this device on ${escapeHtml(d.seen_on)}, but its address belongs to ${escapeHtml(d.network)}">Seen on ${escapeHtml(d.seen_on)}</span>`;
        $('dev-identity').innerHTML = [
            row('Name', escapeHtml(d.name)),
            d.nickname ? row('Host name', escapeHtml(d.hostname || '–')) : row('Nickname', '<span class="form-hint">none: name it on the Clients page</span>'),
            row('Hardware address', mac),
            row('Address', ip),
            row('Network', escapeHtml(d.network || '–')),
            row('Reserved address', d.reserved ? escapeHtml(d.reservation || 'yes') : 'no'),
            d.lease_text ? row('Lease', escapeHtml(d.lease_text)) : '',
        ].join('');

        const rows = [];
        if (d.status === 'online') rows.push(row('Status', 'Online now'));
        else rows.push(row('Status', d.status === 'offline' ? `Offline, last seen ${whenText(d.last_seen)}` : 'Not known (this system cannot ask)'));
        if (d.connection === 'wifi') {
            rows.push(row('Connected by', 'Wi-Fi'));
            rows.push(row('Network', escapeHtml(d.ssid || '–') + (d.band ? ' · ' + escapeHtml(d.band) : '')));
            const w = d.wifi;
            if (w) {
                if (w.signal_dbm != null) rows.push(row('Signal', signalText(w.signal_dbm)));
                if (w.channel) rows.push(row('Channel', String(w.channel)));
                if (w.rx_mbps || w.tx_mbps) rows.push(row('Link speed', `${w.rx_mbps ? '↓ ' + Math.round(w.rx_mbps) : ''}${w.rx_mbps && w.tx_mbps ? ' · ' : ''}${w.tx_mbps ? '↑ ' + Math.round(w.tx_mbps) : ''} Mbit/s`));
                if (w.connected_seconds) rows.push(row('Connected for', durationText(w.connected_seconds)));
            } else if (d.status === 'online') {
                rows.push(row('Signal', '<span class="form-hint">on an access point: its signal is not read here yet</span>'));
            }
        } else if (d.connection === 'wired') {
            rows.push(row('Connected by', 'Cable'));
            rows.push(row('Port', portText(d.attached)));
        } else {
            rows.push(row('Connected by', 'Not known'));
        }
        rows.push(row('Used through the router', `${bytesText(d.total_bytes)} since it was first counted`));
        $('dev-connection').innerHTML = rows.join('');
        renderEvents();
    }

    function eventText(e) {
        const how = e.connection === 'wifi' ? `on Wi-Fi ${escapeHtml(e.ssid || '')}${e.band ? ' ' + escapeHtml(e.band) : ''}` : e.connection === 'wired' ? 'by cable' : '';
        if (!e.online) return 'Went offline';
        return `Online ${how}${e.network ? ' on ' + escapeHtml(e.network) : ''}`.replace(/\s+/g, ' ').trim();
    }

    function renderEvents() {
        const ul = $('dev-events');
        const ev = (detail && detail.events) || [];
        if (detail && !detail.history_kept) { ul.innerHTML = '<li class="form-hint">The history is not kept on this system.</li>'; return; }
        ul.innerHTML = ev.length
            ? ev.slice(0, 12).map(e => `<li>${escapeHtml(whenText(e.at))} <span class="form-hint">${eventText(e)}</span></li>`).join('')
            : '<li class="form-hint">Nothing has been recorded for this device yet: changes are written from now on.</li>';
    }

    async function loadDetail() {
        const res = await apiFetch('/api/v1/clients/' + encodeURIComponent(mac));
        if (!res) return;
        if (res.status === 404) {
            gone = true;
            showMessage('This device is not in the list any more (it was removed, or it has not been seen). Go back to the Clients page.');
            return;
        }
        if (!res.ok) { showMessage('The device could not be read.'); return; }
        gone = false;
        showMessage('');
        detail = await res.json();
        renderDetail();
        renderPresence();
    }

    // ---------------------------------------------------------------- usage

    async function loadHistory() {
        const r = range, m = mac;
        const res = await apiFetch(`/api/v1/clients/${encodeURIComponent(m)}/history?range=${r}`);
        if (!res || !res.ok) { if (res && res.status === 501) { hist = null; drawChart(); } return; }
        const data = await res.json();
        if (r !== range || m !== mac) return;
        hist = data;
        renderStats();
        drawChart();
        renderPresence();
    }

    function renderStats() {
        if (!hist) { $('dev-stats').innerHTML = ''; return; }
        $('dev-stats').innerHTML = [
            row('Received', bytesText(hist.total_down)),
            row('Sent', bytesText(hist.total_up)),
            row('Busiest download', hist.peak_down_bps ? rateText(hist.peak_down_bps) : '–'),
            row('Busiest upload', hist.peak_up_bps ? rateText(hist.peak_up_bps) : '–'),
        ].join('');
    }

    function drawChart() {
        const canvas = $('dev-canvas');
        if (!canvas) return;
        const holder = canvas.parentElement.getBoundingClientRect();
        if (!holder.width) return;
        const dpr = window.devicePixelRatio || 1;
        const width = holder.width, height = 220;
        canvas.width = Math.round(width * dpr);
        canvas.height = Math.round(height * dpr);
        canvas.style.width = width + 'px';
        canvas.style.height = height + 'px';
        const ctx = canvas.getContext('2d');
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        ctx.clearRect(0, 0, width, height);
        ctx.font = `11px ${FONT}`;

        if (!hist || !hist.points.length) {
            ctx.fillStyle = COLOR.muted;
            ctx.textAlign = 'center';
            ctx.fillText(!hist ? (detail && detail.history_kept === false ? 'The history is not kept on this system.' : 'Loading…')
                : 'Nothing was sent or received in this window yet.', width / 2, height / 2);
            return;
        }
        const gw = width - PAD.left - PAD.right, gh = height - PAD.top - PAD.bottom;
        const bucket = hist.bucket_seconds;
        const minT = hist.to - RANGE_MS[range] / 1000, span = RANGE_MS[range] / 1000;
        const rates = hist.points.map(p => ({ t: p.t, up: p.up * 8 / bucket, down: p.down * 8 / bucket }));
        const top = niceCeil(Math.max(1, ...rates.map(p => Math.max(p.up, p.down))) * 1.1);
        const x = (t) => PAD.left + ((t - minT) / span) * gw;
        const y = (v) => PAD.top + gh - (v / top) * gh;

        ctx.strokeStyle = COLOR.grid; ctx.lineWidth = 1; ctx.fillStyle = COLOR.text; ctx.textAlign = 'right';
        for (let i = 0; i <= 4; i++) {
            const gy = PAD.top + (gh / 4) * i;
            ctx.beginPath(); ctx.moveTo(PAD.left, gy); ctx.lineTo(width - PAD.right, gy); ctx.stroke();
            ctx.fillText(rateText(top - (top / 4) * i), PAD.left - 8, gy + 4);
        }
        ctx.textAlign = 'center';
        for (let i = 0; i < 5; i++) {
            const tx = PAD.left + (gw / 4) * i, t = minT + (span / 4) * i;
            const d = new Date(t * 1000);
            const label = range === '30d' ? `${d.getMonth() + 1}/${d.getDate()}` : d.toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit' });
            ctx.fillText(label, tx, height - 10);
        }
        // a bar for every bucket that had traffic: what was received behind, what was sent in front
        const barW = Math.max(2, (bucket / span) * gw - 1);
        rates.forEach(p => {
            const bx = x(p.t);
            ctx.fillStyle = `rgba(${COLOR.down}, 0.6)`;
            ctx.fillRect(bx, y(p.down), barW, PAD.top + gh - y(p.down));
            ctx.fillStyle = `rgba(${COLOR.up}, 0.85)`;
            ctx.fillRect(bx, y(p.up), barW, PAD.top + gh - y(p.up));
        });
        canvas._geo = { minT, span, gw, bucket, rates };
    }

    function hoverMove(e) {
        const canvas = $('dev-canvas'), g = canvas._geo;
        if (!g) return;
        const rect = canvas.getBoundingClientRect();
        const mx = e.clientX - rect.left;
        if (mx < PAD.left || mx > rect.width - PAD.right) { $('dev-hover').innerHTML = '&nbsp;'; return; }
        const t = g.minT + ((mx - PAD.left) / g.gw) * g.span;
        const p = g.rates.find(r => t >= r.t && t < r.t + g.bucket);
        const when = new Date(t * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
        $('dev-hover').textContent = p ? `${when}: received ${rateText(p.down)}, sent ${rateText(p.up)}` : `${when}: nothing`;
    }

    // ---------------------------------------------------------------- presence

    function renderPresence() {
        const strip = $('dev-strip');
        if (!hist) { strip.innerHTML = ''; return; }
        const from = hist.from, to = hist.to, span = Math.max(1, to - from);
        strip.innerHTML = (hist.segments || []).map(s => {
            const l = Math.max(0, (s.from - from) / span * 100), w = Math.max(0.2, (s.to - Math.max(s.from, from)) / span * 100);
            const title = `${s.online ? 'Online' : 'Offline'} ${whenText(Math.max(s.from, from))} – ${whenText(s.to)}`;
            return `<div class="seg ${s.online ? 'on' : 'off'}" style="left:${l}%;width:${w}%" title="${escapeHtml(title)}"></div>`;
        }).join('');
        $('dev-strip-from').textContent = whenText(from);
        $('dev-strip-to').textContent = 'now';
        const known = (hist.segments || []).length > 0;
        $('dev-presence-note').textContent = known ? 'Green: online. Grey: offline.' : 'Nothing is known for this window yet.';
    }

    // ---------------------------------------------------------------- connections now

    async function loadConnections() {
        if (!detail || !detail.ip) return;
        const res = await apiFetch('/api/v1/firewall/connections?limit=25&ip=' + encodeURIComponent(detail.ip));
        if (!res || !res.ok) return;
        const rep = await res.json();
        const body = $('dev-conns');
        if (!rep.known) { body.innerHTML = '<tr><td colspan="5" class="table-placeholder">The connection table cannot be read on this system.</td></tr>'; $('dev-conn-note').textContent = ''; return; }
        $('dev-conn-note').textContent = rep.total > rep.shown ? `the busiest ${rep.shown} of ${rep.total}` : `${rep.total} open`;
        if (!rep.connections.length) { body.innerHTML = '<tr><td colspan="5" class="table-placeholder">Nothing is open right now.</td></tr>'; return; }
        body.innerHTML = rep.connections.map(c => {
            const out = c.src === detail.ip; // the device started it; else it was forwarded to the device
            const remote = out ? `${c.dst}:${c.dport}` : `${c.src}:${c.sport}`;
            const sent = out ? c.bytes : c.reply_bytes, got = out ? c.reply_bytes : c.bytes;
            return `<tr><td>${escapeHtml(c.proto.toUpperCase())}</td><td><code>${escapeHtml(remote)}</code>${out ? '' : ' <span class="form-hint">came in</span>'}</td>
                <td>${escapeHtml(c.state || (c.unreplied ? 'no answer' : ''))}</td><td>${bytesText(sent)}</td><td>${bytesText(got)}</td></tr>`;
        }).join('');
    }

    // ---------------------------------------------------------------- lifecycle

    function pickRange(r) {
        range = r;
        document.querySelectorAll('#dev-picker .time-btn').forEach(b => b.classList.toggle('active', b.dataset.range === r));
        hist = null;
        drawChart();
        loadHistory();
        schedule();
    }

    function schedule() {
        timers.forEach(clearInterval);
        timers = [
            setInterval(() => { if (!gone) loadDetail().then(loadConnections); }, 10000),
            setInterval(() => { if (!gone) loadHistory(); }, REFRESH_MS[range]),
        ];
    }

    function wire() {
        if (wired) return;
        wired = true;
        $('dev-picker').addEventListener('click', (e) => {
            const b = e.target.closest('.time-btn');
            if (b) pickRange(b.dataset.range);
        });
        $('dev-canvas').addEventListener('mousemove', hoverMove);
        $('dev-canvas').addEventListener('mouseleave', () => { $('dev-hover').innerHTML = '&nbsp;'; });
    }

    function init(address) {
        wire();
        mac = String(address || '').toLowerCase();
        detail = null; hist = null; gone = false;
        showMessage('');
        $('dev-title').textContent = 'Device';
        $('dev-subtitle').textContent = '';
        ['dev-identity', 'dev-connection', 'dev-stats', 'dev-events', 'dev-strip', 'dev-conns'].forEach(id => { $(id).innerHTML = ''; });
        document.querySelectorAll('#dev-picker .time-btn').forEach(b => b.classList.toggle('active', b.dataset.range === range));
        drawChart();
        loadDetail().then(loadConnections);
        loadHistory();
        schedule();
        resizeHandler = () => drawChart();
        window.addEventListener('resize', resizeHandler);
        return () => {
            timers.forEach(clearInterval);
            timers = [];
            window.removeEventListener('resize', resizeHandler);
        };
    }

    return { init };
})();
