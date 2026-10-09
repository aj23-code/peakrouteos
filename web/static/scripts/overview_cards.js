// web/static/scripts/overview_cards.js
// The Internet and Network cards at the top of the Overview. The system statistics and the DHCP
// leases are already fetched every second by telemetry.js, which hands them over (onStats,
// onLeases); the cards fetch the slower-moving things (health, addresses) themselves.

window.OverviewCards = (function () {
    const HEALTH_MS = 10000;
    const NETWORK_MS = 30000;

    const BADGE = {
        online: 'Internet online',
        offline: 'Internet offline',
        unknown: 'Checking internet…',
        not_configured: 'Internet not set up',
    };
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
        const host = data.hostname || '';
        const up = data.uptime_seconds ? 'up ' + formatUptime(data.uptime_seconds) : '';
        $('ov-subtitle').textContent = [host, up].filter(Boolean).join(' · ') || ' ';

        const wan = Object.entries(data.interfaces || {}).find(([, m]) => m.role === 'wan');
        $('ov-wan-down').textContent = wan ? formatRate(wan[1].rx_bytes_sec) : '–';
        $('ov-wan-up').textContent = wan ? formatRate(wan[1].tx_bytes_sec) : '–';
    }

    function onLeases(list) {
        leases = Array.isArray(list) ? list : [];
        $('ov-clients').textContent = leases.length === 1 ? '1 connected' : `${leases.length} connected`;
        renderDhcp();
    }

    // ---------------------------------------------------------------- internet

    function renderHealth() {
        if (!history) return;
        const st = history.status || {};
        const gw = st.gateway || {};
        const state = STATE[st.state] ? st.state : 'unknown';

        $('ov-badge').className = 'status-badge ov-badge--' + state;
        $('ov-badge-text').textContent = BADGE[state];
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

    function renderNetwork() {
        if (!network) return;
        const { lan, uplink } = network;

        if (uplink) {
            const live = uplink.live;
            $('ov-wan-port').textContent = uplink.port || 'none';
            $('ov-wan-link').textContent = !uplink.port ? 'choose a port on the Ports page' : live ? (live.up ? 'connected' : 'no link') : '';
            $('ov-wan-addr').textContent = (live && live.address) || (uplink.port ? 'Waiting for an address' : '–');
            $('ov-wan-gw').textContent = (live && live.gateway) || '–';
        }
        if (lan) {
            const ports = lan.ports || [];
            $('ov-lan-ports').innerHTML = ports.length
                ? ports.map(p => `<span class="chip">${escapeHtml(p)}</span>`).join('')
                : '<span class="form-hint">no ports yet</span>';
            $('ov-lan-addr').textContent = lan.ip ? `${lan.ip}/${prefixOf(lan.netmask || '255.255.255.0')}` : '–';
        }
        renderDhcp();
    }

    function renderDhcp() {
        const lan = network && network.lan;
        const box = $('ov-dhcp'), off = $('ov-dhcp-off');
        const d = lan && lan.dhcp;
        const usable = d && d.enabled && d.range_start && d.range_end;
        box.classList.toggle('hidden', !usable);
        off.classList.toggle('hidden', !!usable || !lan || !d);
        if (!usable) return;

        const start = ipToInt(d.range_start), end = ipToInt(d.range_end);
        const size = Math.max(0, end - start + 1);
        const used = leases.filter(l => {
            const ip = l.ip_address || l.ip;
            if (!ip) return false;
            const n = ipToInt(ip);
            return n >= start && n <= end;
        }).length;
        const pct = size ? Math.min(100, Math.round((used / size) * 100)) : 0;
        $('ov-dhcp-val').textContent = `${used} of ${size}`;
        $('ov-dhcp-bar').style.width = pct + '%';
        $('ov-dhcp-bar').className = 'progress-fill ' + (pct >= 90 ? 'fill-orange' : 'fill-green');
    }

    async function loadNetwork() {
        const [u, n] = await Promise.all([apiFetch('/api/v1/uplinks'), apiFetch('/api/v1/networks')]);
        if (!u || !u.ok || !n || !n.ok) return;
        const uplinks = await u.json(), networks = await n.json();
        network = {
            uplink: (uplinks.uplinks || [])[0] || null,
            lan: (networks.networks || []).find(x => x.id === 'lan') || (networks.networks || [])[0] || null,
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
