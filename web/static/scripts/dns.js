// web/static/scripts/dns.js
// DNS page: where the router looks names up (the upstream servers), what the devices on each network are told to use,
// the router's local names and the domains it forwards to other servers. Saving only writes a draft. docs/dns.md
// explains the model.

window.DNSModule = (function () {
    let data = null;             // GET /api/v1/dns
    let upstream = null;         // { mode, servers: text, errors, message }
    let nets = {};               // network id -> { mode, servers: text, errors, message }
    let editing = null;          // { kind: 'host' | 'forward', id (null: new), form, errors, message }
    let confirmDelete = null;    // 'host:h1' / 'forward:f1'
    let busy = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const editable = () => !data || data.editable !== false;
    const split = (text) => text.split(/[\s,]+/).filter(Boolean);
    const join = (list) => (list || []).join(', ');

    function showMessage(text, kind) {
        const box = $('dns-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
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

    // ---------------------------------------------------------------- forms follow the router unless they hold edits

    function upstreamFormOf(u) { return { mode: u.mode === 'custom' || u.static ? 'custom' : 'provider', servers: join(u.servers), errors: {}, message: null }; }
    function netFormOf(n) { return { mode: n.mode, servers: join(n.servers), errors: {}, message: null }; }

    const upstreamDirty = () => {
        const f = upstream, u = data.upstream;
        const was = upstreamFormOf(u);
        return f.mode !== was.mode || (f.mode === 'custom' && split(f.servers).join(',') !== split(was.servers).join(','));
    };
    const netDirty = (n) => {
        const f = nets[n.id], was = netFormOf(n);
        return f.mode !== was.mode || (f.mode === 'custom' && split(f.servers).join(',') !== split(was.servers).join(','));
    };

    function adopt(fresh) {
        data = fresh;
        if (!upstream || !upstreamDirty()) upstream = upstreamFormOf(data.upstream);
        const keep = nets;
        nets = {};
        data.networks.forEach(n => {
            nets[n.id] = keep[n.id] && !netFormDirtyAgainst(keep[n.id], n) ? keep[n.id] : netFormOf(n);
        });
    }
    // an old form is kept only when it differs from the server's state (the person was editing it)
    function netFormDirtyAgainst(f, n) {
        const was = netFormOf(n);
        return f.mode === was.mode && (f.mode !== 'custom' || split(f.servers).join(',') === split(was.servers).join(','));
    }

    // ---------------------------------------------------------------- rendering

    const errorText = (e) => (e ? `<p class="field-error">${escapeHtml(e)}</p>` : '');

    function upstreamHtml() {
        const u = data.upstream, f = upstream;
        const lock = !editable() || busy;
        const provider = (u.provider || []).length ? u.provider.map(s => `<code>${escapeHtml(s)}</code>`).join(', ') : 'none right now';
        const dirty = upstreamDirty();
        return `<section class="card" id="dns-upstream">
            <div class="net-card-head"><h2>Where the router asks</h2>${u.pending ? '<span class="tag tag-draft">Draft</span>' : ''}</div>
            <p class="form-hint">For every name the router does not know itself, it asks these servers.</p>
            <div class="radio-row" role="radiogroup" aria-label="Upstream servers">
                <label class="radio"><input type="radio" name="dns-up" value="provider" data-up="mode"${f.mode === 'provider' ? ' checked' : ''}${lock || u.static ? ' disabled' : ''}> The provider&rsquo;s servers
                    <span class="form-hint">What your internet connection hands out: ${provider}.</span></label>
                <label class="radio"><input type="radio" name="dns-up" value="custom" data-up="mode"${f.mode === 'custom' ? ' checked' : ''}${lock || u.static ? ' disabled' : ''}> These servers instead
                    <span class="form-hint">${u.static ? 'A connection with a fixed address needs servers of its own.' : 'For example 9.9.9.9 and 149.112.112.112.'}</span></label>
            </div>
            <div class="form-grid${f.mode === 'custom' ? '' : ' hidden'}" id="dns-up-servers">
                <div class="form-field form-field--wide"><label for="dns-up-text">Servers</label>
                    <input type="text" id="dns-up-text" data-up="servers" value="${escapeHtml(f.servers)}" placeholder="9.9.9.9, 149.112.112.112" autocomplete="off" spellcheck="false" inputmode="decimal"${lock ? ' disabled' : ''}${f.errors.servers ? ' aria-invalid="true"' : ''}>
                    <p class="form-hint">Up to three, separated by commas. The first one is also what the health chart on the Internet page pings.</p>
                    ${errorText(f.errors.servers)}</div>
            </div>
            ${f.message ? `<div class="form-message" role="alert">${escapeHtml(f.message)}</div>` : ''}
            <div class="form-actions">
                <span class="form-hint" id="dns-up-state">${dirty ? 'You have unsaved changes.' : ''}</span>
                <button type="button" class="cfg-btn cfg-btn--ghost" id="dns-up-reset"${busy || !dirty ? ' disabled' : ''}>Reset</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="dns-up-save"${lock || !dirty ? ' disabled' : ''}>Save to draft</button>
            </div>
        </section>`;
    }

    function networkHtml(n) {
        const f = nets[n.id];
        const lock = !editable() || busy;
        const dirty = netDirty(n);
        const id = escapeHtml(n.id);
        const notes = (n.notes || []).map(t => `<p class="form-hint net-warn">${escapeHtml(t)}</p>`).join('');
        return `<div class="dns-net" data-id="${id}">
            <div class="dns-net-head"><strong>${escapeHtml(n.name)}</strong>${n.default ? ' <span class="tag tag-ok">default</span>' : ''}${n.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}
                ${n.dhcp ? '' : ' <span class="tag">no DHCP from the router</span>'}</div>
            <div class="form-grid">
                <div class="form-field"><label for="dns-net-mode-${id}">Devices are told to use</label>
                    <select id="dns-net-mode-${id}" class="role-select" data-net="${id}" data-field="mode"${lock ? ' disabled' : ''}>
                        <option value="router"${f.mode === 'router' ? ' selected' : ''}>This router (${escapeHtml(n.router)})</option>
                        <option value="custom"${f.mode === 'custom' ? ' selected' : ''}>Other servers</option></select></div>
                <div class="form-field${f.mode === 'custom' ? '' : ' hidden'}" data-servers="${id}"><label for="dns-net-servers-${id}">Servers</label>
                    <input type="text" id="dns-net-servers-${id}" data-net="${id}" data-field="servers" value="${escapeHtml(f.servers)}" placeholder="192.168.1.5, 192.168.1.6" autocomplete="off" spellcheck="false" inputmode="decimal"${lock ? ' disabled' : ''}${f.errors.servers ? ' aria-invalid="true"' : ''}>
                    ${errorText(f.errors.servers)}</div>
            </div>
            ${notes}
            ${f.message ? `<div class="form-message" role="alert">${escapeHtml(f.message)}</div>` : ''}
            <div class="form-actions">
                <span class="form-hint dns-net-state" data-id="${id}">${dirty ? 'You have unsaved changes.' : ''}</span>
                <button type="button" class="cfg-btn cfg-btn--ghost dns-net-reset" data-id="${id}"${busy || !dirty ? ' disabled' : ''}>Reset</button>
                <button type="button" class="cfg-btn cfg-btn--primary dns-net-save" data-id="${id}"${lock || !dirty ? ' disabled' : ''}>Save to draft</button>
            </div>
        </div>`;
    }

    function networksHtml() {
        return `<section class="card">
            <div class="net-card-head"><h2>What each network is told</h2></div>
            <p class="form-hint">When a device gets its address (DHCP) it is also told which DNS server to use. <strong>This router</strong> gives it the router&rsquo;s local names, device names and forwarding; <strong>other servers</strong> sends it to a server you run, such as a Pi-hole. Devices with a fixed address, or their own DNS settings, ignore it. A server on another network needs a firewall rule that lets this network reach it.</p>
            ${data.networks.map(networkHtml).join('')}
        </section>`;
    }

    function rowHtml(kind, e, titleHtml, hint) {
        const lock = !editable() || busy;
        const key = `${kind}:${e.id}`;
        return `<div class="fw-row fw-rule${e.enabled ? '' : ' fw-rule--off'}">
            <label class="toggle fw-switch"><input type="checkbox" class="dns-enable" data-kind="${kind}" data-id="${escapeHtml(e.id)}"${e.enabled ? ' checked' : ''}${lock ? ' disabled' : ''} aria-label="Switch on or off"></label>
            <div class="fw-main"><div class="fw-name">${titleHtml}${e.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}${e.enabled ? '' : ' <span class="tag">Off</span>'}</div>
                <div class="form-hint fw-text">${hint}</div></div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost dns-edit" data-kind="${kind}" data-id="${escapeHtml(e.id)}"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger dns-delete" data-kind="${kind}" data-id="${escapeHtml(e.id)}"${lock ? ' disabled' : ''}>${confirmDelete === key ? 'Click again' : 'Delete'}</button>
            </div></div>`;
    }

    function hostsHtml() {
        const lock = !editable() || busy || !!editing;
        const domain = data.domain
            ? `The local domain is <code>${escapeHtml(data.domain)}</code> (change it in <a href="#" data-goto="settings">Settings</a>): a short name such as <code>nas</code> also answers as <code>nas.${escapeHtml(data.domain)}</code>.`
            : 'No local domain is set (it is set in <a href="#" data-goto="settings">Settings</a>), so names answer exactly as written.';
        return `<section class="card">
            <div class="net-card-head"><h2>Local names</h2><span class="tag">${data.hosts.length}</span>
                <button type="button" class="cfg-btn dns-add" data-kind="host" style="margin-left:auto"${lock ? ' disabled' : ''}>Add name</button></div>
            <p class="form-hint">Names the router answers itself. The usual use: <code>home.example.com</code> pointing at a server&rsquo;s inside address, so devices inside reach a service you port-forward without leaving the network. ${domain}</p>
            <div class="fw-list">${data.hosts.length ? data.hosts.map(h => rowHtml('host', h, `<strong>${escapeHtml(h.name)}</strong>`, escapeHtml(h.text))).join('') : '<div class="fw-row fw-empty">No local names yet.</div>'}</div>
        </section>${editing && editing.kind === 'host' ? hostEditorHtml() : ''}`;
    }

    function forwardsHtml() {
        const lock = !editable() || busy || !!editing;
        return `<section class="card">
            <div class="net-card-head"><h2>Domain forwarding</h2><span class="tag">${data.forwards.length}</span>
                <button type="button" class="cfg-btn dns-add" data-kind="forward" style="margin-left:auto"${lock ? ' disabled' : ''}>Add domain</button></div>
            <p class="form-hint">Ask a different server about one domain and everything under it, for example <code>corp.example.com</code> to your company&rsquo;s DNS server.</p>
            <div class="fw-list">${data.forwards.length ? data.forwards.map(f => rowHtml('forward', f, `<strong>${escapeHtml(f.domain)}</strong>`, escapeHtml(f.text))).join('') : '<div class="fw-row fw-empty">No forwarded domains yet.</div>'}</div>
        </section>${editing && editing.kind === 'forward' ? forwardEditorHtml() : ''}`;
    }

    function fieldHtml(label, id, control, key, hint) {
        return `<div class="form-field"><label for="${id}">${label}</label>${control}${hint ? `<p class="form-hint">${hint}</p>` : ''}${errorText(editing.errors[key])}</div>`;
    }

    function textInput(id, key, placeholder, extra) {
        const lock = !editable() || busy;
        return `<input type="text" id="${id}" data-ed="${key}" value="${escapeHtml(editing.form[key])}" placeholder="${escapeHtml(placeholder)}" autocomplete="off" spellcheck="false" autocapitalize="off"${extra || ''}${editing.errors[key] ? ' aria-invalid="true"' : ''}${lock ? ' disabled' : ''}>`;
    }

    function editorFooter(saveLabel) {
        const lock = !editable() || busy;
        return `${editing.message ? `<div class="form-message" role="alert">${escapeHtml(editing.message)}</div>` : ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="dns-ed-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="dns-ed-save"${lock ? ' disabled' : ''}>${saveLabel}</button>
            </div>`;
    }

    function hostEditorHtml() {
        return `<section class="card fw-editor" id="dns-editor">
            <div class="net-card-head"><h2>${editing.id ? 'Edit local name' : 'New local name'}</h2></div>
            <div class="form-grid">
                ${fieldHtml('Name', 'dns-ed-name', textInput('dns-ed-name', 'name', 'nas or home.example.com'), 'name', 'Lower case; letters, digits and hyphens.')}
                ${fieldHtml('Address', 'dns-ed-address', textInput('dns-ed-address', 'address', '192.168.1.50', ' inputmode="decimal"'), 'address')}
            </div>
            <div class="net-access"><label class="toggle toggle--block"><input type="checkbox" data-ed="enabled"${editing.form.enabled ? ' checked' : ''}${!editable() || busy ? ' disabled' : ''}><span><strong>Switched on</strong><span class="form-hint">Off keeps it here without applying it.</span></span></label></div>
            ${editorFooter(editing.id ? 'Save to draft' : 'Add name')}</section>`;
    }

    function forwardEditorHtml() {
        return `<section class="card fw-editor" id="dns-editor">
            <div class="net-card-head"><h2>${editing.id ? 'Edit forwarded domain' : 'New forwarded domain'}</h2></div>
            <div class="form-grid">
                ${fieldHtml('Domain', 'dns-ed-domain', textInput('dns-ed-domain', 'domain', 'corp.example.com'), 'domain', 'It and everything under it.')}
                ${fieldHtml('Ask these servers', 'dns-ed-servers', textInput('dns-ed-servers', 'servers', '10.0.0.5, 10.0.0.6', ' inputmode="decimal"'), 'servers', 'Up to three, separated by commas.')}
            </div>
            <div class="net-access"><label class="toggle toggle--block"><input type="checkbox" data-ed="enabled"${editing.form.enabled ? ' checked' : ''}${!editable() || busy ? ' disabled' : ''}><span><strong>Switched on</strong><span class="form-hint">Off keeps it here without applying it.</span></span></label></div>
            ${editorFooter(editing.id ? 'Save to draft' : 'Add domain')}</section>`;
    }

    function render() {
        if (!data) return;
        $('dns-body').innerHTML = upstreamHtml() + networksHtml() + hostsHtml() + forwardsHtml() +
            (!editable() ? '<p class="form-hint">A change is being applied or waits for confirmation: DNS cannot be edited right now.</p>' : '');
    }

    // ---------------------------------------------------------------- loading and saving

    async function load() {
        const res = await apiFetch('/api/v1/dns');
        if (!res || !res.ok) { showMessage('Could not load the DNS settings.', 'error'); return; }
        adopt(await res.json());
        render();
    }

    async function saveUpstream() {
        const f = upstream;
        f.errors = {}; f.message = null;
        busy = true; render();
        const res = await apiFetch('/api/v1/dns/upstream', { method: 'PUT', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ servers: f.mode === 'custom' ? split(f.servers) : [] }) });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, 'Saving failed.');
            e.fields.forEach(x => { f.errors.servers = (f.errors.servers ? f.errors.servers + ' ' : '') + x.message; });
            f.message = Object.keys(f.errors).length ? 'Some settings need attention.' : e.general;
            render();
            return;
        }
        upstream = null;
        await load();
        afterChange('Saved as a draft. Review and commit it from the bar at the top when you are ready.');
    }

    async function saveNetwork(id) {
        const f = nets[id];
        f.errors = {}; f.message = null;
        busy = true; render();
        const res = await apiFetch('/api/v1/dns/networks/' + encodeURIComponent(id), { method: 'PUT', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ servers: f.mode === 'custom' ? split(f.servers) : [] }) });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, 'Saving failed.');
            e.fields.forEach(x => { f.errors.servers = (f.errors.servers ? f.errors.servers + ' ' : '') + x.message; });
            f.message = Object.keys(f.errors).length ? 'Some settings need attention.' : e.general;
            render();
            return;
        }
        delete nets[id];
        await load();
        afterChange('Saved as a draft. Review and commit it from the bar at the top when you are ready.');
    }

    function openEditor(kind, id) {
        const list = kind === 'host' ? data.hosts : data.forwards;
        const e = id ? list.find(x => x.id === id) : null;
        const form = kind === 'host'
            ? (e ? { name: e.name, address: e.address, enabled: e.enabled } : { name: '', address: '', enabled: true })
            : (e ? { domain: e.domain, servers: join(e.servers), enabled: e.enabled } : { domain: '', servers: '', enabled: true });
        editing = { kind, id: id || null, form, errors: {}, message: null };
        render();
        const first = $(kind === 'host' ? 'dns-ed-name' : 'dns-ed-domain');
        if (first) first.focus();
    }

    async function saveEditor() {
        const { kind, form } = editing;
        const body = kind === 'host'
            ? { name: form.name, address: form.address, enabled: form.enabled }
            : { domain: form.domain, servers: split(form.servers), enabled: form.enabled };
        const base = kind === 'host' ? '/api/v1/dns/hosts' : '/api/v1/dns/forwards';
        editing.errors = {}; editing.message = null;
        busy = true; render();
        const res = await apiFetch(editing.id ? `${base}/${encodeURIComponent(editing.id)}` : base, {
            method: editing.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, 'Saving failed.');
            const general = [];
            e.fields.forEach(x => {
                if (['name', 'address', 'domain', 'servers'].includes(x.field)) editing.errors[x.field] = (editing.errors[x.field] ? editing.errors[x.field] + ' ' : '') + x.message;
                else general.push(x.message);
            });
            editing.message = general.join(' ') || (Object.keys(editing.errors).length ? 'Some settings need attention.' : e.general);
            render();
            return;
        }
        const wasNew = !editing.id;
        editing = null;
        await load();
        afterChange(wasNew ? 'Added to the draft. Review and commit it from the bar at the top when you are ready.' : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.');
    }

    async function toggle(kind, id, on) {
        const base = kind === 'host' ? '/api/v1/dns/hosts' : '/api/v1/dns/forwards';
        busy = true; render();
        const res = await apiFetch(`${base}/${encodeURIComponent(id)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: on }) });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'The change could not be saved.'); showMessage(e.fields.map(x => x.message).join(' ') || e.general, 'error'); await load(); return; }
        await load();
        afterChange(on ? 'Switched on in the draft.' : 'Switched off in the draft.');
    }

    async function remove(kind, id) {
        const key = `${kind}:${id}`;
        if (confirmDelete !== key) {
            confirmDelete = key; render();
            setTimeout(() => { if (confirmDelete === key) { confirmDelete = null; render(); } }, 4000);
            return;
        }
        confirmDelete = null;
        const base = kind === 'host' ? '/api/v1/dns/hosts' : '/api/v1/dns/forwards';
        busy = true; render();
        const res = await apiFetch(`${base}/${encodeURIComponent(id)}`, { method: 'DELETE' });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'It could not be deleted.'); showMessage(e.general, 'error'); await load(); return; }
        await load();
        afterChange('Removed from the draft. Review and commit it from the bar at the top when you are ready.');
    }

    // ---------------------------------------------------------------- events

    function refreshButtons() {
        // typing must not rebuild the page (the focus would be lost): only the buttons and notes follow
        const up = $('dns-up-save');
        if (up && data) {
            const d = upstreamDirty();
            up.disabled = !editable() || busy || !d;
            $('dns-up-reset').disabled = busy || !d;
            $('dns-up-state').textContent = d ? 'You have unsaved changes.' : '';
        }
        (data ? data.networks : []).forEach(n => {
            const d = netDirty(n);
            const save = document.querySelector(`.dns-net-save[data-id="${n.id}"]`);
            if (save) save.disabled = !editable() || busy || !d;
            const reset = document.querySelector(`.dns-net-reset[data-id="${n.id}"]`);
            if (reset) reset.disabled = busy || !d;
            const state = document.querySelector(`.dns-net-state[data-id="${n.id}"]`);
            if (state) state.textContent = d ? 'You have unsaved changes.' : '';
        });
    }

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('dns-body');
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            const { kind, id } = btn.dataset;
            if (btn.id === 'dns-up-save') saveUpstream();
            else if (btn.id === 'dns-up-reset') { upstream = upstreamFormOf(data.upstream); render(); }
            else if (btn.classList.contains('dns-net-save')) saveNetwork(id);
            else if (btn.classList.contains('dns-net-reset')) { nets[id] = netFormOf(data.networks.find(n => n.id === id)); render(); }
            else if (btn.classList.contains('dns-add')) openEditor(kind, null);
            else if (btn.classList.contains('dns-edit')) openEditor(kind, id);
            else if (btn.classList.contains('dns-delete')) remove(kind, id);
            else if (btn.id === 'dns-ed-save') saveEditor();
            else if (btn.id === 'dns-ed-cancel') { editing = null; render(); }
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.classList.contains('dns-enable')) { toggle(el.dataset.kind, el.dataset.id, el.checked); return; }
            if (el.dataset.up === 'mode') { upstream.mode = el.value; delete upstream.errors.servers; render(); return; }
            if (el.dataset.net && el.dataset.field === 'mode') { nets[el.dataset.net].mode = el.value; delete nets[el.dataset.net].errors.servers; render(); return; }
            if (el.dataset.ed && editing) { editing.form[el.dataset.ed] = el.type === 'checkbox' ? el.checked : el.value; }
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            if (el.dataset.up === 'servers') { upstream.servers = el.value; delete upstream.errors.servers; refreshButtons(); }
            else if (el.dataset.net && el.dataset.field === 'servers') { nets[el.dataset.net].servers = el.value; delete nets[el.dataset.net].errors.servers; refreshButtons(); }
            else if (el.dataset.ed && editing && el.type !== 'checkbox') {
                editing.form[el.dataset.ed] = el.value;
                delete editing.errors[el.dataset.ed];
                el.removeAttribute('aria-invalid');
                const msg = el.closest('.form-field').querySelector('.field-error');
                if (msg) msg.remove();
            }
            if (el.classList && (el.dataset.up || el.dataset.net)) {
                el.removeAttribute('aria-invalid');
                const msg = el.closest('.form-field').querySelector('.field-error');
                if (msg) msg.remove();
            }
        });
        body.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter' || e.target.tagName !== 'INPUT' || e.target.type !== 'text' || busy || !editable()) return;
            if (editing && e.target.dataset.ed) { e.preventDefault(); saveEditor(); }
            else if (e.target.dataset.up && upstreamDirty()) { e.preventDefault(); saveUpstream(); }
            else if (e.target.dataset.net && netDirty(data.networks.find(n => n.id === e.target.dataset.net))) { e.preventDefault(); saveNetwork(e.target.dataset.net); }
        });
    }

    function init() {
        wire();
        showMessage('');
        data = null; upstream = null; nets = {}; editing = null; confirmDelete = null;
        load();
        return () => {};
    }

    return { init };
})();
