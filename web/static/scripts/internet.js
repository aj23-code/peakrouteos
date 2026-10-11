// web/static/scripts/internet.js
// Internet page: connection health (latency to the upstream DNS server) and the connection settings.

window.InternetModule = (function () {
    const RANGE_MS = { '5m': 5 * 60e3, '1h': 3600e3, '24h': 24 * 3600e3, '30d': 30 * 24 * 3600e3 };
    const BUCKET_S = { '5m': 0, '1h': 60, '24h': 900, '30d': 3600 };
    const REFRESH_MS = { '5m': 5000, '1h': 15000, '24h': 30000, '30d': 60000 };

    // The same palette and padding as the Traffic chart, so the two read alike
    const COLOR = {
        avg: '#0072CE',
        avgGradStart: 'rgba(0, 114, 206, 0.28)',
        avgGradEnd: 'rgba(0, 114, 206, 0.01)',
        gateway: '#00A86B',
        gatewayGradStart: 'rgba(0, 168, 107, 0.28)',
        gatewayGradEnd: 'rgba(0, 168, 107, 0.01)',
        loss: '239, 68, 68',
        grid: 'rgba(255, 255, 255, 0.05)',
        text: '#94A3B8',
        muted: '#64748B',
        crosshair: 'rgba(255, 255, 255, 0.25)',
        cardBg: 'rgba(15, 23, 42, 0.95)',
        cardBorder: '#334155',
    };
    const PAD = { top: 20, right: 25, bottom: 35, left: 65 };
    const FONT = 'Inter, system-ui, sans-serif';

    const STATE_TEXT = {
        online: 'Online',
        offline: 'Offline',
        unknown: 'Checking…',
        not_configured: 'Not set up yet',
    };

    let range = '1h';
    let history = null;       // last answer of the health endpoint
    let hoverT = null;        // time (ms) under the mouse
    let timer = null;
    let uplink = null;        // last answer of the uplink endpoint
    let editable = true;
    let saving = false;
    let wired = false;
    let resizeHandler = null;

    const $ = (id) => document.getElementById(id);
    const FIELD_INPUTS = {
        ip: 'inet-ip', netmask: 'inet-netmask', gateway: 'inet-gateway', dns: 'inet-dns',
        vlan: 'inet-vlan', mtu: 'inet-mtu', mac: 'inet-mac', hostname: 'inet-hostname',
        'pppoe.username': 'inet-pppoe-user', 'pppoe.password': 'inet-pppoe-pass', 'pppoe.service': 'inet-pppoe-service',
    };

    // ---------------------------------------------------------------- health

    function fmtTime(ts, mode) {
        const d = new Date(ts);
        const hm = d.toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit' });
        if (mode === 'date') return `${d.getMonth() + 1}/${d.getDate()}`;
        if (mode === 'date-time') return `${d.getMonth() + 1}/${d.getDate()} ${hm}`;
        return hm;
    }

    function renderStatus() {
        if (!history) return;
        const st = history.status || {};
        const gw = st.gateway || {};
        const sum = history.dns.summary || {};
        const state = STATE_TEXT[st.state] ? st.state : 'unknown';

        $('inet-dot').className = 'inet-dot inet-dot--' + state;
        $('inet-state-text').textContent = STATE_TEXT[state];

        let target = '';
        if (st.target) {
            const how = st.method === 'icmp' ? 'Pinging' : st.method === 'dns' ? 'Timing DNS queries to' : 'Checking';
            target = `${how} ${st.target}` + (st.source ? ` – ${st.source}` : '');
            if (st.method === 'dns') target += '. Ping is not permitted for this program here, so a small DNS query is timed instead.';
        } else if (st.problem) {
            target = st.problem;
        }
        $('inet-target').textContent = target;
        $('inet-diagnosis').textContent = st.diagnosis || '';
        $('inet-diagnosis').classList.toggle('hidden', !st.diagnosis);

        $('inet-now').textContent = state === 'offline' ? 'No answer' : formatMs(st.rtt_ms);
        $('inet-avg').textContent = formatMs(sum.avg_ms);
        $('inet-range').textContent = sum.min_ms === null || sum.min_ms === undefined ? '–' : `${formatMs(sum.min_ms)} / ${formatMs(sum.max_ms)}`;
        $('inet-loss').textContent = sum.sent ? `${sum.loss_pct < 10 ? sum.loss_pct.toFixed(1) : sum.loss_pct.toFixed(0)}%` : '–';
        $('inet-loss').classList.toggle('inet-bad', sum.sent > 0 && sum.loss_pct >= 5);

        const gwEl = $('inet-gw');
        if (gw.state === 'online') gwEl.textContent = formatMs(gw.rtt_ms);
        else if (gw.state === 'offline') gwEl.textContent = 'No answer';
        else if (gw.state === 'unknown') gwEl.textContent = '–';
        else gwEl.textContent = 'Not measured';
        gwEl.title = [gw.target ? `Gateway ${gw.target}` : '', gw.problem || ''].filter(Boolean).join(' – ');
        $('inet-gw-key').classList.toggle('hidden', !(history.gateway.timestamps || []).length);
        renderAvailability();
    }

    function fmtDuration(seconds) {
        const m = Math.round(seconds / 60);
        if (m < 60) return m + (m === 1 ? ' minute' : ' minutes');
        const h = Math.floor(m / 60), rest = m % 60;
        if (h >= 48) return Math.floor(h / 24) + ' d ' + (h % 24) + ' h';
        return h + ' h' + (rest ? ' ' + rest + ' min' : '');
    }

    function renderAvailability() {
        const a = history.availability || {};
        $('inet-up24').textContent = formatPercent(a.uptime_24h);
        $('inet-up30').textContent = formatPercent(a.uptime_30d);
        $('inet-up24').classList.toggle('inet-bad', a.uptime_24h !== null && a.uptime_24h !== undefined && a.uptime_24h < 99);

        const outages = a.outages || [];
        const empty = $('inet-outages-empty');
        const wrap = $('inet-outages-table-wrap');
        const more = $('inet-outages-more');
        if (!outages.length) {
            empty.textContent = a.uptime_30d === null || a.uptime_30d === undefined
                ? 'Collecting data\u2026' : 'No outages recorded in the last 30 days.';
            empty.classList.remove('hidden');
            wrap.classList.add('hidden');
            more.classList.add('hidden');
            return;
        }
        empty.classList.add('hidden');
        wrap.classList.remove('hidden');
        $('inet-outages-body').innerHTML = outages.map(o => {
            const when = new Date(o.start).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
            return `<tr><td>${escapeHtml(when)}</td><td>${escapeHtml(fmtDuration(o.seconds))}</td>` +
                `<td>${o.ongoing ? '<span class="tag tag-warn">Ongoing</span>' : ''}</td></tr>`;
        }).join('');
        if (a.outage_count > outages.length) {
            more.textContent = `Showing the newest ${outages.length} of ${a.outage_count}.`;
            more.classList.remove('hidden');
        } else {
            more.classList.add('hidden');
        }
    }

    function niceCeil(v) {
        const steps = [10, 20, 50, 100, 200, 500, 1000, 2000, 5000];
        return steps.find(s => s >= v) || Math.ceil(v / 1000) * 1000;
    }

    // Runs of consecutive answered points; a lost or missing stretch splits the line
    function runsOf(ts, avg, gap) {
        const runs = [];
        let run = [];
        for (let i = 0; i < ts.length; i++) {
            const broken = avg[i] === null || (run.length && ts[i] - ts[run[run.length - 1]] > gap);
            if (broken && run.length) { runs.push(run); run = []; }
            if (avg[i] !== null) run.push(i);
        }
        if (run.length) runs.push(run);
        return runs;
    }

    // A curve through the points, as on the Traffic page
    function smoothPath(ctx, pts) {
        ctx.beginPath();
        ctx.moveTo(pts[0].x, pts[0].y);
        for (let i = 0; i < pts.length - 1; i++) {
            const midX = (pts[i].x + pts[i + 1].x) / 2;
            const midY = (pts[i].y + pts[i + 1].y) / 2;
            if (i === 0) ctx.lineTo(midX, midY);
            else ctx.quadraticCurveTo(pts[i].x, pts[i].y, midX, midY);
        }
        ctx.lineTo(pts[pts.length - 1].x, pts[pts.length - 1].y);
    }

    // One series: a gradient under a smooth line. A run of one point is just a dot.
    function drawSeries(ctx, series, gap, x, y, bottom, top, line, gradStart, gradEnd) {
        runsOf(series.timestamps, series.avg_ms, gap).forEach(run => {
            const pts = run.map(i => ({ x: x(series.timestamps[i]), y: y(series.avg_ms[i]) }));
            if (pts.length === 1) {
                ctx.fillStyle = line;
                ctx.beginPath(); ctx.arc(pts[0].x, pts[0].y, 3, 0, Math.PI * 2); ctx.fill();
                return;
            }
            const grad = ctx.createLinearGradient(0, top, 0, bottom);
            grad.addColorStop(0, gradStart);
            grad.addColorStop(1, gradEnd);
            smoothPath(ctx, pts);
            ctx.lineTo(pts[pts.length - 1].x, bottom);
            ctx.lineTo(pts[0].x, bottom);
            ctx.closePath();
            ctx.fillStyle = grad;
            ctx.fill();

            smoothPath(ctx, pts);
            ctx.strokeStyle = line;
            ctx.lineWidth = 2;
            ctx.lineJoin = 'round';
            ctx.stroke();
        });
    }

    function drawChart() {
        const canvas = $('inet-canvas');
        if (!canvas) return;
        const holder = canvas.parentElement && canvas.parentElement.getBoundingClientRect();
        if (!holder || !holder.width) return; // hidden: drawn again when shown

        const dpr = window.devicePixelRatio || 1;
        const width = holder.width, height = 230;
        canvas.width = Math.round(width * dpr);
        canvas.height = Math.round(height * dpr);
        canvas.style.width = width + 'px';
        canvas.style.height = height + 'px';
        const ctx = canvas.getContext('2d');
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        ctx.clearRect(0, 0, width, height);

        const gw = width - PAD.left - PAD.right;
        const gh = height - PAD.top - PAD.bottom;
        const rangeMs = RANGE_MS[range];
        const minT = Date.now() - rangeMs;
        const dns = history ? history.dns : null;
        const gate = history ? history.gateway : null;

        ctx.font = `11px ${FONT}`;
        if (!dns || (!dns.timestamps.length && !gate.timestamps.length)) {
            ctx.fillStyle = COLOR.muted;
            ctx.textAlign = 'center';
            const st = history && history.status ? history.status.state : '';
            ctx.fillText(st === 'not_configured' ? 'Nothing to measure until an internet port and DNS server are set up.'
                : history ? 'No measurements stored for this window yet.' : 'Loading\u2026', width / 2, height / 2);
            return;
        }

        const tops = [...dns.max_ms, ...dns.avg_ms, ...gate.avg_ms].filter(v => v !== null);
        const maxMs = niceCeil(Math.max(5, ...tops) * 1.1);
        const x = (t) => PAD.left + Math.max(0, Math.min(1, (t - minT) / rangeMs)) * gw;
        const y = (v) => PAD.top + gh - (v / maxMs) * gh;
        const bottom = PAD.top + gh;

        // grid and y labels
        ctx.strokeStyle = COLOR.grid;
        ctx.lineWidth = 1;
        ctx.fillStyle = COLOR.text;
        ctx.textAlign = 'right';
        for (let i = 0; i <= 4; i++) {
            const gy = PAD.top + (gh / 4) * i;
            ctx.beginPath(); ctx.moveTo(PAD.left, gy); ctx.lineTo(width - PAD.right, gy); ctx.stroke();
            ctx.fillText(`${Math.round(maxMs - (maxMs / 4) * i)} ms`, PAD.left - 10, gy + 4);
        }
        // x ticks and labels
        ctx.textAlign = 'center';
        const mode = range === '30d' ? 'date' : range === '24h' ? 'date-time' : 'time';
        for (let i = 0; i < 5; i++) {
            const tx = PAD.left + (gw / 4) * i;
            ctx.beginPath(); ctx.moveTo(tx, bottom); ctx.lineTo(tx, bottom + 4);
            ctx.strokeStyle = COLOR.grid; ctx.stroke();
            ctx.fillStyle = COLOR.text;
            ctx.fillText(fmtTime(minT + (rangeMs / 4) * i, mode), tx, height - 10);
        }

        // Points further apart than this are not joined: the router (or its probing) was not running
        const stepMs = (BUCKET_S[range] || (history.status && history.status.interval_seconds) || 5) * 1000;
        const gap = stepMs * 2.5;
        const barW = Math.max(2, (stepMs / rangeMs) * gw);

        // lost packets: a red column, stronger the more was lost (a stray lost packet is not drawn)
        for (let i = 0; i < dns.timestamps.length; i++) {
            if (dns.loss_pct[i] < 1) continue;
            ctx.fillStyle = `rgba(${COLOR.loss}, ${Math.min(0.7, 0.1 + (dns.loss_pct[i] / 100) * 0.9)})`;
            ctx.fillRect(x(dns.timestamps[i]) - barW / 2, PAD.top, barW, gh);
        }

        drawSeries(ctx, dns, gap, x, y, bottom, PAD.top, COLOR.avg, COLOR.avgGradStart, COLOR.avgGradEnd);
        drawSeries(ctx, gate, gap, x, y, bottom, PAD.top, COLOR.gateway, COLOR.gatewayGradStart, COLOR.gatewayGradEnd);

        if (hoverT !== null) drawTooltip(ctx, width, x, y, gap, bottom);
    }

    // The point of a series closest to a time, if it is close enough to be the same moment
    function nearest(series, t, within) {
        let best = -1, bestD = Infinity;
        series.timestamps.forEach((v, i) => { const d = Math.abs(v - t); if (d < bestD) { bestD = d; best = i; } });
        return bestD <= within ? best : -1;
    }

    // The crosshair, a dot on each line and a card, laid out like the Traffic page's
    function drawTooltip(ctx, width, x, y, gap, bottom) {
        const di = nearest(history.dns, hoverT, gap);
        const gi = nearest(history.gateway, hoverT, gap);
        if (di < 0 && gi < 0) return;
        const t = di >= 0 ? history.dns.timestamps[di] : history.gateway.timestamps[gi];
        const hx = x(t);

        ctx.strokeStyle = COLOR.crosshair;
        ctx.lineWidth = 1;
        ctx.setLineDash([3, 3]);
        ctx.beginPath(); ctx.moveTo(hx, PAD.top); ctx.lineTo(hx, bottom); ctx.stroke();
        ctx.setLineDash([]);

        const dot = (v, color) => {
            if (v === null || v === undefined) return;
            ctx.fillStyle = color;
            ctx.beginPath(); ctx.arc(hx, y(v), 4.5, 0, Math.PI * 2); ctx.fill();
            ctx.strokeStyle = '#0F172A'; ctx.lineWidth = 2; ctx.stroke();
        };
        const lossText = (p) => (p < 10 ? p.toFixed(1) : p.toFixed(0)) + '%';

        // rows: a coloured dot and a line of text, plus plain notes
        const rows = [];
        if (di >= 0) {
            const a = history.dns.avg_ms[di];
            dot(a, COLOR.avg);
            rows.push({ dot: COLOR.avg, text: a === null ? 'DNS server: no answer' : `DNS server:  ${formatMs(a)}` });
            if (a !== null) rows.push({ note: `Best ${formatMs(history.dns.min_ms[di])}   Worst ${formatMs(history.dns.max_ms[di])}` });
            if (history.dns.loss_pct[di]) rows.push({ note: `Lost ${lossText(history.dns.loss_pct[di])}`, color: '#f87171' });
        }
        if (gi >= 0) {
            const g = history.gateway.avg_ms[gi];
            dot(g, COLOR.gateway);
            rows.push({ dot: COLOR.gateway, text: g === null ? 'Gateway: no answer' : `Gateway:  ${formatMs(g)}` });
        }

        const timeStr = fmtTime(t, range === '30d' || range === '24h' ? 'date-time' : 'time');
        const boxWidth = 190;
        const boxHeight = 32 + rows.length * 16;
        let boxX = hx + 12;
        if (boxX + boxWidth > width - PAD.right) boxX = hx - boxWidth - 12;

        ctx.fillStyle = COLOR.cardBg;
        ctx.strokeStyle = COLOR.cardBorder;
        ctx.lineWidth = 1;
        ctx.beginPath(); ctx.roundRect(boxX, PAD.top + 8, boxWidth, boxHeight, 6); ctx.fill(); ctx.stroke();

        ctx.textAlign = 'left';
        ctx.fillStyle = COLOR.muted;
        ctx.font = `10px ${FONT}`;
        ctx.fillText(timeStr, boxX + 12, PAD.top + 25);

        rows.forEach((r, k) => {
            const ty = PAD.top + 43 + k * 16;
            if (r.dot) {
                ctx.fillStyle = r.dot;
                ctx.beginPath(); ctx.arc(boxX + 14, ty - 4, 3.5, 0, Math.PI * 2); ctx.fill();
                ctx.fillStyle = '#E2E8F0';
                ctx.font = `11px ${FONT}`;
                ctx.fillText(r.text, boxX + 24, ty);
            } else {
                ctx.fillStyle = r.color || COLOR.text;
                ctx.font = `10px ${FONT}`;
                ctx.fillText(r.note, boxX + 24, ty);
            }
        });
    }

    async function loadHealth() {
        const res = await apiFetch('/api/v1/uplinks/wan/health?range=' + range);
        if (!res || !res.ok) return;
        const data = await res.json();
        if (data.range !== range) return; // the person switched ranges while this was loading
        history = data;
        hoverT = null;
        renderStatus();
        drawChart();
    }

    function scheduleHealth() {
        clearInterval(timer);
        timer = setInterval(loadHealth, REFRESH_MS[range]);
    }

    function pickRange(r) {
        range = r;
        document.querySelectorAll('#inet-picker .time-btn').forEach(b => b.classList.toggle('active', b.dataset.range === r));
        history = null;
        drawChart();
        loadHealth();
        scheduleHealth();
    }

    function canvasMove(e) {
        if (!history) return;
        const canvas = $('inet-canvas');
        const rect = canvas.getBoundingClientRect();
        const mx = e.clientX - rect.left;
        const gw = rect.width - PAD.left - PAD.right;
        if (mx < PAD.left || mx > rect.width - PAD.right) { hoverT = null; drawChart(); return; }
        const t = Date.now() - RANGE_MS[range] + ((mx - PAD.left) / gw) * RANGE_MS[range];
        // snap to the closest measurement of either series
        let best = null, bestD = Infinity;
        [history.dns.timestamps, history.gateway.timestamps].forEach(list => list.forEach(v => {
            const d = Math.abs(v - t);
            if (d < bestD) { bestD = d; best = v; }
        }));
        hoverT = best;
        drawChart();
    }

    // ---------------------------------------------------------------- usage

    let usageTimer = null;
    let volumeTimer = null;
    let wanPort = null;       // the port whose traffic is shown

    function setUsage(prefix, values) {
        ['now', '24', '30'].forEach((k, i) => { $(`inet-${prefix}-${k}`).textContent = values[i]; });
    }

    function clearUsage(note) {
        setUsage('dl', ['\u2013', '\u2013', '\u2013']);
        setUsage('ul', ['\u2013', '\u2013', '\u2013']);
        $('inet-usage-port').textContent = '\u2013';
        $('inet-usage-note').textContent = note || '';
        $('inet-usage-note').classList.toggle('hidden', !note);
    }

    async function pollUsage() {
        const res = await apiFetch('/api/v1/system/stats');
        if (!res || !res.ok) return;
        const stats = await res.json();
        const entry = Object.entries(stats.interfaces || {}).find(([, m]) => m.role === 'wan');
        if (!entry) {
            if (wanPort !== null) { wanPort = null; }
            clearUsage('No port is set as the internet port yet. Choose one on the Ports page.');
            return;
        }
        const [name, meta] = entry;
        $('inet-usage-note').classList.add('hidden');
        $('inet-usage-port').textContent = name;
        $('inet-dl-now').textContent = formatRate(meta.rx_bytes_sec);
        $('inet-ul-now').textContent = formatRate(meta.tx_bytes_sec);
        if (name !== wanPort) {
            wanPort = name;
            loadVolumes();
        }
    }

    // The history holds the average rate of each bucket, so a bucket's volume is rate x its length.
    // Time the router was off has no buckets and adds nothing, so these are floors, not exact totals.
    async function volumeOf(port, rangeName, bucketSeconds) {
        const res = await apiFetch(`/api/v1/system/traffic?iface=${encodeURIComponent(port)}&range=${rangeName}`);
        if (!res || !res.ok) return null;
        const data = await res.json();
        const sum = (arr) => (arr || []).reduce((a, b) => a + b, 0) * bucketSeconds;
        return { rx: sum(data.rx), tx: sum(data.tx) };
    }

    async function loadVolumes() {
        if (!wanPort) return;
        const port = wanPort;
        const [day, month] = await Promise.all([volumeOf(port, '24h', 900), volumeOf(port, '30d', 3600)]);
        if (port !== wanPort) return;
        $('inet-dl-24').textContent = day ? formatBytes(day.rx) : '\u2013';
        $('inet-ul-24').textContent = day ? formatBytes(day.tx) : '\u2013';
        $('inet-dl-30').textContent = month ? formatBytes(month.rx) : '\u2013';
        $('inet-ul-30').textContent = month ? formatBytes(month.tx) : '\u2013';
    }

    // ---------------------------------------------------------------- connection settings

    function showMessage(text, kind) {
        const box = $('inet-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    function clearFieldErrors() {
        document.querySelectorAll('#inet-card [data-error]').forEach(p => { p.textContent = ''; p.classList.add('hidden'); });
        Object.values(FIELD_INPUTS).forEach(id => $(id).removeAttribute('aria-invalid'));
    }

    function showFieldErrors(errors) {
        const leftovers = [];
        (errors || []).forEach(e => {
            const target = document.querySelector(`#inet-card [data-error="${e.field}"]`);
            if (!target) { leftovers.push(e.message); return; }
            target.textContent = (target.textContent ? target.textContent + ' ' : '') + e.message;
            target.classList.remove('hidden');
            const input = $(FIELD_INPUTS[e.field]);
            if (input) input.setAttribute('aria-invalid', 'true');
        });
        return leftovers;
    }

    const MODES = ['dhcp', 'static', 'pppoe'];
    const mode = () => (MODES.find(m => $('inet-mode-' + m).checked) || 'dhcp');
    const dnsList = () => $('inet-dns').value.split(/[\s,]+/).filter(Boolean);
    const intOf = (v) => (/^\d+$/.test(v.trim()) ? parseInt(v.trim(), 10) : (v.trim() === '' ? 0 : NaN));

    function formValues() {
        return {
            mode: mode(),
            ip: $('inet-ip').value.trim(),
            netmask: $('inet-netmask').value.trim(),
            gateway: $('inet-gateway').value.trim(),
            dns: dnsList(),
            vlan: $('inet-vlan').value.trim(),
            mtu: $('inet-mtu').value.trim(),
            mac: $('inet-mac').value.trim(),
            hostname: $('inet-hostname').value.trim(),
            pppoeUser: $('inet-pppoe-user').value.trim(),
            pppoePass: $('inet-pppoe-pass').value, // empty: the saved one stays
            pppoeService: $('inet-pppoe-service').value.trim(),
        };
    }

    function fillForm(u) {
        MODES.forEach(m => { $('inet-mode-' + m).checked = u.mode === m; });
        $('inet-ip').value = u.ip || '';
        $('inet-netmask').value = u.netmask || '';
        $('inet-gateway').value = u.gateway || '';
        $('inet-dns').value = (u.dns || []).join(', ');
        $('inet-vlan').value = u.vlan ? String(u.vlan) : '';
        $('inet-mtu').value = u.mtu ? String(u.mtu) : '';
        $('inet-mac').value = u.mac || '';
        $('inet-hostname').value = u.hostname || '';
        $('inet-pppoe-user').value = (u.pppoe && u.pppoe.username) || '';
        $('inet-pppoe-pass').value = '';
        $('inet-pppoe-service').value = (u.pppoe && u.pppoe.service) || '';
        // the advanced settings open by themselves when any is in use
        if (u.vlan || u.mtu || u.mac) $('inet-advanced').open = true;
    }

    // What differs from the saved draft. Only the settings of the chosen type count.
    function isDirty() {
        if (!uplink) return false;
        const v = formValues();
        if (v.mode !== uplink.mode) return true;
        const same = (a, b) => String(a) === String(b);
        if (!same(v.vlan, uplink.vlan || '') || !same(v.mtu, uplink.mtu || '') || v.mac !== (uplink.mac || '')) return true;
        if (v.mode === 'static') {
            if (v.dns.join(',') !== (uplink.dns || []).join(',')) return true;
            return v.ip !== (uplink.ip || '') || v.netmask !== (uplink.netmask || '') || v.gateway !== (uplink.gateway || '');
        }
        if (v.mode === 'dhcp') return v.hostname !== (uplink.hostname || '');
        const p = uplink.pppoe || {};
        return v.pppoeUser !== (p.username || '') || v.pppoeService !== (p.service || '') || v.pppoePass !== '';
    }

    function body() {
        const v = formValues();
        const out = { mode: v.mode, vlan: intOf(v.vlan), mtu: intOf(v.mtu), mac: v.mac };
        if (v.mode === 'static') {
            Object.assign(out, { ip: v.ip, netmask: v.netmask, gateway: v.gateway, dns: v.dns });
        } else {
            // the servers the router asks instead of the provider's are chosen on the DNS page: left alone here
            if (v.mode === 'dhcp') out.hostname = v.hostname;
            else out.pppoe = Object.assign({ username: v.pppoeUser, service: v.pppoeService }, v.pppoePass !== '' ? { password: v.pppoePass } : {});
        }
        return out;
    }

    function renderLive() {
        const live = uplink && uplink.live;
        const pppoe = uplink && uplink.mode === 'pppoe';
        $('inet-live-link').textContent = live ? (live.up ? 'Connected' : (pppoe ? 'Not connected' : 'No link')) : '\u2013';
        $('inet-live-address').textContent = (live && live.address) || '\u2013';
        $('inet-live-gateway').textContent = (live && live.gateway) || '\u2013';
        $('inet-live-dns').textContent = (live && live.dns && live.dns.length) ? live.dns.join(', ') : '\u2013';
        $('inet-live-device').textContent = (live && live.device) || '\u2013';
        $('inet-live-mac').textContent = (live && live.mac) || '\u2013';
        $('inet-live-mtu').textContent = (live && live.mtu) ? String(live.mtu) : '\u2013';
    }

    function renderForm() {
        if (!uplink) return;
        $('inet-port').textContent = uplink.port || 'none yet';
        $('inet-draft').classList.toggle('hidden', !uplink.pending);
        renderLive();

        const m = mode();
        $('inet-static-fields').classList.toggle('hidden', m !== 'static');
        $('inet-pppoe-fields').classList.toggle('hidden', m !== 'pppoe');
        $('inet-dhcp-fields').classList.toggle('hidden', m !== 'dhcp');

        // DNS: required here with a static address; otherwise it is chosen on the DNS page
        $('inet-dns-grid').classList.toggle('hidden', m !== 'static');
        $('inet-dns-moved').classList.toggle('hidden', m === 'static');
        $('inet-dns-label').textContent = 'DNS servers';
        $('inet-dns-hint').textContent = 'Up to three, separated by commas. The first one is also what the health chart pings.';
        // the MTU the connection uses when this is empty
        const usual = m === 'pppoe' ? 1492 : 1500;
        $('inet-mtu').placeholder = String(usual);
        $('inet-mtu-hint').textContent = m === 'pppoe'
            ? 'PPPoE uses 1492 unless you set another (at most 1492).'
            : 'Empty means the usual 1500. A smaller value is for connections that lose part of each packet to a tunnel.';

        const saved = uplink.pppoe && uplink.pppoe.password_set;
        $('inet-pppoe-pass').placeholder = saved ? 'unchanged' : '';
        $('inet-pppoe-pass-hint').textContent = saved ? 'A password is saved. Leave this empty to keep it.' : '';

        const lock = !editable || saving;
        ['inet-mode-dhcp', 'inet-mode-static', 'inet-mode-pppoe', 'inet-ip', 'inet-netmask', 'inet-gateway', 'inet-dns',
            'inet-vlan', 'inet-mtu', 'inet-mac', 'inet-hostname', 'inet-pppoe-user', 'inet-pppoe-pass', 'inet-pppoe-service', 'inet-pppoe-reveal']
            .forEach(id => { $(id).disabled = lock; });

        const bad = ['vlan', 'mtu'].filter(k => Number.isNaN(intOf(formValues()[k])));
        const dirty = isDirty();
        $('inet-hint').textContent = !editable
            ? 'A change is being applied or waits for confirmation: the connection cannot be edited right now.'
            : (dirty ? 'You have unsaved changes.' : '');
        $('inet-save').disabled = lock || !dirty || bad.length > 0;
        $('inet-reset').disabled = saving || !dirty;
    }

    async function loadUplink(keepEdits) {
        const res = await apiFetch('/api/v1/uplinks');
        if (!res || !res.ok) { showMessage('Could not load the connection settings.', 'error'); return; }
        const data = await res.json();
        editable = data.editable !== false;
        uplink = (data.uplinks || []).find(u => u.id === 'wan') || (data.uplinks || [])[0] || null;
        if (!uplink) return;
        if (!keepEdits) fillForm(uplink);
        renderForm();
    }

    async function save() {
        clearFieldErrors();
        showMessage('');
        const bad = ['vlan', 'mtu'].filter(k => Number.isNaN(intOf(formValues()[k])));
        if (bad.length) {
            bad.forEach(k => showFieldErrors([{ field: k, message: 'must be a whole number' }]));
            showMessage('Some settings need attention.', 'error');
            return;
        }

        saving = true;
        renderForm();
        const res = await apiFetch('/api/v1/uplinks/' + encodeURIComponent(uplink.id), {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body()),
        });
        saving = false;

        if (!res) { showMessage('The router did not answer. Try again.', 'error'); renderForm(); return; }
        if (!res.ok) {
            let err = {};
            try { err = await res.json(); } catch (e) { /* not JSON */ }
            const leftovers = showFieldErrors(err.errors);
            if (!err.errors || !err.errors.length || leftovers.length) {
                showMessage(leftovers.join(' ') || err.error || `Saving failed (${res.status}).`, 'error');
            } else {
                showMessage('Some settings need attention.', 'error');
            }
            renderForm();
            return;
        }

        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await loadUplink(false);
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function wire() {
        if (wired) return;
        wired = true;

        $('inet-picker').addEventListener('click', (e) => {
            const btn = e.target.closest('.time-btn');
            if (btn) pickRange(btn.dataset.range);
        });
        const canvas = $('inet-canvas');
        canvas.addEventListener('mousemove', canvasMove);
        canvas.addEventListener('mouseleave', () => { hoverT = null; drawChart(); });

        $('inet-card').addEventListener('input', renderForm);
        $('inet-card').addEventListener('change', renderForm);
        $('inet-save').addEventListener('click', save);
        $('inet-pppoe-reveal').addEventListener('click', () => {
            const f = $('inet-pppoe-pass');
            const show = f.type === 'password';
            f.type = show ? 'text' : 'password';
            $('inet-pppoe-reveal').textContent = show ? 'Hide' : 'Show';
            $('inet-pppoe-reveal').setAttribute('aria-pressed', String(show));
        });
        $('inet-reset').addEventListener('click', () => {
            clearFieldErrors();
            showMessage('');
            if (uplink) fillForm(uplink);
            renderForm();
        });
        $('inet-card').addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && e.target.tagName === 'INPUT' && (e.target.type === 'text' || e.target.type === 'password') && !$('inet-save').disabled) {
                e.preventDefault();
                save();
            }
        });
    }

    function init() {
        wire();
        clearFieldErrors();
        showMessage('');
        uplink = null;
        history = null;
        document.querySelectorAll('#inet-picker .time-btn').forEach(b => b.classList.toggle('active', b.dataset.range === range));
        drawChart();
        loadHealth();
        loadUplink(false);
        scheduleHealth();
        wanPort = null;
        clearUsage('');
        pollUsage();
        usageTimer = setInterval(pollUsage, 2000);
        volumeTimer = setInterval(loadVolumes, 60000);
        resizeHandler = () => drawChart();
        window.addEventListener('resize', resizeHandler);
        return () => {
            clearInterval(timer);
            timer = null;
            clearInterval(usageTimer);
            clearInterval(volumeTimer);
            window.removeEventListener('resize', resizeHandler);
        };
    }

    return { init };
})();
