// web/static/scripts/dhcp_server.js
// DHCP page: the DHCP server of every local network in one place (on or off, the range of addresses and the lease time), next to the
// Clients page that lists what it handed out. It edits the same settings as the Local Networks page and saves a draft in the same way.

window.DHCPServerModule = (function () {
    let cards = [];          // { server, form, errors, message }
    let editable = true;
    let busy = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);

    const formOf = (n) => {
        const d = n.dhcp;
        return { enabled: !!(d && d.enabled), start: (d && d.range_start) || '', end: (d && d.range_end) || '', lease: (d && d.lease_time) || '' };
    };
    const norm = (f) => JSON.stringify([f.enabled, f.start.trim(), f.end.trim(), f.lease.trim()]);
    const isDirty = (c) => norm(c.form) !== norm(formOf(c.server));

    const ERROR_FIELD = { 'dhcp.range_start': 'start', 'dhcp.range_end': 'end', 'dhcp.lease_time': 'lease' };

    function showMessage(text, kind) {
        const box = $('dhcpsrv-message');
        if (!box) return;
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    function fieldHtml(c, field, label, placeholder, list) {
        const id = `dhcpsrv-${c.server.id}-${field}`;
        const err = c.errors[field];
        const off = !editable || busy || !c.form.enabled;
        return `<div class="form-field"><label for="${id}">${label}</label>
            <input type="text" id="${id}" data-id="${escapeHtml(c.server.id)}" data-field="${field}" value="${escapeHtml(c.form[field])}" placeholder="${escapeHtml(placeholder)}"
                ${list ? `list="${list}"` : ''} inputmode="decimal" autocomplete="off" spellcheck="false"${err ? ' aria-invalid="true"' : ''}${off ? ' disabled' : ''}>
            ${err ? `<p class="field-error">${escapeHtml(err)}</p>` : ''}</div>`;
    }

    function cardHtml(c) {
        const n = c.server;
        const lock = !editable || busy;
        const dirty = isDirty(c);
        const title = n.default ? 'LAN' : (n.name || n.id);
        return `<section class="card net-card" data-id="${escapeHtml(n.id)}">
            <div class="net-card-head">
                <h2>${escapeHtml(title)}</h2>
                ${n.default ? '<span class="tag tag-ok">default</span>' : `<span class="tag">VLAN ${n.vlan}</span>`}
                ${n.pending ? '<span class="tag tag-draft">Draft</span>' : ''}
                <span class="form-hint">${escapeHtml(n.ip)} / ${escapeHtml(n.netmask)}</span>
            </div>
            <label class="toggle toggle--block"><input type="checkbox" data-id="${escapeHtml(n.id)}" data-field="enabled"${c.form.enabled ? ' checked' : ''}${lock ? ' disabled' : ''}>
                <span><strong>Give addresses to devices on this network</strong><span class="form-hint">Off, devices here must set their own address.</span></span></label>
            ${!c.form.enabled && n.dhcp === null && n.default ? '<p class="form-hint">The DHCP server is not managed by this configuration yet. Turn it on and fill in a range to start managing it here.</p>' : ''}
            <div class="form-grid${c.form.enabled ? '' : ' is-off'}">
                ${fieldHtml(c, 'start', 'First address', '192.168.1.100')}
                ${fieldHtml(c, 'end', 'Last address', '192.168.1.200')}
                ${fieldHtml(c, 'lease', 'Lease time', '12h', 'dhcpsrv-lease-options')}
            </div>
            ${c.message ? `<div class="form-message${c.message.kind === 'ok' ? ' form-message--ok' : ''}" role="alert">${escapeHtml(c.message.text)}</div>` : ''}
            <div class="form-actions">
                <a href="#" class="view-all-link" data-goto="networks">Address, ports and access: Local Networks &rarr;</a>
                <button type="button" class="cfg-btn" data-reset="${escapeHtml(n.id)}"${lock || !dirty ? ' disabled' : ''}>Reset</button>
                <button type="button" class="cfg-btn cfg-btn--primary" data-save="${escapeHtml(n.id)}"${lock || !dirty ? ' disabled' : ''}>Save to draft</button>
            </div>
        </section>`;
    }

    function renderAll() {
        const list = $('dhcpsrv-list');
        if (!list) return;
        list.innerHTML = cards.length ? cards.map(cardHtml).join('') : '<p class="form-hint">Loading&hellip;</p>';
    }

    function renderCard(c) {
        const el = document.querySelector(`#dhcpsrv-list [data-id="${CSS.escape(c.server.id)}"].card`);
        if (!el) { renderAll(); return; }
        el.outerHTML = cardHtml(c);
    }

    async function load() {
        const res = await apiFetch('/api/v1/networks');
        if (!res || !res.ok) { showMessage('The networks could not be loaded.', 'error'); return; }
        const data = await res.json();
        editable = data.editable !== false;
        const kept = new Map(cards.map(c => [c.server.id, c]));
        cards = (data.networks || []).map(n => {
            const old = kept.get(n.id);
            const dirtyOld = old && isDirty(old) && !old.justSaved;
            return { server: n, form: dirtyOld ? old.form : formOf(n), errors: dirtyOld ? old.errors : {}, message: dirtyOld ? old.message : null };
        });
        if (!editable) showMessage('A change is being applied or waits for confirmation: settings cannot be edited right now.');
        renderAll();
    }

    // the whole network is sent (the address and the access rules as they are), with the DHCP settings of this page
    function bodyOf(c) {
        const n = c.server, f = c.form;
        const body = { ip: n.ip, netmask: n.netmask, isolated: !!n.isolated };
        if (!n.default) { body.name = n.name; body.vlan = n.vlan; body.internet = !!n.internet; }
        body.dhcp = { enabled: f.enabled, range_start: f.start.trim(), range_end: f.end.trim(), lease_time: f.lease.trim() };
        return body;
    }

    async function save(c) {
        c.message = null;
        c.errors = {};
        busy = true;
        renderCard(c);
        const res = await apiFetch('/api/v1/networks/' + encodeURIComponent(c.server.id), {
            method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(bodyOf(c)),
        });
        busy = false;
        if (!res) { c.message = { text: 'The router did not answer. Try again.', kind: 'error' }; renderCard(c); return; }
        if (!res.ok) {
            let err = {};
            try { err = await res.json(); } catch (e) { /* not JSON */ }
            const general = [];
            (err.errors || []).forEach(e => {
                const f = ERROR_FIELD[e.field];
                if (f) c.errors[f] = c.errors[f] ? c.errors[f] + ' ' + e.message : e.message;
                else general.push(e.message);
            });
            c.message = { text: general.join(' ') || (Object.keys(c.errors).length ? 'Some settings need attention.' : (err.error || `Saving failed (${res.status}).`)), kind: 'error' };
            renderCard(c);
            return;
        }
        c.justSaved = true;
        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await load();
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('dhcpsrv-list');
        const find = (id) => cards.find(c => c.server.id === id);
        body.addEventListener('change', (e) => {
            const c = find(e.target.dataset.id);
            if (!c || e.target.dataset.field !== 'enabled') return;
            c.form.enabled = e.target.checked;
            // turning it on for a network with no range yet: offer one inside its own subnet
            if (c.form.enabled && !c.form.start && !c.form.end) {
                const base = (c.server.ip || '').split('.').slice(0, 3).join('.');
                if (/^\d+\.\d+\.\d+$/.test(base)) { c.form.start = base + '.100'; c.form.end = base + '.200'; }
                if (!c.form.lease) c.form.lease = '12h';
            }
            renderCard(c);
        });
        body.addEventListener('input', (e) => {
            const c = find(e.target.dataset.id);
            const field = e.target.dataset.field;
            if (!c || !field || field === 'enabled') return;
            c.form[field] = e.target.value;
            c.errors[field] = '';
            const card = e.target.closest('.card');
            const dirty = isDirty(c);
            card.querySelector('[data-save]').disabled = !dirty || busy || !editable;
            card.querySelector('[data-reset]').disabled = !dirty || busy || !editable;
        });
        body.addEventListener('click', (e) => {
            const s = e.target.closest('[data-save]');
            if (s) { const c = find(s.dataset.save); if (c) save(c); return; }
            const r = e.target.closest('[data-reset]');
            if (r) { const c = find(r.dataset.reset); if (c) { c.form = formOf(c.server); c.errors = {}; c.message = null; renderCard(c); } }
        });
    }

    function init() {
        wire();
        showMessage('');
        cards = [];
        load();
        return () => {};
    }

    return { init };
})();
