// web/static/scripts/internet.js
// Internet page: connection health (latency to the upstream DNS server) and the connection settings.

window.InternetModule = (function () {
    const RANGE_MS = { '5m': 5 * 60e3, '1h': 3600e3, '24h': 24 * 3600e3, '30d': 30 * 24 * 3600e3 };
    const BUCKET_S = { '5m': 0, '1h': 60, '24h': 900, '30d': 3600 };
    const REFRESH_MS = { '5m': 5000, '1h': 15000, '24h': 30000, '30d': 60000 };

    const COLOR = {
        avg: '#38bdf8',
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
    let hover = -1;
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
        const sum = history.summary || {};
        const state = STATE_TEXT[st.state] ? st.state : 'unknown';

        $('inet-dot').className = 'inet-dot inet-dot--' + state;
        $('inet-state-text').textContent = STATE_TEXT[state];

        let target = '';
        if (st.target) {
            const how = st.method === 'icmp' ? 'Pinging' : st.method === 'dns' ? 'Timing DNS queries to' : 'Checking';
            target = `${how} ${st.target}` + (st.source ? ` \u2013 ${st.source}` : '');
            if (st.method === 'dns') target += '. Ping is not permitted for this program here, so a small DNS query is timed instead.';
        } else if (st.problem) {
            target = st.problem;
        }
        $('inet-target').textContent = target;

        $('inet-now').textContent = state === 'offline' ? 'No answer' : fmtMs(st.rtt_ms);
        $('inet-avg').textContent = fmtMs(sum.avg_ms);
        $('inet-range').textContent = sum.min_ms === null || sum.min_ms === undefined ? '–' : `${fmtMs(sum.min_ms)} / ${fmtMs(sum.max_ms)}`;
        $('inet-loss').textContent = sum.sent ? `${sum.loss_pct < 10 ? sum.loss_pct.toFixed(1) : sum.loss_pct.toFixed(0)}%` : '–';
        $('inet-loss').classList.toggle('inet-bad', sum.sent > 0 && sum.loss_pct >= 5);
    }

    function niceCeil(v) {
        const steps = [10, 20, 50, 100, 200, 500, 1000, 2000, 5000];
        return steps.find(s => s >= v) || Math.ceil(v / 1000) * 1000;
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
        const ts = history ? history.timestamps : [];
        const avg = history ? history.avg_ms : [];
        const lo = history ? history.min_ms : [];
        const hi = history ? history.max_ms : [];
        const loss = history ? history.loss_pct : [];

        ctx.font = '11px system-ui, sans-serif';
        if (!ts.length) {
            ctx.fillStyle = COLOR.muted;
            ctx.textAlign = 'center';
            const st = history && history.status ? history.status.state : '';
            ctx.fillText(st === 'not_configured' ? 'Nothing to measure until an internet port and DNS server are set up.'
                : history ? 'No measurements stored for this window yet.' : 'Loading…', width / 2, height / 2);
            return;
        }

        const maxMs = niceCeil(Math.max(5, ...hi.filter(v => v !== null), ...avg.filter(v => v !== null)) * 1.1);
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
        for (let i = 0; i < ts.length; i++) {
            if (loss[i] < 1) continue;
            ctx.fillStyle = `rgba(${COLOR.loss}, ${Math.min(0.7, 0.1 + (loss[i] / 100) * 0.9)})`;
            ctx.fillRect(x(ts[i]) - barW / 2, PAD.top, barW, gh);
        }

        // runs of consecutive answered points
        const runs = [];
        let run = [];
        for (let i = 0; i < ts.length; i++) {
            const broken = avg[i] === null || (run.length && ts[i] - ts[run[run.length - 1]] > gap);
            if (broken && run.length) { runs.push(run); run = []; }
            if (avg[i] !== null) run.push(i);
        }
        if (run.length) runs.push(run);

        runs.forEach(r => {
            if (r.length > 1) {
                ctx.beginPath();
                r.forEach((i, k) => (k ? ctx.lineTo(x(ts[i]), y(hi[i])) : ctx.moveTo(x(ts[i]), y(hi[i]))));
                for (let k = r.length - 1; k >= 0; k--) ctx.lineTo(x(ts[r[k]]), y(lo[r[k]]));
                ctx.closePath();
                ctx.fillStyle = COLOR.band;
                ctx.fill();

                ctx.beginPath();
                r.forEach((i, k) => (k ? ctx.lineTo(x(ts[i]), y(avg[i])) : ctx.moveTo(x(ts[i]), y(avg[i]))));
                ctx.strokeStyle = COLOR.avg;
                ctx.lineWidth = 2;
                ctx.lineJoin = 'round';
                ctx.stroke();
            } else {
                ctx.fillStyle = COLOR.avg;
                ctx.beginPath(); ctx.arc(x(ts[r[0]]), y(avg[r[0]]), 2.5, 0, Math.PI * 2); ctx.fill();
            }
        });

        if (hover >= 0 && hover < ts.length) drawTooltip(ctx, width, x, y);
    }

    function drawTooltip(ctx, width, x, y) {
        const i = hover;
        const hx = x(history.timestamps[i]);
        ctx.strokeStyle = COLOR.crosshair;
        ctx.lineWidth = 1;
        ctx.setLineDash([3, 3]);
        ctx.beginPath(); ctx.moveTo(hx, PAD.top); ctx.lineTo(hx, PAD.top + 230 - PAD.top - PAD.bottom); ctx.stroke();
        ctx.setLineDash([]);

        const a = history.avg_ms[i];
        if (a !== null) {
            ctx.fillStyle = COLOR.avg;
            ctx.beginPath(); ctx.arc(hx, y(a), 4.5, 0, Math.PI * 2); ctx.fill();
            ctx.strokeStyle = '#0F172A'; ctx.lineWidth = 2; ctx.stroke();
        }

        const lines = [
            fmtTime(history.timestamps[i], range === '30d' || range === '24h' ? 'date-time' : 'time'),
            a === null ? 'No answer' : `Average ${fmtMs(a)}`,
            a === null ? '' : `Best ${fmtMs(history.min_ms[i])}  Worst ${fmtMs(history.max_ms[i])}`,
            history.loss_pct[i] ? `Lost ${history.loss_pct[i] < 10 ? history.loss_pct[i].toFixed(1) : history.loss_pct[i].toFixed(0)}%` : '',
        ].filter(Boolean);

        const bw = 190, bh = 14 * lines.length + 14;
        let bx = hx + 12;
        if (bx + bw > width - PAD.right) bx = hx - bw - 12;
        ctx.fillStyle = COLOR.cardBg;
        ctx.strokeStyle = COLOR.cardBorder;
        ctx.lineWidth = 1;
        ctx.beginPath(); ctx.roundRect(bx, PAD.top + 8, bw, bh, 6); ctx.fill(); ctx.stroke();
        ctx.textAlign = 'left';
        lines.forEach((l, k) => {
            ctx.fillStyle = k === 0 ? COLOR.muted : (l.startsWith('Lost') ? '#f87171' : '#E2E8F0');
            ctx.font = (k === 0 ? '10px' : '11px') + ' system-ui, sans-serif';
            ctx.fillText(l, bx + 12, PAD.top + 26 + k * 14);
        });
    }

    async function loadHealth() {
        const res = await apiFetch('/api/v1/uplinks/wan/health?range=' + range);
        if (!res || !res.ok) return;
        const data = await res.json();
        if (data.range !== range) return; // the person switched ranges while this was loading
        history = data;
        hover = -1;
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
        if (!history || !history.timestamps.length) return;
        const canvas = $('inet-canvas');
        const rect = canvas.getBoundingClientRect();
        const mx = e.clientX - rect.left;
        const gw = rect.width - PAD.left - PAD.right;
        if (mx < PAD.left || mx > rect.width - PAD.right) { hover = -1; drawChart(); return; }
        const t = Date.now() - RANGE_MS[range] + ((mx - PAD.left) / gw) * RANGE_MS[range];
        let best = -1, bestD = Infinity;
        history.timestamps.forEach((v, i) => { const d = Math.abs(v - t); if (d < bestD) { bestD = d; best = i; } });
        hover = best;
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
        canvas.addEventListener('mouseleave', () => { hover = -1; drawChart(); });

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
