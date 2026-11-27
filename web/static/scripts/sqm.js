// web/static/scripts/sqm.js
// Smart queue management card on the Internet page: shape the connection a little below the line's real speed so that the queue forms in
// the router and not at the provider (docs/sqm.md). Saving only writes a draft; shaping starts when the change is committed.

window.SQMModule = (function () {
    let data = null;      // GET /api/v1/sqm
    let form = null;      // { enabled, down, up, percent, link, perHost } as typed
    let speed = null;     // GET /api/v1/sqm/speedtest: how the speed test is going
    let speedTimer = null;
    let errors = {};
    let message = null;
    let busy = false;
    let timer = null;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const editable = () => !data || data.editable !== false;
    const text = (n) => (n ? String(n) : '');

    function formOf(d) {
        const nets = {};
        (d.networks || []).forEach(n => { nets[n.id] = { priority: n.priority || 'normal', down: text(n.limit_down_mbit), up: text(n.limit_up_mbit) }; });
        // the second internet connection's line, while there is one
        const second = d.second ? { down: text(d.second.download_mbit), up: text(d.second.upload_mbit), link: d.second.link || 'ethernet' } : null;
        return { enabled: !!d.enabled, down: text(d.download_mbit), up: text(d.upload_mbit), percent: String(d.percent || d.default_percent || 95), link: d.link || 'ethernet', perHost: !!d.per_host, nets, second };
    }
    const pct = (f) => { const v = parseInt(String(f.percent), 10); return Number.isFinite(v) ? v : 0; };
    const nice = (n) => (Math.round(n * 10) / 10).toString();
    // what the router will shape to, from the form as typed
    const shaped = (mbit, f) => (mbit > 0 ? nice(Math.max(0.001, mbit * pct(f) / 100)) : null);
    const num = (s) => { const v = parseFloat(String(s).replace(',', '.')); return Number.isFinite(v) && v > 0 ? v : 0; };
    const sameNets = (a, b) => Object.keys(a).length === Object.keys(b).length &&
        Object.keys(a).every(id => b[id] && a[id].priority === b[id].priority && num(a[id].down) === num(b[id].down) && num(a[id].up) === num(b[id].up));
    const sameSecond = (a, b) => (!a && !b) || (!!a && !!b && num(a.down) === num(b.down) && num(a.up) === num(b.up) && a.link === b.link);
    const same = (a, b) => a.enabled === b.enabled && num(a.down) === num(b.down) && num(a.up) === num(b.up) && pct(a) === pct(b) && a.link === b.link && a.perHost === b.perHost && sameNets(a.nets, b.nets) && sameSecond(a.second, b.second);
    const dirty = () => !!(data && form && !same(form, formOf(data)));

    function showMessage(t, kind) {
        message = t ? { text: t, kind } : null;
    }

    const ms = (us) => (us / 1000).toFixed(us < 10000 ? 1 : 0);
    const mbit = (kbit) => (kbit >= 1000 ? (kbit / 1000).toFixed(kbit % 1000 ? 1 : 0) : (kbit / 1000).toFixed(2)) + ' Mbit/s';

    function directionHtml(label, d) {
        if (!d || !d.active) return `<div class="net-row"><strong>${label}</strong> <span class="tag">not shaped</span></div>`;
        return `<div class="net-row"><strong>${label}</strong> <span class="tag tag-ok">${escapeHtml(mbit(d.bandwidth_kbit))}</span>
            <span class="form-hint">queue wait ${ms(d.avg_delay_us)} ms on average, ${ms(d.peak_delay_us)} ms at worst &middot; ${d.drops.toLocaleString()} packets dropped to keep it short</span></div>`;
    }

    // the speed test: a button, how it is going, and what it measured (which is put into the fields above, to be saved or changed)
    function speedHtml(lock, lineId) {
        lineId = lineId || 'wan';
        const running = speed && speed.state === 'running';
        const mine = !speed || (speed.line || 'wan') === lineId; // the answer is about this line's test
        let line = '';
        if (running && mine) line = `<span class="form-hint">Measuring the ${speed.phase === 'upload' ? 'upload' : 'download'}&hellip; this takes about twenty seconds${speed.pauses_shaping ? ', and shaping is paused meanwhile' : ''}.</span>`;
        else if (mine && speed && speed.state === 'done' && speed.result) line = `<span class="form-hint">Measured ${nice(speed.result.download_mbit)} down and ${nice(speed.result.upload_mbit)} up, and put in the fields above. Check them, then save.</span>`;
        else if (mine && speed && speed.state === 'error') line = `<span class="form-hint net-warn">${escapeHtml(speed.error || 'The speed test failed.')}</span>`;
        const two = !!(data && data.second);
        const label = two ? (lineId === 'wan2' ? 'Measure Internet 2' : 'Measure Internet') : 'Measure my speed';
        return `<div class="form-actions" style="justify-content:flex-start;gap:12px">
            <button type="button" class="cfg-btn" id="${lineId === 'wan2' ? 'sqm-measure-2' : 'sqm-measure'}"${lock || running ? ' disabled' : ''}>${running && mine ? 'Measuring&hellip;' : label}</button>${line}</div>
            <p class="form-hint">The test downloads and uploads test data (up to about 400 MB each way, less on a slower line) with Cloudflare&rsquo;s public speed test (speed.cloudflare.com). It is an estimate: run it when nothing else is using the line, and use your plan&rsquo;s speeds instead if you know them.</p>`;
    }

    const PRIORITIES = [['low', 'Low'], ['normal', 'Normal'], ['high', 'High']];

    // each network's priority and limits on the shaped line, and what that makes of it (the numbers follow the form as typed)
    function networksHtml(lock) {
        const nets = data.networks || [];
        if (!nets.length) return '';
        const err = (id, k) => (errors[`net.${id}.${k}`] ? `<p class="field-error">${escapeHtml(errors[`net.${id}.${k}`])}</p>` : '');
        const rows = nets.map(n => {
            const f = form.nets[n.id] || { priority: 'normal', down: '', up: '' };
            const opts = PRIORITIES.map(([v, l]) => `<option value="${v}"${f.priority === v ? ' selected' : ''}>${l}</option>`).join('');
            const fresh = same(form, formOf(data)); // the numbers below are the server's: they follow the saved draft, not what is half typed
            const share = fresh && (n.assured_down_mbit || n.assured_up_mbit)
                ? `<span class="form-hint">assured ${n.assured_down_mbit ? nice(n.assured_down_mbit) + ' down' : ''}${n.assured_down_mbit && n.assured_up_mbit ? ', ' : ''}${n.assured_up_mbit ? nice(n.assured_up_mbit) + ' up' : ''} Mbit/s when the line is full</span>` : '';
            const note = fresh && n.inactive ? `<p class="form-hint net-warn">${escapeHtml(n.name)}: ${escapeHtml(n.inactive)}.</p>` : '';
            return `<div class="sqm-net" data-net="${escapeHtml(n.id)}">
                <div class="net-card-head"><strong>${escapeHtml(n.name)}</strong> ${share}</div>
                <div class="form-grid">
                    <div class="form-field"><label for="sqm-pr-${escapeHtml(n.id)}">Priority</label>
                        <select id="sqm-pr-${escapeHtml(n.id)}" class="role-select" data-sqm-net="${escapeHtml(n.id)}" data-field="priority"${lock ? ' disabled' : ''}>${opts}</select>${err(n.id, 'priority')}</div>
                    <div class="form-field"><label for="sqm-nd-${escapeHtml(n.id)}">Download limit (Mbit/s)</label>
                        <input type="text" id="sqm-nd-${escapeHtml(n.id)}" data-sqm-net="${escapeHtml(n.id)}" data-field="down" inputmode="decimal" value="${escapeHtml(f.down)}" placeholder="no limit" autocomplete="off"${lock ? ' disabled' : ''}>${err(n.id, 'limit_down_mbit')}</div>
                    <div class="form-field"><label for="sqm-nu-${escapeHtml(n.id)}">Upload limit (Mbit/s)</label>
                        <input type="text" id="sqm-nu-${escapeHtml(n.id)}" data-sqm-net="${escapeHtml(n.id)}" data-field="up" inputmode="decimal" value="${escapeHtml(f.up)}" placeholder="no limit" autocomplete="off"${lock ? ' disabled' : ''}>${err(n.id, 'limit_up_mbit')}</div>
                </div>${note}</div>`;
        }).join('');
        return `<h3 class="form-section">Per network</h3>
            <p class="form-hint">When the line is full, a <strong>High</strong> priority network is served first and is assured four times the share of a <strong>Low</strong> one (Normal is twice); a network that leaves its share idle lets the others use it. A <strong>limit</strong> is the most a network may use, whatever the others are doing: handy for a guest network. Everything is left as it is until you change one (all Normal, no limits).</p>${rows}`;
    }

    function statusHtml() {
        const block = (running, device, st) => !running ? '' : `<h3 class="form-section">Running now on ${escapeHtml(device)}</h3>
            ${st ? directionHtml('Upload', st.upload) + directionHtml('Download', st.download) : '<p class="form-hint">The queues could not be read.</p>'}`;
        const out = block(data.running, data.wan_device, data.status) + (data.second ? block(data.second.running, data.second.wan_device, data.second.status) : '');
        if (!out) return '';
        return out + '<p class="form-hint">Dropping a few packets while a line is full is how the queue tells busy connections to slow down: it is what keeps the wait short.</p>';
    }

    // the second internet connection's line: its own speeds and kind (the share and the fairness below are for both)
    function secondHtml(lock) {
        const f = form.second;
        const err = (k) => (errors[k] ? `<p class="field-error">${escapeHtml(errors[k])}</p>` : '');
        const links = (data.links || []).map(l => `<option value="${escapeHtml(l.value)}"${f.link === l.value ? ' selected' : ''}>${escapeHtml(l.label)}</option>`).join('');
        return `<h3 class="form-section">Internet 2 (the second connection)</h3>
            <p class="form-hint">Its line has its own speeds. Leave both empty to leave this connection alone.</p>
            <div class="form-grid">
                <div class="form-field"><label for="sqm-down2">Download speed (Mbit/s)</label>
                    <input type="text" id="sqm-down2" data-sqm="s_down" inputmode="decimal" value="${escapeHtml(f.down)}" placeholder="for example 100" autocomplete="off"${lock ? ' disabled' : ''}>${err('second.download_mbit')}</div>
                <div class="form-field"><label for="sqm-up2">Upload speed (Mbit/s)</label>
                    <input type="text" id="sqm-up2" data-sqm="s_up" inputmode="decimal" value="${escapeHtml(f.up)}" placeholder="for example 10" autocomplete="off"${lock ? ' disabled' : ''}>${err('second.upload_mbit')}</div>
                <div class="form-field"><label>The router will shape to</label>
                    <p id="sqm-shaped2" class="sqm-shaped"><strong>${shapedText(f)}</strong></p></div>
                <div class="form-field"><label for="sqm-link2">Kind of connection</label>
                    <select id="sqm-link2" class="role-select" data-sqm="s_link"${lock ? ' disabled' : ''}>${links}</select>${err('second.link')}</div>
            </div>
            ${speedHtml(lock, 'wan2')}`;
    }

    function shapedText(f) {
        f = f || form;
        const d = shaped(num(f.down), form), u = shaped(num(f.up), form);
        if (!d && !u) return 'nothing yet: enter a speed';
        return `${d ? d + ' Mbit/s down' : 'download not shaped'}, ${u ? u + ' Mbit/s up' : 'upload not shaped'}`;
    }

    function render() {
        const box = $('sqm-card');
        if (!box || !data || !form) return;
        const lock = !editable() || busy;
        const on = form.enabled;
        const links = (data.links || []).map(l => `<option value="${escapeHtml(l.value)}"${form.link === l.value ? ' selected' : ''}>${escapeHtml(l.label)}</option>`).join('');
        const err = (k) => (errors[k] ? `<p class="field-error">${escapeHtml(errors[k])}</p>` : '');
        const d = dirty();
        box.innerHTML = `<section class="card" id="sqm-section">
            <div class="net-card-head"><h2>Smart queue management</h2>${data.pending ? '<span class="tag tag-draft">Draft</span>' : ''}${data.running ? '<span class="tag tag-ok">On</span>' : '<span class="tag">Off</span>'}</div>
            <p class="form-hint">Keeps calls, games and web pages quick while something else is downloading or uploading. Without it a full line queues packets for hundreds of milliseconds at your provider; this makes the router hold the queue instead, short and fair. It works best set a little below the speed your line really delivers.</p>
            <label class="toggle toggle--block"><input type="checkbox" data-sqm="enabled"${on ? ' checked' : ''}${lock ? ' disabled' : ''}><span><strong>Shape the internet connection</strong><span class="form-hint">Off: nothing is changed on the connection.</span></span></label>
            <div class="${on ? '' : 'is-off'}">
                <h3 class="form-section">Your line</h3>
                <p class="form-hint">Type the speeds of your plan (what the provider sells you), or measure them. Leave a speed empty to leave that direction alone.</p>
                <div class="form-grid">
                    <div class="form-field"><label for="sqm-down">Download speed (Mbit/s)</label>
                        <input type="text" id="sqm-down" data-sqm="down" inputmode="decimal" value="${escapeHtml(form.down)}" placeholder="for example 500" autocomplete="off"${lock || !on ? ' disabled' : ''}>${err('download_mbit')}</div>
                    <div class="form-field"><label for="sqm-up">Upload speed (Mbit/s)</label>
                        <input type="text" id="sqm-up" data-sqm="up" inputmode="decimal" value="${escapeHtml(form.up)}" placeholder="for example 40" autocomplete="off"${lock || !on ? ' disabled' : ''}>${err('upload_mbit')}</div>
                </div>
                ${speedHtml(lock || !on)}
                <h3 class="form-section">What the router shapes to</h3>
                <div class="form-grid">
                    <div class="form-field"><label for="sqm-percent">Share of your line (%)</label>
                        <input type="text" id="sqm-percent" data-sqm="percent" inputmode="numeric" value="${escapeHtml(form.percent)}" placeholder="${data.default_percent || 95}" autocomplete="off"${lock || !on ? ' disabled' : ''}>${err('percent')}</div>
                    <div class="form-field"><label>The router will shape to</label>
                        <p id="sqm-shaped" class="sqm-shaped"><strong>${shapedText()}</strong></p></div>
                    <div class="form-field form-field--wide"><label for="sqm-link">Kind of connection</label>
                        <select id="sqm-link" class="role-select" data-sqm="link"${lock || !on ? ' disabled' : ''}>${links}</select>${err('link')}</div>
                </div>
                <p class="form-hint">A line rarely delivers all of what it is sold at, and the queue only works while the router is the slowest point, so the default is ${data.default_percent || 95} %. Test with a bufferbloat test, such as <a href="https://www.waveform.com/tools/bufferbloat" target="_blank" rel="noopener">waveform.com/tools/bufferbloat</a>, after committing: if the grade does not rise, lower the share a little (90 %, then 85 %).</p>
                ${form.second ? secondHtml(lock || !on) : ''}
                ${networksHtml(lock)}
                <label class="toggle toggle--block"><input type="checkbox" data-sqm="perHost"${form.perHost ? ' checked' : ''}${lock || !on ? ' disabled' : ''}><span><strong>Share the line fairly between devices</strong><span class="form-hint">One device with many connections cannot take the line from the others. On is right for a home.</span></span></label>
            </div>
            ${err('enabled')}
            ${message ? `<div class="form-message${message.kind === 'ok' ? ' form-message--ok' : ''}" role="alert">${escapeHtml(message.text)}</div>` : ''}
            ${statusHtml()}
            <div class="form-actions">
                <span class="form-hint">${!editable() ? 'A change is being applied or waits for confirmation: this cannot be edited right now.' : (d ? 'You have unsaved changes.' : '')}</span>
                <button type="button" class="cfg-btn cfg-btn--ghost" id="sqm-reset"${busy || !d ? ' disabled' : ''}>Reset</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="sqm-save"${lock || !d ? ' disabled' : ''}>Save to draft</button>
            </div>
        </section>`;
    }

    async function load(keepForm) {
        const res = await apiFetch('/api/v1/sqm');
        if (!res || !res.ok) return;
        data = await res.json();
        if (!keepForm || !form || !dirty()) form = formOf(data);
    }

    async function save() {
        errors = {};
        showMessage('');
        busy = true; render();
        const res = await apiFetch('/api/v1/sqm', { method: 'PUT', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled: form.enabled, download_mbit: num(form.down), upload_mbit: num(form.up), percent: pct(form), link: form.link, per_host: form.perHost,
                networks: Object.keys(form.nets).map(id => ({ id, priority: form.nets[id].priority, limit_down_mbit: num(form.nets[id].down), limit_up_mbit: num(form.nets[id].up) })),
                ...(form.second ? { second: { download_mbit: num(form.second.down), upload_mbit: num(form.second.up), link: form.second.link } } : {}) }) });
        busy = false;
        if (!res || !res.ok) {
            let e = {};
            try { e = res ? await res.json() : {}; } catch (x) { /* not JSON */ }
            (e.errors || []).forEach(x => { errors[x.field] = (errors[x.field] ? errors[x.field] + ' ' : '') + x.message; });
            showMessage(Object.keys(errors).length ? 'Some settings need attention.' : (e.error || 'Saving failed.'), 'error');
            render();
            return;
        }
        data = await res.json();
        form = formOf(data);
        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
        render();
    }

    async function measure(lineId) {
        lineId = lineId || 'wan';
        showMessage('');
        const res = await apiFetch('/api/v1/sqm/speedtest' + (lineId === 'wan2' ? '?line=wan2' : ''), { method: 'POST' });
        if (!res || (!res.ok && res.status !== 409)) {
            let e = {};
            try { e = res ? await res.json() : {}; } catch (x) { /* not JSON */ }
            speed = { state: 'error', error: e.error || 'The speed test could not be started.' };
            render();
            return;
        }
        speed = await res.json();
        render();
        clearInterval(speedTimer);
        speedTimer = setInterval(async () => {
            const r = await apiFetch('/api/v1/sqm/speedtest');
            if (!r || !r.ok) return;
            speed = await r.json();
            if (speed.state === 'done' && speed.result) {
                // the numbers go into the form, to be checked and saved (a test changes nothing by itself)
                const target = (speed.line === 'wan2' && form.second) ? form.second : form;
                target.down = nice(speed.result.download_mbit);
                target.up = nice(speed.result.upload_mbit);
                if (!form.enabled) form.enabled = true;
            }
            if (speed.state !== 'running') { clearInterval(speedTimer); speedTimer = null; }
            render();
        }, 1000);
    }

    function wire() {
        if (wired) return;
        wired = true;
        const view = $('view-internet');
        if (!view) return;
        view.addEventListener('input', (e) => {
            const nid = e.target.dataset && e.target.dataset.sqmNet;
            if (nid && form && form.nets[nid]) {
                const f = e.target.dataset.field;
                form.nets[nid][f] = e.target.value;
                delete errors[`net.${nid}.${f === 'down' ? 'limit_down_mbit' : 'limit_up_mbit'}`];
                const state = view.querySelector('#sqm-section .form-actions .form-hint');
                const sv = $('sqm-save'), rs = $('sqm-reset');
                if (state) state.textContent = dirty() ? 'You have unsaved changes.' : '';
                if (sv) sv.disabled = !dirty() || !editable();
                if (rs) rs.disabled = !dirty();
                return;
            }
            const k = e.target.dataset && e.target.dataset.sqm;
            if (!k || !form) return;
            if (k === 'down' || k === 'up') { form[k] = e.target.value; delete errors[k === 'down' ? 'download_mbit' : 'upload_mbit']; }
            if ((k === 's_down' || k === 's_up') && form.second) { form.second[k === 's_down' ? 'down' : 'up'] = e.target.value; delete errors[k === 's_down' ? 'second.download_mbit' : 'second.upload_mbit']; }
            if (k === 'percent') { form.percent = e.target.value; delete errors.percent; }
            const sh = $('sqm-shaped');
            if (sh) sh.innerHTML = `<strong>${shapedText()}</strong>`;
            const sh2 = $('sqm-shaped2');
            if (sh2 && form.second) sh2.innerHTML = `<strong>${shapedText(form.second)}</strong>`;
            const state = view.querySelector('#sqm-section .form-actions .form-hint');
            const save = $('sqm-save'), reset = $('sqm-reset');
            if (state) state.textContent = dirty() ? 'You have unsaved changes.' : '';
            if (save) save.disabled = !dirty() || !editable();
            if (reset) reset.disabled = !dirty();
        });
        view.addEventListener('change', (e) => {
            const nid = e.target.dataset && e.target.dataset.sqmNet;
            if (nid && form && form.nets[nid] && e.target.dataset.field === 'priority') { form.nets[nid].priority = e.target.value; delete errors[`net.${nid}.priority`]; render(); return; }
            const k = e.target.dataset && e.target.dataset.sqm;
            if (!k || !form) return;
            if (k === 'enabled' || k === 'perHost') form[k] = e.target.checked;
            if (k === 'link') form.link = e.target.value;
            if (k === 's_link' && form.second) form.second.link = e.target.value;
            render();
        });
        view.addEventListener('click', (e) => {
            if (e.target.id === 'sqm-save') save();
            if (e.target.id === 'sqm-measure') measure('wan');
            if (e.target.id === 'sqm-measure-2') measure('wan2');
            if (e.target.id === 'sqm-reset' && data) { form = formOf(data); errors = {}; showMessage(''); render(); }
        });
    }

    function init() {
        wire();
        data = null; form = null; errors = {}; message = null; busy = false; speed = null;
        load(false).then(render);
        // a test that was started before (or in another tab) is picked up
        apiFetch('/api/v1/sqm/speedtest').then(async (r) => { if (r && r.ok) { const s = await r.json(); if (s.state === 'running') { speed = s; measureResume(); } } });
        // how the queues are doing changes by itself: look again now and then (not while something is being typed)
        timer = setInterval(async () => {
            const active = document.activeElement;
            if (busy || dirty() || (active && active.dataset && active.dataset.sqm)) return;
            await load(true);
            render();
        }, 10000);
        return () => { clearInterval(timer); timer = null; clearInterval(speedTimer); speedTimer = null; };
    }

    // follow a test that is already running
    function measureResume() {
        clearInterval(speedTimer);
        speedTimer = setInterval(async () => {
            const r = await apiFetch('/api/v1/sqm/speedtest');
            if (!r || !r.ok) return;
            speed = await r.json();
            if (speed.state !== 'running') { clearInterval(speedTimer); speedTimer = null; }
            render();
        }, 1000);
        render();
    }

    return { init };
})();
