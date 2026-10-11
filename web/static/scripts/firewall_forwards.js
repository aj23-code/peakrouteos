// web/static/scripts/firewall_forwards.js
// Port forwarding page: connections that arrive from the internet on a port of the router are sent to a device
// inside. Each forward is its own line; saving only writes a draft. docs/firewall.md explains how it is written.

window.FirewallForwardsModule = (function () {
    let data = null;            // GET /api/v1/firewall
    let editing = null;         // { id: null (new) or a forward id, form, errors, message }
    let counters = {};
    let countersKnown = false;
    let confirmDelete = null;
    let publicAddress = '';     // the internet address in effect, for the "reach it at" hint
    let busy = false;
    let wired = false;
    let timer = null;

    const $ = (id) => document.getElementById(id);
    const editable = () => !data || data.editable !== false;
    const PROTOCOLS = [['tcp', 'TCP'], ['udp', 'UDP'], ['tcp+udp', 'TCP and UDP']];
    const FIELDS = ['name', 'protocol', 'external_ports', 'to_ip', 'internal_ports', 'sources'];

    function showMessage(text, kind) {
        const box = $('pf-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    const option = (value, label, current) => `<option value="${escapeHtml(value)}"${value === current ? ' selected' : ''}>${escapeHtml(label)}</option>`;

    function hitsText(p) {
        const c = counters[p.id];
        if (p.pending || !countersKnown || !c) return '–';
        return c.packets.toLocaleString();
    }

    function rowHtml(p) {
        const lock = !editable() || busy;
        const armed = confirmDelete === p.id;
        const reach = publicAddress ? `Reach it at <code>${escapeHtml(publicAddress)}:${escapeHtml(p.external_ports)}</code> from outside.` : '';
        return `<div class="fw-row fw-rule${p.enabled ? '' : ' fw-rule--off'}" data-id="${escapeHtml(p.id)}">
            <label class="toggle fw-switch"><input type="checkbox" class="pf-enable" data-id="${escapeHtml(p.id)}"${p.enabled ? ' checked' : ''}${lock ? ' disabled' : ''} aria-label="Switch ${escapeHtml(p.name)} on or off"></label>
            <div class="fw-main">
                <div class="fw-name"><strong>${escapeHtml(p.name)}</strong>${p.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}${p.enabled ? '' : ' <span class="tag">Off</span>'}</div>
                <div class="form-hint fw-text">${escapeHtml(p.text)}</div>
                ${reach && p.enabled ? `<div class="form-hint fw-text">${reach}</div>` : ''}
            </div>
            <div class="fw-hits" title="Packets that used this forward since the rules were last loaded"><span class="form-hint">Hits</span><span class="fw-hits-n" data-id="${escapeHtml(p.id)}">${hitsText(p)}</span></div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost pf-edit" data-id="${escapeHtml(p.id)}"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger pf-delete" data-id="${escapeHtml(p.id)}"${lock ? ' disabled' : ''}>${armed ? 'Click again' : 'Delete'}</button>
            </div>
        </div>`;
    }

    function blankForm() {
        return { name: '', protocol: 'tcp', external_ports: '', to_ip: '', internal_ports: '', sources: '', enabled: true };
    }

    function formOf(p) {
        return { name: p.name, protocol: p.protocol, external_ports: p.external_ports, to_ip: p.to_ip, internal_ports: p.internal_ports, sources: (p.sources || []).join(', '), enabled: p.enabled };
    }

    function fieldHtml(label, id, control, key, hint) {
        const err = editing.errors[key];
        return `<div class="form-field"><label for="${id}">${label}</label>${control}${hint ? `<p class="form-hint">${hint}</p>` : ''}${err ? `<p class="field-error">${escapeHtml(err)}</p>` : ''}</div>`;
    }

    function editorHtml() {
        const f = editing.form;
        const lock = !editable() || busy;
        const dis = lock ? ' disabled' : '';
        const text = (id, key, ph, extra) => `<input type="text" id="${id}" data-pf="${key}" value="${escapeHtml(f[key])}" placeholder="${escapeHtml(ph)}" autocomplete="off" spellcheck="false" autocapitalize="off"${extra || ''}${editing.errors[key] ? ' aria-invalid="true"' : ''}${dis}>`;
        const range = f.external_ports.includes('-');
        return `<section class="card fw-editor" id="pf-editor">
            <div class="net-card-head"><h2>${editing.id ? 'Edit port forward' : 'New port forward'}</h2></div>
            <div class="form-grid">
                ${fieldHtml('Name', 'pf-f-name', text('pf-f-name', 'name', 'Web server'), 'name')}
                ${fieldHtml('Protocol', 'pf-f-protocol', `<select id="pf-f-protocol" class="role-select" data-pf="protocol"${dis}>${PROTOCOLS.map(o => option(o[0], o[1], f.protocol)).join('')}</select>`, 'protocol')}
                ${fieldHtml('Port on the internet side', 'pf-f-external_ports', text('pf-f-external_ports', 'external_ports', '443 or 5000-5010', ' inputmode="numeric"'), 'external_ports', 'One port, or one range.')}
                ${fieldHtml('Send to', 'pf-f-to_ip', text('pf-f-to_ip', 'to_ip', '192.168.1.50', ' inputmode="decimal" list="pf-devices"'), 'to_ip', 'A device on one of your networks. Giving the device a fixed address keeps the forward working.')}
                ${fieldHtml(`Port on the device ${range ? '' : '<span class="form-hint">(optional)</span>'}`, 'pf-f-internal_ports',
                    range ? `<input type="text" id="pf-f-internal_ports" value="" placeholder="same as the internet side" disabled>` : text('pf-f-internal_ports', 'internal_ports', 'same as the internet side', ' inputmode="numeric"'),
                    'internal_ports', range ? 'A range keeps its port numbers.' : 'Empty keeps the same number.')}
                ${fieldHtml('Only from these addresses <span class="form-hint">(optional)</span>', 'pf-f-sources', text('pf-f-sources', 'sources', 'anyone, or 203.0.113.7, 198.51.100.0/24'), 'sources', 'Up to ten addresses or ranges, separated by commas.')}
            </div>
            <div class="net-access">
                <label class="toggle toggle--block"><input type="checkbox" data-pf="enabled"${f.enabled ? ' checked' : ''}${dis}><span><strong>Switched on</strong><span class="form-hint">Off keeps it here without applying it.</span></span></label>
            </div>
            ${editing.message ? `<div class="form-message" role="alert">${escapeHtml(editing.message)}</div>` : ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="pf-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="pf-save"${lock ? ' disabled' : ''}>${editing.id ? 'Save to draft' : 'Add port forward'}</button>
            </div>
        </section>`;
    }

    function render() {
        if (!data) return;
        $('pf-add').disabled = !editable() || busy || !!editing;
        const list = data.forwards || [];
        $('pf-body').innerHTML = `<section class="card fw-card">
            <div class="net-card-head"><h2>Forwards</h2><span class="tag">${list.length} ${list.length === 1 ? 'forward' : 'forwards'}</span></div>
            <p class="form-hint">Nothing from the internet gets in unless a forward (or a rule on the <a href="#" data-goto="firewall-rules">Rules</a> page) lets it. Each forward lets in exactly the traffic it sends on, and nothing else.</p>
            ${!editable() ? '<p class="form-hint">A change is being applied or waits for confirmation: forwards cannot be edited right now.</p>' : ''}
            <div class="fw-list">${list.length ? list.map(rowHtml).join('') : '<div class="fw-row fw-empty">No port forwards yet.</div>'}</div>
        </section>${editing ? editorHtml() : ''}`;
    }

    function updateHits() {
        document.querySelectorAll('#pf-body .fw-hits-n').forEach(el => {
            const p = (data.forwards || []).find(x => x.id === el.dataset.id);
            if (p) el.textContent = hitsText(p);
        });
    }

    async function load() {
        const res = await apiFetch('/api/v1/firewall');
        if (!res || !res.ok) { showMessage('Could not load the port forwards.', 'error'); return; }
        data = await res.json();
        render();
    }

    async function loadCounters() {
        const res = await apiFetch('/api/v1/firewall/counters');
        if (!res || !res.ok) return;
        const body = await res.json();
        countersKnown = !!body.known;
        counters = body.counters || {};
        if (data) updateHits();
    }

    // suggestions for "Send to": the devices that hold a lease, and where the router is reachable from outside
    async function loadHelpers() {
        const [leases, up] = await Promise.all([apiFetch('/api/v1/dhcp/leases'), apiFetch('/api/v1/uplinks')]);
        if (leases && leases.ok) {
            const list = (await leases.json()) || [];
            $('pf-devices').innerHTML = list.map(l => `<option value="${escapeHtml(l.ip_address)}">${escapeHtml(l.hostname || '')}</option>`).join('');
        }
        if (up && up.ok) {
            const u = ((await up.json()).uplinks || [])[0];
            const addr = u && u.live && u.live.address;
            publicAddress = addr ? addr.split('/')[0] : '';
            if (data) render();
        }
    }

    async function errorOf(res, fallback) {
        if (!res) return { general: 'The router did not answer. Try again.', fields: [] };
        let err = {};
        try { err = await res.json(); } catch (e) { /* not JSON */ }
        return { general: err.error || fallback, fields: err.errors || [] };
    }

    function afterChange(message) {
        showMessage(message, 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    async function save() {
        const f = editing.form;
        const body = {
            name: f.name, protocol: f.protocol, external_ports: f.external_ports, to_ip: f.to_ip,
            internal_ports: f.external_ports.includes('-') ? '' : f.internal_ports,
            sources: f.sources.split(/[\s,]+/).filter(Boolean), enabled: f.enabled,
        };
        editing.errors = {};
        editing.message = null;
        busy = true;
        render();
        const res = await apiFetch(editing.id ? '/api/v1/firewall/forwards/' + encodeURIComponent(editing.id) : '/api/v1/firewall/forwards', {
            method: editing.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, `Saving failed${res ? ' (' + res.status + ')' : ''}.`);
            const general = [];
            e.fields.forEach(x => {
                if (FIELDS.includes(x.field)) editing.errors[x.field] = (editing.errors[x.field] ? editing.errors[x.field] + ' ' : '') + x.message;
                else general.push(x.message);
            });
            editing.message = general.join(' ') || (Object.keys(editing.errors).length ? 'Some settings need attention.' : e.general);
            render();
            return;
        }
        const wasNew = !editing.id;
        editing = null;
        await load();
        afterChange(wasNew ? 'Port forward added to the draft. Review and commit it from the bar at the top when you are ready.' : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.');
    }

    async function toggle(id, on) {
        busy = true; render();
        const res = await apiFetch('/api/v1/firewall/forwards/' + encodeURIComponent(id), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: on }) });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'The port forward could not be changed.'); showMessage(e.fields.map(x => x.message).join(' ') || e.general, 'error'); await load(); return; }
        await load();
        afterChange(on ? 'Port forward switched on in the draft.' : 'Port forward switched off in the draft.');
    }

    async function remove(id) {
        if (confirmDelete !== id) {
            confirmDelete = id;
            render();
            setTimeout(() => { if (confirmDelete === id) { confirmDelete = null; render(); } }, 4000);
            return;
        }
        confirmDelete = null;
        busy = true; render();
        const res = await apiFetch('/api/v1/firewall/forwards/' + encodeURIComponent(id), { method: 'DELETE' });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'The port forward could not be deleted.'); showMessage(e.general, 'error'); await load(); return; }
        await load();
        afterChange('Port forward removed from the draft. Review and commit it from the bar at the top when you are ready.');
    }

    function wire() {
        if (wired) return;
        wired = true;
        $('pf-add').addEventListener('click', () => {
            editing = { id: null, form: blankForm(), errors: {}, message: null };
            render();
            const f = $('pf-f-name'); if (f) f.focus();
        });
        const body = $('pf-body');
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            const id = btn.dataset.id;
            if (btn.classList.contains('pf-delete')) remove(id);
            else if (btn.classList.contains('pf-edit')) {
                const p = (data.forwards || []).find(x => x.id === id);
                if (p) { editing = { id, form: formOf(p), errors: {}, message: null }; render(); const f = $('pf-f-name'); if (f) f.focus(); }
            } else if (btn.id === 'pf-save') save();
            else if (btn.id === 'pf-cancel') { editing = null; render(); }
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.classList.contains('pf-enable')) { toggle(el.dataset.id, el.checked); return; }
            const key = el.dataset && el.dataset.pf;
            if (!key || !editing) return;
            editing.form[key] = el.type === 'checkbox' ? el.checked : el.value;
            delete editing.errors[key];
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            const key = el.dataset && el.dataset.pf;
            if (!key || !editing || el.type === 'checkbox' || el.tagName === 'SELECT') return;
            editing.form[key] = el.value;
            delete editing.errors[key];
            el.removeAttribute('aria-invalid');
            const msg = el.closest('.form-field').querySelector('.field-error');
            if (msg) msg.remove();
            // a range keeps its numbers: the field for the device's port follows what is typed
            if (key === 'external_ports') {
                const internal = $('pf-f-internal_ports');
                const isRange = el.value.includes('-');
                if (internal && internal.disabled !== isRange) { render(); const again = $('pf-f-external_ports'); if (again) { again.focus(); again.setSelectionRange(again.value.length, again.value.length); } }
            }
        });
        body.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && editing && e.target.tagName === 'INPUT' && e.target.type === 'text' && editable() && !busy) { e.preventDefault(); save(); }
        });
    }

    function init() {
        wire();
        showMessage('');
        data = null; editing = null; confirmDelete = null; counters = {}; countersKnown = false; publicAddress = '';
        load();
        loadCounters();
        loadHelpers();
        timer = setInterval(loadCounters, 5000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
