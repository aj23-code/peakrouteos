// web/static/scripts/sqm.js
// Smart queue management card on the Internet page: shape the connection a little below the line's real speed so that the queue forms in
// the router and not at the provider (docs/sqm.md). Saving only writes a draft; shaping starts when the change is committed.

window.SQMModule = (function () {
    let data = null;      // GET /api/v1/sqm
    let form = null;      // { enabled, down, up, link, perHost } as typed
    let errors = {};
    let message = null;
    let busy = false;
    let timer = null;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const editable = () => !data || data.editable !== false;
    const text = (n) => (n ? String(n) : '');

    function formOf(d) {
        return { enabled: !!d.enabled, down: text(d.download_mbit), up: text(d.upload_mbit), link: d.link || 'ethernet', perHost: !!d.per_host };
    }
    const num = (s) => { const v = parseFloat(String(s).replace(',', '.')); return Number.isFinite(v) && v > 0 ? v : 0; };
    const same = (a, b) => a.enabled === b.enabled && num(a.down) === num(b.down) && num(a.up) === num(b.up) && a.link === b.link && a.perHost === b.perHost;
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

    function statusHtml() {
        if (!data.running) return '';
        const st = data.status;
        return `<h3 class="form-section">Running now on ${escapeHtml(data.wan_device)}</h3>
            ${st ? directionHtml('Upload', st.upload) + directionHtml('Download', st.download) : '<p class="form-hint">The queues could not be read.</p>'}
            <p class="form-hint">Dropping a few packets while a line is full is how the queue tells busy connections to slow down: it is what keeps the wait short.</p>`;
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
                <div class="form-grid">
                    <div class="form-field"><label for="sqm-down">Download speed (Mbit/s)</label>
                        <input type="text" id="sqm-down" data-sqm="down" inputmode="decimal" value="${escapeHtml(form.down)}" placeholder="for example 450" autocomplete="off"${lock || !on ? ' disabled' : ''}>${err('download_mbit')}</div>
                    <div class="form-field"><label for="sqm-up">Upload speed (Mbit/s)</label>
                        <input type="text" id="sqm-up" data-sqm="up" inputmode="decimal" value="${escapeHtml(form.up)}" placeholder="for example 40" autocomplete="off"${lock || !on ? ' disabled' : ''}>${err('upload_mbit')}</div>
                    <div class="form-field form-field--wide"><label for="sqm-link">Kind of connection</label>
                        <select id="sqm-link" class="role-select" data-sqm="link"${lock || !on ? ' disabled' : ''}>${links}</select>${err('link')}</div>
                </div>
                <p class="form-hint">Start at 90 to 95 % of what a speed test shows with nothing else using the line (a plan sold as 500 Mbit/s that tests at 470: try 440). Leave a speed empty to leave that direction alone. Then test with a bufferbloat test, such as <a href="https://www.waveform.com/tools/bufferbloat" target="_blank" rel="noopener">waveform.com/tools/bufferbloat</a>: the grade should rise; if it does not, lower the speeds a little more.</p>
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
            body: JSON.stringify({ enabled: form.enabled, download_mbit: num(form.down), upload_mbit: num(form.up), link: form.link, per_host: form.perHost }) });
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

    function wire() {
        if (wired) return;
        wired = true;
        const view = $('view-internet');
        if (!view) return;
        view.addEventListener('input', (e) => {
            const k = e.target.dataset && e.target.dataset.sqm;
            if (!k || !form) return;
            if (k === 'down' || k === 'up') { form[k] = e.target.value; delete errors[k === 'down' ? 'download_mbit' : 'upload_mbit']; }
            const state = view.querySelector('#sqm-section .form-actions .form-hint');
            const save = $('sqm-save'), reset = $('sqm-reset');
            if (state) state.textContent = dirty() ? 'You have unsaved changes.' : '';
            if (save) save.disabled = !dirty() || !editable();
            if (reset) reset.disabled = !dirty();
        });
        view.addEventListener('change', (e) => {
            const k = e.target.dataset && e.target.dataset.sqm;
            if (!k || !form) return;
            if (k === 'enabled' || k === 'perHost') form[k] = e.target.checked;
            if (k === 'link') form.link = e.target.value;
            render();
        });
        view.addEventListener('click', (e) => {
            if (e.target.id === 'sqm-save') save();
            if (e.target.id === 'sqm-reset' && data) { form = formOf(data); errors = {}; showMessage(''); render(); }
        });
    }

    function init() {
        wire();
        data = null; form = null; errors = {}; message = null; busy = false;
        load(false).then(render);
        // how the queues are doing changes by itself: look again now and then (not while something is being typed)
        timer = setInterval(async () => {
            const active = document.activeElement;
            if (busy || dirty() || (active && active.dataset && active.dataset.sqm)) return;
            await load(true);
            render();
        }, 10000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
