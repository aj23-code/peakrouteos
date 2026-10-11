// web/static/scripts/ddns.js
// Dynamic DNS page: the names that are kept pointed at the internet connection's address, how each is going, and an editor.
// Saving only writes a draft; the names start being kept up to date when the change is committed. docs/ddns.md has the rules.

window.DDNSModule = (function () {
    let data = null;             // GET /api/v1/ddns
    let editing = null;          // { id (null: new), form, errors, message }
    let confirmDelete = null;
    let busy = false;
    let notes = {};              // id -> the answer of the last "Update now" (shown until the next refresh)
    let timer = null;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const editable = () => !data || data.editable !== false;

    // what the provider menu offers: the service, and for dyndns2 which server
    const PROVIDERS = [
        { value: 'dyndns2:members.dyndns.org', label: 'DynDNS', provider: 'dyndns2', server: 'members.dyndns.org' },
        { value: 'dyndns2:dynupdate.no-ip.com', label: 'No-IP', provider: 'dyndns2', server: 'dynupdate.no-ip.com' },
        { value: 'dyndns2:api.dynu.com', label: 'Dynu', provider: 'dyndns2', server: 'api.dynu.com' },
        { value: 'dyndns2:', label: 'Another service that speaks dyndns2', provider: 'dyndns2', server: '' },
        { value: 'duckdns', label: 'DuckDNS', provider: 'duckdns' },
        { value: 'cloudflare', label: 'Cloudflare', provider: 'cloudflare' },
        { value: 'custom', label: 'Another service (an address to call)', provider: 'custom' },
    ];
    const providerOf = (form) => form.provider === 'dyndns2'
        ? (PROVIDERS.find(p => p.provider === 'dyndns2' && p.server === form.server) || PROVIDERS[3])
        : PROVIDERS.find(p => p.provider === form.provider);

    function showMessage(text, kind) {
        const box = $('ddns-message');
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

    function ago(ms) {
        const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
        if (s < 60) return 'just now';
        if (s < 3600) return `${Math.round(s / 60)} min ago`;
        if (s < 86400) return `${Math.round(s / 3600)} h ago`;
        return `${Math.round(s / 86400)} days ago`;
    }
    const inText = (ms) => {
        const s = Math.max(0, Math.round((ms - Date.now()) / 1000));
        return s < 90 ? 'in a minute' : (s < 3600 ? `in ${Math.round(s / 60)} min` : `in ${Math.round(s / 3600)} h`);
    };

    // ---------------------------------------------------------------- rendering

    function statusHtml(e) {
        const n = notes[e.id];
        const st = n || e.status;
        if (!e.enabled) return '<span class="form-hint">Switched off.</span>';
        if (!st) return `<span class="form-hint">${e.pending ? 'Starts when you commit the change.' : 'Waiting to be updated.'}</span>`;
        let tag, text = escapeHtml(st.message);
        if (st.attention) tag = '<span class="tag tag-warn">Stopped</span>';
        else if (st.ok) tag = '<span class="tag tag-ok">Up to date</span>';
        else if (!st.next_try) tag = '<span class="tag">Waiting</span>';   // no address to publish yet: not a failure
        else tag = '<span class="tag tag-draft">Trouble</span>';
        let when = '';
        if (st.last_success) when = ` <span class="form-hint">Last told ${ago(st.last_success)}.</span>`;
        if (!st.ok && !st.attention && st.next_try) when += ` <span class="form-hint">Trying again ${inText(st.next_try)}.</span>`;
        if (st.attention) when += ' <span class="form-hint">Put the problem right (edit the name and commit, or press Update now) and it carries on.</span>';
        return `${tag} ${text}${when}`;
    }

    function rowHtml(e) {
        const lock = !editable() || busy || !!editing;
        const id = escapeHtml(e.id);
        const running = !!e.status && !e.pending && e.enabled;
        return `<div class="fw-row fw-rule${e.enabled ? '' : ' fw-rule--off'} ddns-row">
            <label class="toggle fw-switch"><input type="checkbox" class="ddns-enable" data-id="${id}"${e.enabled ? ' checked' : ''}${lock ? ' disabled' : ''} aria-label="Switch on or off"></label>
            <div class="fw-main"><div class="fw-name"><strong>${escapeHtml(e.hostname)}</strong>${e.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}</div>
                <div class="form-hint fw-text">${escapeHtml(e.text)}</div>
                <div class="ddns-status">${statusHtml(e)}</div></div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost ddns-update" data-id="${id}"${busy || !running ? ' disabled' : ''} title="${running ? 'Tell the service the address now' : 'Available once the name is running'}">Update now</button>
                <button type="button" class="cfg-btn cfg-btn--ghost ddns-edit" data-id="${id}"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger ddns-delete" data-id="${id}"${lock ? ' disabled' : ''}>${confirmDelete === e.id ? 'Click again' : 'Delete'}</button>
            </div></div>`;
    }

    function wanHtml() {
        if (!data.wan_address) return '<p class="form-hint">The internet connection has no address right now.</p>';
        return data.wan_public
            ? `<p class="form-hint">The internet connection's address right now: <code>${escapeHtml(data.wan_address)}</code>.</p>`
            : `<p class="form-hint net-warn">The internet connection's address, <code>${escapeHtml(data.wan_address)}</code>, is not a public address: this router is behind another router or a carrier's NAT. A name pointed at it could not be reached. Choose <em>the address an internet service reports</em> for names below, and know that reaching this router from outside also needs the other router to forward to it.</p>`;
    }

    function listHtml() {
        const lock = !editable() || busy || !!editing;
        return `<section class="card">
            <div class="net-card-head"><h2>Names</h2><span class="tag">${data.entries.length}</span>
                <button type="button" class="cfg-btn" id="ddns-add" style="margin-left:auto"${lock || data.entries.length >= 10 ? ' disabled' : ''}>Add a name</button></div>
            <p class="form-hint">Keeps a name such as <code>home.example.com</code> pointed at this router, even when the provider changes its address: when the address changes (and once a week, so that the service does not drop an unused name) the router tells the service. The names start being kept up to date when you commit.</p>
            ${wanHtml()}
            <div class="fw-list">${data.entries.length ? data.entries.map(rowHtml).join('') : '<div class="fw-row fw-empty">No names yet.</div>'}</div>
        </section>`;
    }

    function field(label, id, control, key, hint, cls) {
        return `<div class="form-field${cls ? ' ' + cls : ''}"><label for="${id}">${label}</label>${control}${hint ? `<p class="form-hint">${hint}</p>` : ''}${errorText(editing.errors[key])}</div>`;
    }

    function input(id, key, value, extra) {
        const lock = !editable() || busy;
        return `<input type="${extra && extra.type || 'text'}" id="${id}" data-ed="${key}" value="${escapeHtml(value)}"${extra && extra.placeholder ? ` placeholder="${escapeHtml(extra.placeholder)}"` : ''} autocomplete="${extra && extra.autocomplete || 'off'}" spellcheck="false" autocapitalize="off"${editing.errors[key] ? ' aria-invalid="true"' : ''}${lock ? ' disabled' : ''}>`;
    }

    function editorHtml() {
        const f = editing.form;
        const prov = providerOf(f);
        const lock = !editable() || busy;
        const pwLabel = { dyndns2: 'Password', duckdns: 'Token', cloudflare: 'API token', custom: 'Password' }[f.provider];
        const pwHint = {
            dyndns2: 'Of the account at the service (some services give a separate update key).',
            duckdns: 'The token on your DuckDNS account page.',
            cloudflare: 'An API token that may edit DNS for the zone (My Profile, API Tokens, "Edit zone DNS"). The record is changed, or added when there is none.',
            custom: 'Used where the address says {password}.',
        }[f.provider];
        const saved = editing.id && f.passwordSet;
        const parts = [];
        parts.push(field('Service', 'ddns-ed-provider', `<select id="ddns-ed-provider" class="role-select" data-ed="providerChoice"${lock ? ' disabled' : ''}>${PROVIDERS.map(p => `<option value="${escapeHtml(p.value)}"${p === prov ? ' selected' : ''}>${escapeHtml(p.label)}</option>`).join('')}</select>`, 'provider'));
        parts.push(field('Name to keep up to date', 'ddns-ed-hostname', input('ddns-ed-hostname', 'hostname', f.hostname, { placeholder: f.provider === 'duckdns' ? 'mine.duckdns.org' : 'home.example.com' }), 'hostname',
            f.provider === 'duckdns' ? 'A name under duckdns.org.' : 'The full name, in lower case.'));
        if (f.provider === 'dyndns2' && prov.value === 'dyndns2:') {
            parts.push(field('Update server', 'ddns-ed-server', input('ddns-ed-server', 'server', f.server, { placeholder: 'updates.example.com' }), 'server', 'The name of the service’s update server (not an address, not a web address).'));
        }
        if (f.provider === 'dyndns2' || f.provider === 'custom') {
            parts.push(field('User name', 'ddns-ed-username', input('ddns-ed-username', 'username', f.username), 'username', f.provider === 'custom' ? 'Used where the address says {username}.' : ''));
        }
        parts.push(field(pwLabel, 'ddns-ed-password', input('ddns-ed-password', 'password', f.password, { type: 'password', autocomplete: 'new-password', placeholder: saved ? 'unchanged' : '' }), 'password',
            `${pwHint}${saved ? ' One is saved; leave this empty to keep it.' : ''} It is never shown again.`));
        if (f.provider === 'custom') {
            parts.push(field('Address to call', 'ddns-ed-url', input('ddns-ed-url', 'url', f.url, { placeholder: 'https://dynamicdns.example.com/update?host={hostname}&ip={ip}' }), 'url',
                'An https address. {ip} is replaced by the address (needed), {hostname}, {username} and {password} by what is set here. A successful answer (HTTP 2xx) counts as done.', 'form-field--wide'));
        }
        return `<section class="card fw-editor" id="ddns-editor">
            <div class="net-card-head"><h2>${editing.id ? 'Edit name' : 'New name'}</h2></div>
            <div class="form-grid">${parts.join('')}</div>
            <div class="radio-row" role="radiogroup" aria-label="Which address">
                <label class="radio"><input type="radio" name="ddns-source" value="wan" data-ed="source"${f.source === 'wan' ? ' checked' : ''}${lock ? ' disabled' : ''}> The address of the internet connection
                    <span class="form-hint">The usual choice: what the provider gave this router.</span></label>
                <label class="radio"><input type="radio" name="ddns-source" value="web" data-ed="source"${f.source === 'web' ? ' checked' : ''}${lock ? ' disabled' : ''}> The address an internet service reports
                    <span class="form-hint">Behind another router or a carrier's NAT: the router asks a service (api.ipify.org) what address its requests come from.</span></label>
            </div>
            <div class="net-access"><label class="toggle toggle--block"><input type="checkbox" data-ed="enabled"${f.enabled ? ' checked' : ''}${lock ? ' disabled' : ''}><span><strong>Switched on</strong><span class="form-hint">Off keeps it here without updating it.</span></span></label></div>
            ${editing.message ? `<div class="form-message" role="alert">${escapeHtml(editing.message)}</div>` : ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="ddns-ed-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="ddns-ed-save"${lock ? ' disabled' : ''}>${editing.id ? 'Save to draft' : 'Add name'}</button>
            </div></section>`;
    }

    function render() {
        if (!data) return;
        $('ddns-body').innerHTML = listHtml() + (editing ? editorHtml() : '') +
            (!editable() ? '<p class="form-hint">A change is being applied or waits for confirmation: this page cannot be edited right now.</p>' : '');
    }

    // ---------------------------------------------------------------- loading and saving

    async function load() {
        const res = await apiFetch('/api/v1/ddns');
        if (!res || !res.ok) { showMessage('Could not load the Dynamic DNS settings.', 'error'); return; }
        data = await res.json();
        render();
    }

    function openEditor(id) {
        const e = id ? data.entries.find(x => x.id === id) : null;
        const form = e
            ? { provider: e.provider, hostname: e.hostname, server: e.server, username: e.username, password: '', url: e.url, source: e.source, enabled: e.enabled, passwordSet: e.password_set }
            : { provider: 'dyndns2', hostname: '', server: 'members.dyndns.org', username: '', password: '', url: '', source: 'wan', enabled: true, passwordSet: false };
        editing = { id: id || null, form, errors: {}, message: null };
        render();
        const first = $('ddns-ed-hostname');
        if (first) first.focus();
    }

    async function saveEditor() {
        const f = editing.form;
        const body = { provider: f.provider, hostname: f.hostname, server: f.server, username: f.username, url: f.url, source: f.source, enabled: f.enabled };
        if (f.password !== '') body.password = f.password;
        editing.errors = {}; editing.message = null;
        busy = true; render();
        const base = '/api/v1/ddns';
        const res = await apiFetch(editing.id ? `${base}/${encodeURIComponent(editing.id)}` : base, {
            method: editing.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, 'Saving failed.');
            const general = [];
            Object.keys(e.fields).forEach(k => { if (['provider', 'hostname', 'server', 'username', 'password', 'url', 'source'].includes(k)) editing.errors[k] = e.fields[k]; else general.push(e.fields[k]); });
            f.password = '';
            editing.message = general.join(' ') || (Object.keys(editing.errors).length ? 'Some settings need attention.' : e.general);
            render();
            return;
        }
        const wasNew = !editing.id;
        editing = null;
        await load();
        showMessage(wasNew ? 'Added to the draft. Review and commit it from the bar at the top; it starts then.' : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    async function toggle(id, on) {
        busy = true; render();
        const res = await apiFetch(`/api/v1/ddns/${encodeURIComponent(id)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: on }) });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'The change could not be saved.'); showMessage(Object.values(e.fields).join(' ') || e.general, 'error'); await load(); return; }
        await load();
        showMessage(on ? 'Switched on in the draft.' : 'Switched off in the draft.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    async function remove(id) {
        if (confirmDelete !== id) {
            confirmDelete = id; render();
            setTimeout(() => { if (confirmDelete === id) { confirmDelete = null; render(); } }, 4000);
            return;
        }
        confirmDelete = null;
        busy = true; render();
        const res = await apiFetch(`/api/v1/ddns/${encodeURIComponent(id)}`, { method: 'DELETE' });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'It could not be deleted.'); showMessage(e.general, 'error'); await load(); return; }
        await load();
        showMessage('Removed from the draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    async function updateNow(id) {
        busy = true; render();
        const res = await apiFetch(`/api/v1/ddns/${encodeURIComponent(id)}/update`, { method: 'POST' });
        busy = false;
        if (res && res.ok) notes[id] = await res.json();
        else showMessage((await errorOf(res, 'The update could not be made.')).general, 'error');
        render();
    }

    // ---------------------------------------------------------------- events

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('ddns-body');
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            const id = btn.dataset.id;
            if (btn.id === 'ddns-add') openEditor(null);
            else if (btn.id === 'ddns-ed-save') saveEditor();
            else if (btn.id === 'ddns-ed-cancel') { editing = null; render(); }
            else if (btn.classList.contains('ddns-edit')) openEditor(id);
            else if (btn.classList.contains('ddns-delete')) remove(id);
            else if (btn.classList.contains('ddns-update')) updateNow(id);
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.classList.contains('ddns-enable')) { toggle(el.dataset.id, el.checked); return; }
            if (!editing || !el.dataset.ed) return;
            const f = editing.form;
            if (el.dataset.ed === 'providerChoice') {
                const p = PROVIDERS.find(x => x.value === el.value);
                f.provider = p.provider;
                f.server = p.provider === 'dyndns2' ? p.server : '';
                editing.errors = {};
                render();
            } else if (el.dataset.ed === 'enabled') f.enabled = el.checked;
            else if (el.dataset.ed === 'source') f.source = el.value;
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            if (!editing || !el.dataset.ed || el.type === 'checkbox' || el.type === 'radio' || el.tagName === 'SELECT') return;
            editing.form[el.dataset.ed] = el.value;
            delete editing.errors[el.dataset.ed];
            el.removeAttribute('aria-invalid');
            el.closest('.form-field')?.querySelector('.field-error')?.remove();
        });
        body.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && editing && e.target.dataset.ed && e.target.tagName === 'INPUT' && e.target.type !== 'checkbox' && e.target.type !== 'radio' && !busy) { e.preventDefault(); saveEditor(); }
        });
    }

    function init() {
        wire();
        showMessage('');
        data = null; editing = null; confirmDelete = null; busy = false; notes = {};
        load();
        // how the names are going changes by itself: look again now and then (not while one is being edited)
        timer = setInterval(async () => {
            if (editing || busy) return;
            const res = await apiFetch('/api/v1/ddns');
            if (res && res.ok && !editing && !busy) { data = await res.json(); notes = {}; render(); }
        }, 15000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
