// web/static/scripts/netdevices.js
// Network devices page: the switches (and, later, access points) the router watches, how each is answering, their ports and what is behind them.
// Devices are read over SNMP; a change here takes effect at once (it is not part of the router's configuration draft). docs/device-pages.md.

window.NetDevicesModule = (function () {
    let data = null;             // GET /api/v1/network-devices
    let editing = null;          // { id (null: new), form, errors, message }
    let open = {};               // id -> the ports are shown
    let confirmDelete = null;
    let busy = false;
    let timer = null;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const AUTH = ['', 'MD5', 'SHA', 'SHA224', 'SHA256', 'SHA384', 'SHA512'];
    const PRIV = ['', 'DES', 'AES', 'AES192', 'AES256'];

    function showMessage(text, kind) {
        const box = $('netdev-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    async function errorOf(res, fallback) {
        if (!res) return { general: 'The router did not answer. Try again.', fields: {} };
        let err = {};
        try { err = await res.json(); } catch (e) { /* not JSON */ }
        const fields = {};
        (err.errors || []).forEach(x => { fields[x.field] = (fields[x.field] ? fields[x.field] + ' ' : '') + x.message; });
        return { general: err.error || fallback, fields };
    }

    const errorText = (e) => (e ? `<p class="field-error">${escapeHtml(e)}</p>` : '');

    function ago(sec) {
        const s = Math.max(0, Math.round(Date.now() / 1000 - sec));
        if (s < 60) return 'just now';
        if (s < 3600) return `${Math.round(s / 60)} min ago`;
        if (s < 86400) return `${Math.round(s / 3600)} h ago`;
        return `${Math.round(s / 86400)} days ago`;
    }
    function span(sec) {
        if (sec >= 86400) { const n = Math.floor(sec / 86400); return `${n} day${n === 1 ? "" : "s"}`; }
        if (sec >= 3600) return `${Math.floor(sec / 3600)} h`;
        return `${Math.max(1, Math.floor(sec / 60))} min`;
    }
    function speed(m) { return m >= 1000 ? `${m / 1000} Gbit/s` : `${m} Mbit/s`; }

    // ---------------------------------------------------------------- rendering

    function statusHtml(d) {
        if (!d.enabled) return '<span class="tag">Switched off</span>';
        if (!d.polled) return '<span class="tag">Reading…</span>';
        if (d.reachable) return `<span class="tag tag-ok">Answering</span> <span class="form-hint">${d.clients} device${d.clients === 1 ? '' : 's'} seen${d.uptime_seconds ? `, up ${span(d.uptime_seconds)}` : ''}.</span>`;
        return `<span class="tag tag-warn">Not answering</span> <span class="form-hint">${escapeHtml(d.error || '')}${d.last_ok ? ` Last answered ${ago(d.last_ok)}.` : ''}</span>`;
    }

    function portsHtml(d) {
        if (!d.ports.length) return '<p class="form-hint">No ports read yet.</p>';
        const rows = d.ports.map(p => {
            const n = p.neighbour;
            const behind = n ? `${escapeHtml(n.name || n.id || '')}${n.port ? ` (${escapeHtml(n.port)})` : ''}` : '';
            return `<tr><td>${escapeHtml(p.name)}${p.alias ? ` <span class="form-hint">${escapeHtml(p.alias)}</span>` : ''}</td>
                <td>${p.up ? '<span class="tag tag-ok">Up</span>' : '<span class="tag">Down</span>'}</td>
                <td>${p.up && p.speed_mbps ? speed(p.speed_mbps) : ''}</td><td>${p.vlan || ''}</td><td>${p.up ? p.macs : ''}</td><td>${behind}</td></tr>`;
        }).join('');
        return `<div class="table-container"><table class="data-table"><thead><tr><th>Port</th><th>Link</th><th>Speed</th><th>VLAN</th><th>Devices</th><th>Connected to</th></tr></thead><tbody>${rows}</tbody></table></div>`;
    }

    function rowHtml(d) {
        const lock = busy || !!editing;
        const id = escapeHtml(d.id);
        const shown = !!open[d.id];
        const v2c = d.snmp && d.snmp.version === '2c';
        return `<div class="fw-row fw-rule${d.enabled ? '' : ' fw-rule--off'} netdev-row">
            <div class="fw-main"><div class="fw-name"><strong>${escapeHtml(d.name)}</strong> <span class="form-hint">${escapeHtml(d.address)} · SNMP ${d.snmp ? escapeHtml(d.snmp.version) : ''}</span></div>
                <div class="ddns-status">${statusHtml(d)}</div>
                ${d.description ? `<div class="form-hint fw-text">${escapeHtml(d.system_name ? d.system_name + ' · ' : '')}${escapeHtml(d.description)}</div>` : ''}
                ${v2c && d.enabled ? '<div class="form-hint">SNMP version 2c sends its community in clear text on the network; version 3 does not.</div>' : ''}
                ${shown ? portsHtml(d) : ''}</div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost nd-ports" data-id="${id}">${shown ? 'Hide ports' : 'Ports'}</button>
                <button type="button" class="cfg-btn cfg-btn--ghost nd-poll" data-id="${id}"${busy || !d.enabled ? ' disabled' : ''}>Read now</button>
                <button type="button" class="cfg-btn cfg-btn--ghost nd-edit" data-id="${id}"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger nd-delete" data-id="${id}"${lock ? ' disabled' : ''}>${confirmDelete === d.id ? 'Click again' : 'Delete'}</button>
            </div></div>`;
    }

    function listHtml() {
        const lock = busy || !!editing;
        return `<section class="card">
            <div class="net-card-head"><h2>Watched devices</h2><span class="tag">${data.devices.length}</span>
                <button type="button" class="cfg-btn" id="nd-add" style="margin-left:auto"${lock || data.devices.length >= data.max ? ' disabled' : ''}>Add a device</button></div>
            <p class="form-hint">Switches the router reads over SNMP. Each says which of its ports a device is on, so a device's page can show the switch and port it is plugged into, and the speed and VLAN of that port. The router only reads: it never changes anything on the switch. Turn SNMP on in the switch first and allow this router's address to read it.</p>
            <div class="fw-list">${data.devices.length ? data.devices.map(rowHtml).join('') : '<div class="fw-row fw-empty">No devices yet.</div>'}</div>
        </section>`;
    }

    function field(label, id, control, key, hint, cls) {
        return `<div class="form-field${cls ? ' ' + cls : ''}"><label for="${id}">${label}</label>${control}${hint ? `<p class="form-hint">${hint}</p>` : ''}${errorText(editing.errors[key])}</div>`;
    }

    function input(id, key, value, extra) {
        return `<input type="${extra && extra.type || 'text'}" id="${id}" data-ed="${key}" value="${escapeHtml(value)}"${extra && extra.placeholder ? ` placeholder="${escapeHtml(extra.placeholder)}"` : ''} autocomplete="${extra && extra.autocomplete || 'off'}" spellcheck="false" autocapitalize="off"${editing.errors[key] ? ' aria-invalid="true"' : ''}${busy ? ' disabled' : ''}>`;
    }

    function select(id, key, value, options) {
        return `<select id="${id}" class="role-select" data-ed="${key}"${busy ? ' disabled' : ''}>${options.map(o => `<option value="${o}"${o === value ? ' selected' : ''}>${o || 'None'}</option>`).join('')}</select>`;
    }

    function editorHtml() {
        const f = editing.form;
        const saved = (set) => editing.id && set ? ' One is saved; leave this empty to keep it.' : '';
        const parts = [];
        parts.push(field('Name', 'nd-ed-name', input('nd-ed-name', 'name', f.name, { placeholder: 'Hall switch' }), 'name', 'What the pages call it.'));
        parts.push(field('Address', 'nd-ed-address', input('nd-ed-address', 'address', f.address, { placeholder: '192.168.1.2' }), 'address', 'Its address on the network, or its host name.'));
        parts.push(field('SNMP version', 'nd-ed-version', `<select id="nd-ed-version" class="role-select" data-ed="version"${busy ? ' disabled' : ''}><option value="3"${f.version === '3' ? ' selected' : ''}>3 (user and passwords, can be encrypted)</option><option value="2c"${f.version === '2c' ? ' selected' : ''}>2c (a community)</option></select>`, 'snmp.version'));
        if (f.version === '2c') {
            parts.push(field('Community', 'nd-ed-community', input('nd-ed-community', 'community', f.community, { type: 'password', autocomplete: 'new-password', placeholder: f.communitySet ? 'unchanged' : '' }), 'snmp.community',
                `The read-only community set on the switch. It is sent unencrypted whenever the router reads it, so use version 3 when the switch has it.${saved(f.communitySet)}`));
        } else {
            parts.push(field('User name', 'nd-ed-user', input('nd-ed-user', 'user', f.user), 'snmp.user', 'A user of the switch with read-only access.'));
            parts.push(field('Password check', 'nd-ed-authproto', select('nd-ed-authproto', 'authProto', f.authProto, AUTH), 'snmp.auth_proto', 'How the user is proven. None sends the user name only.'));
            if (f.authProto) parts.push(field('Password', 'nd-ed-authpass', input('nd-ed-authpass', 'authPass', f.authPass, { type: 'password', autocomplete: 'new-password', placeholder: f.authPassSet ? 'unchanged' : '' }), 'snmp.auth_pass', `At least 8 characters.${saved(f.authPassSet)}`));
            if (f.authProto) {
                parts.push(field('Encryption', 'nd-ed-privproto', select('nd-ed-privproto', 'privProto', f.privProto, PRIV), 'snmp.priv_proto', 'Hides what is read from anyone on the network.'));
                if (f.privProto) parts.push(field('Encryption password', 'nd-ed-privpass', input('nd-ed-privpass', 'privPass', f.privPass, { type: 'password', autocomplete: 'new-password', placeholder: f.privPassSet ? 'unchanged' : '' }), 'snmp.priv_pass', `At least 8 characters.${saved(f.privPassSet)}`));
            }
        }
        parts.push(field('Port', 'nd-ed-port', input('nd-ed-port', 'port', f.port, { placeholder: '161' }), 'snmp.port', 'Left empty: 161.'));
        return `<section class="card fw-editor" id="nd-editor">
            <div class="net-card-head"><h2>${editing.id ? 'Edit device' : 'New device'}</h2></div>
            <div class="form-grid">${parts.join('')}</div>
            <div class="net-access"><label class="toggle toggle--block"><input type="checkbox" data-ed="enabled"${f.enabled ? ' checked' : ''}${busy ? ' disabled' : ''}><span><strong>Switched on</strong><span class="form-hint">Off keeps it here without reading it.</span></span></label></div>
            ${editing.message ? `<div class="form-message" role="alert">${escapeHtml(editing.message)}</div>` : ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="nd-ed-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="nd-ed-save"${busy ? ' disabled' : ''}>${editing.id ? 'Save' : 'Add device'}</button>
            </div></section>`;
    }

    function render() {
        if (!data) return;
        $('netdev-body').innerHTML = listHtml() + (editing ? editorHtml() : '');
    }

    // ---------------------------------------------------------------- loading and saving

    async function load() {
        const res = await apiFetch('/api/v1/network-devices');
        if (!res || !res.ok) { showMessage('Could not load the watched devices.', 'error'); return; }
        data = await res.json();
        render();
    }

    function openEditor(id) {
        const d = id ? data.devices.find(x => x.id === id) : null;
        const s = d && d.snmp;
        const form = d
            ? { name: d.name, address: d.address, enabled: d.enabled, version: s.version, port: s.port ? String(s.port) : '', community: '', user: s.user || '', authProto: s.auth_proto || '', authPass: '', privProto: s.priv_proto || '', privPass: '',
                communitySet: s.community_set, authPassSet: s.auth_pass_set, privPassSet: s.priv_pass_set }
            : { name: '', address: '', enabled: true, version: '3', port: '', community: '', user: '', authProto: 'SHA', authPass: '', privProto: 'AES', privPass: '', communitySet: false, authPassSet: false, privPassSet: false };
        editing = { id: id || null, form, errors: {}, message: null };
        render();
        const first = $('nd-ed-name');
        if (first) first.focus();
    }

    async function saveEditor() {
        const f = editing.form;
        const snmp = { version: f.version, port: parseInt(f.port, 10) || 0 };
        if (f.version === '2c') {
            if (f.community !== '') snmp.community = f.community;
        } else {
            snmp.user = f.user; snmp.auth_proto = f.authProto; snmp.priv_proto = f.authProto ? f.privProto : '';
            if (f.authPass !== '') snmp.auth_pass = f.authPass;
            if (f.privPass !== '') snmp.priv_pass = f.privPass;
        }
        const body = { name: f.name, address: f.address, kind: 'snmp', enabled: f.enabled, snmp };
        editing.errors = {}; editing.message = null;
        busy = true; render();
        const base = '/api/v1/network-devices';
        const res = await apiFetch(editing.id ? `${base}/${encodeURIComponent(editing.id)}` : base, {
            method: editing.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, 'Saving failed.');
            const general = [];
            Object.keys(e.fields).forEach(k => { if (['name', 'address', 'snmp.version', 'snmp.community', 'snmp.user', 'snmp.auth_proto', 'snmp.auth_pass', 'snmp.priv_proto', 'snmp.priv_pass', 'snmp.port'].includes(k)) editing.errors[k] = e.fields[k]; else general.push(e.fields[k]); });
            f.community = f.authPass = f.privPass = '';
            editing.message = general.join(' ') || (Object.keys(editing.errors).length ? 'Some settings need attention.' : e.general);
            render();
            return;
        }
        const wasNew = !editing.id;
        editing = null;
        await load();
        showMessage(wasNew ? 'Added. The router is reading it now.' : 'Saved. The router is reading it again.', 'ok');
        setTimeout(load, 3000);
    }

    async function remove(id) {
        if (confirmDelete !== id) {
            confirmDelete = id; render();
            setTimeout(() => { if (confirmDelete === id) { confirmDelete = null; render(); } }, 4000);
            return;
        }
        confirmDelete = null;
        busy = true; render();
        const res = await apiFetch(`/api/v1/network-devices/${encodeURIComponent(id)}`, { method: 'DELETE' });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'It could not be removed.'); showMessage(e.general, 'error'); await load(); return; }
        await load();
        showMessage('Removed.', 'ok');
    }

    async function pollNow(id) {
        busy = true; render();
        const res = await apiFetch(`/api/v1/network-devices/${encodeURIComponent(id)}/poll`, { method: 'POST' });
        busy = false;
        if (!res || !res.ok) showMessage((await errorOf(res, 'It could not be read.')).general, 'error');
        await load();
    }

    // ---------------------------------------------------------------- events

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('netdev-body');
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            const id = btn.dataset.id;
            if (btn.id === 'nd-add') openEditor(null);
            else if (btn.id === 'nd-ed-save') saveEditor();
            else if (btn.id === 'nd-ed-cancel') { editing = null; render(); }
            else if (btn.classList.contains('nd-edit')) openEditor(id);
            else if (btn.classList.contains('nd-delete')) remove(id);
            else if (btn.classList.contains('nd-poll')) pollNow(id);
            else if (btn.classList.contains('nd-ports')) { open[id] = !open[id]; render(); }
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (!editing || !el.dataset.ed) return;
            const f = editing.form;
            if (el.dataset.ed === 'enabled') f.enabled = el.checked;
            else if (['version', 'authProto', 'privProto'].includes(el.dataset.ed)) {
                f[el.dataset.ed] = el.value;
                if (el.dataset.ed === 'authProto' && !el.value) f.privProto = '';
                editing.errors = {};
                render();
            }
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            if (!editing || !el.dataset.ed || el.type === 'checkbox' || el.tagName === 'SELECT') return;
            editing.form[el.dataset.ed] = el.value;
            const key = { community: 'snmp.community', user: 'snmp.user', authPass: 'snmp.auth_pass', privPass: 'snmp.priv_pass', port: 'snmp.port' }[el.dataset.ed] || el.dataset.ed;
            delete editing.errors[key];
            el.removeAttribute('aria-invalid');
            el.closest('.form-field')?.querySelector('.field-error')?.remove();
        });
        body.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && editing && e.target.dataset.ed && e.target.tagName === 'INPUT' && e.target.type !== 'checkbox' && !busy) { e.preventDefault(); saveEditor(); }
        });
    }

    function init() {
        wire();
        showMessage('');
        data = null; editing = null; confirmDelete = null; busy = false;
        load();
        timer = setInterval(async () => {
            if (editing || busy) return;
            const res = await apiFetch('/api/v1/network-devices');
            if (res && res.ok && !editing && !busy) { data = await res.json(); render(); }
        }, 15000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
