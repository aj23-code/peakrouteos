// web/static/scripts/internet.js
// Internet page: connection health (latency to the upstream DNS server) and the connection settings.

window.InternetModule = (function () {
    const RANGE_MS = { '5m': 5 * 60e3, '1h': 3600e3, '24h': 24 * 3600e3, '30d': 30 * 24 * 3600e3 };
    const BUCKET_S = { '5m': 0, '1h': 60, '24h': 900, '30d': 3600 };
    const REFRESH_MS = { '5m': 5000, '1h': 15000, '24h': 30000, '30d': 60000 };

    const COLOR = {
        avg: '#38bdf8',
        gateway: '#34d399',
        band: 'rgba(56, 189, 248, 0.18)',
        loss: '239, 68, 68',
        grid: 'rgba(255, 255, 255, 0.05)',
        text: '#94A3B8',
        muted: '#64748B',
        crosshair: 'rgba(255, 255, 255, 0.25)',
        cardBg: 'rgba(15, 23, 42, 0.95)',
        cardBorder: '#334155',
    };
    const PAD = { top: 20, right: 25, bottom: 35, left: 60 };

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
    const FIELD_INPUTS = { ip: 'inet-ip', netmask: 'inet-netmask', gateway: 'inet-gateway', dns: 'inet-dns' };

    // ---------------------------------------------------------------- health

    function fmtMs(v) {
        if (v === null || v === undefined) return '–';
        return (v >= 100 ? v.toFixed(0) : v.toFixed(1)) + ' ms';
    }

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

        $('inet-now').textContent = state === 'offline' ? 'No answer' : fmtMs(st.rtt_ms);
        $('inet-avg').textContent = fmtMs(sum.avg_ms);
        $('inet-range').textContent = sum.min_ms === null || sum.min_ms === undefined ? '–' : `${fmtMs(sum.min_ms)} / ${fmtMs(sum.max_ms)}`;
        $('inet-loss').textContent = sum.sent ? `${sum.loss_pct < 10 ? sum.loss_pct.toFixed(1) : sum.loss_pct.toFixed(0)}%` : '–';
        $('inet-loss').classList.toggle('inet-bad', sum.sent > 0 && sum.loss_pct >= 5);

        const gwEl = $('inet-gw');
        if (gw.state === 'online') gwEl.textContent = fmtMs(gw.rtt_ms);
        else if (gw.state === 'offline') gwEl.textContent = 'No answer';
        else if (gw.state === 'unknown') gwEl.textContent = '–';
        else gwEl.textContent = 'Not measured';
        gwEl.title = [gw.target ? `Gateway ${gw.target}` : '', gw.problem || ''].filter(Boolean).join(' – ');
        $('inet-gw-key').classList.toggle('hidden', !(history.gateway.timestamps || []).length);
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
        const now = Date.now();
        const minT = now - rangeMs;
        const dns = history ? history.dns : null;
        const gate = history ? history.gateway : null;

        ctx.font = '11px system-ui, sans-serif';
        if (!dns || (!dns.timestamps.length && !gate.timestamps.length)) {
            ctx.fillStyle = COLOR.muted;
            ctx.textAlign = 'center';
            const st = history && history.status ? history.status.state : '';
            ctx.fillText(st === 'not_configured' ? 'Nothing to measure until an internet port and DNS server are set up.'
                : history ? 'No measurements stored for this window yet.' : 'Loading…', width / 2, height / 2);
            return;
        }

        const tops = [...dns.max_ms, ...dns.avg_ms, ...gate.avg_ms].filter(v => v !== null);
        const maxMs = niceCeil(Math.max(5, ...tops) * 1.1);
        const x = (t) => PAD.left + Math.max(0, Math.min(1, (t - minT) / rangeMs)) * gw;
        const y = (v) => PAD.top + gh - (v / maxMs) * gh;

        // grid and y labels
        ctx.strokeStyle = COLOR.grid;
        ctx.lineWidth = 1;
        ctx.fillStyle = COLOR.text;
        ctx.textAlign = 'right';
        for (let i = 0; i <= 4; i++) {
            const gy = PAD.top + (gh / 4) * i;
            ctx.beginPath(); ctx.moveTo(PAD.left, gy); ctx.lineTo(width - PAD.right, gy); ctx.stroke();
            ctx.fillText(`${Math.round(maxMs - (maxMs / 4) * i)} ms`, PAD.left - 8, gy + 4);
        }
        // x labels
        ctx.textAlign = 'center';
        const mode = range === '30d' ? 'date' : range === '24h' ? 'date-time' : 'time';
        for (let i = 0; i < 5; i++) {
            const t = minT + (rangeMs / 4) * i;
            ctx.fillText(fmtTime(t, mode), PAD.left + (gw / 4) * i, height - 10);
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

        // the DNS server: best-to-worst band and the average line
        runsOf(dns.timestamps, dns.avg_ms, gap).forEach(r => {
            const ts = dns.timestamps;
            if (r.length > 1) {
                ctx.beginPath();
                r.forEach((i, k) => (k ? ctx.lineTo(x(ts[i]), y(dns.max_ms[i])) : ctx.moveTo(x(ts[i]), y(dns.max_ms[i]))));
                for (let k = r.length - 1; k >= 0; k--) ctx.lineTo(x(ts[r[k]]), y(dns.min_ms[r[k]]));
                ctx.closePath();
                ctx.fillStyle = COLOR.band;
                ctx.fill();

                ctx.beginPath();
                r.forEach((i, k) => (k ? ctx.lineTo(x(ts[i]), y(dns.avg_ms[i])) : ctx.moveTo(x(ts[i]), y(dns.avg_ms[i]))));
                ctx.strokeStyle = COLOR.avg;
                ctx.lineWidth = 2;
                ctx.lineJoin = 'round';
                ctx.stroke();
            } else {
                ctx.fillStyle = COLOR.avg;
                ctx.beginPath(); ctx.arc(x(ts[r[0]]), y(dns.avg_ms[r[0]]), 2.5, 0, Math.PI * 2); ctx.fill();
            }
        });

        // the gateway: a thinner line on top
        runsOf(gate.timestamps, gate.avg_ms, gap).forEach(r => {
            const ts = gate.timestamps;
            if (r.length > 1) {
                ctx.beginPath();
                r.forEach((i, k) => (k ? ctx.lineTo(x(ts[i]), y(gate.avg_ms[i])) : ctx.moveTo(x(ts[i]), y(gate.avg_ms[i]))));
                ctx.strokeStyle = COLOR.gateway;
                ctx.lineWidth = 1.5;
                ctx.lineJoin = 'round';
                ctx.stroke();
            } else {
                ctx.fillStyle = COLOR.gateway;
                ctx.beginPath(); ctx.arc(x(ts[r[0]]), y(gate.avg_ms[r[0]]), 2, 0, Math.PI * 2); ctx.fill();
            }
        });

        if (hoverT !== null) drawTooltip(ctx, width, x, y, gap);
    }

    // The point of a series closest to a time, if it is close enough to be the same moment
    function nearest(series, t, within) {
        let best = -1, bestD = Infinity;
        series.timestamps.forEach((v, i) => { const d = Math.abs(v - t); if (d < bestD) { bestD = d; best = i; } });
        return bestD <= within ? best : -1;
    }

    function drawTooltip(ctx, width, x, y, gap) {
        const di = nearest(history.dns, hoverT, gap);
        const gi = nearest(history.gateway, hoverT, gap);
        if (di < 0 && gi < 0) return;
        const t = di >= 0 ? history.dns.timestamps[di] : history.gateway.timestamps[gi];
        const hx = x(t);

        ctx.strokeStyle = COLOR.crosshair;
        ctx.lineWidth = 1;
        ctx.setLineDash([3, 3]);
        ctx.beginPath(); ctx.moveTo(hx, PAD.top); ctx.lineTo(hx, 230 - PAD.bottom); ctx.stroke();
        ctx.setLineDash([]);

        const dot = (v, color) => {
            if (v === null || v === undefined) return;
            ctx.fillStyle = color;
            ctx.beginPath(); ctx.arc(hx, y(v), 4.5, 0, Math.PI * 2); ctx.fill();
            ctx.strokeStyle = '#0F172A'; ctx.lineWidth = 2; ctx.stroke();
        };
        const lossText = (p) => (p < 10 ? p.toFixed(1) : p.toFixed(0)) + '%';

        const lines = [{ text: fmtTime(t, range === '30d' || range === '24h' ? 'date-time' : 'time'), color: COLOR.muted, small: true }];
        if (di >= 0) {
            const a = history.dns.avg_ms[di];
            dot(a, COLOR.avg);
            lines.push({ text: a === null ? 'DNS server: no answer' : `DNS server  ${fmtMs(a)}`, color: '#E2E8F0' });
            if (a !== null) lines.push({ text: `Best ${fmtMs(history.dns.min_ms[di])}  Worst ${fmtMs(history.dns.max_ms[di])}`, color: COLOR.text });
            if (history.dns.loss_pct[di]) lines.push({ text: `Lost ${lossText(history.dns.loss_pct[di])}`, color: '#f87171' });
        }
        if (gi >= 0) {
            const g = history.gateway.avg_ms[gi];
            dot(g, COLOR.gateway);
            lines.push({ text: g === null ? 'Gateway: no answer' : `Gateway  ${fmtMs(g)}`, color: COLOR.gateway });
        }

        const bw = 200, bh = 15 * lines.length + 12;
        let bx = hx + 12;
        if (bx + bw > width - PAD.right) bx = hx - bw - 12;
        ctx.fillStyle = COLOR.cardBg;
        ctx.strokeStyle = COLOR.cardBorder;
        ctx.lineWidth = 1;
        ctx.beginPath(); ctx.roundRect(bx, PAD.top + 8, bw, bh, 6); ctx.fill(); ctx.stroke();
        ctx.textAlign = 'left';
        lines.forEach((l, k) => {
            ctx.fillStyle = l.color;
            ctx.font = (l.small ? '10px' : '11px') + ' system-ui, sans-serif';
            ctx.fillText(l.text, bx + 12, PAD.top + 26 + k * 15);
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

    const mode = () => ($('inet-mode-static').checked ? 'static' : 'dhcp');
    const dnsList = () => $('inet-dns').value.split(/[\s,]+/).filter(Boolean);

    function formValues() {
        return {
            mode: mode(),
            ip: $('inet-ip').value.trim(),
            netmask: $('inet-netmask').value.trim(),
            gateway: $('inet-gateway').value.trim(),
            dns: dnsList(),
        };
    }

    function fillForm(u) {
        $('inet-mode-static').checked = u.mode === 'static';
        $('inet-mode-dhcp').checked = u.mode !== 'static';
        $('inet-ip').value = u.ip || '';
        $('inet-netmask').value = u.netmask || '';
        $('inet-gateway').value = u.gateway || '';
        $('inet-dns').value = (u.dns || []).join(', ');
    }

    function isDirty() {
        if (!uplink) return false;
        const v = formValues();
        if (v.mode !== uplink.mode) return true;
        if (v.mode === 'dhcp') return false; // the static values do not matter
        return v.ip !== (uplink.ip || '') || v.netmask !== (uplink.netmask || '') ||
            v.gateway !== (uplink.gateway || '') || v.dns.join(',') !== (uplink.dns || []).join(',');
    }

    function renderLive() {
        const live = uplink && uplink.live;
        $('inet-live-link').textContent = live ? (live.up ? 'Connected' : 'No link') : '–';
        $('inet-live-address').textContent = (live && live.address) || '–';
        $('inet-live-gateway').textContent = (live && live.gateway) || '–';
        $('inet-live-dns').textContent = (live && live.dns && live.dns.length) ? live.dns.join(', ') : '–';
    }

    function renderForm() {
        if (!uplink) return;
        $('inet-port').textContent = uplink.port || 'none yet';
        $('inet-draft').classList.toggle('hidden', !uplink.pending);
        renderLive();

        const isStatic = mode() === 'static';
        $('inet-static-fields').classList.toggle('hidden', !isStatic);
        const lock = !editable || saving;
        ['inet-mode-dhcp', 'inet-mode-static', 'inet-ip', 'inet-netmask', 'inet-gateway', 'inet-dns'].forEach(id => { $(id).disabled = lock; });

        const dirty = isDirty();
        $('inet-hint').textContent = !editable
            ? 'A change is being applied or waits for confirmation: the connection cannot be edited right now.'
            : (dirty ? 'You have unsaved changes.' : '');
        $('inet-save').disabled = lock || !dirty;
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
        const v = formValues();
        const body = v.mode === 'dhcp' ? { mode: 'dhcp' } : v;

        saving = true;
        renderForm();
        const res = await apiFetch('/api/v1/uplinks/' + encodeURIComponent(uplink.id), {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
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
        $('inet-reset').addEventListener('click', () => {
            clearFieldErrors();
            showMessage('');
            if (uplink) fillForm(uplink);
            renderForm();
        });
        $('inet-card').addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && e.target.tagName === 'INPUT' && e.target.type === 'text' && !$('inet-save').disabled) {
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
        resizeHandler = () => drawChart();
        window.addEventListener('resize', resizeHandler);
        return () => {
            clearInterval(timer);
            timer = null;
            window.removeEventListener('resize', resizeHandler);
        };
    }

    return { init };
})();
