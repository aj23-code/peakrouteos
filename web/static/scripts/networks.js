// web/static/scripts/networks.js
// Local Networks page: one card per network (the default one and any VLANs added), each with its
// address, DHCP server and what it may reach. Saving only writes a draft.

window.NetworksModule = (function () {
    let cards = [];          // { key, isNew, server, form, errors, message, confirmDelete }
    let selected = 'lan';    // the key of the card on show: one network at a time
    let editable = true;
    let busy = false;
    let wired = false;
    let newCount = 0;

    const $ = (id) => document.getElementById(id);

    // ---------------------------------------------------------------- form state

    function formOf(n) {
        const d = n.dhcp;
        return {
            name: n.name || '',
            vlan: n.vlan ? String(n.vlan) : '',
            ip: n.ip || '',
            netmask: n.netmask || '',
            dhcpEnabled: !!(d && d.enabled),
            start: (d && d.range_start) || '',
            end: (d && d.range_end) || '',
            lease: (d && d.lease_time) || '',
            internet: !!n.internet,
            isolated: !!n.isolated,
        };
    }

    const norm = (f) => JSON.stringify([f.name.trim(), f.vlan.trim(), f.ip.trim(), f.netmask.trim(), f.dhcpEnabled,
        f.start.trim(), f.end.trim(), f.lease.trim(), f.internet, f.isolated]);

    function isDirty(c) {
        return c.isNew || norm(c.form) !== norm(formOf(c.server));
    }

    // The next free VLAN number and a /24 to go with it: 192.168.<vlan>.0/24 unless that is taken
    function suggestion() {
        const used = new Set(cards.filter(c => !c.isNew).map(c => Number(c.server.vlan)));
        let vlan = 10;
        while (used.has(vlan)) vlan += 10;
        const taken = new Set(cards.filter(c => c.server).map(c => (c.server.ip || '').split('.').slice(0, 3).join('.')));
        let octet = vlan;
        while (taken.has(`192.168.${octet}`) && octet < 255) octet++;
        return {
            vlan: String(vlan), ip: `192.168.${octet}.1`, netmask: '255.255.255.0',
            start: `192.168.${octet}.100`, end: `192.168.${octet}.200`,
        };
    }

    // ---------------------------------------------------------------- rendering

    const FIELDS = ['name', 'vlan', 'ip', 'netmask', 'start', 'end', 'lease'];
    const ERROR_FIELD = { name: 'name', vlan: 'vlan', ip: 'ip', netmask: 'netmask', 'dhcp.range_start': 'start', 'dhcp.range_end': 'end', 'dhcp.lease_time': 'lease' };

    function inputHtml(c, field, label, opts) {
        const id = `${c.key}-${field}`;
        const err = c.errors[field];
        const off = !editable || busy || (opts.disabled === true);
        return `<div class="form-field${opts.wide ? ' form-field--wide' : ''}">
            <label for="${id}">${label}</label>
            <input type="text" id="${id}" data-key="${c.key}" data-field="${field}" value="${escapeHtml(c.form[field])}"
                ${opts.placeholder ? `placeholder="${escapeHtml(opts.placeholder)}"` : ''} ${opts.list ? `list="${opts.list}"` : ''}
                ${opts.mode ? `inputmode="${opts.mode}"` : ''} autocomplete="off" spellcheck="false"
                ${err ? 'aria-invalid="true"' : ''}${off ? ' disabled' : ''}>
            ${err ? `<p class="field-error">${escapeHtml(err)}</p>` : ''}
        </div>`;
    }

    function toggleHtml(c, field, label, hint, disabled) {
        const off = !editable || busy || disabled;
        return `<label class="toggle toggle--block"><input type="checkbox" data-key="${c.key}" data-field="${field}"${c.form[field] ? ' checked' : ''}${off ? ' disabled' : ''}>
            <span><strong>${label}</strong><span class="form-hint">${hint}</span></span></label>`;
    }

    function portsHtml(c) {
        if (c.isNew) return '<p class="form-hint">Choose which ports carry this network on the <a href="#" data-goto="network-physical">Ports</a> page once it is saved.</p>';
        const n = c.server;
        const chips = (n.ports || []).map(p => `<span class="chip">${escapeHtml(p)}</span>`)
            .concat((n.tagged_ports || []).map(p => `<span class="chip chip--tag" title="carried as a VLAN">${escapeHtml(p)} <small>tagged</small></span>`));
        return `<div class="chips">${chips.length ? chips.join('') : '<span class="form-hint">No port carries this network yet.</span>'}</div>
            <p class="form-hint">Which ports carry it is set on the <a href="#" data-goto="network-physical">Ports</a> page.</p>`;
    }

    function cardHtml(c) {
        const n = c.server;
        const isDefault = !c.isNew && n.default;
        const dirty = isDirty(c);
        const lock = !editable || busy;
        const title = c.isNew ? (c.form.name.trim() || 'New network') : (isDefault ? 'LAN' : (n.name || n.id));
        const dhcpOn = c.form.dhcpEnabled;
        const vlanTag = c.isNew ? '' : `<span class="tag">VLAN ${n.vlan}</span>`;

        return `<section class="card net-card" data-key="${c.key}">
            <div class="net-card-head">
                <h2>${escapeHtml(title)}</h2>
                ${isDefault ? '<span class="tag tag-ok">default</span>' : ''}
                ${vlanTag}
                ${!c.isNew && n.pending ? '<span class="tag tag-draft">Draft</span>' : ''}
                ${c.isNew ? '<span class="tag tag-draft">New</span>' : ''}
            </div>

            <div class="form-grid">
                ${isDefault ? '' : inputHtml(c, 'name', 'Name', { placeholder: 'Guest' })}
                ${isDefault ? '' : inputHtml(c, 'vlan', 'VLAN ID', { placeholder: '20', mode: 'numeric' })}
                ${inputHtml(c, 'ip', 'Router address', { placeholder: '192.168.1.1', mode: 'decimal' })}
                ${inputHtml(c, 'netmask', 'Netmask', { placeholder: '255.255.255.0', mode: 'decimal' })}
                <div class="form-field form-field--wide"><span class="form-label">Ports</span>${portsHtml(c)}</div>
            </div>

            <h3 class="form-section">DHCP server</h3>
            ${toggleHtml(c, 'dhcpEnabled', 'Give addresses to devices on this network', '', false)}
            ${!dhcpOn && !c.isNew && n.dhcp === null && isDefault ? '<p class="form-hint">The DHCP server is not managed by this configuration yet. Turn it on and fill in a range to start managing it here.</p>' : ''}
            <div class="form-grid${dhcpOn ? '' : ' is-off'}">
                ${inputHtml(c, 'start', 'First address', { placeholder: '192.168.20.100', mode: 'decimal', disabled: !dhcpOn })}
                ${inputHtml(c, 'end', 'Last address', { placeholder: '192.168.20.200', mode: 'decimal', disabled: !dhcpOn })}
                ${inputHtml(c, 'lease', 'Lease time', { placeholder: '12h', list: 'net-lease-options', disabled: !dhcpOn })}
            </div>

            <h3 class="form-section">What this network can reach</h3>
            <div class="net-access">
                ${isDefault
                    ? '<label class="toggle toggle--block"><input type="checkbox" checked disabled><span><strong>Internet access</strong><span class="form-hint">Always on for the default network.</span></span></label>'
                    : toggleHtml(c, 'internet', 'Allow internet access', 'Off, devices here can reach this router for an address and DNS, and nothing else.', false)}
                ${toggleHtml(c, 'isolated', 'Isolate from other networks', 'Devices here cannot reach other networks, and devices on other networks cannot reach them.', false)}
            </div>
            ${!isDefault && dhcpOn && !c.form.internet ? '<p class="form-hint net-warn">Devices will get an address but cannot reach the internet until you allow it.</p>' : ''}

            ${c.message ? `<div class="form-message${c.message.kind === 'ok' ? ' form-message--ok' : ''}" role="alert">${escapeHtml(c.message.text)}</div>` : ''}

            <div class="form-actions">
                ${isDefault || c.isNew ? '' : `<button type="button" class="cfg-btn cfg-btn--danger net-delete" data-key="${c.key}"${lock ? ' disabled' : ''}>${c.confirmDelete ? 'Click again to delete' : 'Delete network'}</button>`}
                <span class="form-hint net-state" data-key="${c.key}">${!editable ? 'A change is being applied or waits for confirmation: networks cannot be edited right now.' : (dirty && !c.isNew ? 'You have unsaved changes.' : '')}</span>
                ${c.isNew
                    ? `<button type="button" class="cfg-btn cfg-btn--ghost net-cancel" data-key="${c.key}"${busy ? ' disabled' : ''}>Cancel</button>`
                    : `<button type="button" class="cfg-btn cfg-btn--ghost net-reset" data-key="${c.key}"${busy || !dirty ? ' disabled' : ''}>Reset</button>`}
                <button type="button" class="cfg-btn cfg-btn--primary net-save" data-key="${c.key}"${lock || !dirty ? ' disabled' : ''}>${c.isNew ? 'Add network' : 'Save to draft'}</button>
            </div>
        </section>`;
    }

    function renderCard(c) {
        if (c.key !== selected) return;
        const el = document.querySelector(`#net-list .net-card[data-key="${c.key}"]`);
        if (!el) { renderAll(); return; }
        el.outerHTML = cardHtml(c);
        renderPicker();
    }

    // What the picker says about a network: its VLAN, and whether it has a draft or unsaved edits
    function optionLabel(c) {
        if (c.isNew) return (c.form.name.trim() || 'New network') + ' (new)';
        const n = c.server;
        let label = n.default ? 'LAN' : `${n.name || n.id} (VLAN ${n.vlan})`;
        if (isDirty(c)) label += ' \u2022 unsaved';
        else if (n.pending) label += ' \u2022 draft';
        return label;
    }

    function renderPicker() {
        const sel = $('net-select');
        if (!cards.some(c => c.key === selected)) selected = cards.length ? cards[0].key : 'lan';
        sel.innerHTML = cards.map(c => `<option value="${escapeHtml(c.key)}"${c.key === selected ? ' selected' : ''}>${escapeHtml(optionLabel(c))}</option>`).join('');
        $('net-add').disabled = !editable || busy || cards.some(c => c.isNew) || cards.length >= 17;
    }

    function renderAll() {
        if (!cards.some(c => c.key === selected)) selected = cards.length ? cards[0].key : 'lan';
        const c = cards.find(x => x.key === selected);
        $('net-list').innerHTML = c ? cardHtml(c) : '';
        renderPicker();
    }

    function showMessage(text, kind) {
        const box = $('net-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    // ---------------------------------------------------------------- loading

    async function load() {
        const res = await apiFetch('/api/v1/networks');
        if (!res || !res.ok) { showMessage('Could not load the networks.', 'error'); return; }
        const data = await res.json();
        editable = data.editable !== false;
        const fresh = (data.networks || []).map(n => ({ key: n.id, isNew: false, server: n, form: formOf(n), errors: {}, message: null, confirmDelete: false }));
        // a card being added stays where it was
        const adding = cards.filter(c => c.isNew);
        // edits in a card that is not on show are kept, as long as the network still exists
        const kept = new Map(cards.filter(c => !c.isNew).map(c => [c.key, c]));
        cards = fresh.map(f => { const old = kept.get(f.key); return old && !old.justSaved && isDirty(old) ? Object.assign(f, { form: old.form }) : f; }).concat(adding);
        renderAll();
    }

    // ---------------------------------------------------------------- saving

    function bodyOf(c) {
        const f = c.form;
        const body = { ip: f.ip.trim(), netmask: f.netmask.trim(), isolated: f.isolated };
        const isDefault = !c.isNew && c.server.default;
        if (!isDefault) {
            body.name = f.name.trim();
            body.vlan = /^\d+$/.test(f.vlan.trim()) ? parseInt(f.vlan.trim(), 10) : 0;
            body.internet = f.internet;
        }
        // A network with no DHCP server that stays that way (or a default network whose DHCP is not
        // managed yet and was left alone) sends nothing about DHCP
        const had = !c.isNew && c.server.dhcp;
        const touched = f.dhcpEnabled || f.start.trim() || f.end.trim() || f.lease.trim();
        if (had || touched) {
            body.dhcp = { enabled: f.dhcpEnabled, range_start: f.start.trim(), range_end: f.end.trim(), lease_time: f.lease.trim() };
        }
        return body;
    }

    function setErrors(c, errors) {
        c.errors = {};
        const general = [];
        (errors || []).forEach(e => {
            const f = ERROR_FIELD[e.field];
            if (f) c.errors[f] = c.errors[f] ? c.errors[f] + ' ' + e.message : e.message;
            else general.push(e.message);
        });
        return general;
    }

    async function save(c) {
        c.message = null;
        c.errors = {};
        if (c.form.vlan.trim() && !/^\d+$/.test(c.form.vlan.trim()) && !(!c.isNew && c.server.default)) {
            c.errors.vlan = 'must be a number between 2 and 4094';
            c.message = { text: 'Some settings need attention.', kind: 'error' };
            renderCard(c);
            return;
        }

        busy = true;
        renderCard(c);
        const url = c.isNew ? '/api/v1/networks' : '/api/v1/networks/' + encodeURIComponent(c.server.id);
        const res = await apiFetch(url, {
            method: c.isNew ? 'POST' : 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(bodyOf(c)),
        });
        busy = false;

        if (!res) { c.message = { text: 'The router did not answer. Try again.', kind: 'error' }; renderCard(c); return; }
        if (!res.ok) {
            let err = {};
            try { err = await res.json(); } catch (e) { /* not JSON */ }
            const general = setErrors(c, err.errors);
            const text = general.join(' ') || (Object.keys(c.errors).length ? 'Some settings need attention.' : (err.error || `Saving failed (${res.status}).`));
            c.message = { text, kind: 'error' };
            renderCard(c);
            return;
        }

        const wasNew = c.isNew;
        if (wasNew) {
            // show the network that was just added
            try { selected = (await res.json()).id || selected; } catch (e) { selected = 'lan'; }
        }
        c.justSaved = true; // its form now matches what the server holds
        cards = cards.filter(x => x !== c || !wasNew); // the new card is replaced by the saved network
        showMessage(wasNew ? 'Network added to the draft. Review and commit it from the bar at the top when you are ready.'
            : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await load();
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    async function remove(c) {
        if (!c.confirmDelete) {
            c.confirmDelete = true;
            renderCard(c);
            setTimeout(() => { if (c.confirmDelete) { c.confirmDelete = false; renderCard(c); } }, 4000);
            return;
        }
        c.confirmDelete = false;
        c.message = null;
        busy = true;
        renderCard(c);
        const res = await apiFetch('/api/v1/networks/' + encodeURIComponent(c.server.id), { method: 'DELETE' });
        busy = false;
        if (!res || !res.ok) {
            let err = {};
            try { err = await res.json(); } catch (e) { /* not JSON */ }
            c.message = { text: (err.errors && err.errors.map(e => e.message).join(' ')) || err.error || 'The network could not be deleted.', kind: 'error' };
            renderCard(c);
            return;
        }
        showMessage('Network removed from the draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        selected = 'lan';
        await load();
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    // ---------------------------------------------------------------- events

    const cardOf = (key) => cards.find(c => c.key === key);

    function wire() {
        if (wired) return;
        wired = true;
        const list = $('net-list');

        list.addEventListener('input', (e) => {
            const el = e.target;
            const c = el.dataset && cardOf(el.dataset.key);
            if (!c || el.type === 'checkbox') return;
            c.form[el.dataset.field] = el.value;
            delete c.errors[el.dataset.field];
            el.removeAttribute('aria-invalid');
            const msg = el.parentElement.querySelector('.field-error');
            if (msg) msg.remove();
            refreshButtons(c);
        });
        list.addEventListener('change', (e) => {
            const el = e.target;
            const c = el.dataset && cardOf(el.dataset.key);
            if (!c || el.type !== 'checkbox') return;
            c.form[el.dataset.field] = el.checked;
            renderCard(c); // the DHCP fields and the notes depend on the switches
        });
        list.addEventListener('click', (e) => {
            const btn = e.target.closest('button[data-key]');
            if (!btn) return;
            const c = cardOf(btn.dataset.key);
            if (!c) return;
            if (btn.classList.contains('net-save')) save(c);
            else if (btn.classList.contains('net-delete')) remove(c);
            else if (btn.classList.contains('net-reset')) { c.form = formOf(c.server); c.errors = {}; c.message = null; renderCard(c); }
            else if (btn.classList.contains('net-cancel')) { cards = cards.filter(x => x !== c); selected = 'lan'; renderAll(); }
        });
        list.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter' || e.target.tagName !== 'INPUT' || e.target.type !== 'text') return;
            const c = cardOf(e.target.dataset.key);
            if (c && isDirty(c) && editable && !busy) { e.preventDefault(); save(c); }
        });

        $('net-add').addEventListener('click', () => {
            const s = suggestion();
            newCount++;
            cards.push({
                key: 'new-' + newCount, isNew: true, server: null, errors: {}, message: null, confirmDelete: false,
                form: { name: '', vlan: s.vlan, ip: s.ip, netmask: s.netmask, dhcpEnabled: true, start: s.start, end: s.end, lease: '12h', internet: false, isolated: true },
            });
            selected = 'new-' + newCount;
            renderAll();
            const nameField = document.getElementById(`new-${newCount}-name`);
            if (nameField) nameField.focus();
        });

        $('net-select').addEventListener('change', (e) => {
            selected = e.target.value;
            renderAll();
        });
    }

    // Typing must not rebuild the card (that would drop the focus): only the buttons and the note follow
    function refreshButtons(c) {
        const el = document.querySelector(`#net-list .net-card[data-key="${c.key}"]`);
        if (!el) return;
        const dirty = isDirty(c);
        const save = el.querySelector('.net-save'), reset = el.querySelector('.net-reset');
        if (save) save.disabled = !editable || busy || !dirty;
        if (reset) reset.disabled = busy || !dirty;
        const state = el.querySelector('.net-state');
        if (state && editable) state.textContent = dirty && !c.isNew ? 'You have unsaved changes.' : '';
        const title = el.querySelector('.net-card-head h2');
        if (title && c.isNew) title.textContent = c.form.name.trim() || 'New network';
        const opt = document.querySelector(`#net-select option[value="${c.key}"]`);
        if (opt) opt.textContent = optionLabel(c);
    }

    function init() {
        wire();
        showMessage('');
        cards = [];
        selected = 'lan';
        load();
        return () => {};
    }

    return { init };
})();
