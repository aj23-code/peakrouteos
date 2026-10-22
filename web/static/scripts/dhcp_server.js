// web/static/scripts/dhcp_server.js
// DHCP page: the DHCP server of every local network in one place (on or off, the range of addresses and the lease time), next to the
// Clients page that lists what it handed out. It edits the same settings as the Local Networks page and saves a draft in the same way.

window.DHCPServerModule = (function () {
    let cards = [];          // { server, form, errors, message }
    // the reserved addresses: what the router has in the draft, what the rows show, the errors by "reservations[2].ip", and the devices
    // that are connected now (to pick from)
    let resv = { server: [], rows: [], errors: {}, saving: false, devices: [], message: null };
    let prefill = null;      // a device the Clients page asked to reserve an address for
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

    // ---------------------------------------------------------------- reserved addresses

    const blankRow = () => ({ name: '', mac: '', ip: '', fresh: true });
    const rowOf = (r) => ({ name: r.name || '', mac: r.mac || '', ip: r.ip || '' });
    const rowNorm = (rows) => JSON.stringify(rows.map(r => [r.name.trim(), r.mac.trim().toLowerCase().replace(/-/g, ':'), r.ip.trim()]));
    const resvDirty = () => rowNorm(resv.rows.filter(r => !isBlank(r))) !== rowNorm(resv.server.map(rowOf));
    const isBlank = (r) => !r.name.trim() && !r.mac.trim() && !r.ip.trim();
    const resvLocked = () => !editable || resv.saving;

    function showResvMessage(text, kind) { resv.message = text ? { text, kind } : null; }

    // the network an address is in, for the label beside the row (the draft's own networks, as the cards above hold them)
    function networkOf(ip) {
        const a = ipToInt(ip.trim());
        if (a === null) return '';
        for (const c of cards) {
            const base = ipToInt(c.server.ip), mask = ipToInt(c.server.netmask);
            if (base !== null && mask !== null && ((a & mask) >>> 0) === ((base & mask) >>> 0)) return c.server.default ? 'LAN' : (c.server.name || c.server.id);
        }
        return '';
    }
    function ipToInt(s) {
        const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s || '');
        if (!m) return null;
        const p = m.slice(1).map(Number);
        if (p.some(n => n > 255)) return null;
        return ((p[0] << 24) | (p[1] << 16) | (p[2] << 8) | p[3]) >>> 0;
    }

    function resvFieldHtml(i, field, label, value, attrs) {
        const err = resv.errors[`reservations[${i}].${field}`];
        const id = `resv-${i}-${field}`;
        return `<div class="form-field"><label for="${id}">${label}</label>
            <input type="text" id="${id}" data-resv-row="${i}" data-resv-field="${field}" value="${escapeHtml(value)}" autocomplete="off" spellcheck="false"${attrs || ''}${err ? ' aria-invalid="true"' : ''}${resvLocked() ? ' disabled' : ''}>
            ${err ? `<p class="field-error">${escapeHtml(err)}</p>` : ''}</div>`;
    }

    // a host name from what the device calls itself, or nothing when that is not a valid name
    function nameFrom(hostname) {
        const n = (hostname || '').trim();
        return /^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/.test(n) ? n : '';
    }

    function resvHtml() {
        const reservedMacs = new Set(resv.rows.map(r => r.mac.trim().toLowerCase().replace(/-/g, ':')));
        const options = resv.devices.filter(d => !reservedMacs.has((d.mac_address || '').toLowerCase()))
            .map(d => `<option value="${escapeHtml(d.mac_address)}">${escapeHtml(d.hostname && d.hostname !== 'Unknown Device' ? d.hostname : d.mac_address)} \u00b7 ${escapeHtml(d.ip_address)}</option>`).join('');
        const dirty = resvDirty();
        const rows = resv.rows.map((r, i) => {
            const net = networkOf(r.ip);
            return `<div class="route-row${r.fresh ? ' route-row--new' : ''}" data-resv-row="${i}">
                <div class="route-fields">
                    ${resvFieldHtml(i, 'name', 'Name', r.name, ' placeholder="Optional, e.g. printer" maxlength="63"')}
                    ${resvFieldHtml(i, 'mac', 'Hardware address', r.mac, ' placeholder="f8:94:c2:a4:13:a7"')}
                    ${resvFieldHtml(i, 'ip', 'Address', r.ip, ' placeholder="192.168.1.50" inputmode="decimal"')}
                </div>
                <div class="route-controls">
                    ${net ? `<span class="chip" title="The local network this address is in">${escapeHtml(net)}</span>` : (r.ip.trim() ? '<span class="form-hint">in no network</span>' : '')}
                    <button type="button" class="cfg-btn cfg-btn--ghost" data-resv-remove="${i}" aria-label="Remove reserved address ${i + 1}"${resvLocked() ? ' disabled' : ''}>Remove</button>
                </div>
            </div>`;
        }).join('');
        return `<section class="card" id="dhcpsrv-resv-card">
            <div class="net-card-head"><h2>Reserved addresses</h2>${changedTag()}</div>
            <p class="form-hint">A device with a reserved address always gets that address from this router, on whichever network it is. This is the better way to give a device a fixed address: it is kept in one place, cannot clash with the addresses handed out to others, and a device reset to its defaults gets it back. The device takes it the next time it asks for an address (when it reconnects or renews, or after you restart it). An address inside or outside the range above both work; the router never gives a reserved address to anyone else.</p>
            ${rows || '<p class="form-hint">No device has a reserved address yet.</p>'}
            ${resv.message ? `<div class="form-message${resv.message.kind === 'ok' ? ' form-message--ok' : ''}" role="alert">${escapeHtml(resv.message.text)}</div>` : ''}
            ${resv.errors.reservations ? `<p class="field-error">${escapeHtml(resv.errors.reservations)}</p>` : ''}
            <div class="form-actions">
                <label class="net-picker"><span class="form-hint">Reserve for</span>
                    <select id="resv-pick" class="role-select" aria-label="Choose a connected device to reserve an address for"${resvLocked() ? ' disabled' : ''}><option value="">a connected device&hellip;</option>${options}</select></label>
                <button type="button" class="cfg-btn" id="resv-add"${resvLocked() ? ' disabled' : ''}>Add by hand</button>
                <button type="button" class="cfg-btn" id="resv-reset"${resvLocked() || !dirty ? ' disabled' : ''}>Reset</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="resv-save"${resvLocked() || !dirty ? ' disabled' : ''}>Save to draft</button>
            </div>
        </section>`;
    }
    const changedTag = () => (resv.pending ? '<span class="tag tag-draft">Draft</span>' : '');

    function renderResv() {
        const box = $('dhcpsrv-reserved');
        if (box) box.innerHTML = resvHtml();
    }

    async function loadResv() {
        const [r, l] = await Promise.all([apiFetch('/api/v1/dhcp/reservations'), apiFetch('/api/v1/dhcp/leases')]);
        if (r && r.ok) {
            const data = await r.json();
            const wasDirty = resvDirty() && resv.rows.length > 0 && !resv.justSaved;
            resv.server = data.reservations || [];
            resv.pending = !!data.pending;
            if (!wasDirty) { resv.rows = resv.server.map(rowOf); resv.errors = {}; }
            resv.justSaved = false;
        } else {
            showResvMessage('The reserved addresses could not be loaded.', 'error');
        }
        if (l && l.ok) resv.devices = (await l.json()) || [];
        addPrefill();
        renderResv();
        focusRow();
    }

    // the Clients page asked for an address for this device: it keeps the address it has now unless told otherwise
    function addPrefill() {
        if (!prefill) return;
        const d = prefill;
        prefill = null;
        const mac = (d.mac || '').toLowerCase();
        const have = resv.rows.findIndex(r => r.mac.trim().toLowerCase() === mac);
        if (have < 0) resv.rows.push({ name: nameFrom(d.hostname), mac, ip: d.ip || '', fresh: true });
        resv.focusRow = have < 0 ? resv.rows.length - 1 : have;
    }

    async function saveResv() {
        resv.errors = {};
        showResvMessage('');
        resv.saving = true;
        renderResv();
        resv.rows.forEach(r => { delete r.fresh; });
        const body = { reservations: resv.rows.filter(r => !isBlank(r)).map(r => ({ name: r.name.trim(), mac: r.mac.trim(), ip: r.ip.trim() })) };
        const res = await apiFetch('/api/v1/dhcp/reservations', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        resv.saving = false;
        if (!res) { showResvMessage('The router did not answer. Try again.', 'error'); renderResv(); return; }
        if (!res.ok) {
            let err = {};
            try { err = await res.json(); } catch (e) { /* not JSON */ }
            // the errors name a row of what was sent: blank rows were left out, so map them back to the rows on screen
            const shown = resv.rows.map((r, i) => i).filter(i => !isBlank(resv.rows[i]));
            const general = [];
            (err.errors || []).forEach(e => {
                const m = /^reservations\[(\d+)\]\.(\w+)$/.exec(e.field);
                if (m) {
                    const key = `reservations[${shown[Number(m[1])] ?? m[1]}].${m[2]}`;
                    resv.errors[key] = resv.errors[key] ? resv.errors[key] + ' ' + e.message : e.message;
                } else general.push(e.message);
            });
            if (general.length) resv.errors.reservations = general.join(' ');
            showResvMessage(Object.keys(resv.errors).length ? 'Some reserved addresses need attention.' : (err.error || `Saving failed (${res.status}).`), 'error');
            renderResv();
            return;
        }
        resv.justSaved = true;
        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await loadResv();
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function wireResv() {
        const box = $('dhcpsrv-reserved');
        box.addEventListener('input', (e) => {
            const i = e.target.dataset.resvRow, f = e.target.dataset.resvField;
            if (i === undefined || !f) return;
            resv.rows[Number(i)][f] = e.target.value;
            delete resv.errors[`reservations[${i}].${f}`];
            const dirty = resvDirty();
            $('resv-save').disabled = !dirty || resvLocked();
            $('resv-reset').disabled = !dirty || resvLocked();
            if (f === 'ip') {
                const net = networkOf(e.target.value);
                const chip = e.target.closest('.route-row').querySelector('.route-controls .chip, .route-controls .form-hint');
                if (chip) { chip.textContent = net || 'in no network'; }
            }
        });
        box.addEventListener('change', (e) => {
            if (e.target.id !== 'resv-pick' || !e.target.value) return;
            const d = resv.devices.find(x => (x.mac_address || '').toLowerCase() === e.target.value.toLowerCase());
            if (d) { prefill = { mac: d.mac_address, ip: d.ip_address, hostname: d.hostname }; addPrefill(); }
            renderResv();
            focusRow();
        });
        box.addEventListener('click', (e) => {
            if (e.target.id === 'resv-add') { resv.rows.push(blankRow()); resv.focusRow = resv.rows.length - 1; renderResv(); focusRow(); return; }
            if (e.target.id === 'resv-save') { saveResv(); return; }
            if (e.target.id === 'resv-reset') { resv.rows = resv.server.map(rowOf); resv.errors = {}; showResvMessage(''); renderResv(); return; }
            const rm = e.target.closest('[data-resv-remove]');
            if (rm) { resv.rows.splice(Number(rm.dataset.resvRemove), 1); resv.errors = {}; renderResv(); }
        });
    }

    // brings the row that was just added into view (the card is below the networks, so it would otherwise be off screen) and puts the
    // cursor in the first thing left to fill in, or on Save when the row is complete
    function focusRow() {
        if (resv.focusRow === undefined) return;
        const i = resv.focusRow;
        resv.focusRow = undefined;
        const row = resv.rows[i];
        if (!row) return;
        const missing = !row.name ? 'name' : (!row.mac ? 'mac' : (!row.ip ? 'ip' : ''));
        const el = missing ? $(`resv-${i}-${missing}`) : $('resv-save');
        const card = document.querySelector(`#dhcpsrv-reserved [data-resv-row="${i}"]`);
        if (card) card.scrollIntoView({ block: 'center', behavior: 'smooth' });
        if (el) el.focus({ preventScroll: true });
    }

    // called by the Clients page: reserve an address for this device (it keeps the one it has)
    function reserve(device) {
        prefill = device;
        if (typeof loadView === 'function') loadView('dhcp-server');
    }

    function wire() {
        if (wired) return;
        wired = true;
        wireResv();
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
        // the networks first: the rows below show which network an address is in
        load().then(loadResv);
        return () => {};
    }

    return { init, reserve };
})();
